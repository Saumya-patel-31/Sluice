// Package telemetry provides Prometheus-compatible instrumentation and the
// decision ledger that makes routing choices explainable after the fact.
package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metric families and renders them in the Prometheus text
// exposition format.
//
// This is a deliberate ~300 lines rather than a dependency. Sluice ships as a
// single static binary that operators drop into a cluster, and the exposition
// format is a stable, documented contract — implementing it directly keeps the
// supply chain to zero third-party modules, which for a component that sits in
// the authorisation path of every request is worth more than the convenience.
type Registry struct {
	mu       sync.RWMutex
	families map[string]*family
	order    []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: make(map[string]*family)}
}

type metricType string

const (
	typeCounter   metricType = "counter"
	typeGauge     metricType = "gauge"
	typeHistogram metricType = "histogram"
)

type family struct {
	name       string
	help       string
	typ        metricType
	labelNames []string
	buckets    []float64

	mu     sync.RWMutex
	series map[string]*seriesEntry
}

type seriesEntry struct {
	labels []string

	// value holds the float64 bits for counters and gauges.
	value atomic.Uint64

	// Histogram state.
	bucketCounts []atomic.Uint64
	sum          atomic.Uint64
	count        atomic.Uint64
}

func (s *seriesEntry) addFloat(delta float64) {
	for {
		old := s.value.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if s.value.CompareAndSwap(old, next) {
			return
		}
	}
}

func (s *seriesEntry) setFloat(v float64) { s.value.Store(math.Float64bits(v)) }
func (s *seriesEntry) getFloat() float64  { return math.Float64frombits(s.value.Load()) }

func (s *seriesEntry) addSum(delta float64) {
	for {
		old := s.sum.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if s.sum.CompareAndSwap(old, next) {
			return
		}
	}
}

func (r *Registry) family(name, help string, typ metricType, labelNames []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()

	if f, ok := r.families[name]; ok {
		if f.typ != typ {
			panic(fmt.Sprintf("telemetry: metric %q already registered as %s, requested %s", name, f.typ, typ))
		}
		if strings.Join(f.labelNames, ",") != strings.Join(labelNames, ",") {
			panic(fmt.Sprintf("telemetry: metric %q already registered with labels %v, requested %v",
				name, f.labelNames, labelNames))
		}
		return f
	}

	f := &family{
		name:       name,
		help:       help,
		typ:        typ,
		labelNames: labelNames,
		buckets:    buckets,
		series:     make(map[string]*seriesEntry),
	}
	r.families[name] = f
	r.order = append(r.order, name)
	sort.Strings(r.order)
	return f
}

func (f *family) entry(labels []string) *seriesEntry {
	if len(labels) != len(f.labelNames) {
		panic(fmt.Sprintf("telemetry: metric %q expects %d label values, got %d",
			f.name, len(f.labelNames), len(labels)))
	}
	key := strings.Join(labels, "\x00")

	f.mu.RLock()
	e, ok := f.series[key]
	f.mu.RUnlock()
	if ok {
		return e
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.series[key]; ok {
		return e
	}
	e = &seriesEntry{labels: append([]string(nil), labels...)}
	if f.typ == typeHistogram {
		e.bucketCounts = make([]atomic.Uint64, len(f.buckets))
	}
	f.series[key] = e
	return e
}

// -----------------------------------------------------------------------------
// Counters
// -----------------------------------------------------------------------------

// CounterVec is a family of monotonically increasing counters.
type CounterVec struct{ f *family }

// Counter registers (or returns) a counter family.
func (r *Registry) Counter(name, help string, labelNames ...string) *CounterVec {
	return &CounterVec{f: r.family(name, help, typeCounter, labelNames, nil)}
}

// With returns the counter for a label combination.
func (v *CounterVec) With(labels ...string) *Counter {
	return &Counter{e: v.f.entry(labels)}
}

// Counter is a single counter series.
type Counter struct{ e *seriesEntry }

// Inc adds one.
func (c *Counter) Inc() { c.e.addFloat(1) }

// Add increases the counter. Negative deltas are ignored rather than panicking:
// a metrics bug must not be able to take down the request path.
func (c *Counter) Add(delta float64) {
	if delta < 0 || math.IsNaN(delta) {
		return
	}
	c.e.addFloat(delta)
}

// Value returns the current total.
func (c *Counter) Value() float64 { return c.e.getFloat() }

// -----------------------------------------------------------------------------
// Gauges
// -----------------------------------------------------------------------------

// GaugeVec is a family of gauges.
type GaugeVec struct{ f *family }

// Gauge registers (or returns) a gauge family.
func (r *Registry) Gauge(name, help string, labelNames ...string) *GaugeVec {
	return &GaugeVec{f: r.family(name, help, typeGauge, labelNames, nil)}
}

// With returns the gauge for a label combination.
func (v *GaugeVec) With(labels ...string) *Gauge {
	return &Gauge{e: v.f.entry(labels)}
}

// Reset drops every series in the family.
//
// Gauges that are keyed by a set which shrinks — per-backend traffic share,
// for example — must be cleared when the set changes, or a removed backend
// keeps reporting its last value forever and the dashboard shows traffic going
// to a region that no longer exists.
func (v *GaugeVec) Reset() {
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	v.f.series = make(map[string]*seriesEntry)
}

// Gauge is a single gauge series.
type Gauge struct{ e *seriesEntry }

// Set replaces the value.
func (g *Gauge) Set(v float64) {
	if math.IsNaN(v) {
		return
	}
	g.e.setFloat(v)
}

// Add adjusts the value.
func (g *Gauge) Add(delta float64) { g.e.addFloat(delta) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return g.e.getFloat() }

// -----------------------------------------------------------------------------
// Histograms
// -----------------------------------------------------------------------------

// DefaultLatencyBuckets covers sub-millisecond control-plane work through
// multi-second upstream calls.
var DefaultLatencyBuckets = []float64{
	0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025,
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// HistogramVec is a family of histograms.
type HistogramVec struct{ f *family }

// Histogram registers (or returns) a histogram family. Buckets are upper
// bounds and must be sorted ascending; +Inf is implicit.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultLatencyBuckets
	}
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	return &HistogramVec{f: r.family(name, help, typeHistogram, labelNames, sorted)}
}

