package router

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/policy"
	"github.com/Saumya-patel-31/sluice/internal/signals"
)

// DecisionSink receives every decision the engine makes. The ledger and the
// metrics registry both implement it; the engine does not know about either,
// which keeps the decision path free of reporting concerns.
type DecisionSink interface {
	Record(*model.Decision)
}

// Config configures the engine.
type Config struct {
	// Plan holds the allocation tuning applied to every route that does not
	// override it.
	Plan PlanConfig
	// ControlInterval is how often the traffic distribution is recomputed.
	ControlInterval time.Duration
	// PolicyCacheSize and PolicyCacheTTL bound the authorisation cache.
	PolicyCacheSize int
	PolicyCacheTTL  time.Duration
	// DefaultRequestBytes is the response size assumed when a caller does not
	// declare one, used to convert per-GB prices into per-request figures.
	DefaultRequestBytes int64
	// DefaultObjectives applies to routes that do not set their own.
	DefaultObjectives model.Vector
}

// DefaultConfig returns production-sane engine settings.
func DefaultConfig() Config {
	return Config{
		Plan:                DefaultPlanConfig(),
		ControlInterval:     time.Second,
		PolicyCacheSize:     8192,
		PolicyCacheTTL:      5 * time.Second,
		DefaultRequestBytes: 64 << 10,
		DefaultObjectives:   model.Vector{0.35, 0.35, 0.20, 0.10},
	}
}

// state is the immutable view the request path reads.
//
// The control loop rebuilds it wholesale and swaps the pointer, so a request
// never observes a plan computed against one snapshot alongside signals from
// another, and no request ever waits on a lock held by the control loop.
type state struct {
	snapshot signals.Snapshot
	byID     map[string]signals.BackendState
	// plans holds each route's plan under its own configured objectives. This
	// is what the dashboard and the metrics report.
	plans map[string]*Plan
	// profilePlans holds a plan per (route, objective profile). A `prefer`
	// policy reshapes the objectives for one class of traffic, and a class
	// that optimises for carbon needs a different traffic distribution from
	// one optimising for latency — not merely a different number recorded
	// against the same distribution.
	profilePlans map[string]*Plan
	routes       []model.Route // longest PathPrefix first
	routeByID    map[string]model.Route
}

// objectiveKey identifies an objective vector for plan lookup. Quantising to
// four decimals keeps floating-point noise from splitting one profile into
// several near-identical plans.
func objectiveKey(v model.Vector) string {
	n := v.Normalized()
	var sb strings.Builder
	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		if d > 0 {
			sb.WriteByte(':')
		}
		sb.WriteString(strconv.FormatFloat(n[d], 'f', 4, 64))
	}
	return sb.String()
}

func planKey(routeID string, v model.Vector) string { return routeID + "|" + objectiveKey(v) }

// observedProfile records an objective vector seen on the request path, along
// with when it was last used.
type observedProfile struct {
	objectives model.Vector
	lastSeen   atomic.Int64 // unix nanoseconds
}

// Engine makes routing decisions and maintains the traffic distribution.
type Engine struct {
	cfg   Config
	store *signals.Store
	log   *slog.Logger

	policySet atomic.Pointer[policy.Set]
	cache     atomic.Pointer[policy.Cache]
	current   atomic.Pointer[state]

	mu     sync.RWMutex
	routes map[string]model.Route

	sinks   []DecisionSink
	sinksMu sync.RWMutex

	routeRate   sync.Map // routeID -> *rateCounter
	backendRate sync.Map // backendID -> *rateCounter
	// profiles are the objective vectors actually observed on the request
	// path, discovered lazily.
	//
	// The alternative — enumerating them from the policy document — is not
	// tractable: several `prefer` policies can match one request and their
	// overrides compose, so the reachable set is combinatorial in the number
	// of prefer rules. Observing what traffic actually produces bounds the set
	// by real usage instead, at the cost of one control interval before a
	// newly seen profile has its own plan.
	profiles sync.Map // profile key -> *observedProfile

	idPrefix string
	idSeq    atomic.Uint64

	decisions  atomic.Uint64
	denials    atomic.Uint64
	generation atomic.Uint64
}

