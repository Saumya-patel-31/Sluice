package app

import (
	"runtime"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/router"
	"github.com/Saumya-patel-31/sluice/internal/signals"
	"github.com/Saumya-patel-31/sluice/internal/telemetry"
)

// Collector translates decisions and control-loop state into Prometheus
// metrics. It implements router.DecisionSink.
type Collector struct {
	reg *telemetry.Registry

	decisions     *telemetry.CounterVec
	denials       *telemetry.CounterVec
	decisionTime  *telemetry.HistogramVec
	costAvoided   *telemetry.CounterVec
	costAdded     *telemetry.CounterVec
	carbonAvoided *telemetry.CounterVec
	carbonAdded   *telemetry.CounterVec
	latencyDebt   *telemetry.CounterVec
	latencyCredit *telemetry.CounterVec
	bytesRouted   *telemetry.CounterVec

	weight        *telemetry.GaugeVec
	egressPrice   *telemetry.GaugeVec
	carbonPerGB   *telemetry.GaugeVec
	gridIntensity *telemetry.GaugeVec
	latencyP50    *telemetry.GaugeVec
	latencyP95    *telemetry.GaugeVec
	errorRate     *telemetry.GaugeVec
	circuitState  *telemetry.GaugeVec
	inFlight      *telemetry.GaugeVec
	signalAge     *telemetry.GaugeVec

	projectedP95 *telemetry.GaugeVec
	sloMet       *telemetry.GaugeVec
	planChurn    *telemetry.GaugeVec
	planGen      *telemetry.GaugeVec
	routeRPS     *telemetry.GaugeVec

	cacheHitRatio *telemetry.GaugeVec
	controlLoop   *telemetry.HistogramVec
	buildInfo     *telemetry.GaugeVec
}

const (
	backendLabels = 3 // backend, cloud, region
)

// NewCollector registers Sluice's metric families.
func NewCollector(reg *telemetry.Registry, version string) *Collector {
	c := &Collector{reg: reg}

	c.decisions = reg.Counter("sluice_decisions_total",
		"Routing decisions by route, verdict and selected cloud.", "route", "verdict", "cloud")
	c.denials = reg.Counter("sluice_denials_total",
		"Requests refused, by the reason the policy engine gave.", "reason")
	c.decisionTime = reg.Histogram("sluice_decision_duration_seconds",
		"Wall time to authorise and select a backend.",
		[]float64{5e-6, 1e-5, 2.5e-5, 5e-5, 1e-4, 2.5e-4, 5e-4, 1e-3, 5e-3, 1e-2}, "route")

	c.costAvoided = reg.Counter("sluice_egress_cost_avoided_usd_total",
		"Egress spend avoided versus a latency-only baseline.", "cloud")
	c.costAdded = reg.Counter("sluice_egress_cost_added_usd_total",
		"Extra egress spend accepted versus a latency-only baseline.", "cloud")
	c.carbonAvoided = reg.Counter("sluice_carbon_avoided_grams_total",
		"Emissions avoided versus a latency-only baseline.", "cloud")
	c.carbonAdded = reg.Counter("sluice_carbon_added_grams_total",
		"Extra emissions accepted versus a latency-only baseline.", "cloud")
	c.latencyDebt = reg.Counter("sluice_latency_debt_seconds_total",
		"Extra p95 latency accepted in exchange for cost or carbon savings.", "route")
	c.latencyCredit = reg.Counter("sluice_latency_credit_seconds_total",
		"p95 latency saved relative to the baseline choice.", "route")
	c.bytesRouted = reg.Counter("sluice_routed_bytes_total",
		"Bytes attributed to routing decisions, by destination.", "cloud", "region")

	c.weight = reg.Gauge("sluice_backend_traffic_share",
		"Fraction of a route's traffic allocated to a backend.", "route", "backend", "cloud", "region")
	c.egressPrice = reg.Gauge("sluice_backend_egress_usd_per_gb",
		"Resolved egress price.", "backend", "cloud", "region")
	c.carbonPerGB = reg.Gauge("sluice_backend_carbon_grams_per_gb",
		"Emissions per GB transferred, derived from grid intensity and PUE.", "backend", "cloud", "region")
	c.gridIntensity = reg.Gauge("sluice_backend_grid_intensity_grams_per_kwh",
		"Carbon intensity of the electricity grid serving this region.", "backend", "cloud", "region")
	c.latencyP50 = reg.Gauge("sluice_backend_latency_p50_seconds",
		"Median observed round-trip latency.", "backend", "cloud", "region")
	c.latencyP95 = reg.Gauge("sluice_backend_latency_p95_seconds",
		"95th percentile observed round-trip latency.", "backend", "cloud", "region")
	c.errorRate = reg.Gauge("sluice_backend_error_rate",
		"Time-decayed error rate.", "backend", "cloud", "region")
	c.circuitState = reg.Gauge("sluice_backend_circuit_state",
		"Circuit breaker: 0 closed, 1 half-open, 2 open.", "backend", "cloud", "region")
	c.inFlight = reg.Gauge("sluice_backend_inflight_requests",
		"Requests currently in flight to a backend.", "backend", "cloud", "region")
	c.signalAge = reg.Gauge("sluice_signal_age_seconds",
		"Age of the newest quote for a signal. A rising value means the router is deciding on stale data.",
		"backend", "signal")

	c.projectedP95 = reg.Gauge("sluice_route_projected_p95_seconds",
		"Traffic-weighted p95 the current distribution is expected to deliver.", "route")
	c.sloMet = reg.Gauge("sluice_route_slo_met",
		"1 when the projected p95 is within the route's SLO, 0 otherwise.", "route")
	c.planChurn = reg.Gauge("sluice_route_plan_churn",
		"L1 distance between the newly computed distribution and the published one.", "route")
	c.planGen = reg.Gauge("sluice_route_plan_generation",
		"How many times the distribution has actually been republished.", "route")
	c.routeRPS = reg.Gauge("sluice_route_requests_per_second",
		"Observed request rate.", "route")

	c.cacheHitRatio = reg.Gauge("sluice_policy_cache_hit_ratio",
		"Lifetime hit rate of the authorisation cache.")
	c.controlLoop = reg.Histogram("sluice_control_loop_duration_seconds",
		"Wall time to recompute every route's traffic distribution.",
		[]float64{1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1})
	c.buildInfo = reg.Gauge("sluice_build_info",
		"Build metadata; the value is always 1.", "version", "goversion")
	c.buildInfo.With(version, runtime.Version()).Set(1)

	return c
}

