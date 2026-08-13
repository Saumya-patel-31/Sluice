package router

import (
	"math"
	"testing"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/policy"
	"github.com/saumyapatel/sluice/internal/signals"
)

func approx(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (+/- %v)", what, got, want, tol)
	}
}

func sum(xs []float64) float64 {
	var t float64
	for _, x := range xs {
		t += x
	}
	return t
}

// state builds a BackendState with the signals the router reads.
func bs(id string, cloud model.Cloud, region, jur string, price, p95, carbon, errRate float64) signals.BackendState {
	return signals.BackendState{
		Backend: model.Backend{
			ID: id, Cloud: cloud, Region: region, Jurisdiction: jur,
			Tier: "primary", Bias: 1, Enabled: true,
		},
		Egress:      signals.Quote{Value: price},
		CarbonPerGB: signals.Quote{Value: carbon},
		LatencyP95:  signals.Quote{Value: p95},
		LatencyP50:  signals.Quote{Value: p95 * 0.6},
		ErrorRate:   signals.Quote{Value: errRate},
		Breaker:     signals.BreakerState{State: signals.BreakerClosed, TrafficMultiplier: 1},
		Healthy:     true,
	}
}

func TestNormalize(t *testing.T) {
	got := normalize([]float64{10, 20, 30})
	want := []float64{0, 0.5, 1}
	for i := range want {
		approx(t, got[i], want[i], 1e-9, "normalize")
	}

	// A dimension where every candidate is identical cannot discriminate, and
	// must contribute nothing rather than a constant penalty.
	flat := normalize([]float64{7, 7, 7})
	for i, v := range flat {
		if v != 0 {
			t.Errorf("flat[%d] = %v, want 0", i, v)
		}
	}
	if len(normalize(nil)) != 0 {
		t.Error("normalize(nil) should be empty")
	}
}

func TestSoftmaxWeights(t *testing.T) {
	scores := []float64{0.9, 0.5, 0.1}

	hot := softmaxWeights(scores, nil, 0.02)
	approx(t, sum(hot), 1, 1e-9, "low-temperature sum")
	if hot[0] < 0.99 {
		t.Errorf("low temperature should approach winner-take-all, got %v", hot)
	}

	warm := softmaxWeights(scores, nil, 10)
	approx(t, sum(warm), 1, 1e-9, "high-temperature sum")
	if warm[0]-warm[2] > 0.05 {
		t.Errorf("high temperature should approach uniform, got %v", warm)
	}

	// Ordering must always follow score.
	mid := softmaxWeights(scores, nil, 0.15)
	if !(mid[0] > mid[1] && mid[1] > mid[2]) {
		t.Errorf("weights must be ordered by score, got %v", mid)
	}

	// Bias acts as a multiplicative prior.
	biased := softmaxWeights([]float64{0.5, 0.5}, []float64{3, 1}, 0.2)
	approx(t, biased[0]/biased[1], 3, 1e-6, "bias ratio on equal scores")

	// A single candidate takes everything, and a zero temperature degrades to
	// argmax rather than dividing by zero.
	if w := softmaxWeights([]float64{0.4}, nil, 0.1); w[0] != 1 {
		t.Errorf("single candidate should get all traffic, got %v", w)
	}
	zero := softmaxWeights(scores, nil, 0)
	if zero[0] != 1 || zero[1] != 0 {
		t.Errorf("zero temperature should be winner-take-all, got %v", zero)
	}
}

func TestApplyCapacityCaps(t *testing.T) {
	// The favourite can only take 100 of 1000 rps; the rest must move.
	w := []float64{0.8, 0.15, 0.05}
	applyCapacityCaps(w, []float64{100, 1000, 1000}, 1000)
	renormalize(w)
	approx(t, sum(w), 1, 1e-9, "sum after capping")
	if w[0] > 0.101 {
		t.Errorf("capped backend should hold ~10%% share, got %v", w)
	}
	if w[1] <= 0.15 {
		t.Errorf("overflow should have moved to backends with headroom, got %v", w)
	}

	// Zero capacity means "unlimited"; nothing should change.
	w2 := []float64{0.6, 0.4}
	applyCapacityCaps(w2, []float64{0, 0}, 1000)
	approx(t, w2[0], 0.6, 1e-9, "uncapped weight")
}

