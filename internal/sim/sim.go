// Package sim provides a self-contained multi-cloud environment: synthetic
// upstreams that behave like real regions, drifting prices, and injectable
// incidents.
//
// The simulator exists so that `sluiced --demo` produces a genuinely closed
// loop with no cloud accounts, no credentials and no network. The upstreams
// are real HTTP servers on real sockets, so the prober, the proxy and the
// byte accounting all exercise the same code paths they would in production.
// Nothing about the router is mocked; only the world it observes is.
package sim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/signals"
)

// IncidentKind classifies a fault the operator can inject.
type IncidentKind string

const (
	// IncidentBrownout multiplies a region's latency without failing requests.
	// This is the interesting case: nothing is down, the SLO is just quietly
	// being missed, and a latency-blind cost optimiser would keep sending
	// traffic there.
	IncidentBrownout IncidentKind = "brownout"
	// IncidentOutage makes a region return errors.
	IncidentOutage IncidentKind = "outage"
	// IncidentPriceSpike raises a region's egress price, as a surprise
	// tier change or an expiring commitment would.
	IncidentPriceSpike IncidentKind = "price_spike"
	// IncidentCarbonSpike raises a region's grid intensity, as a still,
	// cloudy evening does to a renewables-heavy grid.
	IncidentCarbonSpike IncidentKind = "carbon_spike"
)

// Valid reports whether k is a known incident kind.
func (k IncidentKind) Valid() bool {
	switch k {
	case IncidentBrownout, IncidentOutage, IncidentPriceSpike, IncidentCarbonSpike:
		return true
	}
	return false
}

// Incident is an injected fault with a lifetime.
type Incident struct {
	ID        string       `json:"id"`
	Kind      IncidentKind `json:"kind"`
	BackendID string       `json:"backendId"`
	// Magnitude is a multiplier for brownout, price and carbon spikes, and an
	// absolute error rate in [0,1] for outages.
	Magnitude float64   `json:"magnitude"`
	StartedAt time.Time `json:"startedAt"`
	EndsAt    time.Time `json:"endsAt"`
	Note      string    `json:"note"`
}

// Active reports whether the incident is in effect at t.
func (i Incident) Active(t time.Time) bool {
	return !t.Before(i.StartedAt) && t.Before(i.EndsAt)
}

// Remaining returns how long the incident has left.
func (i Incident) Remaining(t time.Time) time.Duration {
	if !i.Active(t) {
		return 0
	}
	return i.EndsAt.Sub(t)
}

// -----------------------------------------------------------------------------
// Upstream
// -----------------------------------------------------------------------------

// Upstream is one synthetic region: a real HTTP server whose latency and error
// behaviour follow a configured profile.
type Upstream struct {
	BackendID   string
	Region      string
	Cloud       model.Cloud
	baseLatency time.Duration
	baseErrors  float64

	ln  net.Listener
	srv *http.Server

	served int64
	failed int64
	mu     sync.Mutex
}

// Addr returns the upstream's origin URL.
func (u *Upstream) Addr() string {
	if u.ln == nil {
		return ""
	}
	return "http://" + u.ln.Addr().String()
}

// sampleLatency draws a round-trip time from a lognormal body with an
// occasional heavy tail.
//
// A fixed base plus uniform jitter would give p95 barely above p50, and the
// whole point of scoring on p95 is that real regions have tails. The 2% spike
// branch is what produces a p95 meaningfully worse than the median, which is
// what the SLO guardrail and the breaker are there to react to.
func sampleLatency(base time.Duration, mult float64) time.Duration {
	if base <= 0 {
		base = time.Millisecond
	}
	v := float64(base) * mult * math.Exp(0.22*mrand.NormFloat64())
	if mrand.Float64() < 0.02 {
		v *= 2.5 + 5*mrand.Float64()
	}
	if v < float64(time.Millisecond) {
		v = float64(time.Millisecond)
	}
	return time.Duration(v)
}

func (u *Upstream) handler(fleet *Fleet) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mult, errRate := fleet.effects(u.BackendID)
		// Probes traverse the same network as requests, so they pay a share
		// of the latency. They do not pay the full request-processing cost.
		time.Sleep(sampleLatency(u.baseLatency/2, mult))
		if mrand.Float64() < errRate {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Sluice-Backend", u.BackendID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mult, errRate := fleet.effects(u.BackendID)
		time.Sleep(sampleLatency(u.baseLatency, mult))

		u.mu.Lock()
		u.served++
		u.mu.Unlock()

		if mrand.Float64() < errRate {
			u.mu.Lock()
			u.failed++
			u.mu.Unlock()
			http.Error(w, "upstream failure", http.StatusBadGateway)
			return
		}

		size := 8 << 10
		if v := r.Header.Get("X-Sluice-Size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8<<20 {
				size = n
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Sluice-Backend", u.BackendID)
		w.Header().Set("X-Sluice-Region", u.Region)
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload(size))
	})

	return mux
}

