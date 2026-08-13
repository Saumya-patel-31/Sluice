package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/policy"
	"github.com/saumyapatel/sluice/internal/signals"
	"github.com/saumyapatel/sluice/internal/sim"
	"github.com/saumyapatel/sluice/internal/telemetry"
)

func telemetryFilter(limit int) telemetry.Filter { return telemetry.Filter{Limit: limit} }

// -----------------------------------------------------------------------------
// Backends
// -----------------------------------------------------------------------------

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	ov := s.buildOverview(8)
	writeJSON(w, http.StatusOK, map[string]any{
		"backends":    ov.Backends,
		"carbonModel": s.app.Store.CarbonModel(),
		"asOf":        time.Now(),
	})
}

func (s *Server) handleBackendHistory(w http.ResponseWriter, r *http.Request) {
	id := normalizePathID(r.PathValue("id"))
	if _, ok := s.app.Store.Backend(id); !ok {
		writeError(w, http.StatusNotFound, "unknown backend "+id)
		return
	}
	n := queryInt(r, "points", 120, 8, 600)

	out := map[string]any{"backendId": id}
	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		out[d.String()] = s.app.Store.History(id, d, n)
	}
	out["weight"] = s.app.Store.WeightHistory(id, n)
	out["rps"] = s.app.Store.RPSHistory(id, n)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleZones(w http.ResponseWriter, r *http.Request) {
	zones := signals.Zones()
	list := make([]signals.GridZone, 0, len(zones))
	for _, z := range zones {
		list = append(list, z)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].BaseIntensity < list[j].BaseIntensity })
	writeJSON(w, http.StatusOK, map[string]any{"zones": list})
}

// -----------------------------------------------------------------------------
// Routes and topology
// -----------------------------------------------------------------------------

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	ov := s.buildOverview(8)
	writeJSON(w, http.StatusOK, map[string]any{"routes": ov.Routes})
}

// handleTopology renders the routing graph for one route.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	routeID := r.URL.Query().Get("route")
	plans := s.app.Engine.Plans()

	if routeID == "" {
		// Default to the busiest route, which is what an operator opening the
		// view almost always wants to look at.
		best := -1.0
		for id := range plans {
			if rps := s.app.Engine.RouteRPS(id); rps > best {
				best, routeID = rps, id
			}
		}
	}
	plan := plans[routeID]
	if plan == nil {
		writeError(w, http.StatusNotFound, "unknown route "+routeID)
		return
	}

	snap := s.app.Engine.Snapshot()
	byID := make(map[string]signals.BackendState, len(snap.Backends))
	for _, b := range snap.Backends {
		byID[b.Backend.ID] = b
	}
	backendRPS := s.app.Engine.BackendRPS()
	routeRPS := s.app.Engine.RouteRPS(routeID)

	top := Topology{RouteID: routeID}
	top.Nodes = append(top.Nodes, TopologyNode{
		ID: "gateway", Kind: "gateway", Label: "Sluice gateway",
		Share: 1, RPS: routeRPS, Healthy: true,
	})

	cloudShare := map[model.Cloud]float64{}
	cloudRPS := map[model.Cloud]float64{}

	for _, c := range plan.Candidates {
		b, ok := byID[c.BackendID]
		if !ok {
			continue
		}
		rps := routeRPS * c.Weight
		cloudShare[b.Backend.Cloud] += c.Weight
		cloudRPS[b.Backend.Cloud] += rps

		top.Nodes = append(top.Nodes, TopologyNode{
			ID: c.BackendID, Kind: "region", Label: b.Backend.DisplayName,
			Cloud: b.Backend.Cloud, Region: b.Backend.Region,
			Jurisdiction: b.Backend.Jurisdiction, GridZone: b.Backend.GridZone,
			Share: c.Weight, RPS: rps,
			LatencyP95: b.LatencyP95.Value, CarbonPerGB: b.CarbonPerGB.Value,
			EgressPrice: b.Egress.Value, Healthy: b.Healthy && c.Eligible,
			Breaker: string(b.Breaker.State), Reason: c.Reason,
		})
		top.Edges = append(top.Edges, TopologyEdge{
			From: string(b.Backend.Cloud), To: c.BackendID,
			Share: c.Weight, RPS: rps, Active: c.Weight > 0,
		})
	}

	clouds := make([]model.Cloud, 0, len(cloudShare))
	for c := range cloudShare {
		clouds = append(clouds, c)
	}
	sort.Slice(clouds, func(i, j int) bool { return clouds[i] < clouds[j] })
	for _, c := range clouds {
		top.Nodes = append(top.Nodes, TopologyNode{
			ID: string(c), Kind: "cloud", Label: c.Display(), Cloud: c,
			Share: cloudShare[c], RPS: cloudRPS[c], Healthy: true,
		})
		top.Edges = append(top.Edges, TopologyEdge{
			From: "gateway", To: string(c),
			Share: cloudShare[c], RPS: cloudRPS[c], Active: cloudShare[c] > 0,
		})
	}

	_ = backendRPS
	writeJSON(w, http.StatusOK, top)
}

