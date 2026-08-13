package signals

import (
	"sync"
	"time"
)

// Point is one timestamped sample in a Series.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Series is a fixed-capacity ring buffer of timestamped samples. It backs the
// dashboard sparklines and the streaming charts.
//
// A ring buffer rather than a growable slice: the control plane runs
// indefinitely and holds a series per backend per dimension, so retention has
// to be bounded by construction, not by a cleanup goroutine that might fall
// behind.
type Series struct {
	mu   sync.RWMutex
	buf  []Point
	head int // index of the next write
	size int // number of valid entries
}

// NewSeries returns a Series retaining the most recent capacity samples.
func NewSeries(capacity int) *Series {
	if capacity < 1 {
		capacity = 1
	}
	return &Series{buf: make([]Point, capacity)}
}

// Add appends a sample, evicting the oldest when at capacity.
func (s *Series) Add(t time.Time, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf[s.head] = Point{T: t, V: v}
	s.head = (s.head + 1) % len(s.buf)
	if s.size < len(s.buf) {
		s.size++
	}
}

// Points returns the samples in chronological order. The returned slice is a
// copy and is safe to retain.
func (s *Series) Points() []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Point, 0, s.size)
	start := (s.head - s.size + len(s.buf)) % len(s.buf)
	for i := 0; i < s.size; i++ {
		out = append(out, s.buf[(start+i)%len(s.buf)])
	}
	return out
}

// Last returns the most recent sample.
func (s *Series) Last() (Point, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.size == 0 {
		return Point{}, false
	}
	return s.buf[(s.head-1+len(s.buf))%len(s.buf)], true
}

// Len returns the number of retained samples.
func (s *Series) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.size
}

// Downsample returns at most n points, evenly strided over the retained
// window. The dashboard requests far fewer points than a series holds; doing
// the reduction here keeps the JSON payload small instead of shipping the full
// buffer and throwing it away in the browser.
func (s *Series) Downsample(n int) []Point {
	all := s.Points()
	if n <= 0 || len(all) <= n {
		return all
	}
	out := make([]Point, 0, n)
	step := float64(len(all)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, all[int(float64(i)*step+0.5)])
	}
	return out
}
