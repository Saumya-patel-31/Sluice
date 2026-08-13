package router

import (
	"sync"
	"time"
)

// rateCounter measures events per second over a sliding window of one-second
// buckets.
//
// A plain "count since start divided by uptime" reading is useless for
// capacity decisions, because it cannot fall. Bucketing keeps the reading
// responsive to the last few seconds while staying cheap: one integer
// increment per event, no allocation, no timer.
type rateCounter struct {
	mu      sync.Mutex
	buckets []int64
	epochs  []int64 // unix second each bucket holds
	now     func() time.Time
}

func newRateCounter(windowSeconds int, now func() time.Time) *rateCounter {
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	if now == nil {
		now = time.Now
	}
	return &rateCounter{
		buckets: make([]int64, windowSeconds),
		epochs:  make([]int64, windowSeconds),
		now:     now,
	}
}

// Add records n events at the current instant.
func (r *rateCounter) Add(n int64) {
	sec := r.now().Unix()
	idx := int(sec % int64(len(r.buckets)))

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epochs[idx] != sec {
		r.epochs[idx] = sec
		r.buckets[idx] = 0
	}
	r.buckets[idx] += n
}

// Rate returns events per second over the window, excluding the in-progress
// second so a partially elapsed bucket does not read as a rate drop.
func (r *rateCounter) Rate() float64 {
	sec := r.now().Unix()

	r.mu.Lock()
	defer r.mu.Unlock()

	var total int64
	var counted int
	for i := range r.buckets {
		age := sec - r.epochs[i]
		if age <= 0 || age >= int64(len(r.buckets)) {
			continue
		}
		total += r.buckets[i]
		counted++
	}
	if counted == 0 {
		return 0
	}
	return float64(total) / float64(counted)
}