func TestApplyExplorationFloor(t *testing.T) {
	w := []float64{0.98, 0.02, 0.0}
	applyExplorationFloor(w, 0.05)
	approx(t, sum(w), 1, 1e-9, "sum after floor")
	for i, x := range w {
		if x < 0.05-1e-9 {
			t.Errorf("w[%d] = %v is below the floor", i, x)
		}
	}
	if w[0] < 0.8 {
		t.Errorf("the winner should still dominate, got %v", w)
	}

	// An unsatisfiable floor falls back to uniform rather than overflowing.
	w2 := []float64{0.5, 0.5, 0.0}
	applyExplorationFloor(w2, 0.5)
	approx(t, sum(w2), 1, 1e-9, "sum with unsatisfiable floor")
}

func TestPruneDust(t *testing.T) {
	w := []float64{0.7, 0.295, 0.005}
	pruneDust(w, 0.01)
	if w[2] != 0 {
		t.Errorf("sub-threshold weight should be zeroed, got %v", w)
	}
	approx(t, sum(w), 1, 1e-9, "sum after pruning")
}

// -----------------------------------------------------------------------------
// Plan
// -----------------------------------------------------------------------------

func planStates() []signals.BackendState {
	return []signals.BackendState{
		//                        price   p95   carbon  err
		bs("aws-us-east-1", model.CloudAWS, "us-east-1", "US", 0.090, 22, 6.1, 0.001),
		bs("gcp-europe-north1", model.CloudGCP, "europe-north1", "EU", 0.120, 95, 1.1, 0.001),
		bs("azure-centralindia", model.CloudAzure, "centralindia", "IN", 0.1093, 180, 11.1, 0.002),
	}
}

func TestPlanFollowsObjectives(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Smoothing = 1 // no damping, so one cycle reaches the target

	latencyFirst := BuildPlan("r", planStates(), model.Vector{0.05, 0.85, 0.05, 0.05}, cfg, nil)
	if got := topBackend(latencyFirst); got != "aws-us-east-1" {
		t.Errorf("latency-weighted plan should favour the fastest backend, got %q", got)
	}

	carbonFirst := BuildPlan("r", planStates(), model.Vector{0.05, 0.05, 0.85, 0.05}, cfg, nil)
	if got := topBackend(carbonFirst); got != "gcp-europe-north1" {
		t.Errorf("carbon-weighted plan should favour the cleanest grid, got %q", got)
	}

	costFirst := BuildPlan("r", planStates(), model.Vector{0.85, 0.05, 0.05, 0.05}, cfg, nil)
	if got := topBackend(costFirst); got != "aws-us-east-1" {
		t.Errorf("cost-weighted plan should favour the cheapest egress, got %q", got)
	}

	approx(t, sumWeights(latencyFirst), 1, 1e-6, "plan weights sum")
}

func topBackend(p *Plan) string {
	best, id := -1.0, ""
	for k, v := range p.Weights {
		if v > best {
			best, id = v, k
		}
	}
	return id
}

func sumWeights(p *Plan) float64 {
	var t float64
	for _, v := range p.Weights {
		t += v
	}
	return t
}

func TestPlanExplainsItsArithmetic(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Smoothing = 1
	obj := model.Vector{0.4, 0.3, 0.2, 0.1}
	p := BuildPlan("r", planStates(), obj, cfg, nil)

	for _, c := range p.Candidates {
		// contribution must equal normalized x weight, and score must equal
		// 1 minus the total contribution. This is the invariant the
		// explainability view relies on.
		var total float64
		for d := model.Dimension(0); d < model.NumDimensions; d++ {
			want := c.Normalized[d] * p.Objectives[d]
			approx(t, c.Contribution[d], want, 1e-9,
				c.BackendID+" contribution["+d.String()+"]")
			total += c.Contribution[d]
		}
		approx(t, c.Score, clamp01(1-total), 1e-9, c.BackendID+" score")
	}
}

