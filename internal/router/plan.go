package router

import (
	"fmt"
	"sort"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/signals"
)

// PlanConfig tunes how a candidate set becomes a traffic distribution.
type PlanConfig struct {
	// Temperature controls how sharply score differences become traffic
	// share. Small values approach winner-take-all.
	Temperature float64
	// Smoothing is the fraction of the newly computed target folded into the
	// running distribution each cycle, in (0,1]. 1 disables damping.
	Smoothing float64
	// Deadband is the L1 distance the smoothed distribution must move before
	// it is republished to the data plane.
	Deadband float64
	// MinWeight zeroes shares below it, so the published plan does not carry
	// fractions of a percent.
	MinWeight float64
	// ExplorationFloor guarantees each eligible backend a minimum share.
	ExplorationFloor float64
	// LatencySLOMs is the p95 the blended route must hold. Zero disables the
	// guardrail.
	LatencySLOMs float64
	// TotalRPS is the route's current request rate, used to convert weights
	// into absolute load for capacity capping.
	TotalRPS float64
}

// DefaultPlanConfig returns settings tuned for a service handling interactive
// traffic: responsive enough to react to a regional brownout within a few
// cycles, damped enough not to rewrite the data plane every second.
func DefaultPlanConfig() PlanConfig {
	return PlanConfig{
		Temperature:      0.12,
		Smoothing:        0.35,
		Deadband:         0.04,
		MinWeight:        0.01,
		ExplorationFloor: 0.02,
		LatencySLOMs:     0,
	}
}

// ShedRecord explains one backend removed from the distribution.
type ShedRecord struct {
	BackendID string `json:"backendId"`
	Reason    string `json:"reason"`
}

// Plan is the computed traffic allocation for one route.
type Plan struct {
	RouteID    string       `json:"routeId"`
	Generation uint64       `json:"generation"`
	ComputedAt time.Time    `json:"computedAt"`
	Objectives model.Vector `json:"objectives"`

	// Candidates carries the full scoring derivation. Weight on each record is
	// the published share.
	Candidates []model.CandidateScore `json:"candidates"`
	// Weights is the distribution currently in effect in the data plane.
	Weights map[string]float64 `json:"weights"`
	// Target is what this cycle computed before damping — the distribution
	// the router is moving toward.
	Target map[string]float64 `json:"target"`

	// ProjectedP95 is the traffic-weighted p95 across the distribution.
	//
	// This is an approximation. The true p95 of a mixture is the point where
	// the weighted tail probabilities sum to 0.05, which needs each backend's
	// full distribution rather than a single quantile. The weighted mean of
	// per-backend p95s is the standard cheap estimator and is accurate when
	// the backends have similar distribution shapes; WorstP95 is reported
	// alongside it as the pessimistic bound.
	ProjectedP95 float64 `json:"projectedP95"`
	// WorstP95 is the highest p95 among backends carrying real traffic.
	WorstP95 float64 `json:"worstP95"`
	SLOMs    float64 `json:"sloMs"`
	SLOMet   bool    `json:"sloMet"`

	// Shed lists backends excluded, with the reason.
	Shed []ShedRecord `json:"shed,omitempty"`
	// Churn is the L1 distance between this distribution and the previously
	// published one.
	Churn float64 `json:"churn"`
	// Republished reports whether this plan replaced what the data plane had.
	Republished bool `json:"republished"`

	// smoothed is the damped internal state. It keeps moving toward the
	// target even on cycles that do not republish, so a slow drift eventually
	// crosses the deadband instead of being repeatedly rounded away.
	smoothed map[string]float64
}

// WeightOf returns a backend's published share.
func (p *Plan) WeightOf(id string) float64 {
	if p == nil {
		return 0
	}
	return p.Weights[id]
}