// payloadPool serves response bodies without allocating per request.
var payloadPool = struct {
	sync.Mutex
	buf []byte
}{}

func payload(n int) []byte {
	payloadPool.Lock()
	defer payloadPool.Unlock()
	if len(payloadPool.buf) < n {
		grown := make([]byte, n)
		for i := range grown {
			grown[i] = byte('a' + i%26)
		}
		payloadPool.buf = grown
	}
	return payloadPool.buf[:n]
}

// Stats returns lifetime request counts for this upstream.
func (u *Upstream) Stats() (served, failed int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.served, u.failed
}

// -----------------------------------------------------------------------------
// Fleet
// -----------------------------------------------------------------------------

// Fleet is the set of synthetic upstreams plus the active incident list.
type Fleet struct {
	log *slog.Logger

	mu        sync.RWMutex
	upstreams map[string]*Upstream
	order     []string
	incidents []Incident
}

// NewFleet builds an upstream for every backend, reading its latency and
// error profile from the backend's sim.* labels.
func NewFleet(backends []model.Backend, log *slog.Logger) (*Fleet, error) {
	if log == nil {
		log = slog.Default()
	}
	f := &Fleet{log: log, upstreams: make(map[string]*Upstream, len(backends))}

	for _, b := range backends {
		latency := 40 * time.Millisecond
		if v := b.Label("sim.latencyMs"); v != "" {
			if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
				latency = time.Duration(ms * float64(time.Millisecond))
			}
		}
		errRate := 0.001
		if v := b.Label("sim.errorRate"); v != "" {
			if e, err := strconv.ParseFloat(v, 64); err == nil && e >= 0 {
				errRate = e
			}
		}

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			f.closeAll()
			return nil, fmt.Errorf("sim: listen for %s: %w", b.ID, err)
		}

		u := &Upstream{
			BackendID: b.ID, Region: b.Region, Cloud: b.Cloud,
			baseLatency: latency, baseErrors: errRate, ln: ln,
		}
		u.srv = &http.Server{
			Handler:           u.handler(f),
			ReadHeaderTimeout: 5 * time.Second,
		}
		f.upstreams[b.ID] = u
		f.order = append(f.order, b.ID)
	}
	return f, nil
}

// Start serves every upstream.
func (f *Fleet) Start() {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, id := range f.order {
		u := f.upstreams[id]
		go func(u *Upstream) {
			if err := u.srv.Serve(u.ln); err != nil && err != http.ErrServerClosed {
				f.log.Warn("sim upstream stopped", "backend", u.BackendID, "err", err)
			}
		}(u)
	}
}

// Close shuts every upstream down.
func (f *Fleet) Close() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.closeAllLocked()
}

func (f *Fleet) closeAll() { f.mu.RLock(); defer f.mu.RUnlock(); _ = f.closeAllLocked() }

func (f *Fleet) closeAllLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var firstErr error
	for _, id := range f.order {
		u := f.upstreams[id]
		if u.srv != nil {
			if err := u.srv.Shutdown(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Addr returns an upstream's origin URL.
func (f *Fleet) Addr(backendID string) string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if u, ok := f.upstreams[backendID]; ok {
		return u.Addr()
	}
	return ""
}

// Upstreams returns the synthetic regions.
func (f *Fleet) Upstreams() []*Upstream {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*Upstream, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.upstreams[id])
	}
	return out
}

// effects resolves the current latency multiplier and error rate for a
// backend, folding in any active incidents.
func (f *Fleet) effects(backendID string) (latencyMult, errorRate float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	u := f.upstreams[backendID]
	latencyMult, errorRate = 1, 0
	if u != nil {
		errorRate = u.baseErrors
	}

	now := time.Now()
	for _, inc := range f.incidents {
		if inc.BackendID != backendID || !inc.Active(now) {
			continue
		}
		switch inc.Kind {
		case IncidentBrownout:
			latencyMult *= inc.Magnitude
		case IncidentOutage:
			if inc.Magnitude > errorRate {
				errorRate = inc.Magnitude
			}
		}
	}
	return latencyMult, errorRate
}

// SignalMultipliers returns the price and carbon multipliers in effect for a
// backend, consumed by the drifting price and carbon sources.
func (f *Fleet) SignalMultipliers(backendID string) (price, carbon float64) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	price, carbon = 1, 1
	now := time.Now()
	for _, inc := range f.incidents {
		if inc.BackendID != backendID || !inc.Active(now) {
			continue
		}
		switch inc.Kind {
		case IncidentPriceSpike:
			price *= inc.Magnitude
		case IncidentCarbonSpike:
			carbon *= inc.Magnitude
		}
	}
	return price, carbon
}

