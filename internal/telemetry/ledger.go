package telemetry

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
)

// Ledger is an append-only, bounded record of routing decisions.
//
// Every entry retains the complete derivation: the policy trace, all candidate
// scores with their raw and normalised signals, and the counterfactual the
// decision was measured against. That is the point — a router that silently
// moves a customer's traffic to another continent to save money has to be able
// to answer "why" long after the request completed, without the operator
// having to reproduce the signal state that existed at the time.
type Ledger struct {
	mu    sync.RWMutex
	buf   []*model.Decision
	head  int
	size  int
	index map[string]*model.Decision

	// Lifetime aggregates, kept incrementally so a summary never has to walk
	// the buffer.
	total       uint64
	byVerdict   map[model.Verdict]uint64
	byCloud     map[model.Cloud]uint64
	byRoute     map[string]uint64
	denyReasons map[string]uint64
	savedUSD    float64
	savedGrams  float64
	latencyDebt float64
	cacheHits   uint64

	subMu   sync.Mutex
	subs    map[int]chan *model.Decision
	nextSub int
	dropped uint64
}

// NewLedger returns a ledger retaining the most recent capacity decisions.
func NewLedger(capacity int) *Ledger {
	if capacity < 1 {
		capacity = 1
	}
	return &Ledger{
		buf:         make([]*model.Decision, capacity),
		index:       make(map[string]*model.Decision, capacity),
		byVerdict:   make(map[model.Verdict]uint64, 3),
		byCloud:     make(map[model.Cloud]uint64, 4),
		byRoute:     make(map[string]uint64, 8),
		denyReasons: make(map[string]uint64, 16),
		subs:        make(map[int]chan *model.Decision),
	}
}

// Record stores a decision. It satisfies router.DecisionSink.
func (l *Ledger) Record(d *model.Decision) {
	if d == nil {
		return
	}

	l.mu.Lock()
	// Evict the entry this slot is about to overwrite.
	if old := l.buf[l.head]; old != nil {
		delete(l.index, old.ID)
	}
	l.buf[l.head] = d
	l.index[d.ID] = d
	l.head = (l.head + 1) % len(l.buf)
	if l.size < len(l.buf) {
		l.size++
	}

	l.total++
	l.byVerdict[d.Verdict]++
	l.byRoute[d.RouteID]++
	if d.Cached {
		l.cacheHits++
	}
	if d.Verdict == model.VerdictAllow {
		l.byCloud[d.Cloud]++
		l.savedUSD += d.SavedUSD
		l.savedGrams += d.SavedGrams
		l.latencyDebt += d.LatencyDeltaMs
	} else if d.DenyReason != "" {
		l.denyReasons[d.DenyReason]++
	}
	l.mu.Unlock()

	l.publish(d)
}

// publish fans a decision out to live subscribers.
//
// Sends are non-blocking. A dashboard on a slow connection must never be able
// to apply back-pressure to the request path; dropping updates for that client
// is the correct trade, and the drop count is exported so the loss is visible
// rather than silent.
func (l *Ledger) publish(d *model.Decision) {
	l.subMu.Lock()
	defer l.subMu.Unlock()
	for _, ch := range l.subs {
		select {
		case ch <- d:
		default:
			l.dropped++
		}
	}
}

// Subscribe returns a channel of new decisions and a function to unsubscribe.
func (l *Ledger) Subscribe(buffer int) (<-chan *model.Decision, func()) {
	if buffer < 1 {
		buffer = 64
	}
	ch := make(chan *model.Decision, buffer)

	l.subMu.Lock()
	id := l.nextSub
	l.nextSub++
	l.subs[id] = ch
	l.subMu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			l.subMu.Lock()
			delete(l.subs, id)
			l.subMu.Unlock()
			close(ch)
		})
	}
}

// Get returns a retained decision by ID.
func (l *Ledger) Get(id string) (*model.Decision, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	d, ok := l.index[id]
	return d, ok
}

// Filter narrows a ledger query.
type Filter struct {
	Verdict string
	Cloud   string
	Region  string
	RouteID string
	Backend string
	// Subject matches as a case-insensitive substring of the identity.
	Subject string
	// Path matches as a case-insensitive substring of the request path.
	Path string
	// MinSavedUSD keeps only decisions that saved at least this much.
	MinSavedUSD float64
	Since       time.Time
	Limit       int
}