// -----------------------------------------------------------------------------
// Decisions
// -----------------------------------------------------------------------------

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := telemetry.Filter{
		Verdict: q.Get("verdict"),
		Cloud:   q.Get("cloud"),
		Region:  q.Get("region"),
		RouteID: q.Get("route"),
		Backend: q.Get("backend"),
		Subject: q.Get("subject"),
		Path:    q.Get("path"),
		Limit:   queryInt(r, "limit", 100, 1, 1000),
	}
	if v := q.Get("minSavedUsd"); v != "" {
		if f64, err := strconv.ParseFloat(v, 64); err == nil {
			f.MinSavedUSD = f64
		}
	}
	if v := q.Get("sinceSeconds"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			f.Since = time.Now().Add(-time.Duration(secs * float64(time.Second)))
		}
	}

	decisions := s.app.Ledger.Recent(f)

	// The list view does not render candidate arrays or policy traces, and
	// shipping them for a thousand rows is most of the payload. The detail
	// endpoint serves the full record for the one decision being inspected.
	if q.Get("full") != "true" {
		brief := make([]map[string]any, 0, len(decisions))
		for _, d := range decisions {
			brief = append(brief, briefDecision(d))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"decisions": brief, "count": len(brief), "retained": s.app.Ledger.Retained(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decisions": decisions, "count": len(decisions), "retained": s.app.Ledger.Retained(),
	})
}

func briefDecision(d *model.Decision) map[string]any {
	return map[string]any{
		"id":              d.ID,
		"ts":              d.Timestamp,
		"routeId":         d.RouteID,
		"verdict":         d.Verdict,
		"denyReason":      d.DenyReason,
		"subject":         d.Subject.ID,
		"service":         d.Subject.Service,
		"mtls":            d.Subject.MTLS,
		"method":          d.Request.Method,
		"path":            d.Request.Path,
		"dataClass":       d.Request.DataClass,
		"chosenBackend":   d.ChosenBackend,
		"cloud":           d.Cloud,
		"region":          d.Region,
		"baselineBackend": d.BaselineBackend,
		"savedUsd":        d.SavedUSD,
		"savedGrams":      d.SavedGrams,
		"latencyDeltaMs":  d.LatencyDeltaMs,
		"computeMicros":   d.ComputeMicros,
		"cached":          d.Cached,
		"bytes":           d.Request.EstimatedBytes,
	}
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	id := normalizePathID(r.PathValue("id"))
	d, ok := s.app.Ledger.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound,
			"decision "+id+" is not retained; the ledger keeps the most recent "+
				strconv.Itoa(s.app.Cfg.Ledger.Capacity))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// -----------------------------------------------------------------------------
// Policy
// -----------------------------------------------------------------------------

