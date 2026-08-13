// Package model defines the domain types shared by the routing engine, the
// policy engine, the data plane and the API surface.
//
// Nothing here imports another Sluice package, which keeps the dependency
// graph acyclic and makes these types safe to embed in wire formats.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Cloud identifies a cloud provider. Sluice treats providers as opaque
// routing domains; adding a fourth is a matter of registering backends and
// price/carbon rows, not changing the engine.
type Cloud string

const (
	CloudAWS    Cloud = "aws"
	CloudGCP    Cloud = "gcp"
	CloudAzure  Cloud = "azure"
	CloudOnPrem Cloud = "onprem"
)

// Valid reports whether c is a cloud Sluice ships datasets for.
func (c Cloud) Valid() bool {
	switch c {
	case CloudAWS, CloudGCP, CloudAzure, CloudOnPrem:
		return true
	}
	return false
}

// Display returns the human-facing provider name.
func (c Cloud) Display() string {
	switch c {
	case CloudAWS:
		return "AWS"
	case CloudGCP:
		return "Google Cloud"
	case CloudAzure:
		return "Azure"
	case CloudOnPrem:
		return "On-Prem"
	}
	return string(c)
}

// -----------------------------------------------------------------------------
// Objective vector
// -----------------------------------------------------------------------------

// Dimension enumerates the objectives the router optimises over. Every
// dimension is expressed as "lower is better" so normalisation and scoring can
// treat them uniformly.
type Dimension int

const (
	// DimCost is egress price in USD per GB.
	DimCost Dimension = iota
	// DimLatency is observed p95 round-trip latency in milliseconds.
	DimLatency
	// DimCarbon is grams of CO2-equivalent per GB transferred.
	DimCarbon
	// DimReliability is the observed error rate in [0,1].
	DimReliability

	NumDimensions
)

var dimensionNames = [NumDimensions]string{"cost", "latency", "carbon", "reliability"}

// String returns the lowercase wire name of the dimension.
func (d Dimension) String() string {
	if d < 0 || d >= NumDimensions {
		return fmt.Sprintf("dim(%d)", int(d))
	}
	return dimensionNames[d]
}

// Unit returns the display unit for the dimension.
func (d Dimension) Unit() string {
	switch d {
	case DimCost:
		return "USD/GB"
	case DimLatency:
		return "ms"
	case DimCarbon:
		return "gCO2e/GB"
	case DimReliability:
		return "err rate"
	}
	return ""
}

// ParseDimension resolves a wire name to a Dimension.
func ParseDimension(s string) (Dimension, bool) {
	for i, name := range dimensionNames {
		if strings.EqualFold(s, name) {
			return Dimension(i), true
		}
	}
	return 0, false
}

// Vector is a value per objective dimension. It is used for raw signals,
// normalised signals, per-dimension weighted contributions and objective
// weights alike, which is what lets the explainability trace line up exactly
// with the arithmetic the router performed.
type Vector [NumDimensions]float64

// Sum returns the total across all dimensions.
func (v Vector) Sum() float64 {
	var t float64
	for _, x := range v {
		t += x
	}
	return t
}

// Scale returns v with every component multiplied by f.
func (v Vector) Scale(f float64) Vector {
	for i := range v {
		v[i] *= f
	}
	return v
}

// Mul returns the element-wise product of v and o.
func (v Vector) Mul(o Vector) Vector {
	for i := range v {
		v[i] *= o[i]
	}
	return v
}

// Normalized returns v scaled so its components sum to 1. A zero vector is
// returned as a uniform distribution, which keeps callers from having to
// special-case the degenerate configuration.
func (v Vector) Normalized() Vector {
	total := v.Sum()
	if total <= 0 {
		var u Vector
		for i := range u {
			u[i] = 1 / float64(NumDimensions)
		}
		return u
	}
	return v.Scale(1 / total)
}

// MarshalJSON emits the vector as a named object so API consumers never have
// to know the dimension ordering.
func (v Vector) MarshalJSON() ([]byte, error) {
	m := make(map[string]float64, NumDimensions)
	for i, name := range dimensionNames {
		m[name] = v[i]
	}
	return json.Marshal(m)
}

// UnmarshalJSON accepts the named-object form produced by MarshalJSON.
// Unknown keys are rejected so a typo in a config file surfaces immediately
// rather than silently zeroing an objective.
func (v *Vector) UnmarshalJSON(b []byte) error {
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out Vector
	for k, val := range m {
		d, ok := ParseDimension(k)
		if !ok {
			return fmt.Errorf("model: unknown objective dimension %q", k)
		}
		out[d] = val
	}
	*v = out
	return nil
}

// -----------------------------------------------------------------------------
// Backends
// -----------------------------------------------------------------------------

