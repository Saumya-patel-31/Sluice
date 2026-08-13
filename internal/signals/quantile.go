package signals

import (
	"math"
	"sort"
	"sync"
)

// P2Quantile is a streaming quantile estimator using the P-square algorithm
// (Jain & Chlamtac, 1985). It tracks a single quantile in O(1) memory and O(1)
// time per observation, with no sample retention.
//
// This matters here because Sluice probes every backend continuously and keeps
// per-backend latency distributions in the hot path of every routing decision.
// Storing samples to sort later would cost memory proportional to probe rate
// times backend count; P-square costs 5 float64s per backend regardless.
//
// P2Quantile is not safe for concurrent use; RollingQuantile wraps it with a
// mutex and a sliding window.
type P2Quantile struct {
	p float64 // target quantile in (0,1)

	q  [5]float64 // marker heights
	n  [5]int     // marker positions
	np [5]float64 // desired marker positions
	dn [5]float64 // desired position increments

	count int
}

// NewP2Quantile returns an estimator for the p-th quantile, p in (0,1).
func NewP2Quantile(p float64) *P2Quantile {
	e := &P2Quantile{p: p}
	e.dn = [5]float64{0, p / 2, p, (1 + p) / 2, 1}
	return e
}

// Count returns how many observations have been recorded.
func (e *P2Quantile) Count() int { return e.count }

// Add records an observation.
func (e *P2Quantile) Add(x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return
	}

	// Bootstrap: the first five observations become the initial markers.
	if e.count < 5 {
		e.q[e.count] = x
		e.count++
		if e.count == 5 {
			sort.Float64s(e.q[:])
			for i := 0; i < 5; i++ {
				e.n[i] = i + 1
				e.np[i] = 1 + 4*e.dn[i]
			}
		}
		return
	}

	// Locate the cell containing x, extending the extreme markers if needed.
	var k int
	switch {
	case x < e.q[0]:
		e.q[0] = x
		k = 0
	case x >= e.q[4]:
		e.q[4] = x
		k = 3
	default:
		k = 3
		for i := 1; i < 4; i++ {
			if x < e.q[i] {
				k = i - 1
				break
			}
		}
	}

	for i := k + 1; i < 5; i++ {
		e.n[i]++
	}
	for i := 0; i < 5; i++ {
		e.np[i] += e.dn[i]
	}

	// Nudge the three interior markers toward their desired positions,
	// preferring the piecewise-parabolic prediction and falling back to
	// linear when parabolic would violate marker ordering.
	for i := 1; i < 4; i++ {
		d := e.np[i] - float64(e.n[i])
		if (d >= 1 && e.n[i+1]-e.n[i] > 1) || (d <= -1 && e.n[i-1]-e.n[i] < -1) {
			sign := 1
			if d < 0 {
				sign = -1
			}
			cand := e.parabolic(i, float64(sign))
			if e.q[i-1] < cand && cand < e.q[i+1] {
				e.q[i] = cand
			} else {
				e.q[i] = e.linear(i, sign)
			}
			e.n[i] += sign
		}
	}
	e.count++
}

func (e *P2Quantile) parabolic(i int, d float64) float64 {
	nPrev := float64(e.n[i-1])
	nCur := float64(e.n[i])
	nNext := float64(e.n[i+1])
	return e.q[i] + d/(nNext-nPrev)*
		((nCur-nPrev+d)*(e.q[i+1]-e.q[i])/(nNext-nCur)+
			(nNext-nCur-d)*(e.q[i]-e.q[i-1])/(nCur-nPrev))
}

func (e *P2Quantile) linear(i, d int) float64 {
	return e.q[i] + float64(d)*(e.q[i+d]-e.q[i])/float64(e.n[i+d]-e.n[i])
}

// Value returns the current quantile estimate. With fewer than five
// observations it falls back to exact interpolation over what it has, so the
// estimator is usable from the first sample rather than only after warm-up.
func (e *P2Quantile) Value() float64 {
	if e.count == 0 {
		return 0
	}
	if e.count < 5 {
		s := make([]float64, e.count)
		copy(s, e.q[:e.count])
		sort.Float64s(s)
		idx := e.p * float64(len(s)-1)
		lo := int(math.Floor(idx))
		hi := int(math.Ceil(idx))
		if lo == hi {
			return s[lo]
		}
		frac := idx - float64(lo)
		return s[lo]*(1-frac) + s[hi]*frac
	}
	return e.q[2]
}