func TestSLOGuardShedsSlowBackends(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Smoothing = 1
	cfg.LatencySLOMs = 40

	// Carbon-weighted objectives want the 95ms European region, but the SLO
	// forbids a blend that slow.
	p := BuildPlan("r", planStates(), model.Vector{0.1, 0.1, 0.7, 0.1}, cfg, nil)

	if !p.SLOMet {
		t.Fatalf("guardrail should have produced a compliant plan, projected %.1fms", p.ProjectedP95)
	}
	if p.ProjectedP95 > cfg.LatencySLOMs {
		t.Errorf("projected p95 %.1fms exceeds the %.0fms SLO", p.ProjectedP95, cfg.LatencySLOMs)
	}
	if len(p.Shed) == 0 {
		t.Error("expected the guardrail to record what it shed")
	}
	if p.Weights["azure-centralindia"] > 0 {
		t.Error("the 180ms backend should carry no traffic under a 40ms SLO")
	}

	// The reason has to name the SLO, or the dashboard cannot explain itself.
	for _, c := range p.Candidates {
		if c.BackendID == "azure-centralindia" && c.Reason == "" {
			t.Error("shed candidate should carry a reason")
		}
	}
}

func TestBreakerOpenRemovesTraffic(t *testing.T) {
	states := planStates()
	states[0].Breaker = signals.BreakerState{State: signals.BreakerOpen, Trips: 2, TrafficMultiplier: 0}
	states[0].Healthy = false

	cfg := DefaultPlanConfig()
	cfg.Smoothing = 1
	p := BuildPlan("r", states, model.Vector{0.25, 0.25, 0.25, 0.25}, cfg, nil)

	if p.Weights["aws-us-east-1"] != 0 {
		t.Errorf("an open breaker must take zero traffic, got %v", p.Weights["aws-us-east-1"])
	}
	approx(t, sumWeights(p), 1, 1e-6, "weights still sum to 1 after ejection")
}

func TestHalfOpenGetsTrickle(t *testing.T) {
	states := planStates()
	states[2].Breaker = signals.BreakerState{State: signals.BreakerHalfOpen, TrafficMultiplier: 0.05}

	cfg := DefaultPlanConfig()
	cfg.Smoothing = 1
	cfg.MinWeight = 0 // keep the trickle visible rather than pruning it as dust
	cfg.ExplorationFloor = 0
	p := BuildPlan("r", states, model.Vector{0.25, 0.25, 0.25, 0.25}, cfg, nil)

	w := p.Weights["azure-centralindia"]
	if w <= 0 {
		t.Fatal("a half-open backend should receive a probe trickle, not zero")
	}
	if w > 0.15 {
		t.Errorf("half-open share %v is too large to be a trickle", w)
	}
}

func TestDampingAndDeadband(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Smoothing = 0.35
	cfg.Deadband = 0.04

	first := BuildPlan("r", planStates(), model.Vector{0.25, 0.25, 0.25, 0.25}, cfg, nil)
	if !first.Republished {
		t.Fatal("the first plan must publish")
	}

	// An identical cycle produces no movement, so nothing is pushed.
	second := BuildPlan("r", planStates(), model.Vector{0.25, 0.25, 0.25, 0.25}, cfg, first)
	if second.Republished {
		t.Errorf("an unchanged input must not republish (churn %.4f)", second.Churn)
	}

	// A large objective swing must move traffic, but damping means the first
	// cycle only travels part of the way.
	swung := BuildPlan("r", planStates(), model.Vector{0.05, 0.05, 0.85, 0.05}, cfg, first)
	if !swung.Republished {
		t.Fatal("a large objective change should cross the deadband")
	}
	target := swung.Target["gcp-europe-north1"]
	published := swung.Weights["gcp-europe-north1"]
	prior := first.Weights["gcp-europe-north1"]
	if !(published > prior && published < target) {
		t.Errorf("damping should land between prior %.3f and target %.3f, got %.3f",
			prior, target, published)
	}
}

