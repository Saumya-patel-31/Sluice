// Package api serves the Sluice control-plane REST API, the live event
// stream, the Prometheus endpoint and the embedded dashboard.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/saumyapatel/sluice/internal/app"
	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/signals"
	"github.com/saumyapatel/sluice/web"
)

// Server exposes the control plane over HTTP.
type Server struct {
	app  *app.App
	log  *slog.Logger
	mux  *http.ServeMux
	auth *Authenticator

	// streams bounds concurrent event-stream subscribers. Each one holds a
	// goroutine and a buffered channel for the lifetime of the connection, so
	// an unbounded count is a resource-exhaustion path that needs no
	// credentials — reads are open.
	streams    atomic.Int64
	maxStreams int64
}

// New builds the HTTP surface.
func New(a *app.App) *Server {
	maxStreams := int64(a.Cfg.API.MaxEventStreams)
	if maxStreams <= 0 {
		maxStreams = 64
	}
	s := &Server{
		app:        a,
		log:        a.Log,
		mux:        http.NewServeMux(),
		maxStreams: maxStreams,
		auth: NewAuthenticator(
			a.Cfg.API.Token,
			a.Cfg.API.RequireAuthForReads,
			a.Cfg.API.AllowAnonymousMutations,
			a.Log,
		),
	}
	s.routes()
	return s
}

// Auth exposes the authenticator so the app can report its state.
func (s *Server) Auth() *Authenticator { return s.auth }

// Handler returns the root handler with common middleware applied.
//
// Order is deliberate, outermost first: the correlation id has to exist before
// anything can log it, panic recovery has to be outside the access log so a
// panicking handler still produces one line rather than none, and the security
// headers sit innermost so they are set on every response including the ones
// the middleware itself writes.
func (s *Server) Handler() http.Handler {
	return withRequestID(
		recoverPanics(s.log,
			accessLog(s.log,
				securityHeaders(s.mux))))
}

func (s *Server) routes() {
	// The API lives on its own mux so one Handle call can put every endpoint
	// behind the authenticator. Registering the middleware per route is how a
	// new endpoint eventually ships without it.
	api := http.NewServeMux()

	api.HandleFunc("GET /api/status", s.handleStatus)
	api.HandleFunc("GET /api/overview", s.handleOverview)
	api.HandleFunc("GET /api/backends", s.handleBackends)
	api.HandleFunc("GET /api/backends/{id}/history", s.handleBackendHistory)
	api.HandleFunc("GET /api/routes", s.handleRoutes)
	api.HandleFunc("GET /api/topology", s.handleTopology)
	api.HandleFunc("GET /api/decisions", s.handleDecisions)
	api.HandleFunc("GET /api/decisions/{id}", s.handleDecision)
	api.HandleFunc("GET /api/policy", s.handlePolicyGet)
	api.HandleFunc("PUT /api/policy", s.handlePolicyPut)
	api.HandleFunc("POST /api/policy/backtest", s.handlePolicyBacktest)
	api.HandleFunc("GET /api/zones", s.handleZones)
	api.HandleFunc("GET /api/incidents", s.handleIncidentsGet)
	api.HandleFunc("POST /api/incidents", s.handleIncidentPost)
	api.HandleFunc("DELETE /api/incidents/{id}", s.handleIncidentDelete)
	api.HandleFunc("GET /api/stream", s.handleStream)

	// Anything else under /api is a client error, not a page. Falling through
	// to the dashboard handler would answer a mistyped endpoint with 200 and a
	// lump of HTML, which every API client parses as success.
	api.HandleFunc("/api/", s.handleAPINotFound)

	s.mux.Handle("/api/", s.auth.Middleware(api))

	// /metrics carries cost and topology detail, so it follows the read
	// policy. Prometheus can send an Authorization header when that is on.
	s.mux.Handle("GET /metrics", s.auth.Middleware(http.HandlerFunc(s.handleMetrics)))

	// Liveness and readiness stay open unconditionally: they expose nothing,
	// and a probe that needs a credential is a probe that fails during the
	// exact incident where the credential source is unavailable.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("GET /readyz", s.handleReady)

	// Registered without a method, because "GET /" and "/api/" are an
	// ambiguous pair to ServeMux — one is more specific by method, the other
	// by path, so it refuses to choose. Method filtering happens in the
	// handler instead, which also lets a POST to a page return 405 rather
	// than the 404 the mux would have produced.
	s.mux.Handle("/", dashboardOnlyGET(web.Handler()))
}