// NewEngine returns an engine bound to a signal store.
func NewEngine(store *signals.Store, cfg Config, log *slog.Logger) *Engine {
	if cfg.ControlInterval <= 0 {
		cfg.ControlInterval = time.Second
	}
	if cfg.PolicyCacheSize <= 0 {
		cfg.PolicyCacheSize = 8192
	}
	if cfg.PolicyCacheTTL <= 0 {
		cfg.PolicyCacheTTL = 5 * time.Second
	}
	if cfg.DefaultRequestBytes <= 0 {
		cfg.DefaultRequestBytes = 64 << 10
	}
	if cfg.DefaultObjectives.Sum() == 0 {
		cfg.DefaultObjectives = model.Vector{0.35, 0.35, 0.20, 0.10}
	}
	if log == nil {
		log = slog.Default()
	}

	var seed [6]byte
	_, _ = rand.Read(seed[:])

	e := &Engine{
		cfg:      cfg,
		store:    store,
		log:      log,
		routes:   map[string]model.Route{},
		idPrefix: hex.EncodeToString(seed[:]),
	}
	e.policySet.Store(policy.MustCompileDefault())
	e.cache.Store(policy.NewCache(cfg.PolicyCacheSize, cfg.PolicyCacheTTL))
	e.current.Store(&state{
		byID:         map[string]signals.BackendState{},
		plans:        map[string]*Plan{},
		profilePlans: map[string]*Plan{},
		routeByID:    map[string]model.Route{},
	})
	return e
}

// profileTTL is how long an unused objective profile keeps its plan. A policy
// edit that removes a `prefer` rule should stop costing control-loop work
// shortly afterwards.
const profileTTL = 10 * time.Minute

// noteProfile records that an objective vector was used, so the next control
// loop builds a plan for it.
func (e *Engine) noteProfile(key string, v model.Vector) {
	now := time.Now().UnixNano()
	if existing, ok := e.profiles.Load(key); ok {
		existing.(*observedProfile).lastSeen.Store(now)
		return
	}
	p := &observedProfile{objectives: v}
	p.lastSeen.Store(now)
	e.profiles.Store(key, p)
}

// AddSink registers a decision observer.
func (e *Engine) AddSink(s DecisionSink) {
	e.sinksMu.Lock()
	defer e.sinksMu.Unlock()
	e.sinks = append(e.sinks, s)
}

// SetPolicy installs a new policy set and invalidates the authorisation cache.
func (e *Engine) SetPolicy(s *policy.Set) {
	e.policySet.Store(s)
	// A new document invalidates every memoised verdict. Replacing the cache
	// wholesale rather than purging in place also drops entries that are
	// mid-read by another goroutine without blocking it.
	e.cache.Store(policy.NewCache(e.cfg.PolicyCacheSize, e.cfg.PolicyCacheTTL))
	e.log.Info("policy set installed", "hash", s.Hash(), "policies", s.Len())
}

// Policy returns the live policy set.
func (e *Engine) Policy() *policy.Set { return e.policySet.Load() }

// PolicyCacheStats returns the authorisation cache hit rate.
func (e *Engine) PolicyCacheStats() (hits, misses uint64, rate float64) {
	c := e.cache.Load()
	h, m := c.Stats()
	return h, m, c.HitRate()
}

// UpsertRoute adds or replaces a route.
func (e *Engine) UpsertRoute(r model.Route) {
	if r.Objectives.Sum() == 0 {
		r.Objectives = e.cfg.DefaultObjectives
	}
	if r.Temperature <= 0 {
		r.Temperature = e.cfg.Plan.Temperature
	}
	e.mu.Lock()
	e.routes[r.ID] = r
	e.mu.Unlock()
}

// Routes returns the configured routes, longest prefix first.
func (e *Engine) Routes() []model.Route {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]model.Route, 0, len(e.routes))
	for _, r := range e.routes {
		out = append(out, r)
	}
	sortRoutes(out)
	return out
}

func sortRoutes(rs []model.Route) {
	sort.SliceStable(rs, func(i, j int) bool {
		if len(rs[i].PathPrefix) != len(rs[j].PathPrefix) {
			return len(rs[i].PathPrefix) > len(rs[j].PathPrefix)
		}
		return rs[i].ID < rs[j].ID
	})
}

// Plans returns the current traffic distribution for every route.
func (e *Engine) Plans() map[string]*Plan {
	st := e.current.Load()
	out := make(map[string]*Plan, len(st.plans))
	for k, v := range st.plans {
		out[k] = v
	}
	return out
}

// PlanFor returns the current distribution for one route.
func (e *Engine) PlanFor(routeID string) *Plan { return e.current.Load().plans[routeID] }

// Snapshot returns the signal snapshot the current plans were computed from.
func (e *Engine) Snapshot() signals.Snapshot { return e.current.Load().snapshot }