// With returns the histogram for a label combination.
func (v *HistogramVec) With(labels ...string) *Histogram {
	return &Histogram{f: v.f, e: v.f.entry(labels)}
}

// Histogram is a single histogram series.
type Histogram struct {
	f *family
	e *seriesEntry
}

// Observe records a sample.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		return
	}
	// Buckets are cumulative in the exposition format, so a sample increments
	// every bucket at or above its value. Storing them cumulatively here
	// rather than at render time keeps rendering allocation-free.
	i := sort.SearchFloat64s(h.f.buckets, v)
	if i < len(h.f.buckets) && h.f.buckets[i] < v {
		i++
	}
	for ; i < len(h.f.buckets); i++ {
		h.e.bucketCounts[i].Add(1)
	}
	h.e.count.Add(1)
	h.e.addSum(v)
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 { return h.e.count.Load() }

// Sum returns the total of all observations.
func (h *Histogram) Sum() float64 { return math.Float64frombits(h.e.sum.Load()) }

// -----------------------------------------------------------------------------
// Exposition
// -----------------------------------------------------------------------------

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	var sb strings.Builder

	r.mu.RLock()
	names := append([]string(nil), r.order...)
	fams := make([]*family, 0, len(names))
	for _, n := range names {
		fams = append(fams, r.families[n])
	}
	r.mu.RUnlock()

	for _, f := range fams {
		f.mu.RLock()
		entries := make([]*seriesEntry, 0, len(f.series))
		for _, e := range f.series {
			entries = append(entries, e)
		}
		f.mu.RUnlock()

		if len(entries) == 0 {
			continue
		}
		sort.Slice(entries, func(i, j int) bool {
			return strings.Join(entries[i].labels, "\x00") < strings.Join(entries[j].labels, "\x00")
		})

		fmt.Fprintf(&sb, "# HELP %s %s\n", f.name, escapeHelp(f.help))
		fmt.Fprintf(&sb, "# TYPE %s %s\n", f.name, f.typ)

		for _, e := range entries {
			switch f.typ {
			case typeHistogram:
				for i, b := range f.buckets {
					sb.WriteString(f.name)
					sb.WriteString("_bucket")
					writeLabels(&sb, f.labelNames, e.labels, "le", formatFloat(b))
					sb.WriteByte(' ')
					sb.WriteString(strconv.FormatUint(e.bucketCounts[i].Load(), 10))
					sb.WriteByte('\n')
				}
				sb.WriteString(f.name)
				sb.WriteString("_bucket")
				writeLabels(&sb, f.labelNames, e.labels, "le", "+Inf")
				sb.WriteByte(' ')
				sb.WriteString(strconv.FormatUint(e.count.Load(), 10))
				sb.WriteByte('\n')

				sb.WriteString(f.name)
				sb.WriteString("_sum")
				writeLabels(&sb, f.labelNames, e.labels, "", "")
				sb.WriteByte(' ')
				sb.WriteString(formatFloat(math.Float64frombits(e.sum.Load())))
				sb.WriteByte('\n')

				sb.WriteString(f.name)
				sb.WriteString("_count")
				writeLabels(&sb, f.labelNames, e.labels, "", "")
				sb.WriteByte(' ')
				sb.WriteString(strconv.FormatUint(e.count.Load(), 10))
				sb.WriteByte('\n')

			default:
				sb.WriteString(f.name)
				writeLabels(&sb, f.labelNames, e.labels, "", "")
				sb.WriteByte(' ')
				sb.WriteString(formatFloat(e.getFloat()))
				sb.WriteByte('\n')
			}
		}
		sb.WriteByte('\n')
	}

	n, err := io.WriteString(w, sb.String())
	return int64(n), err
}

func writeLabels(sb *strings.Builder, names, values []string, extraName, extraValue string) {
	if len(names) == 0 && extraName == "" {
		return
	}
	sb.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(n)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(values[i]))
		sb.WriteByte('"')
	}
	if extraName != "" {
		if len(names) > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(extraName)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(extraValue))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
}

// escapeLabelValue applies the escaping the exposition format requires.
// Label values here include operator-supplied strings such as region names and
// deny reasons, so this cannot be skipped.
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func escapeHelp(s string) string {
	if !strings.ContainsAny(s, `\`+"\n") {
		return s
	}
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// String renders the registry, for tests and debugging.
func (r *Registry) String() string {
	var sb strings.Builder
	_, _ = r.WriteTo(&sb)
	return sb.String()
}