// dashboardOnlyGET serves the dashboard for reads and refuses everything else.
func dashboardOnlyGET(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, r.Method+" is not allowed on "+r.URL.Path)
		}
	})
}

// handleAPINotFound answers unmatched API paths in the API's own format.
func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":  "unknown_endpoint",
		"detail": r.Method + " " + r.URL.Path + " is not a Sluice API endpoint",
		"endpoints": []string{
			"GET /api/status", "GET /api/overview", "GET /api/backends",
			"GET /api/backends/{id}/history", "GET /api/routes", "GET /api/topology",
			"GET /api/decisions", "GET /api/decisions/{id}", "GET /api/zones",
			"GET /api/policy", "PUT /api/policy", "POST /api/policy/backtest",
			"GET /api/incidents", "POST /api/incidents", "DELETE /api/incidents/{id}",
			"GET /api/stream",
		},
	})
}

// securityHeaders applies a conservative baseline to every response.
//
// The dashboard is entirely self-contained — no CDN scripts, no external
// fonts, no remote images — so it can run under a content security policy
// that forbids every external origin. A control plane that can authorise
// traffic should not itself be loading code from the internet.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; font-src 'self'; connect-src 'self'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, def, min, max int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// -----------------------------------------------------------------------------
// Status and overview
// -----------------------------------------------------------------------------