func TestDampingConvergesToTarget(t *testing.T) {
	cfg := DefaultPlanConfig()
	cfg.Smoothing = 0.35
	obj := model.Vector{0.05, 0.05, 0.85, 0.05}

	var p *Plan
	for i := 0; i < 60; i++ {
		p = BuildPlan("r", planStates(), obj, cfg, p)
	}
	approx(t, p.Weights["gcp-europe-north1"], p.Target["gcp-europe-north1"], 0.02,
		"damped weight converges to target")
}

func TestEmptyCandidateSet(t *testing.T) {
	p := BuildPlan("r", nil, model.Vector{0.25, 0.25, 0.25, 0.25}, DefaultPlanConfig(), nil)
	if p == nil || len(p.Weights) != 0 {
		t.Fatal("an empty candidate set should yield an empty, non-nil plan")
	}
}

// -----------------------------------------------------------------------------
// Engine
// -----------------------------------------------------------------------------

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	store := signals.NewStore(signals.DefaultStoreConfig())

	specs := []struct {
		id, region, jur  string
		cloud            model.Cloud
		price, p95, grid float64
	}{
		{"aws-us-east-1", "us-east-1", "US", model.CloudAWS, 0.090, 20, 355},
		{"gcp-europe-north1", "europe-north1", "EU", model.CloudGCP, 0.120, 90, 70},
		{"azure-northeurope", "northeurope", "EU", model.CloudAzure, 0.087, 85, 285},
	}
	for _, s := range specs {
		store.Register(model.Backend{
			ID: s.id, Cloud: s.cloud, Region: s.region, Jurisdiction: s.jur,
			Tier: "primary", Bias: 1, Enabled: true, Address: "http://" + s.id,
		})
		store.SetPrice(s.id, signals.Quote{Value: s.price, Source: "test", AsOf: time.Now()})
		store.SetGridIntensity(s.id, signals.Quote{Value: s.grid, Source: "test", AsOf: time.Now()})
		for i := 0; i < 40; i++ {
			store.ObserveProbe(s.id, time.Duration(s.p95)*time.Millisecond, true)
		}
	}

	e := NewEngine(store, DefaultConfig(), nil)
	e.UpsertRoute(model.Route{
		ID: "default", DisplayName: "Default", PathPrefix: "/",
		Objectives: model.Vector{0.35, 0.35, 0.20, 0.10}, Temperature: 0.12,
	})
	e.Recompute()
	return e
}

func authedSubject() *model.Subject {
	return &model.Subject{
		ID: "spiffe://prod.internal/ns/api/sa/gateway", TrustDomain: "prod.internal",
		Namespace: "api", Service: "gateway", MTLS: true, Authenticated: true,
	}
}

func TestEngineAllowsAndChooses(t *testing.T) {
	e := newTestEngine(t)
	d := e.Decide(authedSubject(), &model.Request{Method: "GET", Path: "/api/v1/items"})

	if d.Verdict != model.VerdictAllow {
		t.Fatalf("want allow, got %s (%s)", d.Verdict, d.DenyReason)
	}
	if d.ChosenBackend == "" {
		t.Fatal("an allowed decision must name a backend")
	}
	if d.ID == "" || d.ComputeMicros < 0 {
		t.Fatal("decision metadata is incomplete")
	}
	if len(d.Candidates) != 3 {
		t.Fatalf("want 3 candidate records, got %d", len(d.Candidates))
	}

	var total float64
	for _, c := range d.Candidates {
		total += c.Weight
	}
	approx(t, total, 1, 1e-6, "decision weights sum")

	if d.BaselineBackend != "aws-us-east-1" {
		t.Errorf("the latency-only baseline should be the 20ms backend, got %q", d.BaselineBackend)
	}
}

func TestEngineDeniesAnonymous(t *testing.T) {
	e := newTestEngine(t)
	anon := model.Anonymous()
	d := e.Decide(&anon, &model.Request{Method: "GET", Path: "/api/v1/items"})

	if d.Verdict != model.VerdictDeny {
		t.Fatalf("want deny, got %s", d.Verdict)
	}
	if d.ChosenBackend != "" {
		t.Error("a denied decision must not name a backend")
	}
	if len(d.PolicyTrace) == 0 {
		t.Error("a denial must carry the policy trace that produced it")
	}
	if _, denials := e.Stats(); denials != 1 {
		t.Errorf("want 1 denial recorded, got %d", denials)
	}
}

