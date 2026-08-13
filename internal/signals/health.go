package signals

import (
	"sync"
	"sync/atomic"
	"time"
)

// BreakerPhase is the state of a backend's circuit breaker.
type BreakerPhase string

const (
	// BreakerClosed is normal operation: the backend takes its full share.
	BreakerClosed BreakerPhase = "closed"
	// BreakerOpen means the backend is ejected and receives no traffic.
	BreakerOpen BreakerPhase = "open"
	// BreakerHalfOpen means the backend is receiving a trickle of traffic to
	// test whether it has recovered.
	BreakerHalfOpen BreakerPhase = "half_open"
)

// BreakerConfig tunes outlier ejection.
type BreakerConfig struct {
	// ErrorRateThreshold trips the breaker when the decayed error rate
	// exceeds it, in [0,1].
	ErrorRateThreshold float64 `json:"errorRateThreshold"`
	// MinObservations is how many results must be seen before the error rate
	// is trusted. Without this a single failed probe against a freshly
	// registered backend reads as a 100% error rate and ejects it.
	MinObservations int `json:"minObservations"`
	// ConsecutiveFailures trips the breaker immediately, regardless of rate.
	// A backend refusing every connection should not have to wait for an
	// EWMA to climb.
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// OpenDuration is how long to stay fully ejected before probing again.
	OpenDuration time.Duration `json:"openDuration"`
	// HalfOpenShare is the fraction of normal traffic a half-open backend
	// receives, in [0,1].
	HalfOpenShare float64 `json:"halfOpenShare"`
	// HalfOpenSuccesses is how many consecutive successes close the breaker.
	HalfOpenSuccesses int `json:"halfOpenSuccesses"`
	// MaxOpenDuration caps exponential backoff of the open interval.
	MaxOpenDuration time.Duration `json:"maxOpenDuration"`
}

// DefaultBreakerConfig returns production-sane ejection settings.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		ErrorRateThreshold:  0.25,
		MinObservations:     8,
		ConsecutiveFailures: 5,
		OpenDuration:        10 * time.Second,
		HalfOpenShare:       0.05,
		HalfOpenSuccesses:   3,
		MaxOpenDuration:     2 * time.Minute,
	}
}

// BreakerState is an immutable view of a breaker.
type BreakerState struct {
	State BreakerPhase `json:"state"`
	// Trips is the lifetime count of transitions into open.
	Trips int `json:"trips"`
	// ConsecutiveFailures is the current failure streak.
	ConsecutiveFailures int `json:"consecutiveFailures"`
	// OpenedAt is when the breaker last opened.
	OpenedAt time.Time `json:"openedAt,omitempty"`
	// RetryAt is when a half-open probe will next be permitted.
	RetryAt time.Time `json:"retryAt,omitempty"`
	// TrafficMultiplier is what the router should multiply this backend's
	// computed weight by: 1 when closed, HalfOpenShare when half-open, 0
	// when open.
	TrafficMultiplier float64 `json:"trafficMultiplier"`
}

// HealthTracker maintains a backend's error rate and circuit breaker.
//
// The breaker exposes a traffic multiplier rather than a boolean permit,
// because Sluice distributes traffic by weight rather than picking one backend
// per request. "Send this backend 5% of what it would otherwise get" expresses
// half-open recovery exactly, and lets a recovering region ramp back in
// naturally instead of being flipped fully on by a single successful probe.
type HealthTracker struct {
	cfg BreakerConfig
	now func() time.Time

	err      *EWMA
	inFlight atomic.Int64

	mu           sync.Mutex
	phase        BreakerPhase
	observations int
	consecFail   int
	consecOK     int
	trips        int
	openedAt     time.Time
	retryAt      time.Time
	backoff      time.Duration
}

// NewHealthTracker returns a tracker with a closed breaker.
func NewHealthTracker(cfg BreakerConfig, errorHalfLife time.Duration, now func() time.Time) *HealthTracker {
	if now == nil {
		now = time.Now
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 10 * time.Second
	}
	if cfg.MaxOpenDuration <= 0 {
		cfg.MaxOpenDuration = 2 * time.Minute
	}
	if cfg.HalfOpenSuccesses <= 0 {
		cfg.HalfOpenSuccesses = 3
	}
	return &HealthTracker{
		cfg:     cfg,
		now:     now,
		err:     NewEWMA(int64(errorHalfLife), func() int64 { return now().UnixNano() }),
		phase:   BreakerClosed,
		backoff: cfg.OpenDuration,
	}
}

// Observe records the outcome of one request or probe.
func (h *HealthTracker) Observe(ok bool) {
	if ok {
		h.err.Add(0)
	} else {
		h.err.Add(1)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.observations++
	if ok {
		h.consecOK++
		h.consecFail = 0
	} else {
		h.consecFail++
		h.consecOK = 0
	}

	switch h.phase {
	case BreakerClosed:
		tripOnStreak := h.cfg.ConsecutiveFailures > 0 && h.consecFail >= h.cfg.ConsecutiveFailures
		tripOnRate := h.observations >= h.cfg.MinObservations &&
			h.cfg.ErrorRateThreshold > 0 &&
			h.err.Value() > h.cfg.ErrorRateThreshold
		if tripOnStreak || tripOnRate {
			h.openLocked()
		}

	case BreakerHalfOpen:
		if !ok {
			// A failure during recovery re-opens with a longer interval, so a
			// persistently sick region is probed less and less aggressively
			// rather than being hammered on a fixed cadence.
			h.backoff *= 2
			if h.backoff > h.cfg.MaxOpenDuration {
				h.backoff = h.cfg.MaxOpenDuration
			}
			h.openLocked()
		} else if h.consecOK >= h.cfg.HalfOpenSuccesses {
			h.phase = BreakerClosed
			h.backoff = h.cfg.OpenDuration
			h.observations = 0
		}

	case BreakerOpen:
		// Results can still arrive from in-flight requests issued before the
		// breaker opened; they update the error rate but not the phase.
	}
}

func (h *HealthTracker) openLocked() {
	now := h.now()
	h.phase = BreakerOpen
	h.openedAt = now
	h.retryAt = now.Add(h.backoff)
	h.trips++
	h.consecOK = 0
}

// State returns the current breaker view, promoting an expired open breaker to
// half-open. Reading the state is what drives the transition; there is no
// timer goroutine per backend.
func (h *HealthTracker) State() BreakerState {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.phase == BreakerOpen && !h.now().Before(h.retryAt) {
		h.phase = BreakerHalfOpen
		h.consecOK = 0
		h.observations = 0
	}

	mult := 0.0
	switch h.phase {
	case BreakerClosed:
		mult = 1
	case BreakerHalfOpen:
		mult = h.cfg.HalfOpenShare
		if mult <= 0 {
			mult = 0.05
		}
	}

	return BreakerState{
		State:               h.phase,
		Trips:               h.trips,
		ConsecutiveFailures: h.consecFail,
		OpenedAt:            h.openedAt,
		RetryAt:             h.retryAt,
		TrafficMultiplier:   mult,
	}
}

// ErrorRate returns the time-decayed error rate in [0,1].
func (h *HealthTracker) ErrorRate() float64 { return h.err.Value() }

// ForceOpen ejects the backend immediately. Used by the admin API to drain a
// region ahead of maintenance.
func (h *HealthTracker) ForceOpen() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.openLocked()
}

// Reset closes the breaker and clears the failure history.
func (h *HealthTracker) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.phase = BreakerClosed
	h.consecFail, h.consecOK, h.observations = 0, 0, 0
	h.backoff = h.cfg.OpenDuration
}