func (s *Server) status() Status {
	a := s.app
	set := a.Engine.Policy()
	return Status{
		Version:        a.Version,
		StartedAt:      a.StartedAt,
		UptimeSeconds:  a.Uptime().Seconds(),
		PolicyHash:     set.Hash(),
		PolicyCount:    set.Len(),
		PolicyPath:     a.PolicyPath(),
		PolicyLoaded:   set.LoadedAt(),
		Generation:     a.Engine.Generation(),
		DemoMode:       a.Fleet != nil,
		Backends:       len(a.Store.Backends()),
		Routes:         len(a.Engine.Routes()),
		PriceTableAsOf: signals.PriceTableAsOf,
		CarbonModel:    a.Store.CarbonModel(),

		AuthEnabled:        s.auth.Enabled(),
		AnonymousMutations: a.Cfg.API.AllowAnonymousMutations,
		DeniedAPIRequests:  s.auth.Denied(),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	// Ready means the control loop has produced at least one distribution.
	// Before that the router would deny everything for lack of a plan, and
	// reporting ready would send it traffic it cannot serve.
	if s.app.Engine.Generation() == 0 {
		writeError(w, http.StatusServiceUnavailable, "no traffic plan computed yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":      true,
		"generation": s.app.Engine.Generation(),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = s.app.Registry.WriteTo(w)
}

// buildOverview assembles the dashboard's primary payload.
func (s *Server) buildOverview(seriesPoints int) Overview {
	a := s.app
	snap := a.Engine.Snapshot()
	plans := a.Engine.Plans()
	backendRPS := a.Engine.BackendRPS()
	summary := a.Ledger.Summary()

	ov := Overview{
		Status:  s.status(),
		Summary: summary,
		Now:     time.Now(),
		Series:  a.Rollup.All(seriesPoints),
	}

	// Per-backend view, including the largest share it holds on any route.
	shareOf := map[string]float64{}
	perRoute := map[string]map[string]float64{}
	for routeID, plan := range plans {
		for id, wgt := range plan.Weights {
			if wgt > shareOf[id] {
				shareOf[id] = wgt
			}
			if perRoute[id] == nil {
				perRoute[id] = map[string]float64{}
			}
			perRoute[id][routeID] = wgt
		}
	}

	type cloudAgg struct {
		backends, healthy      int
		share, rps             float64
		price, carbon, latency float64
		spent, emitted         float64
	}
	agg := map[model.Cloud]*cloudAgg{}

	var (
		healthy, openBreakers, stale int
		totalBytes                   int64
	)

	for _, b := range snap.Backends {
		zone, _ := signals.ZoneFor(b.Backend)
		view := BackendView{
			BackendState: b,
			Share:        shareOf[b.Backend.ID],
			PerRoute:     perRoute[b.Backend.ID],
			RPS:          backendRPS[b.Backend.ID],
			Zone:         zone,
		}
		ov.Backends = append(ov.Backends, view)

		if b.Healthy {
			healthy++
		}
		if b.Breaker.State == signals.BreakerOpen {
			openBreakers++
		}
		stale += len(b.Stale)
		totalBytes += b.BytesOut

		c := agg[b.Backend.Cloud]
		if c == nil {
			c = &cloudAgg{}
			agg[b.Backend.Cloud] = c
		}
		c.backends++
		if b.Healthy {
			c.healthy++
		}
		c.share += view.Share
		c.rps += view.RPS
		c.price += b.Egress.Value
		c.carbon += b.CarbonPerGB.Value
		c.latency += b.LatencyP95.Value
		c.spent += b.SpentUSD
		c.emitted += b.EmittedGrams
	}

	for cloud, c := range agg {
		n := float64(c.backends)
		ov.Clouds = append(ov.Clouds, CloudSummary{
			Cloud: cloud, Display: cloud.Display(),
			Backends: c.backends, Healthy: c.healthy,
			Share: c.share, RPS: c.rps,
			Decisions:   summary.ByCloud[string(cloud)],
			AvgEgress:   c.price / n,
			AvgCarbon:   c.carbon / n,
			AvgLatency:  c.latency / n,
			SpentUSD:    c.spent,
			EmittedGram: c.emitted,
		})
	}
	sort.Slice(ov.Clouds, func(i, j int) bool { return ov.Clouds[i].Cloud < ov.Clouds[j].Cloud })

	// Routes, and the traffic-weighted p95 across all of them.
	var totalRPS, weightedP95 float64
	var breaching int
	for _, route := range a.Engine.Routes() {
		plan := plans[route.ID]
		if plan == nil {
			continue
		}
		rps := a.Engine.RouteRPS(route.ID)
		totalRPS += rps
		weightedP95 += rps * plan.ProjectedP95
		if !plan.SLOMet {
			breaching++
		}

		rs := RouteSummary{
			Route: route, RPS: rps,
			Weights: plan.Weights, Candidates: plan.Candidates,
			Objectives:   plan.Objectives,
			ProjectedP95: plan.ProjectedP95, WorstP95: plan.WorstP95,
			SLOMet: plan.SLOMet, Churn: plan.Churn, Generation: plan.Generation,
		}
		for _, sh := range plan.Shed {
			rs.Shed = append(rs.Shed, shedView{sh.BackendID, sh.Reason})
		}
		ov.Routes = append(ov.Routes, rs)
	}
	if totalRPS > 0 {
		weightedP95 /= totalRPS
	}

	// Decision-latency statistics from the recent ledger window.
	var meanUs float64
	var maxUs int64
	if recent := a.Ledger.Recent(telemetryFilter(200)); len(recent) > 0 {
		var total int64
		for _, d := range recent {
			total += d.ComputeMicros
			if d.ComputeMicros > maxUs {
				maxUs = d.ComputeMicros
			}
		}
		meanUs = float64(total) / float64(len(recent))
	}

	_, _, cacheRate := a.Engine.PolicyCacheStats()
	allowed := summary.ByVerdict["allow"]
	denied := summary.ByVerdict["deny"] + summary.ByVerdict["no_capacity"]
	var allowRate, denyRate float64
	if summary.Total > 0 {
		allowRate = float64(allowed) / float64(summary.Total)
		denyRate = float64(denied) / float64(summary.Total)
	}

	usdPerHour := lastValue(ov.Series["savings.usdPerHour"])
	gramsPerHour := lastValue(ov.Series["savings.gramsPerHour"])

	ov.KPIs = KPIs{
		DecisionsPerSecond:  totalRPS,
		TotalDecisions:      summary.Total,
		AllowRate:           allowRate,
		DenyRate:            denyRate,
		SavedUSD:            summary.SavedUSD,
		SavedGrams:          summary.SavedGrams,
		SavingsUSDPerHour:   usdPerHour,
		SavingsGramsPerHour: gramsPerHour,
		ProjectedAnnualUSD:  usdPerHour * 24 * 365,
		EquivalentKmDriven:  summary.SavedGrams / 120,
		LatencyDebtMs:       summary.LatencyDebtMs,
		BlendedP95Ms:        weightedP95,
		DecisionMeanUs:      meanUs,
		DecisionMaxUs:       maxUs,
		PolicyCacheHitRate:  cacheRate,
		HealthyBackends:     healthy,
		TotalBackends:       len(snap.Backends),
		OpenBreakers:        openBreakers,
		StaleSignals:        stale,
		RoutesBreachingSLO:  breaching,
		BytesRouted:         totalBytes,
	}

	ov.Incidents = s.incidentViews()
	return ov
}

func lastValue(pts []signals.Point) float64 {
	if len(pts) == 0 {
		return 0
	}
	return pts[len(pts)-1].V
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildOverview(queryInt(r, "points", 120, 8, 600)))
}

// normalizePathID guards against a path parameter containing separators.
func normalizePathID(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "/", ""))
}
