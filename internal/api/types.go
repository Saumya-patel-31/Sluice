package api

import (
	"time"

	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/signals"
	"github.com/saumyapatel/sluice/internal/telemetry"
)

// Status is the control plane's identity and health.
type Status struct {
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds float64   `json:"uptimeSeconds"`
	PolicyHash    string    `json:"policyHash"`
	PolicyCount   int       `json:"policyCount"`
	PolicyPath    string    `json:"policyPath,omitempty"`
	PolicyLoaded  time.Time `json:"policyLoadedAt"`
	Generation    uint64    `json:"generation"`
	DemoMode      bool      `json:"demoMode"`
	Backends      int       `json:"backends"`
	Routes        int       `json:"routes"`
	// AuthEnabled reports whether a token gates mutating API calls.
	AuthEnabled bool `json:"authEnabled"`
	// AnonymousMutations is true when the write API was explicitly opened.
	// The dashboard shows a standing warning while it is, because an open
	// policy editor is not a state anyone should reach without noticing.
	AnonymousMutations bool `json:"anonymousMutations"`
	// DeniedAPIRequests counts refused control-plane calls, so probing the
	// admin surface is visible rather than silent.
	DeniedAPIRequests uint64 `json:"deniedApiRequests"`
	// PriceTableAsOf dates the bundled list prices, so the UI can say plainly
	// where a number came from.
	PriceTableAsOf string `json:"priceTableAsOf"`
	// CarbonModel is surfaced because the emissions figures are only as
	// meaningful as the assumption behind them.
	CarbonModel signals.CarbonModel `json:"carbonModel"`
}

// KPIs are the headline numbers.
type KPIs struct {
	DecisionsPerSecond float64 `json:"decisionsPerSecond"`
	TotalDecisions     uint64  `json:"totalDecisions"`
	AllowRate          float64 `json:"allowRate"`
	DenyRate           float64 `json:"denyRate"`

	// SavedUSD and SavedGrams are cumulative deltas against the latency-only
	// baseline, and can be negative when the router deliberately spends more.
	SavedUSD   float64 `json:"savedUsd"`
	SavedGrams float64 `json:"savedGrams"`
	// Run rates are what an operator watches; totals only ever grow.
	SavingsUSDPerHour   float64 `json:"savingsUsdPerHour"`
	SavingsGramsPerHour float64 `json:"savingsGramsPerHour"`
	// ProjectedAnnualUSD extrapolates the current run rate. It is a
	// projection, labelled as one, not a measurement.
	ProjectedAnnualUSD float64 `json:"projectedAnnualUsd"`
	// EquivalentKmDriven converts avoided grams into a familiar unit at
	// roughly 120 gCO2e per passenger-kilometre.
	EquivalentKmDriven float64 `json:"equivalentKmDriven"`

	// LatencyDebtMs is the total extra latency accepted for those savings.
	LatencyDebtMs  float64 `json:"latencyDebtMs"`
	BlendedP95Ms   float64 `json:"blendedP95Ms"`
	DecisionMeanUs float64 `json:"decisionMeanUs"`
	DecisionMaxUs  int64   `json:"decisionMaxUs"`

	PolicyCacheHitRate float64 `json:"policyCacheHitRate"`
	HealthyBackends    int     `json:"healthyBackends"`
	TotalBackends      int     `json:"totalBackends"`
	OpenBreakers       int     `json:"openBreakers"`
	StaleSignals       int     `json:"staleSignals"`
	RoutesBreachingSLO int     `json:"routesBreachingSlo"`
	BytesRouted        int64   `json:"bytesRouted"`
}

// CloudSummary aggregates one provider.
type CloudSummary struct {
	Cloud       model.Cloud `json:"cloud"`
	Display     string      `json:"display"`
	Backends    int         `json:"backends"`
	Healthy     int         `json:"healthy"`
	Share       float64     `json:"share"`
	RPS         float64     `json:"rps"`
	Decisions   uint64      `json:"decisions"`
	AvgEgress   float64     `json:"avgEgressUsdPerGb"`
	AvgCarbon   float64     `json:"avgCarbonGramsPerGb"`
	AvgLatency  float64     `json:"avgLatencyP95Ms"`
	SpentUSD    float64     `json:"spentUsd"`
	EmittedGram float64     `json:"emittedGrams"`
}

// RouteSummary is one route and its current distribution.
type RouteSummary struct {
	Route        model.Route            `json:"route"`
	RPS          float64                `json:"rps"`
	Weights      map[string]float64     `json:"weights"`
	Candidates   []model.CandidateScore `json:"candidates"`
	Objectives   model.Vector           `json:"objectives"`
	ProjectedP95 float64                `json:"projectedP95Ms"`
	WorstP95     float64                `json:"worstP95Ms"`
	SLOMet       bool                   `json:"sloMet"`
	Churn        float64                `json:"churn"`
	Generation   uint64                 `json:"generation"`
	Shed         []shedView             `json:"shed,omitempty"`
}

type shedView struct {
	BackendID string `json:"backendId"`
	Reason    string `json:"reason"`
}

