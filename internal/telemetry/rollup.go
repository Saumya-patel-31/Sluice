package telemetry

import (
	"sort"
	"sync"
	"time"

	"github.com/saumyapatel/sluice/internal/signals"
)

// Rollup keeps named aggregate time series on a fixed cadence.
//
// The per-backend history in the signal store answers "what did this region
// look like"; a rollup answers "what did the fleet look like" — traffic by
// cloud, savings rate, denial rate. Sampling on a timer rather than per event
// gives the dashboard an even time base, so a chart's x-axis means elapsed
// time regardless of how bursty the traffic was.
type Rollup struct {
	mu       sync.RWMutex
	capacity int
	series   map[string]*signals.Series
	order    []string
}

// NewRollup returns a rollup retaining capacity samples per series.
func NewRollup(capacity int) *Rollup {
	if capacity < 1 {
		capacity = 300
	}
	return &Rollup{capacity: capacity, series: make(map[string]*signals.Series)}
}

// Observe appends a sample to a named series, creating it on first use.
func (r *Rollup) Observe(key string, t time.Time, v float64) {
	r.mu.RLock()
	s, ok := r.series[key]
	r.mu.RUnlock()
	if ok {
		s.Add(t, v)
		return
	}

	r.mu.Lock()
	if s, ok = r.series[key]; !ok {
		s = signals.NewSeries(r.capacity)
		r.series[key] = s
		r.order = append(r.order, key)
		sort.Strings(r.order)
	}
	r.mu.Unlock()
	s.Add(t, v)
}

// ObserveMany appends one sample to each named series at a shared timestamp,
// so a stacked chart's components always line up on the x-axis.
func (r *Rollup) ObserveMany(t time.Time, values map[string]float64) {
	for k, v := range values {
		r.Observe(k, t, v)
	}
}

// Series returns a series downsampled to at most n points.
func (r *Rollup) Series(key string, n int) []signals.Point {
	r.mu.RLock()
	s, ok := r.series[key]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	return s.Downsample(n)
}

// Keys returns the known series names in sorted order.
func (r *Rollup) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// All returns every series downsampled to at most n points.
func (r *Rollup) All(n int) map[string][]signals.Point {
	keys := r.Keys()
	out := make(map[string][]signals.Point, len(keys))
	for _, k := range keys {
		out[k] = r.Series(k, n)
	}
	return out
}