// BuildPlan computes the next traffic distribution for a route.
//
// The pipeline is: score, eject unhealthy, shed for the latency SLO, cap by
// capacity, floor for exploration, damp against the previous cycle, then
// decide whether the movement is large enough to be worth pushing.
func BuildPlan(routeID string, states []signals.BackendState, objectives model.Vector, cfg PlanConfig, prev *Plan) *Plan {
	now := time.Now()
	plan := &Plan{
		RouteID:    routeID,
		ComputedAt: now,
		Objectives: objectives.Normalized(),
		Weights:    map[string]float64{},
		Target:     map[string]float64{},
		smoothed:   map[string]float64{},
		SLOMs:      cfg.LatencySLOMs,
		SLOMet:     true,
	}
	if prev != nil {
		plan.Generation = prev.Generation
	}
	if len(states) == 0 {
		plan.Republished = prev == nil
		return plan
	}

	// Dust pruning must not fight the exploration floor. If the floor is
	// enabled and set below MinWeight, every floored backend would be
	// immediately pruned back to zero, and the two settings would trade the
	// same backend back and forth forever.
	if cfg.ExplorationFloor > 0 && cfg.MinWeight > cfg.ExplorationFloor {
		cfg.MinWeight = cfg.ExplorationFloor
	}

	// Deterministic ordering so a plan is reproducible from the same inputs.
	ordered := append([]signals.BackendState(nil), states...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Backend.ID < ordered[j].Backend.ID })

	scores := ScoreCandidates(ordered, plan.Objectives)

	// Ejection: a backend whose breaker is open takes no traffic, and one in
	// half-open takes a trickle.
	active := make([]int, 0, len(ordered))
	for i, s := range ordered {
		switch {
		case !s.Backend.Enabled:
			scores[i].Eligible = false
			scores[i].Reason = "backend disabled"
			plan.Shed = append(plan.Shed, ShedRecord{s.Backend.ID, "disabled"})
		case s.Breaker.State == signals.BreakerOpen:
			scores[i].Eligible = false
			scores[i].Reason = fmt.Sprintf("circuit open (%d trips, retry %s)",
				s.Breaker.Trips, s.Breaker.RetryAt.Format(time.TimeOnly))
			plan.Shed = append(plan.Shed, ShedRecord{s.Backend.ID, "circuit breaker open"})
		default:
			active = append(active, i)
		}
	}

	if len(active) > 0 {
		shed := allocate(ordered, scores, active, cfg, plan)
		plan.Shed = append(plan.Shed, shed...)
	}

	// Collect the target distribution.
	for i := range scores {
		plan.Target[scores[i].BackendID] = scores[i].Weight
	}

	eligible := make(map[string]bool, len(scores))
	for i := range scores {
		eligible[scores[i].BackendID] = scores[i].Eligible
	}

	// Damping. Backends without prior history adopt the target directly;
	// there is nothing to smooth against and starting them at zero would
	// delay a newly registered region for several cycles.
	alpha := cfg.Smoothing
	if alpha <= 0 || alpha > 1 {
		alpha = 1
	}
	for id, target := range plan.Target {
		// Ejection and SLO shedding are safety actions, not preference
		// changes. They take effect at once rather than decaying in over
		// several cycles — a backend that just started failing must stop
		// receiving traffic now, not in four seconds.
		if !eligible[id] {
			plan.smoothed[id] = 0
			continue
		}
		if prev != nil {
			if prior, ok := prev.smoothed[id]; ok {
				plan.smoothed[id] = alpha*target + (1-alpha)*prior
				continue
			}
		}
		plan.smoothed[id] = target
	}

	// Normalise and de-dust the damped distribution.
	ids := make([]string, 0, len(plan.smoothed))
	for id := range plan.smoothed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	vals := make([]float64, len(ids))
	for i, id := range ids {
		vals[i] = plan.smoothed[id]
	}
	renormalize(vals)
	published := append([]float64(nil), vals...)
	pruneDust(published, cfg.MinWeight)

	// Deadband. Republish when the distribution has moved materially, when
	// the candidate set changed, or when a backend crossed into or out of
	// zero traffic — safety changes must not wait for a threshold.
	//
	// The comparison is published-against-published. Comparing the undamped
	// target against the published weights instead would never converge,
	// because the two are produced by different pipelines: a backend held at
	// the exploration floor has a non-zero target and a pruned-to-zero
	// published weight forever, which would force a push every cycle.
	prevVals := make([]float64, len(ids))
	setChanged := prev == nil || len(prev.Weights) != len(ids)
	for i, id := range ids {
		if prev != nil {
			v, ok := prev.Weights[id]
			if !ok {
				setChanged = true
			}
			prevVals[i] = v
		}
	}
	plan.Churn = l1Distance(published, prevVals)

	forcePublish := setChanged || zeroSetChanged(published, prevVals)
	switch {
	case forcePublish || plan.Churn > cfg.Deadband:
		for i, id := range ids {
			plan.Weights[id] = published[i]
			plan.smoothed[id] = vals[i]
		}
		plan.Republished = true
		plan.Generation++
	default:
		// Hold the published distribution but keep the damped state moving,
		// so a gradual drift eventually accumulates past the deadband.
		for i, id := range ids {
			plan.Weights[id] = prevVals[i]
			plan.smoothed[id] = vals[i]
		}
	}

	// Reflect the published weights back onto the candidate records.
	for i := range scores {
		scores[i].Weight = plan.Weights[scores[i].BackendID]
	}

	plan.ProjectedP95, plan.WorstP95 = projectLatency(ordered, scores)
	plan.SLOMet = cfg.LatencySLOMs <= 0 || plan.ProjectedP95 <= cfg.LatencySLOMs

	sortCandidatesByWeight(scores)
	plan.Candidates = scores
	return plan
}