// BackendView is one backend with its signals and current share.
type BackendView struct {
	signals.BackendState
	// Share is the highest weight this backend holds on any route, which is
	// what the topology view draws.
	Share float64 `json:"share"`
	// PerRoute breaks that down.
	PerRoute map[string]float64 `json:"perRoute"`
	RPS      float64            `json:"rps"`
	Zone     signals.GridZone   `json:"zone"`
}

// Overview is the dashboard's primary payload.
type Overview struct {
	Status    Status                     `json:"status"`
	KPIs      KPIs                       `json:"kpis"`
	Clouds    []CloudSummary             `json:"clouds"`
	Routes    []RouteSummary             `json:"routes"`
	Backends  []BackendView              `json:"backends"`
	Series    map[string][]signals.Point `json:"series"`
	Summary   telemetry.Summary          `json:"summary"`
	Incidents []incidentView             `json:"incidents"`
	Now       time.Time                  `json:"now"`
}

type incidentView struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	BackendID    string    `json:"backendId"`
	Magnitude    float64   `json:"magnitude"`
	StartedAt    time.Time `json:"startedAt"`
	EndsAt       time.Time `json:"endsAt"`
	Note         string    `json:"note"`
	Active       bool      `json:"active"`
	RemainingSec float64   `json:"remainingSeconds"`
}

// TopologyNode is a vertex in the routing graph.
type TopologyNode struct {
	ID     string      `json:"id"`
	Kind   string      `json:"kind"` // "gateway" | "cloud" | "region"
	Label  string      `json:"label"`
	Cloud  model.Cloud `json:"cloud,omitempty"`
	Region string      `json:"region,omitempty"`
	// Jurisdiction and GridZone drive the residency and carbon overlays.
	Jurisdiction string  `json:"jurisdiction,omitempty"`
	GridZone     string  `json:"gridZone,omitempty"`
	Share        float64 `json:"share"`
	RPS          float64 `json:"rps"`
	LatencyP95   float64 `json:"latencyP95Ms,omitempty"`
	CarbonPerGB  float64 `json:"carbonGramsPerGb,omitempty"`
	EgressPrice  float64 `json:"egressUsdPerGb,omitempty"`
	Healthy      bool    `json:"healthy"`
	Breaker      string  `json:"breaker,omitempty"`
	Reason       string  `json:"reason,omitempty"`
}

// TopologyEdge is a traffic path between two nodes.
type TopologyEdge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Share  float64 `json:"share"`
	RPS    float64 `json:"rps"`
	Active bool    `json:"active"`
}

// Topology is the graph the topology view renders.
type Topology struct {
	RouteID string         `json:"routeId"`
	Nodes   []TopologyNode `json:"nodes"`
	Edges   []TopologyEdge `json:"edges"`
}

// PolicyView is the policy editor's payload.
type PolicyView struct {
	Source     string              `json:"source"`
	Hash       string              `json:"hash"`
	LoadedAt   time.Time           `json:"loadedAt"`
	Path       string              `json:"path,omitempty"`
	Policies   []*policyBrief      `json:"policies"`
	Attributes map[string][]string `json:"attributes"`
	Builtins   map[string]string   `json:"builtins"`
}

type policyBrief struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Priority    int                `json:"priority"`
	Effect      string             `json:"effect"`
	Message     string             `json:"message,omitempty"`
	Tags        []string           `json:"tags,omitempty"`
	When        string             `json:"when,omitempty"`
	Require     string             `json:"require,omitempty"`
	Prefer      map[string]float64 `json:"prefer,omitempty"`
}

// BacktestResult reports what a candidate policy document would change,
// measured by replaying retained decisions through both the live document and
// the candidate and diffing the two outcomes.
type BacktestResult struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Policies int    `json:"policies"`
	Hash     string `json:"hash,omitempty"`

	// Replayed is how many retained decisions were re-evaluated.
	Replayed int `json:"replayed"`
	// Unchanged, NewlyDenied, NewlyAllowed and NarrowedPool classify the
	// difference against what actually happened.
	Unchanged    int `json:"unchanged"`
	NewlyDenied  int `json:"newlyDenied"`
	NewlyAllowed int `json:"newlyAllowed"`
	NarrowedPool int `json:"narrowedPool"`
	WidenedPool  int `json:"widenedPool"`

	// Samples are illustrative changed decisions, capped for readability.
	Samples []BacktestSample `json:"samples"`
}

// BacktestSample is one decision whose outcome would change.
type BacktestSample struct {
	DecisionID  string    `json:"decisionId"`
	Timestamp   time.Time `json:"ts"`
	Subject     string    `json:"subject"`
	Path        string    `json:"path"`
	DataClass   string    `json:"dataClass,omitempty"`
	Was         string    `json:"was"`
	Now         string    `json:"now"`
	Change      string    `json:"change"`
	Reason      string    `json:"reason,omitempty"`
	EligibleWas int       `json:"eligibleWas"`
	EligibleNow int       `json:"eligibleNow"`
}