func (s *Server) handlePolicyGet(w http.ResponseWriter, r *http.Request) {
	set := s.app.Engine.Policy()
	view := PolicyView{
		Source:     set.Source(),
		Hash:       set.Hash(),
		LoadedAt:   set.LoadedAt(),
		Path:       s.app.PolicyPath(),
		Attributes: policy.AttributeCatalogue(),
		Builtins:   policy.BuiltinDocs(),
	}
	for _, p := range set.Policies() {
		b := &policyBrief{
			Name: p.Name, Description: p.Description, Priority: p.Priority,
			Effect: string(p.Effect), Message: p.Message, Tags: p.Tags, Prefer: p.Prefer,
		}
		if p.When != nil {
			b.When = p.When.String()
		}
		if p.Require != nil {
			b.Require = p.Require.String()
		}
		view.Policies = append(view.Policies, b)
	}
	writeJSON(w, http.StatusOK, view)
}

type policyBody struct {
	Source string `json:"source"`
}

func readPolicyBody(r *http.Request) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var pb policyBody
	if err := json.Unmarshal(body, &pb); err == nil && pb.Source != "" {
		return pb.Source, nil
	}
	// Also accept a raw document body, which makes curl usage pleasant.
	return string(body), nil
}

func (s *Server) handlePolicyPut(w http.ResponseWriter, r *http.Request) {
	src, err := readPolicyBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.app.SetPolicySource(src); err != nil {
		status := http.StatusBadRequest
		resp := map[string]any{"error": err.Error()}
		if se, ok := err.(*policy.SyntaxError); ok {
			resp["line"], resp["column"], resp["message"] = se.Line, se.Col, se.Msg
		}
		writeJSON(w, status, resp)
		return
	}
	set := s.app.Engine.Policy()
	s.log.Info("policy installed via API", "hash", set.Hash(), "policies", set.Len())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "hash": set.Hash(), "policies": set.Len(),
	})
}