// Stats returns lifetime decision counts.
func (e *Engine) Stats() (decisions, denials uint64) {
	return e.decisions.Load(), e.denials.Load()
}

// RouteRPS returns the observed request rate for a route.
func (e *Engine) RouteRPS(routeID string) float64 {
	if c, ok := e.routeRate.Load(routeID); ok {
		return c.(*rateCounter).Rate()
	}
	return 0
}

// BackendRPS returns the observed request rate per backend.
func (e *Engine) BackendRPS() map[string]float64 {
	out := map[string]float64{}
	e.backendRate.Range(func(k, v any) bool {
		out[k.(string)] = v.(*rateCounter).Rate()
		return true
	})
	return out
}

func (e *Engine) rateFor(m *sync.Map, key string) *rateCounter {
	if c, ok := m.Load(key); ok {
		return c.(*rateCounter)
	}
	c, _ := m.LoadOrStore(key, newRateCounter(10, time.Now))
	return c.(*rateCounter)
}

// -----------------------------------------------------------------------------
// Control loop
// -----------------------------------------------------------------------------

// Recompute runs one control-loop iteration: take a coherent signal snapshot,
// rebuild every route's traffic distribution, and publish the result.
func (e *Engine) Recompute() {
	snap := e.store.Snapshot()
	byID := make(map[string]signals.BackendState, len(snap.Backends))
	for _, b := range snap.Backends {
		byID[b.Backend.ID] = b
	}

	routes := e.Routes()
	prev := e.current.Load()

	next := &state{
		snapshot:     snap,
		byID:         byID,
		plans:        make(map[string]*Plan, len(routes)),
		profilePlans: make(map[string]*Plan, len(routes)*2),
		routes:       routes,
		routeByID:    make(map[string]model.Route, len(routes)),
	}

	profiles := e.liveProfiles()

	for _, r := range routes {
		next.routeByID[r.ID] = r

		pool := make([]signals.BackendState, 0, len(snap.Backends))
		if len(r.BackendIDs) == 0 {
			pool = append(pool, snap.Backends...)
		} else {
			for _, id := range r.BackendIDs {
				if b, ok := byID[id]; ok {
					pool = append(pool, b)
				}
			}
		}

		cfg := e.cfg.Plan
		cfg.Temperature = r.Temperature
		cfg.LatencySLOMs = r.LatencySLOMs
		cfg.TotalRPS = e.RouteRPS(r.ID)

		build := func(objectives model.Vector) *Plan {
			key := planKey(r.ID, objectives)
			// Each profile carries its own damping state, so a batch
			// distribution converging toward a clean region does not drag the
			// interactive one along with it.
			p := BuildPlan(r.ID, pool, objectives, cfg, prev.profilePlans[key])
			next.profilePlans[key] = p
			return p
		}

		base := build(r.Objectives)
		next.plans[r.ID] = base

		baseKey := objectiveKey(r.Objectives)
		for key, prof := range profiles {
			if key == baseKey {
				continue
			}
			build(prof)
		}
	}

	e.current.Store(next)
	e.generation.Add(1)

	// Feed the history series that back the dashboard charts.
	weights := map[string]float64{}
	for _, p := range next.plans {
		for id, w := range p.Weights {
			if w > weights[id] {
				weights[id] = w
			}
		}
	}
	e.store.RecordSample(weights, e.BackendRPS())
}

// liveProfiles returns the objective vectors seen recently on the request
// path, evicting ones that have aged out.
func (e *Engine) liveProfiles() map[string]model.Vector {
	cutoff := time.Now().Add(-profileTTL).UnixNano()
	out := map[string]model.Vector{}
	e.profiles.Range(func(k, v any) bool {
		p := v.(*observedProfile)
		if p.lastSeen.Load() < cutoff {
			e.profiles.Delete(k)
			return true
		}
		out[k.(string)] = p.objectives
		return true
	})
	return out
}

// Profiles returns how many distinct objective profiles currently have plans.
func (e *Engine) Profiles() int { return len(e.current.Load().profilePlans) }

// Run drives the control loop until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.Recompute()
	t := time.NewTicker(e.cfg.ControlInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Recompute()
		}
	}
}

// Generation returns how many control-loop iterations have completed.
func (e *Engine) Generation() uint64 { return e.generation.Load() }

// -----------------------------------------------------------------------------
// Decisions
// -----------------------------------------------------------------------------

// RouteFor resolves the route serving a path, longest prefix wins.
func (e *Engine) RouteFor(path string) (model.Route, bool) {
	st := e.current.Load()
	for _, r := range st.routes {
		if r.PathPrefix == "" || strings.HasPrefix(path, r.PathPrefix) {
			return r, true
		}
	}
	return model.Route{}, false
}