// Backend is one routable upstream: a single deployment of a service in one
// cloud region.
type Backend struct {
	ID          string `json:"id"`
	Cloud       Cloud  `json:"cloud"`
	Region      string `json:"region"`
	DisplayName string `json:"displayName"`
	// Address is the upstream origin, e.g. "https://10.4.1.7:8443".
	Address string `json:"address"`
	// Jurisdiction is the legal region the data lands in ("US", "EU", "IN",
	// "APAC"). Residency policies match on this rather than on Region so a
	// policy survives a region rename.
	Jurisdiction string `json:"jurisdiction"`
	// GridZone keys into the carbon-intensity dataset, e.g. "US-MIDA-PJM".
	GridZone string `json:"gridZone"`
	// Tier lets policies distinguish e.g. "primary" from "burst" capacity.
	Tier string `json:"tier,omitempty"`
	// Labels are arbitrary key/value pairs addressable from policy.
	Labels map[string]string `json:"labels,omitempty"`
	// Bias multiplies the final score. 1.0 is neutral; use it to express a
	// commercial commitment ("we prepaid for GCP capacity") without distorting
	// the measured signals.
	Bias float64 `json:"bias"`
	// Capacity is the sustainable request rate for this backend. The router
	// will not allocate a weight implying more than this.
	CapacityRPS float64 `json:"capacityRps"`
	Enabled     bool    `json:"enabled"`
}

// Label returns the value of a label, or "" when unset.
func (b *Backend) Label(k string) string {
	if b.Labels == nil {
		return ""
	}
	return b.Labels[k]
}

// -----------------------------------------------------------------------------
// Routes
// -----------------------------------------------------------------------------

// Route is a logical service that traffic is balanced across. It carries the
// objective weights and the SLO the router must not trade away in pursuit of
// cheaper or greener egress.
type Route struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	// Match is the path prefix this route claims.
	PathPrefix string `json:"pathPrefix"`
	// BackendIDs restricts the candidate pool. Empty means all backends.
	BackendIDs []string `json:"backendIds,omitempty"`
	// Objectives are the relative weights of cost/latency/carbon/reliability.
	// They are normalised to sum to 1 before scoring.
	Objectives Vector `json:"objectives"`
	// LatencySLOMs is the p95 the blended route must stay under. When the
	// cheapest blend would breach it, the router sheds the slowest candidates
	// until the projection fits. Zero disables the guardrail.
	LatencySLOMs float64 `json:"latencySloMs"`
	// Temperature controls how sharply score differences translate into
	// traffic share. Near 0 is winner-take-all; larger values spread traffic.
	Temperature float64 `json:"temperature"`
	// RequireMTLS rejects requests that arrive without a verified peer cert.
	RequireMTLS bool `json:"requireMtls"`
}

// -----------------------------------------------------------------------------
// Zero-trust identity
// -----------------------------------------------------------------------------

// Subject is the authenticated identity behind a request. Sluice never trusts
// network position: every decision is made against this struct, which is
// populated only from cryptographically verified material (a client
// certificate's URI SAN, or a validated JWT).
type Subject struct {
	// ID is the SPIFFE ID or JWT subject, e.g.
	// "spiffe://prod.internal/ns/payments/sa/checkout".
	ID string `json:"id"`
	// TrustDomain is the SPIFFE trust domain, "prod.internal" above.
	TrustDomain string `json:"trustDomain,omitempty"`
	// Namespace and Service are parsed out of a SPIFFE workload path when
	// present, so policies can match them without string surgery.
	Namespace string `json:"namespace,omitempty"`
	Service   string `json:"service,omitempty"`
	// Issuer is the CA subject or JWT issuer that vouched for this identity.
	Issuer string `json:"issuer,omitempty"`
	// MTLS reports whether the identity came from a verified client cert.
	MTLS bool `json:"mtls"`
	// Claims carries additional verified attributes (JWT claims, cert
	// extensions) addressable from policy as subject.claims["..."].
	Claims map[string]string `json:"claims,omitempty"`
	// Authenticated is false for anonymous traffic. Under zero trust the
	// default policy denies these; it is not an implicit allow.
	Authenticated bool `json:"authenticated"`
}

// Anonymous is the Subject attached to unauthenticated requests.
func Anonymous() Subject {
	return Subject{ID: "anonymous", Authenticated: false}
}

// Claim returns a verified claim value, or "" when absent.
func (s *Subject) Claim(k string) string {
	if s.Claims == nil {
		return ""
	}
	return s.Claims[k]
}

// -----------------------------------------------------------------------------
// Requests
// -----------------------------------------------------------------------------

// DataClass describes the sensitivity of the payload, which residency and
// sovereignty policies key off.
type DataClass string

const (
	DataPublic       DataClass = "public"
	DataInternal     DataClass = "internal"
	DataConfidential DataClass = "confidential"
	DataPII          DataClass = "pii"
	DataRegulated    DataClass = "regulated"
)