// Inject adds an incident, assigning an ID and start time if unset.
func (f *Fleet) Inject(inc Incident) (Incident, error) {
	if !inc.Kind.Valid() {
		return Incident{}, fmt.Errorf("sim: unknown incident kind %q", inc.Kind)
	}
	f.mu.RLock()
	_, ok := f.upstreams[inc.BackendID]
	f.mu.RUnlock()
	if !ok {
		return Incident{}, fmt.Errorf("sim: unknown backend %q", inc.BackendID)
	}

	if inc.ID == "" {
		var b [5]byte
		_, _ = rand.Read(b[:])
		inc.ID = "inc_" + hex.EncodeToString(b[:])
	}
	if inc.StartedAt.IsZero() {
		inc.StartedAt = time.Now()
	}
	if inc.EndsAt.IsZero() {
		inc.EndsAt = inc.StartedAt.Add(90 * time.Second)
	}
	if inc.Magnitude <= 0 {
		switch inc.Kind {
		case IncidentOutage:
			inc.Magnitude = 0.85
		default:
			inc.Magnitude = 4
		}
	}
	if inc.Kind == IncidentOutage && inc.Magnitude > 1 {
		inc.Magnitude = 1
	}

	f.mu.Lock()
	f.incidents = append(f.incidents, inc)
	f.pruneLocked()
	f.mu.Unlock()

	f.log.Info("incident injected",
		"id", inc.ID, "kind", inc.Kind, "backend", inc.BackendID,
		"magnitude", inc.Magnitude, "for", inc.EndsAt.Sub(inc.StartedAt))
	return inc, nil
}

// Resolve ends an incident early.
func (f *Fleet) Resolve(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.incidents {
		if f.incidents[i].ID == id {
			f.incidents[i].EndsAt = time.Now()
			return true
		}
	}
	return false
}

// Incidents returns incidents that are active or ended recently.
func (f *Fleet) Incidents() []Incident {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]Incident, len(f.incidents))
	copy(out, f.incidents)
	return out
}

// ActiveIncidents returns only those currently in effect.
func (f *Fleet) ActiveIncidents() []Incident {
	now := time.Now()
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []Incident
	for _, inc := range f.incidents {
		if inc.Active(now) {
			out = append(out, inc)
		}
	}
	return out
}

// pruneLocked drops incidents that ended more than five minutes ago, so the
// list stays useful as a recent-history view without growing forever.
func (f *Fleet) pruneLocked() {
	cutoff := time.Now().Add(-5 * time.Minute)
	kept := f.incidents[:0]
	for _, inc := range f.incidents {
		if inc.EndsAt.After(cutoff) {
			kept = append(kept, inc)
		}
	}
	f.incidents = kept
}

// -----------------------------------------------------------------------------
// Drifting signal sources
// -----------------------------------------------------------------------------

// DriftingPricer applies slow market drift and incident spikes on top of the
// bundled list prices.
//
// Real egress bills move: volume tiers roll over, commitments lapse, spot
// capacity reprices. A static table would make the cost dimension inert and
// the whole cost-aware routing story untestable, so the demo gives prices a
// gentle oscillation with a per-region phase.
type DriftingPricer struct {
	Fleet *Fleet
	Base  signals.Pricer
	// Amplitude is the fractional swing around the list price.
	Amplitude float64
	// Period is how long a full drift cycle takes.
	Period time.Duration
	Now    func() time.Time
}

// Name identifies the source.
func (d *DriftingPricer) Name() string { return "sim:drifting-price" }

// Price returns the drifted price for a backend.
func (d *DriftingPricer) Price(ctx context.Context, b model.Backend) (float64, bool, error) {
	base, ok, err := d.Base.Price(ctx, b)
	if err != nil || !ok {
		return 0, ok, err
	}

	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	period := d.Period
	if period <= 0 {
		period = 6 * time.Minute
	}
	amp := d.Amplitude
	if amp <= 0 {
		amp = 0.18
	}

	// A per-backend phase offset keeps regions from moving in lockstep, which
	// would make the cost dimension flat across candidates and therefore
	// unable to discriminate.
	phase := float64(hashString(b.ID)%1000) / 1000 * 2 * math.Pi
	t := float64(now().UnixNano()) / float64(period)
	drift := 1 + amp*math.Sin(2*math.Pi*t+phase)

	spike, _ := d.Fleet.SignalMultipliers(b.ID)
	return base * drift * spike, true, nil
}

// DriftingCarbon applies incident spikes on top of the modeled diurnal curve.
type DriftingCarbon struct {
	Fleet *Fleet
	Base  signals.CarbonSource
}

// Name identifies the source.
func (d *DriftingCarbon) Name() string { return "sim:" + d.Base.Name() }

// Intensity returns the modeled intensity with any active spike applied.
func (d *DriftingCarbon) Intensity(ctx context.Context, b model.Backend) (float64, bool, error) {
	v, ok, err := d.Base.Intensity(ctx, b)
	if err != nil || !ok {
		return 0, ok, err
	}
	_, spike := d.Fleet.SignalMultipliers(b.ID)
	return v * spike, true, nil
}

func hashString(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