func (e *Engine) nextID() string {
	return "dec_" + e.idPrefix + "_" + strconv.FormatUint(e.idSeq.Add(1), 36)
}

// Decide authorises a request and selects a backend.
//
// The decision path deliberately does no scoring. Normalisation and the
// softmax ran in the control loop against a coherent snapshot; here the engine
// only applies policy, restricts the published distribution to what policy
// allows, and samples from it. That keeps per-request work proportional to the
// number of candidates rather than to the number of objectives times
// candidates, and it means two simultaneous requests cannot disagree about the
// weights.
func (e *Engine) Decide(sub *model.Subject, req *model.Request) *model.Decision {
	start := time.Now()
	st := e.current.Load()

	if sub == nil {
		a := model.Anonymous()
		sub = &a
	}
	if req == nil {
		req = &model.Request{Method: "GET", Path: "/"}
	}
	if req.EstimatedBytes <= 0 {
		req.EstimatedBytes = e.cfg.DefaultRequestBytes
	}

	d := &model.Decision{
		ID:        e.nextID(),
		Timestamp: start,
		Subject:   *sub,
		Request:   *req,
	}

	route, ok := e.RouteFor(req.Path)
	if !ok {
		d.Verdict = model.VerdictDeny
		d.DenyReason = "no route matches path " + req.Path
		e.finish(d, start)
		return d
	}
	d.RouteID = route.ID
	e.rateFor(&e.routeRate, route.ID).Add(1)

	plan := st.plans[route.ID]
	if plan == nil {
		d.Verdict = model.VerdictNoCapacity
		d.DenyReason = "route has no computed traffic plan yet"
		e.finish(d, start)
		return d
	}
	d.Objectives = plan.Objectives

	// Zero trust gate: mTLS is a route-level requirement checked before any
	// policy runs, because a route that requires proof of identity should not
	// depend on an operator remembering to write the policy.
	if route.RequireMTLS && !sub.MTLS {
		d.Verdict = model.VerdictDeny
		d.DenyReason = "route " + route.ID + " requires mutual TLS"
		d.Candidates = plan.Candidates
		e.finish(d, start)
		return d
	}

	candidates := make([]model.Backend, 0, len(plan.Candidates))
	for _, c := range plan.Candidates {
		if b, ok := st.byID[c.BackendID]; ok {
			candidates = append(candidates, b.Backend)
		}
	}

	set := e.policySet.Load()
	in := policy.Input{
		Subject:        sub,
		Request:        req,
		Candidates:     candidates,
		Now:            start,
		BaseObjectives: plan.Objectives,
	}

	cache := e.cache.Load()
	key := policy.CacheKey(set.Hash(), in)
	res, cached := cache.Get(key)
	if !cached {
		res = set.Evaluate(in)
		cache.Put(key, res)
	}
	d.Cached = cached
	d.PolicyTrace = res.Trace

	// A `prefer` policy may have reshaped the objectives for this class of
	// traffic. Swap to the plan computed under those objectives, so the
	// candidate scores the decision records are the ones its weights actually
	// came from. Recording one objective vector alongside scores derived from
	// another would make the explainability trace arithmetic that does not
	// add up.
	if key := objectiveKey(res.Objectives); key != objectiveKey(plan.Objectives) {
		e.noteProfile(key, res.Objectives)
		if p := st.profilePlans[planKey(route.ID, res.Objectives)]; p != nil {
			plan = p
		}
		// Otherwise this profile was seen for the first time just now and has
		// no plan yet. The base plan serves this request and the next control
		// loop builds the profile's own.
	}
	d.Objectives = plan.Objectives

	if res.Verdict != model.VerdictAllow {
		d.Verdict = res.Verdict
		d.DenyReason = res.DenyReason
		d.Candidates = markPruned(plan.Candidates, res.Pruned, nil)
		e.finish(d, start)
		return d
	}

	allowed := make(map[string]bool, len(res.Eligible))
	for _, id := range res.Eligible {
		allowed[id] = true
	}

	// Restrict the published distribution to what policy permits and
	// renormalise. A backend that policy has excluded contributes no weight,
	// and its share is redistributed among the survivors in proportion to the
	// preferences the control loop already computed.
	cands := markPruned(plan.Candidates, res.Pruned, allowed)
	var total float64
	for i := range cands {
		if cands[i].Eligible {
			total += cands[i].Weight
		}
	}
	if total <= 0 {
		// Policy allows the request and left candidates standing, but all of
		// them carry zero weight — every survivor is ejected or shed. That is
		// a capacity failure, not an authorisation one.
		d.Verdict = model.VerdictNoCapacity
		d.DenyReason = "every policy-eligible backend is currently ejected or shed"
		d.Candidates = cands
		e.finish(d, start)
		return d
	}
	for i := range cands {
		if cands[i].Eligible {
			cands[i].Weight /= total
		} else {
			cands[i].Weight = 0
		}
	}

	chosen := sample(cands, mrand.Float64())
	d.Verdict = model.VerdictAllow
	d.ChosenBackend = chosen
	d.Candidates = cands
	if bs, ok := st.byID[chosen]; ok {
		d.Cloud = bs.Backend.Cloud
		d.Region = bs.Backend.Region
	}
	e.rateFor(&e.backendRate, chosen).Add(1)

	e.attributeSavings(d, st, cands)
	e.finish(d, start)
	return d
}

