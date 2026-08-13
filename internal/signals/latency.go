package signals

import "time"

// LatencyTracker maintains the median and tail latency for one backend over a
// sliding window.
//
// The router scores on p95 rather than the mean: an average hides the case
// where a region is fine most of the time and catastrophic occasionally, which
// is precisely the failure mode that cost-driven routing would otherwise walk
// straight into. p50 is retained for display, so an operator can see the gap
// between typical and tail behaviour.
type LatencyTracker struct {
	p50 *RollingQuantile
	p95 *RollingQuantile
}

// NewLatencyTracker returns a tracker over the given sliding window.
func NewLatencyTracker(window time.Duration, nowFn func() int64) *LatencyTracker {
	return &LatencyTracker{
		p50: NewRollingQuantile(0.50, int64(window), nowFn),
		p95: NewRollingQuantile(0.95, int64(window), nowFn),
	}
}

// Observe records a round-trip time.
func (t *LatencyTracker) Observe(rtt time.Duration) {
	ms := float64(rtt) / float64(time.Millisecond)
	t.p50.Add(ms)
	t.p95.Add(ms)
}

// Values returns the current p50 and p95 in milliseconds.
func (t *LatencyTracker) Values() (p50, p95 float64) {
	return t.p50.Value(), t.p95.Value()
}

// Samples returns the number of observations in the active window.
func (t *LatencyTracker) Samples() int { return t.p95.Count() }