// Record folds one decision into the counters. It satisfies
// router.DecisionSink and runs on the request path, so it does no allocation
// beyond label lookup.
func (c *Collector) Record(d *model.Decision) {
	if d == nil {
		return
	}
	cloud := string(d.Cloud)
	if cloud == "" {
		cloud = "none"
	}
	route := d.RouteID
	if route == "" {
		route = "unmatched"
	}

	c.decisions.With(route, string(d.Verdict), cloud).Inc()
	c.decisionTime.With(route).Observe(float64(d.ComputeMicros) / 1e6)

	if d.Verdict != model.VerdictAllow {
		reason := d.DenyReason
		if reason == "" {
			reason = string(d.Verdict)
		}
		c.denials.With(reason).Inc()
		return
	}

	// Savings are signed. Splitting them into two monotonic counters keeps
	// them usable with rate() while still reporting honestly that a decision
	// sometimes costs more than the baseline would have.
	if d.SavedUSD >= 0 {
		c.costAvoided.With(cloud).Add(d.SavedUSD)
	} else {
		c.costAdded.With(cloud).Add(-d.SavedUSD)
	}
	if d.SavedGrams >= 0 {
		c.carbonAvoided.With(cloud).Add(d.SavedGrams)
	} else {
		c.carbonAdded.With(cloud).Add(-d.SavedGrams)
	}
	if d.LatencyDeltaMs >= 0 {
		c.latencyDebt.With(route).Add(d.LatencyDeltaMs / 1000)
	} else {
		c.latencyCredit.With(route).Add(-d.LatencyDeltaMs / 1000)
	}
	c.bytesRouted.With(cloud, d.Region).Add(float64(d.Request.EstimatedBytes))
}

// ObserveState refreshes the gauges from the current control-loop state.
//
// Every per-backend gauge family is reset first. Without that, a backend
// removed from configuration keeps exporting its final traffic share forever,
// and an alert on "traffic to a decommissioned region" never clears.
func (c *Collector) ObserveState(e *router.Engine, snap signals.Snapshot, elapsed time.Duration) {
	c.controlLoop.With().Observe(elapsed.Seconds())

	for _, g := range []*telemetry.GaugeVec{
		c.weight, c.egressPrice, c.carbonPerGB, c.gridIntensity,
		c.latencyP50, c.latencyP95, c.errorRate, c.circuitState,
		c.inFlight, c.signalAge, c.projectedP95, c.sloMet,
		c.planChurn, c.planGen, c.routeRPS,
	} {
		g.Reset()
	}

	now := snap.Taken
	for _, b := range snap.Backends {
		id, cloud, region := b.Backend.ID, string(b.Backend.Cloud), b.Backend.Region

		c.egressPrice.With(id, cloud, region).Set(b.Egress.Value)
		c.carbonPerGB.With(id, cloud, region).Set(b.CarbonPerGB.Value)
		c.gridIntensity.With(id, cloud, region).Set(b.GridIntensity.Value)
		c.latencyP50.With(id, cloud, region).Set(b.LatencyP50.Value / 1000)
		c.latencyP95.With(id, cloud, region).Set(b.LatencyP95.Value / 1000)
		c.errorRate.With(id, cloud, region).Set(b.ErrorRate.Value)
		c.inFlight.With(id, cloud, region).Set(float64(b.InFlight))

		state := 0.0
		switch b.Breaker.State {
		case signals.BreakerHalfOpen:
			state = 1
		case signals.BreakerOpen:
			state = 2
		}
		c.circuitState.With(id, cloud, region).Set(state)

		c.signalAge.With(id, "egress").Set(b.Egress.Age(now).Seconds())
		c.signalAge.With(id, "gridIntensity").Set(b.GridIntensity.Age(now).Seconds())
	}

	for routeID, plan := range e.Plans() {
		c.projectedP95.With(routeID).Set(plan.ProjectedP95 / 1000)
		c.planChurn.With(routeID).Set(plan.Churn)
		c.planGen.With(routeID).Set(float64(plan.Generation))
		c.routeRPS.With(routeID).Set(e.RouteRPS(routeID))
		if plan.SLOMet {
			c.sloMet.With(routeID).Set(1)
		} else {
			c.sloMet.With(routeID).Set(0)
		}
		for _, cand := range plan.Candidates {
			c.weight.With(routeID, cand.BackendID, string(cand.Cloud), cand.Region).Set(cand.Weight)
		}
	}

	_, _, ratio := e.PolicyCacheStats()
	c.cacheHitRatio.With().Set(ratio)
}