// Request is the attribute set a decision is made against. It is deliberately
// small: anything not in here cannot influence routing, which makes decisions
// reproducible from the ledger alone.
type Request struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Host     string `json:"host,omitempty"`
	SourceIP string `json:"sourceIp,omitempty"`
	// SourceGeo is a coarse origin hint ("us-east", "eu-west") used to bias
	// latency estimates before any probe data exists.
	SourceGeo string    `json:"sourceGeo,omitempty"`
	DataClass DataClass `json:"dataClass,omitempty"`
	// EstimatedBytes is the expected response size, used to convert a per-GB
	// price into a per-request cost. Defaults to the route's rolling mean.
	EstimatedBytes int64             `json:"estimatedBytes,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
}

// Header returns a request header value by canonical-insensitive key.
func (r *Request) Header(k string) string {
	if r.Headers == nil {
		return ""
	}
	if v, ok := r.Headers[k]; ok {
		return v
	}
	return r.Headers[strings.ToLower(k)]
}

// -----------------------------------------------------------------------------
// Decisions
// -----------------------------------------------------------------------------

// Verdict is the zero-trust outcome of a request.
type Verdict string

const (
	// VerdictAllow means the request was authorised and routed.
	VerdictAllow Verdict = "allow"
	// VerdictDeny means policy rejected the request outright.
	VerdictDeny Verdict = "deny"
	// VerdictNoCapacity means policy allowed the request but no backend
	// survived the constraints — a distinct failure mode from a deny, and one
	// that should page someone.
	VerdictNoCapacity Verdict = "no_capacity"
)

// PolicyHit records one policy statement that was evaluated, and whether it
// fired. The full trace of every request is retained so an operator can answer
// "why was this allowed?" without re-running anything.
type PolicyHit struct {
	Policy  string `json:"policy"`
	Effect  string `json:"effect"`
	Matched bool   `json:"matched"`
	// Detail explains a constraint's effect, e.g. "pruned 2 backends
	// (jurisdiction not in [EU])".
	Detail string `json:"detail,omitempty"`
	// Error is set when the expression failed to evaluate. A policy that
	// errors is treated as matching a deny — fail closed.
	Error string `json:"error,omitempty"`
}

// CandidateScore is the complete arithmetic for one backend in one decision.
// Raw, Normalized and Contribution are the same vector at three stages of the
// pipeline, so the dashboard can render the derivation step by step.
type CandidateScore struct {
	BackendID string `json:"backendId"`
	Cloud     Cloud  `json:"cloud"`
	Region    string `json:"region"`
	Eligible  bool   `json:"eligible"`
	// Reason explains ineligibility (policy prune, open circuit, SLO shed).
	Reason string `json:"reason,omitempty"`
	// Raw holds the measured signal values in their natural units.
	Raw Vector `json:"raw"`
	// Normalized holds each signal min-max scaled to [0,1] across the
	// candidate set for this decision. 0 is best.
	Normalized Vector `json:"normalized"`
	// Contribution is Normalized multiplied by the route's objective weights.
	// Its sum is the backend's penalty; Score is 1 minus that.
	Contribution Vector `json:"contribution"`
	// Score in [0,1]; higher is better. Includes the backend Bias multiplier.
	Score float64 `json:"score"`
	// Weight is the share of traffic allocated to this backend in [0,1].
	Weight float64 `json:"weight"`
}

// Decision is the full, self-contained record of one routing decision.
type Decision struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"ts"`
	RouteID   string    `json:"routeId"`
	Subject   Subject   `json:"subject"`
	Request   Request   `json:"request"`
	Verdict   Verdict   `json:"verdict"`
	// DenyReason is set when Verdict is not allow.
	DenyReason string `json:"denyReason,omitempty"`
	// ChosenBackend is the backend traffic was sent to.
	ChosenBackend string `json:"chosenBackend,omitempty"`
	Cloud         Cloud  `json:"cloud,omitempty"`
	Region        string `json:"region,omitempty"`
	// Objectives is the weight vector actually used, after normalisation.
	Objectives  Vector           `json:"objectives"`
	Candidates  []CandidateScore `json:"candidates"`
	PolicyTrace []PolicyHit      `json:"policyTrace"`
	// Baseline is the backend a naive latency-only load balancer would have
	// picked. The savings figures are measured against this counterfactual.
	BaselineBackend string `json:"baselineBackend,omitempty"`
	// SavedUSD and SavedGrams are per-request deltas versus the baseline.
	// They can be negative when the router deliberately spends more to hold
	// an SLO, and the dashboard shows that honestly.
	SavedUSD   float64 `json:"savedUsd"`
	SavedGrams float64 `json:"savedGrams"`
	// LatencyDeltaMs is the p95 penalty accepted versus the baseline.
	LatencyDeltaMs float64 `json:"latencyDeltaMs"`
	// ComputeMicros is how long the decision itself took.
	ComputeMicros int64 `json:"computeMicros"`
	// Cached reports that the policy verdict was served from the decision
	// cache rather than re-evaluated.
	Cached bool `json:"cached"`
}

// Allowed reports whether traffic actually flowed.
func (d *Decision) Allowed() bool { return d.Verdict == VerdictAllow }

// Candidate returns the score record for a backend ID.
func (d *Decision) Candidate(backendID string) (CandidateScore, bool) {
	for _, c := range d.Candidates {
		if c.BackendID == backendID {
			return c, true
		}
	}
	return CandidateScore{}, false
}