// Reset returns the estimator to its empty state, reusing the allocation.
func (e *P2Quantile) Reset() {
	e.q = [5]float64{}
	e.n = [5]int{}
	e.np = [5]float64{}
	e.count = 0
}

// -----------------------------------------------------------------------------
// Rolling window
// -----------------------------------------------------------------------------

// RollingQuantile keeps a quantile estimate over a sliding time window by
// rotating between two P-square estimators.
//
// A plain P-square estimator weights an observation from an hour ago exactly
// as heavily as one from a second ago, which makes it useless for detecting
// that a region just got slow. Rotating two half-window estimators bounds how
// long stale data can influence the answer to one window, while always having
// a fully warmed estimator to read from.
type RollingQuantile struct {
	mu       sync.Mutex
	window   int64 // nanoseconds
	cur      *P2Quantile
	prev     *P2Quantile
	rotateAt int64
	nowFn    func() int64
}

// NewRollingQuantile returns an estimator for quantile p over windowNanos.
func NewRollingQuantile(p float64, windowNanos int64, nowFn func() int64) *RollingQuantile {
	return &RollingQuantile{
		window:   windowNanos,
		cur:      NewP2Quantile(p),
		prev:     NewP2Quantile(p),
		rotateAt: nowFn() + windowNanos,
		nowFn:    nowFn,
	}
}

// Add records an observation, rotating the window first if it has elapsed.
func (r *RollingQuantile) Add(x float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeRotateLocked()
	r.cur.Add(x)
}

func (r *RollingQuantile) maybeRotateLocked() {
	now := r.nowFn()
	if now < r.rotateAt {
		return
	}
	// More than one window may have elapsed under low traffic; in that case
	// both estimators are stale and we start clean rather than reporting an
	// answer derived from arbitrarily old samples.
	elapsed := now - r.rotateAt
	if elapsed >= r.window {
		r.prev.Reset()
	} else {
		r.prev, r.cur = r.cur, r.prev
		r.cur.Reset()
	}
	r.rotateAt = now + r.window
}

// Value returns the estimate, preferring the current window once it has
// enough samples to be meaningful and falling back to the previous one while
// the current window is still filling.
func (r *RollingQuantile) Value() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeRotateLocked()
	if r.cur.Count() >= 5 {
		return r.cur.Value()
	}
	if r.prev.Count() > 0 {
		return r.prev.Value()
	}
	return r.cur.Value()
}

// Count returns the number of observations in the active window.
func (r *RollingQuantile) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.Count()
}

// -----------------------------------------------------------------------------
// EWMA
// -----------------------------------------------------------------------------

// EWMA is a time-decayed exponentially weighted moving average. Decay is
// computed from elapsed wall time rather than sample count, so a backend that
// stops receiving probes decays toward its last value at the same rate as one
// probed continuously — sample-count decay would freeze a silent backend's
// reading forever and hide an outage.
type EWMA struct {
	mu       sync.Mutex
	halfLife float64 // nanoseconds
	value    float64
	last     int64
	init     bool
	nowFn    func() int64
}

// NewEWMA returns an EWMA with the given half-life in nanoseconds.
func NewEWMA(halfLifeNanos int64, nowFn func() int64) *EWMA {
	return &EWMA{halfLife: float64(halfLifeNanos), nowFn: nowFn}
}

// Add folds an observation into the average.
func (e *EWMA) Add(x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.nowFn()
	if !e.init {
		e.value, e.last, e.init = x, now, true
		return
	}
	dt := float64(now - e.last)
	if dt < 0 {
		dt = 0
	}
	alpha := 1 - math.Exp2(-dt/e.halfLife)
	e.value += alpha * (x - e.value)
	e.last = now
}

// Value returns the current average, or 0 before the first observation.
func (e *EWMA) Value() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.value
}

// Initialized reports whether any observation has been recorded.
func (e *EWMA) Initialized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.init
}