func (f Filter) matches(d *model.Decision) bool {
	if f.Verdict != "" && string(d.Verdict) != f.Verdict {
		return false
	}
	if f.Cloud != "" && string(d.Cloud) != f.Cloud {
		return false
	}
	if f.Region != "" && d.Region != f.Region {
		return false
	}
	if f.RouteID != "" && d.RouteID != f.RouteID {
		return false
	}
	if f.Backend != "" && d.ChosenBackend != f.Backend {
		return false
	}
	if f.Subject != "" && !strings.Contains(strings.ToLower(d.Subject.ID), strings.ToLower(f.Subject)) {
		return false
	}
	if f.Path != "" && !strings.Contains(strings.ToLower(d.Request.Path), strings.ToLower(f.Path)) {
		return false
	}
	if f.MinSavedUSD > 0 && d.SavedUSD < f.MinSavedUSD {
		return false
	}
	if !f.Since.IsZero() && d.Timestamp.Before(f.Since) {
		return false
	}
	return true
}

// Recent returns matching decisions, newest first.
func (l *Ledger) Recent(f Filter) []*model.Decision {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]*model.Decision, 0, limit)
	for i := 0; i < l.size && len(out) < limit; i++ {
		idx := (l.head - 1 - i + len(l.buf)*2) % len(l.buf)
		d := l.buf[idx]
		if d == nil {
			continue
		}
		if f.matches(d) {
			out = append(out, d)
		}
	}
	return out
}

// Retained returns how many decisions the ledger currently holds.
func (l *Ledger) Retained() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.size
}

// ReasonCount pairs a deny reason with how often it fired.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  uint64 `json:"count"`
}

// Summary aggregates lifetime ledger statistics.
type Summary struct {
	Total     uint64            `json:"total"`
	ByVerdict map[string]uint64 `json:"byVerdict"`
	ByCloud   map[string]uint64 `json:"byCloud"`
	ByRoute   map[string]uint64 `json:"byRoute"`
	// SavedUSD and SavedGrams are cumulative deltas against the
	// latency-only baseline. They can be negative, and are reported that way.
	SavedUSD   float64 `json:"savedUsd"`
	SavedGrams float64 `json:"savedGrams"`
	// LatencyDebtMs is the cumulative extra latency accepted in exchange for
	// those savings, summed across allowed requests.
	LatencyDebtMs  float64       `json:"latencyDebtMs"`
	TopDenyReasons []ReasonCount `json:"topDenyReasons"`
	Retained       int           `json:"retained"`
	Capacity       int           `json:"capacity"`
	CacheHits      uint64        `json:"policyCacheHits"`
	DroppedToSubs  uint64        `json:"droppedToSubscribers"`
	Subscribers    int           `json:"subscribers"`
}

// Summary returns the current aggregates.
func (l *Ledger) Summary() Summary {
	l.mu.RLock()
	s := Summary{
		Total:         l.total,
		ByVerdict:     make(map[string]uint64, len(l.byVerdict)),
		ByCloud:       make(map[string]uint64, len(l.byCloud)),
		ByRoute:       make(map[string]uint64, len(l.byRoute)),
		SavedUSD:      l.savedUSD,
		SavedGrams:    l.savedGrams,
		LatencyDebtMs: l.latencyDebt,
		Retained:      l.size,
		Capacity:      len(l.buf),
		CacheHits:     l.cacheHits,
	}
	for k, v := range l.byVerdict {
		s.ByVerdict[string(k)] = v
	}
	for k, v := range l.byCloud {
		if k == "" {
			continue
		}
		s.ByCloud[string(k)] = v
	}
	for k, v := range l.byRoute {
		s.ByRoute[k] = v
	}
	for reason, n := range l.denyReasons {
		s.TopDenyReasons = append(s.TopDenyReasons, ReasonCount{reason, n})
	}
	l.mu.RUnlock()

	sort.Slice(s.TopDenyReasons, func(i, j int) bool {
		if s.TopDenyReasons[i].Count != s.TopDenyReasons[j].Count {
			return s.TopDenyReasons[i].Count > s.TopDenyReasons[j].Count
		}
		return s.TopDenyReasons[i].Reason < s.TopDenyReasons[j].Reason
	})
	if len(s.TopDenyReasons) > 8 {
		s.TopDenyReasons = s.TopDenyReasons[:8]
	}

	l.subMu.Lock()
	s.DroppedToSubs = l.dropped
	s.Subscribers = len(l.subs)
	l.subMu.Unlock()

	return s
}