func TestEngineResidencyPrunesNonEU(t *testing.T) {
	e := newTestEngine(t)
	sub := authedSubject()
	sub.Claims = map[string]string{"residency": "eu"}

	for i := 0; i < 25; i++ {
		d := e.Decide(sub, &model.Request{Method: "POST", Path: "/api/v1/profile", DataClass: model.DataPII})
		if d.Verdict != model.VerdictAllow {
			t.Fatalf("want allow, got %s (%s)", d.Verdict, d.DenyReason)
		}
		if d.ChosenBackend == "aws-us-east-1" {
			t.Fatal("EU personal data was routed to a US region")
		}
	}
}

func TestEngineSavingsAreMeasuredAgainstTheBaseline(t *testing.T) {
	e := newTestEngine(t)
	sub := authedSubject()

	// Batch traffic weights cost and carbon, so it should sometimes choose a
	// backend other than the latency winner and book a saving for doing so.
	var sawSaving, sawLatencyCost bool
	for i := 0; i < 200; i++ {
		d := e.Decide(sub, &model.Request{
			Method: "POST", Path: "/batch/reindex", EstimatedBytes: 1 << 30,
		})
		if d.Verdict != model.VerdictAllow {
			t.Fatalf("want allow, got %s (%s)", d.Verdict, d.DenyReason)
		}
		if d.ChosenBackend != d.BaselineBackend {
			if d.SavedGrams != 0 || d.SavedUSD != 0 {
				sawSaving = true
			}
			if d.LatencyDeltaMs > 0 {
				sawLatencyCost = true
			}
		}
	}
	if !sawSaving {
		t.Error("expected at least one decision to book a measured saving")
	}
	if !sawLatencyCost {
		t.Error("expected the latency cost of a non-baseline choice to be recorded")
	}
}

func TestEngineUsesThePolicyCache(t *testing.T) {
	e := newTestEngine(t)
	sub := authedSubject()
	req := &model.Request{Method: "GET", Path: "/api/v1/items"}

	for i := 0; i < 50; i++ {
		e.Decide(sub, req)
	}
	hits, _, rate := e.PolicyCacheStats()
	if hits == 0 {
		t.Fatal("repeated identical requests should hit the policy cache")
	}
	if rate < 0.9 {
		t.Errorf("hit rate %.2f is lower than expected for identical requests", rate)
	}

	// Installing a new document must invalidate everything.
	e.SetPolicy(policy.MustCompileDefault())
	h2, _, _ := e.PolicyCacheStats()
	if h2 != 0 {
		t.Errorf("policy reload should reset the cache, got %d hits", h2)
	}
}