// handlePolicyBacktest replays retained decisions through a candidate document
// and reports what would change.
//
// This is the feature that makes editing authorisation policy survivable. The
// question an operator actually has before pressing apply is not "does this
// compile" but "what does this break", and the ledger already holds the
// traffic needed to answer it.
func (s *Server) handlePolicyBacktest(w http.ResponseWriter, r *http.Request) {
	src, err := readPolicyBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	set, err := policy.Compile(src)
	if err != nil {
		res := BacktestResult{OK: false, Error: err.Error()}
		if se, ok := err.(*policy.SyntaxError); ok {
			res.Line, res.Column, res.Error = se.Line, se.Col, se.Msg
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	limit := queryInt(r, "limit", 500, 1, 2000)
	decisions := s.app.Ledger.Recent(telemetry.Filter{Limit: limit})

	// Candidate backends are taken from the live registry rather than from
	// each decision's record. The alternative would be reconstructing a
	// historical topology from the ledger, and the backends a decision could
	// have used are exactly the ones registered now in every case that
	// matters — a fleet change between then and now is itself a reason to
	// re-run the backtest.
	registry := make(map[string]model.Backend)
	for _, b := range s.app.Store.Backends() {
		registry[b.ID] = b
	}

	// Both sides are evaluated here, rather than comparing the candidate
	// against what the ledger recorded.
	//
	// A stored decision's eligible set is the policy result after the SLO
	// guardrail and circuit breakers have also had their say, so comparing a
	// policy-only result against it would attribute every shed backend to the
	// operator's edit. Running the live document over the same inputs isolates
	// the change to exactly what the edit did.
	live := s.app.Engine.Policy()
	res := BacktestResult{OK: true, Policies: set.Len(), Hash: set.Hash()}

	for _, d := range decisions {
		candidates := make([]model.Backend, 0, len(d.Candidates))
		for _, c := range d.Candidates {
			if b, ok := registry[c.BackendID]; ok {
				candidates = append(candidates, b)
			}
		}
		if len(candidates) == 0 {
			for _, b := range registry {
				candidates = append(candidates, b)
			}
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		}

		sub, req := d.Subject, d.Request
		in := policy.Input{
			Subject: &sub, Request: &req, Candidates: candidates,
			Now: d.Timestamp, BaseObjectives: d.Objectives,
		}
		before := live.Evaluate(in)
		after := set.Evaluate(in)
		res.Replayed++

		eligibleWas := len(before.Eligible)
		eligibleNow := len(after.Eligible)
		wasAllowed := before.Verdict == model.VerdictAllow
		nowAllowed := after.Verdict == model.VerdictAllow

		change := ""
		switch {
		case wasAllowed && !nowAllowed:
			res.NewlyDenied++
			change = "newly-denied"
		case !wasAllowed && nowAllowed:
			res.NewlyAllowed++
			change = "newly-allowed"
		case wasAllowed && eligibleNow < eligibleWas:
			res.NarrowedPool++
			change = "narrowed-pool"
		case wasAllowed && eligibleNow > eligibleWas:
			res.WidenedPool++
			change = "widened-pool"
		default:
			res.Unchanged++
		}

		if change != "" && len(res.Samples) < 25 {
			res.Samples = append(res.Samples, BacktestSample{
				DecisionID: d.ID, Timestamp: d.Timestamp,
				Subject: d.Subject.ID, Path: d.Request.Path,
				DataClass:   string(d.Request.DataClass),
				Was:         string(before.Verdict),
				Now:         string(after.Verdict),
				Change:      change,
				Reason:      after.DenyReason,
				EligibleWas: eligibleWas, EligibleNow: eligibleNow,
			})
		}
	}

	writeJSON(w, http.StatusOK, res)
}

// -----------------------------------------------------------------------------
// Incidents
// -----------------------------------------------------------------------------

func (s *Server) incidentViews() []incidentView {
	if s.app.Fleet == nil {
		return nil
	}
	now := time.Now()
	incs := s.app.Fleet.Incidents()
	out := make([]incidentView, 0, len(incs))
	for _, i := range incs {
		out = append(out, incidentView{
			ID: i.ID, Kind: string(i.Kind), BackendID: i.BackendID,
			Magnitude: i.Magnitude, StartedAt: i.StartedAt, EndsAt: i.EndsAt,
			Note: i.Note, Active: i.Active(now),
			RemainingSec: i.Remaining(now).Seconds(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

func (s *Server) handleIncidentsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"incidents": s.incidentViews(),
		"available": s.app.Fleet != nil,
		"kinds": []string{
			string(sim.IncidentBrownout), string(sim.IncidentOutage),
			string(sim.IncidentPriceSpike), string(sim.IncidentCarbonSpike),
		},
	})
}

type incidentRequest struct {
	Kind      string  `json:"kind"`
	BackendID string  `json:"backendId"`
	Magnitude float64 `json:"magnitude"`
	Seconds   float64 `json:"seconds"`
	Note      string  `json:"note"`
}

func (s *Server) handleIncidentPost(w http.ResponseWriter, r *http.Request) {
	if s.app.Fleet == nil {
		writeError(w, http.StatusConflict,
			"incident injection requires demo mode; Sluice is pointed at real backends")
		return
	}
	var req incidentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	inc := sim.Incident{
		Kind: sim.IncidentKind(req.Kind), BackendID: req.BackendID,
		Magnitude: req.Magnitude, Note: req.Note,
	}
	if req.Seconds > 0 {
		inc.EndsAt = time.Now().Add(time.Duration(req.Seconds * float64(time.Second)))
	}

	created, err := s.app.Fleet.Inject(inc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleIncidentDelete(w http.ResponseWriter, r *http.Request) {
	if s.app.Fleet == nil {
		writeError(w, http.StatusConflict, "incident injection requires demo mode")
		return
	}
	id := normalizePathID(r.PathValue("id"))
	if !s.app.Fleet.Resolve(id) {
		writeError(w, http.StatusNotFound, "unknown incident "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "resolved": id})
}