// markPruned copies the plan's candidate records and applies policy
// eligibility on top of the control loop's own exclusions.
func markPruned(src []model.CandidateScore, pruned map[string]string, allowed map[string]bool) []model.CandidateScore {
	out := make([]model.CandidateScore, len(src))
	copy(out, src)
	for i := range out {
		if reason, ok := pruned[out[i].BackendID]; ok {
			out[i].Eligible = false
			out[i].Reason = reason
			out[i].Weight = 0
			continue
		}
		if allowed != nil && !allowed[out[i].BackendID] {
			out[i].Eligible = false
			if out[i].Reason == "" {
				out[i].Reason = "excluded by policy"
			}
			out[i].Weight = 0
		}
	}
	return out
}

// sample draws a backend from the weight distribution.
func sample(cands []model.CandidateScore, r float64) string {
	var cum float64
	last := ""
	for _, c := range cands {
		if !c.Eligible || c.Weight <= 0 {
			continue
		}
		last = c.BackendID
		cum += c.Weight
		if r < cum {
			return c.BackendID
		}
	}
	// Floating-point residue can leave r just above the final cumulative sum;
	// fall back to the last eligible candidate rather than returning nothing.
	return last
}

// attributeSavings measures the decision against the counterfactual a
// latency-only load balancer would have produced.
//
// Reporting savings against "the most expensive option" would be flattering
// and meaningless. The honest comparison is against what a conventional
// balancer — which optimises latency alone, subject to the same policy
// constraints — would have chosen for this same request. Where that
// counterfactual is also the cheapest option, the reported saving is correctly
// zero, and where Sluice accepts extra latency to save money the delta is
// recorded as a positive cost, not hidden.
func (e *Engine) attributeSavings(d *model.Decision, st *state, cands []model.CandidateScore) {
	chosen, ok := st.byID[d.ChosenBackend]
	if !ok {
		return
	}

	baselineID, bestLatency := "", math.Inf(1)
	for _, c := range cands {
		if !c.Eligible {
			continue
		}
		bs, ok := st.byID[c.BackendID]
		if !ok || !bs.Healthy {
			continue
		}
		if v := bs.LatencyP95.Value; v < bestLatency {
			bestLatency, baselineID = v, c.BackendID
		}
	}
	if baselineID == "" {
		return
	}
	base, ok := st.byID[baselineID]
	if !ok {
		return
	}

	d.BaselineBackend = baselineID
	gb := float64(d.Request.EstimatedBytes) / (1 << 30)
	d.SavedUSD = (base.Egress.Value - chosen.Egress.Value) * gb
	d.SavedGrams = (base.CarbonPerGB.Value - chosen.CarbonPerGB.Value) * gb
	d.LatencyDeltaMs = chosen.LatencyP95.Value - base.LatencyP95.Value
}

func (e *Engine) finish(d *model.Decision, start time.Time) {
	d.ComputeMicros = time.Since(start).Microseconds()
	e.decisions.Add(1)
	if d.Verdict != model.VerdictAllow {
		e.denials.Add(1)
	}

	e.sinksMu.RLock()
	sinks := e.sinks
	e.sinksMu.RUnlock()
	for _, s := range sinks {
		s.Record(d)
	}
}

// ObserveResult feeds a completed request back into the signal store, closing
// the loop between what the router predicted and what actually happened.
func (e *Engine) ObserveResult(backendID string, rtt time.Duration, ok bool, bytesOut int64) {
	e.store.ObserveRequest(backendID, rtt, ok, bytesOut)
}