// A `prefer` policy has to change where traffic goes, not merely what number
// the decision records. This is the invariant that broke once: the decision
// carried the policy-adjusted objectives while its candidate scores had been
// computed under the route's, so the recorded arithmetic did not add up and
// the reshaping had no effect on routing at all.
func TestPreferPolicyMovesTrafficAndStaysConsistent(t *testing.T) {
	e := newTestEngine(t)
	e.UpsertRoute(model.Route{
		ID: "default", DisplayName: "Default", PathPrefix: "/",
		Objectives:  model.Vector{0.05, 0.85, 0.05, 0.05}, // latency-dominated
		Temperature: 0.12,
	})

	set, err := policy.Compile(`
policy "allow-all" { priority 900 effect allow when true }

policy "batch-chases-clean-grids" {
  priority 200
  effect   prefer
  when     request.path startswith "/batch"
  prefer   { carbon: 0.90, cost: 0.05, latency: 0.03, reliability: 0.02 }
}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	e.SetPolicy(set)
	e.Recompute()

	sub := authedSubject()
	interactive := &model.Request{Method: "GET", Path: "/api/v1/items"}
	batch := &model.Request{Method: "POST", Path: "/batch/reindex"}

	// The first batch request discovers the profile; the next control loop
	// builds its plan.
	e.Decide(sub, batch)
	e.Recompute()
	e.Recompute()

	countCleanest := func(req *model.Request, n int) int {
		hits := 0
		for i := 0; i < n; i++ {
			d := e.Decide(sub, req)
			if d.Verdict != model.VerdictAllow {
				t.Fatalf("want allow, got %s (%s)", d.Verdict, d.DenyReason)
			}
			// Every decision must be internally consistent regardless of
			// which profile served it.
			for _, c := range d.Candidates {
				var penalty float64
				for dim := model.Dimension(0); dim < model.NumDimensions; dim++ {
					approx(t, c.Contribution[dim], c.Normalized[dim]*d.Objectives[dim], 1e-9,
						c.BackendID+" contribution["+dim.String()+"]")
					penalty += c.Contribution[dim]
				}
				approx(t, c.Score, clamp01(1-penalty), 1e-9, c.BackendID+" score")
			}
			// gcp-europe-north1 sits on a 70 gCO2e/kWh grid; the others are
			// on 355 and 285.
			if d.ChosenBackend == "gcp-europe-north1" {
				hits++
			}
		}
		return hits
	}

	const n = 400
	batchClean := countCleanest(batch, n)
	interactiveClean := countCleanest(interactive, n)

	if batchClean <= interactiveClean {
		t.Errorf("the carbon-weighted profile should send far more traffic to the clean grid: "+
			"batch %d/%d vs interactive %d/%d", batchClean, n, interactiveClean, n)
	}
	if batchClean < n/2 {
		t.Errorf("a 0.90 carbon weight should dominate, got %d/%d to the cleanest region", batchClean, n)
	}

	// Both profiles now have their own plan, plus the route's base.
	if got := e.Profiles(); got < 2 {
		t.Errorf("want a plan per observed objective profile, got %d", got)
	}
}

func TestEngineNoRouteIsADeny(t *testing.T) {
	store := signals.NewStore(signals.DefaultStoreConfig())
	e := NewEngine(store, DefaultConfig(), nil)
	e.UpsertRoute(model.Route{ID: "api", PathPrefix: "/api", Temperature: 0.12})
	e.Recompute()

	d := e.Decide(authedSubject(), &model.Request{Method: "GET", Path: "/other"})
	if d.Verdict != model.VerdictDeny {
		t.Fatalf("an unmatched path must be denied, got %s", d.Verdict)
	}
}

func TestSampleRespectsDistribution(t *testing.T) {
	cands := []model.CandidateScore{
		{BackendID: "a", Eligible: true, Weight: 0.5},
		{BackendID: "b", Eligible: true, Weight: 0.3},
		{BackendID: "c", Eligible: true, Weight: 0.2},
		{BackendID: "d", Eligible: false, Weight: 0},
	}
	counts := map[string]int{}
	const n = 100000
	for i := 0; i < n; i++ {
		counts[sample(cands, float64(i)/n)]++
	}
	approx(t, float64(counts["a"])/n, 0.5, 0.01, "share of a")
	approx(t, float64(counts["b"])/n, 0.3, 0.01, "share of b")
	approx(t, float64(counts["c"])/n, 0.2, 0.01, "share of c")
	if counts["d"] != 0 {
		t.Error("an ineligible candidate must never be sampled")
	}

	// Floating-point residue at the very top of the range must still resolve.
	if got := sample(cands, 1.0); got == "" {
		t.Error("sampling at r=1 should fall back to the last eligible candidate")
	}
}

func TestRateCounter(t *testing.T) {
	now := time.Unix(1000, 0)
	rc := newRateCounter(10, func() time.Time { return now })

	for i := 0; i < 5; i++ {
		rc.Add(10)
		now = now.Add(time.Second)
	}
	// Four complete seconds of 10 events each; the in-progress second is
	// excluded, so the rate is 10/s rather than a partial-bucket dip.
	approx(t, rc.Rate(), 10, 0.001, "rate")

	now = now.Add(20 * time.Second)
	approx(t, rc.Rate(), 0, 0.001, "rate after the window empties")
}