// allocate turns scores into weights for the active subset, applying the SLO
// guardrail, capacity caps and the exploration floor.
func allocate(states []signals.BackendState, scores []model.CandidateScore, active []int, cfg PlanConfig, plan *Plan) []ShedRecord {
	var shed []ShedRecord

	// SLO guardrail. Cost and carbon optimisation is only legitimate inside
	// the latency budget; when the blend would breach it, drop the slowest
	// candidate and re-solve. Each round removes exactly one backend, so this
	// terminates, and it degrades in the right direction — the router gives
	// up savings before it gives up the SLO.
	pool := append([]int(nil), active...)
	for {
		weights := solve(states, scores, pool, cfg)
		if cfg.LatencySLOMs <= 0 || len(pool) <= 1 {
			commit(scores, pool, weights)
			return shed
		}

		var projected float64
		for i, idx := range pool {
			projected += weights[i] * states[idx].LatencyP95.Value
		}
		if projected <= cfg.LatencySLOMs {
			commit(scores, pool, weights)
			return shed
		}

		// Drop the slowest member of the pool and try again.
		worst, worstAt := -1.0, -1
		for pi, idx := range pool {
			if v := states[idx].LatencyP95.Value; v > worst {
				worst, worstAt = v, pi
			}
		}
		if worstAt < 0 {
			commit(scores, pool, weights)
			return shed
		}
		idx := pool[worstAt]
		scores[idx].Eligible = false
		scores[idx].Weight = 0
		scores[idx].Reason = fmt.Sprintf(
			"shed to hold the %.0fms p95 SLO (this backend is at %.0fms; blend projected %.0fms)",
			cfg.LatencySLOMs, worst, projected)
		shed = append(shed, ShedRecord{states[idx].Backend.ID, "latency SLO guardrail"})
		pool = append(pool[:worstAt], pool[worstAt+1:]...)
	}
}

// solve produces the weight vector for a pool of candidate indices.
func solve(states []signals.BackendState, scores []model.CandidateScore, pool []int, cfg PlanConfig) []float64 {
	sc := make([]float64, len(pool))
	bias := make([]float64, len(pool))
	caps := make([]float64, len(pool))
	for i, idx := range pool {
		sc[i] = scores[idx].Score
		bias[i] = states[idx].Backend.Bias * states[idx].Breaker.TrafficMultiplier
		caps[i] = states[idx].Backend.CapacityRPS
	}

	w := softmaxWeights(sc, bias, cfg.Temperature)
	applyCapacityCaps(w, caps, cfg.TotalRPS)
	renormalize(w)
	applyExplorationFloor(w, cfg.ExplorationFloor)
	return w
}

func commit(scores []model.CandidateScore, pool []int, weights []float64) {
	for i, idx := range pool {
		scores[idx].Weight = weights[i]
	}
}

// projectLatency returns the traffic-weighted p95 and the worst p95 among
// backends carrying meaningful traffic.
func projectLatency(states []signals.BackendState, scores []model.CandidateScore) (projected, worst float64) {
	byID := make(map[string]float64, len(scores))
	for _, c := range scores {
		byID[c.BackendID] = c.Weight
	}
	for _, s := range states {
		w := byID[s.Backend.ID]
		if w <= 0 {
			continue
		}
		projected += w * s.LatencyP95.Value
		if s.LatencyP95.Value > worst {
			worst = s.LatencyP95.Value
		}
	}
	return projected, worst
}

// zeroSetChanged reports whether any backend crossed into or out of zero
// traffic between two published distributions. Such a change is a routing
// event the data plane must learn about immediately, however small the
// numeric movement was.
func zeroSetChanged(next, prev []float64) bool {
	for i := range next {
		var p float64
		if i < len(prev) {
			p = prev[i]
		}
		if (next[i] <= 0) != (p <= 0) {
			return true
		}
	}
	return false
}
