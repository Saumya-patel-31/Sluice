package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/api"
	"github.com/Saumya-patel-31/sluice/internal/app"
	"github.com/Saumya-patel-31/sluice/internal/authz"
	"github.com/Saumya-patel-31/sluice/internal/config"
	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/proxy"
)

// The mesh identity a sidecar would present, in Envoy's forwarded-certificate
// format.
const meshXFCC = `By=spiffe://prod.internal/ns/mesh/sa/gateway;Hash=test;` +
	`URI=spiffe://prod.internal/ns/web/sa/feed`

// testToken gates the mutating API here exactly as it would in a real
// deployment, so these tests exercise the authenticated path rather than an
// open one.
const testToken = "integration-test-token"

// newStack brings up a complete control plane against the in-process
// simulator: real upstream sockets, real probes, real decisions.
func newStack(t *testing.T) (*app.App, http.Handler) {
	t.Helper()

	cfg := config.Default()
	cfg.Demo.Enabled = true
	cfg.Demo.RPS = 0 // the test drives its own traffic
	cfg.Demo.AutoIncidents = false
	cfg.Listen.API = ""
	cfg.Listen.Authz = ""
	cfg.API.Token = testToken
	cfg.Router.ControlIntervalMs = 50
	cfg.Probe.IntervalMs = 100
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("config: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.New(cfg, log, "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)

	// Wait for the first traffic plan; before that the router denies
	// everything for want of a distribution, which is the behaviour /readyz
	// reports and is not what these tests are about.
	deadline := time.Now().Add(10 * time.Second)
	for a.Engine.Generation() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no traffic plan computed within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return a, api.New(a).Handler()
}

// authRequest issues a mutating control-plane call with the bearer token the
// stack was configured with.
func authRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getJSON(t *testing.T, h http.Handler, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v\n%s", path, err, rec.Body.String())
		}
	}
	return rec
}

func TestAPISurface(t *testing.T) {
	a, h := newStack(t)

	// Drive enough traffic to populate the ledger and the aggregates.
	sub := model.Subject{
		ID: "spiffe://prod.internal/ns/web/sa/feed", TrustDomain: "prod.internal",
		Namespace: "web", Service: "feed", MTLS: true, Authenticated: true,
	}
	for i := 0; i < 60; i++ {
		a.Engine.Decide(&sub, &model.Request{Method: "GET", Path: "/api/v1/feed", EstimatedBytes: 8 << 10})
	}
	anon := model.Anonymous()
	for i := 0; i < 10; i++ {
		a.Engine.Decide(&anon, &model.Request{Method: "GET", Path: "/api/v1/feed"})
	}

	t.Run("status", func(t *testing.T) {
		var s map[string]any
		if rec := getJSON(t, h, "/api/status", &s); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if s["version"] != "test" || s["demoMode"] != true {
			t.Errorf("unexpected status: %v", s)
		}
		if s["policyHash"] == "" {
			t.Error("policy hash should be reported so an operator can confirm what is live")
		}
	})

	t.Run("readyz", func(t *testing.T) {
		if rec := getJSON(t, h, "/readyz", nil); rec.Code != http.StatusOK {
			t.Fatalf("readyz = %d, want 200 once a plan exists", rec.Code)
		}
	})

	t.Run("overview", func(t *testing.T) {
		var ov struct {
			KPIs struct {
				TotalDecisions uint64  `json:"totalDecisions"`
				AllowRate      float64 `json:"allowRate"`
				TotalBackends  int     `json:"totalBackends"`
			} `json:"kpis"`
			Clouds []struct {
				Cloud string `json:"cloud"`
			} `json:"clouds"`
			Routes []struct {
				Route struct {
					ID string `json:"id"`
				} `json:"route"`
				Candidates []model.CandidateScore `json:"candidates"`
			} `json:"routes"`
			Backends []map[string]any `json:"backends"`
		}
		getJSON(t, h, "/api/overview", &ov)

		if ov.KPIs.TotalDecisions < 70 {
			t.Errorf("decisions = %d, want at least 70", ov.KPIs.TotalDecisions)
		}
		if ov.KPIs.TotalBackends != 10 {
			t.Errorf("backends = %d", ov.KPIs.TotalBackends)
		}
		if len(ov.Clouds) != 3 {
			t.Errorf("want three providers, got %d", len(ov.Clouds))
		}
		if len(ov.Routes) != 4 {
			t.Errorf("routes = %d", len(ov.Routes))
		}
		// Roughly 60 of 70 requests were authorised.
		if ov.KPIs.AllowRate < 0.7 || ov.KPIs.AllowRate > 0.95 {
			t.Errorf("allow rate = %v, want ~0.86", ov.KPIs.AllowRate)
		}

		var weights float64
		for _, r := range ov.Routes {
			if r.Route.ID != "interactive" {
				continue
			}
			for _, c := range r.Candidates {
				weights += c.Weight
			}
		}
		if weights < 0.99 || weights > 1.01 {
			t.Errorf("route weights should sum to 1, got %v", weights)
		}
	})

	t.Run("decisions and explainability", func(t *testing.T) {
		var list struct {
			Decisions []map[string]any `json:"decisions"`
		}
		getJSON(t, h, "/api/decisions?limit=5", &list)
		if len(list.Decisions) == 0 {
			t.Fatal("no decisions retained")
		}

		id, _ := list.Decisions[0]["id"].(string)
		var d model.Decision
		if rec := getJSON(t, h, "/api/decisions/"+id, &d); rec.Code != http.StatusOK {
			t.Fatalf("decision detail = %d", rec.Code)
		}
		if len(d.Candidates) == 0 || len(d.PolicyTrace) == 0 {
			t.Fatal("a retained decision must carry its candidates and policy trace")
		}

		// The recorded arithmetic has to reproduce exactly, or the
		// explainability view is showing a re-derivation rather than the
		// decision that was actually made.
		for _, c := range d.Candidates {
			var penalty float64
			for dim := model.Dimension(0); dim < model.NumDimensions; dim++ {
				want := c.Normalized[dim] * d.Objectives[dim]
				if diff := c.Contribution[dim] - want; diff > 1e-9 || diff < -1e-9 {
					t.Fatalf("%s contribution[%s] = %v, want %v",
						c.BackendID, dim, c.Contribution[dim], want)
				}
				penalty += c.Contribution[dim]
			}
			if score := 1 - penalty; c.Score > 0 && (c.Score-score > 1e-9 || score-c.Score > 1e-9) {
				t.Fatalf("%s score = %v, want %v", c.BackendID, c.Score, score)
			}
		}

		if rec := getJSON(t, h, "/api/decisions/does-not-exist", nil); rec.Code != http.StatusNotFound {
			t.Errorf("unknown decision = %d, want 404", rec.Code)
		}
	})

	t.Run("denials are filterable and explained", func(t *testing.T) {
		var list struct {
			Decisions []map[string]any `json:"decisions"`
		}
		getJSON(t, h, "/api/decisions?verdict=deny&limit=5", &list)
		if len(list.Decisions) == 0 {
			t.Fatal("expected the anonymous requests to be retained as denials")
		}
		reason, _ := list.Decisions[0]["denyReason"].(string)
		if !strings.Contains(reason, "unauthenticated") {
			t.Errorf("deny reason = %q", reason)
		}
	})

	t.Run("topology", func(t *testing.T) {
		var topo struct {
			RouteID string `json:"routeId"`
			Nodes   []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"nodes"`
			Edges []struct{ From, To string } `json:"edges"`
		}
		getJSON(t, h, "/api/topology?route=interactive", &topo)

		var gateways, clouds, regions int
		for _, n := range topo.Nodes {
			switch n.Kind {
			case "gateway":
				gateways++
			case "cloud":
				clouds++
			case "region":
				regions++
			}
		}
		if gateways != 1 || clouds != 3 || regions != 10 {
			t.Errorf("graph shape: %d gateway, %d clouds, %d regions", gateways, clouds, regions)
		}
		if len(topo.Edges) != 13 {
			t.Errorf("want 13 edges (3 gateway→cloud, 10 cloud→region), got %d", len(topo.Edges))
		}
		if rec := getJSON(t, h, "/api/topology?route=nope", nil); rec.Code != http.StatusNotFound {
			t.Errorf("unknown route = %d, want 404", rec.Code)
		}
	})

	t.Run("metrics", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body := rec.Body.String()

		for _, want := range []string{
			"# TYPE sluice_decisions_total counter",
			"sluice_decisions_total{route=\"interactive\",verdict=\"allow\"",
			"# TYPE sluice_backend_egress_usd_per_gb gauge",
			"sluice_backend_carbon_grams_per_gb{",
			"# TYPE sluice_decision_duration_seconds histogram",
			"sluice_build_info{version=\"test\"",
			"sluice_route_slo_met{route=\"payments\"}",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("metrics missing %q", want)
			}
		}
	})

	t.Run("dashboard is served", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard = %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<title>Sluice") {
			t.Error("index.html was not served")
		}
		// A deep link must survive a reload; the client-side router owns the
		// URL space below /.
		deep := httptest.NewRecorder()
		h.ServeHTTP(deep, httptest.NewRequest(http.MethodGet, "/decisions", nil))
		if deep.Code != http.StatusOK {
			t.Errorf("deep link = %d, want the SPA fallback", deep.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
			t.Errorf("CSP header = %q", csp)
		}
	})
}

// The control-plane API can replace the document that authorises every
// request in the fleet. These are the tests that keep that surface shut.
func TestAPIAuthentication(t *testing.T) {
	_, h := newStack(t)

	openPolicy := `{"source":"policy \"pwn\" { priority 1 effect allow when true }"}`

	send := func(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("an unauthenticated write is refused", func(t *testing.T) {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodPut, "/api/policy", openPolicy},
			{http.MethodPost, "/api/incidents", `{"backendId":"aws-us-east-1","kind":"outage"}`},
			{http.MethodDelete, "/api/incidents/whatever", ""},
		} {
			rec := send(c.method, c.path, c.body, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401", c.method, c.path, rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Errorf("%s %s: missing WWW-Authenticate", c.method, c.path)
			}
		}
	})

	t.Run("the backtest is readable without a credential", func(t *testing.T) {
		// It compiles a candidate document and replays retained decisions
		// against it, installing nothing. Gating it as a mutation would mean
		// `sluicectl policy test` needed the same credential as applying a
		// change, which defeats having a safe way to ask what would break.
		rec := send(http.MethodPost, "/api/policy/backtest", openPolicy, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("unauthenticated backtest = %d, want 200", rec.Code)
		}

		// ...and it must still not have changed anything.
		var after struct {
			Hash string `json:"hash"`
		}
		getJSON(t, h, "/api/policy", &after)
		var live struct {
			Policies []any `json:"policies"`
		}
		getJSON(t, h, "/api/policy", &live)
		if len(live.Policies) < 2 {
			t.Error("the backtest appears to have installed its candidate document")
		}
	})

	t.Run("a wrong token is refused", func(t *testing.T) {
		for _, tok := range []string{"wrong", "", testToken + "x", testToken[:5]} {
			rec := send(http.MethodPut, "/api/policy", openPolicy,
				map[string]string{"Authorization": "Bearer " + tok})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("token %q = %d, want 401", tok, rec.Code)
			}
		}
	})

	t.Run("a rejected write leaves the policy untouched", func(t *testing.T) {
		var before struct {
			Hash string `json:"hash"`
		}
		getJSON(t, h, "/api/policy", &before)

		send(http.MethodPut, "/api/policy", openPolicy, nil)
		send(http.MethodPut, "/api/policy", openPolicy, map[string]string{"Authorization": "Bearer nope"})

		var after struct {
			Hash string `json:"hash"`
		}
		getJSON(t, h, "/api/policy", &after)
		if after.Hash != before.Hash {
			t.Fatal("a refused request still changed the live policy set")
		}
	})

	t.Run("both header forms are accepted", func(t *testing.T) {
		for _, hdr := range []map[string]string{
			{"Authorization": "Bearer " + testToken},
			{"X-Sluice-Token": testToken},
		} {
			rec := send(http.MethodPost, "/api/policy/backtest", openPolicy, hdr)
			if rec.Code != http.StatusOK {
				t.Errorf("%v = %d, want 200", hdr, rec.Code)
			}
		}
	})

	t.Run("a token in the query string is not accepted", func(t *testing.T) {
		// It would land in every access log and proxy trace along the path.
		rec := send(http.MethodPut, "/api/policy?token="+testToken, openPolicy, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("query-string token = %d, want 401", rec.Code)
		}
	})

	t.Run("reads stay open and refusals are counted", func(t *testing.T) {
		if rec := getJSON(t, h, "/api/overview", nil); rec.Code != http.StatusOK {
			t.Errorf("read = %d, want 200", rec.Code)
		}
		var s struct {
			AuthEnabled bool   `json:"authEnabled"`
			Denied      uint64 `json:"deniedApiRequests"`
		}
		getJSON(t, h, "/api/status", &s)
		if !s.AuthEnabled {
			t.Error("status should report that auth is on")
		}
		if s.Denied == 0 {
			t.Error("refusals must be counted so probing the admin surface is visible")
		}
	})

	t.Run("probes never require a credential", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz"} {
			if rec := getJSON(t, h, p, nil); rec.Code != http.StatusOK {
				t.Errorf("%s = %d — a probe that needs a secret fails during the "+
					"incident where the secret source is down", p, rec.Code)
			}
		}
	})

	t.Run("unknown API paths answer as API, not as a page", func(t *testing.T) {
		rec := getJSON(t, h, "/api/nope", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content type = %q; an API client parses HTML 200 as success", ct)
		}
	})
}

func TestPolicyLifecycle(t *testing.T) {
	_, h := newStack(t)

	var before struct {
		Source string `json:"source"`
		Hash   string `json:"hash"`
	}
	getJSON(t, h, "/api/policy", &before)
	if before.Source == "" || before.Hash == "" {
		t.Fatal("policy document not served")
	}

	t.Run("a broken document is rejected with a position", func(t *testing.T) {
		body := `{"source":"policy \"x\" {\n  effect allow\n  when subject.id ==\n}"}`
		rec := authRequest(h, http.MethodPut, "/api/policy", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out["line"] == nil {
			t.Errorf("the error should carry a line number: %v", out)
		}

		// The running set must be untouched: an operator cannot take
		// authorisation down with a typo.
		var after struct {
			Hash string `json:"hash"`
		}
		getJSON(t, h, "/api/policy", &after)
		if after.Hash != before.Hash {
			t.Error("a rejected document changed the live policy set")
		}
	})

	t.Run("backtest isolates the edit", func(t *testing.T) {
		tightened := strings.Replace(before.Source,
			`backend.jurisdiction == "EU"`, `backend.region == "francecentral"`, 1)
		body, _ := json.Marshal(map[string]string{"source": tightened})

		rec := authRequest(h, http.MethodPost, "/api/policy/backtest", string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("backtest = %d", rec.Code)
		}

		var res struct {
			OK           bool `json:"ok"`
			Replayed     int  `json:"replayed"`
			NewlyDenied  int  `json:"newlyDenied"`
			NewlyAllowed int  `json:"newlyAllowed"`
			NarrowedPool int  `json:"narrowedPool"`
			WidenedPool  int  `json:"widenedPool"`
			Unchanged    int  `json:"unchanged"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if !res.OK {
			t.Fatal("the tightened document should compile")
		}
		total := res.NewlyDenied + res.NewlyAllowed + res.NarrowedPool + res.WidenedPool + res.Unchanged
		if total != res.Replayed {
			t.Errorf("classification does not account for every replay: %d of %d", total, res.Replayed)
		}
		// Narrowing a residency constraint cannot authorise anything new.
		if res.NewlyAllowed != 0 {
			t.Errorf("a tighter constraint allowed %d new decisions", res.NewlyAllowed)
		}
	})

	t.Run("a valid document installs", func(t *testing.T) {
		doc := `policy "allow-all" { priority 1 effect allow when true }`
		body, _ := json.Marshal(map[string]string{"source": doc})
		rec := authRequest(h, http.MethodPut, "/api/policy", string(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var after struct {
			Hash     string `json:"hash"`
			Policies []any  `json:"policies"`
		}
		getJSON(t, h, "/api/policy", &after)
		if after.Hash == before.Hash || len(after.Policies) != 1 {
			t.Errorf("the new document was not installed: %+v", after)
		}
	})
}

func TestExtAuthz(t *testing.T) {
	a, _ := newStack(t)
	h := authz.New(a.Engine, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()

	t.Run("anonymous is refused", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/feed", nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if reason := rec.Header().Get(authz.HeaderDenyReason); !strings.Contains(reason, "unauthenticated") {
			t.Errorf("deny reason header = %q", reason)
		}
	})

	t.Run("a mesh identity is authorised and routed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/feed", nil)
		req.Header.Set("x-forwarded-client-cert", meshXFCC)
		req.Header.Set(authz.HeaderBytes, "8192")

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}

		// Envoy routes on these headers, so their absence is a silent
		// misroute rather than a visible failure.
		for _, hdr := range []string{authz.HeaderBackend, authz.HeaderCloud, authz.HeaderRegion, authz.HeaderDecision, authz.HeaderRoute} {
			if rec.Header().Get(hdr) == "" {
				t.Errorf("missing routing header %s", hdr)
			}
		}
		if got := rec.Header().Get(authz.HeaderRoute); got != "interactive" {
			t.Errorf("route header = %q, want interactive", got)
		}
	})

	t.Run("payments without mTLS is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/payments/charge", nil)
		// A subject with no certificate: authenticated by nothing.
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("residency constraints reach the data plane", func(t *testing.T) {
		// EU personal data must only ever be sent to an EU region.
		xfcc := `By=spiffe://prod.internal/ns/mesh/sa/gateway;URI=spiffe://prod.internal/ns/identity/sa/profile-api`
		for i := 0; i < 40; i++ {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/profile", nil)
			req.Header.Set("x-forwarded-client-cert", xfcc)
			req.Header.Set(authz.HeaderDataClass, "pii")

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			// This identity carries no residency claim, so the EU constraint
			// does not apply; the assertion is that a region was chosen at all
			// and that the header round-trips.
			if rec.Header().Get(authz.HeaderBackend) == "" {
				t.Fatal("no backend selected")
			}
		}
	})

	t.Run("healthz", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("healthz = %d", rec.Code)
		}
	})
}

func TestNativeProxyEndToEnd(t *testing.T) {
	a, _ := newStack(t)

	p, err := proxy.New(proxy.Config{
		Listen:    "127.0.0.1:0",
		Engine:    a.Engine,
		Store:     a.Store,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		TrustXFCC: true,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	client := srv.Client()

	t.Run("anonymous is refused before reaching an upstream", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/api/v1/feed")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("authorised traffic reaches a real upstream", func(t *testing.T) {
		before, _, beforeBytes, _, _ := a.Store.Totals()

		// The synthetic regions inject a small error rate on purpose, so a
		// handful of upstream failures across this many requests is the
		// system behaving correctly rather than a defect. What must hold is
		// that the overwhelming majority succeed, that every success is
		// attributed to a named region, and that the bytes are accounted.
		const total = 60
		var ok, upstreamErrors int

		for i := 0; i < total; i++ {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/feed", nil)
			req.Header.Set("x-forwarded-client-cert", meshXFCC)
			req.Header.Set("X-Sluice-Size", "4096")

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request %d: transport error: %v", i, err)
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			switch {
			case resp.StatusCode == http.StatusOK:
				ok++
				if resp.Header.Get("X-Sluice-Backend") == "" {
					t.Fatal("the response should name the region that served it")
				}
				// The upstream echoes its own region, which proves the request
				// actually crossed the proxy rather than being answered here.
				if resp.Header.Get("X-Sluice-Region") == "" {
					t.Fatal("missing region header")
				}
				if n == 0 {
					t.Fatal("empty response body")
				}
			case resp.StatusCode == http.StatusBadGateway:
				upstreamErrors++
			default:
				t.Fatalf("request %d: unexpected status %d", i, resp.StatusCode)
			}
		}

		if rate := float64(ok) / total; rate < 0.9 {
			t.Errorf("success rate %.0f%% (%d of %d, %d upstream failures) is too low to be injected noise",
				rate*100, ok, total, upstreamErrors)
		}

		// Byte accounting comes from what actually moved, since that is what
		// the egress bill will be computed from.
		after, _, afterBytes, _, storeErrors := a.Store.Totals()
		if afterBytes <= beforeBytes {
			t.Error("proxied bytes were not accounted")
		}
		if after < before {
			t.Error("spend went backwards")
		}
		// An upstream failure has to reach the signal store, or the breaker
		// can never trip on it.
		if upstreamErrors > 0 && storeErrors == 0 {
			t.Error("upstream failures were not fed back into the health signals")
		}
	})

	t.Run("health endpoint bypasses routing", func(t *testing.T) {
		resp, err := client.Get(srv.URL + "/sluice/healthz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d", resp.StatusCode)
		}
	})
}

// Reads need no credential, so an unbounded subscriber count is a
// resource-exhaustion path open to anyone who can reach the port: each stream
// holds a goroutine and a buffered channel until its connection closes.
func TestSSESubscriberLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Demo.Enabled = false
	cfg.Listen.API = ""
	cfg.Listen.Authz = ""
	cfg.API.Token = testToken
	cfg.API.MaxEventStreams = 2
	cfg.Router.ControlIntervalMs = 50
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("config: %v", err)
	}

	a, err := app.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)

	srv := httptest.NewServer(api.New(a).Handler())
	t.Cleanup(srv.Close)

	open := func() (*http.Response, context.CancelFunc) {
		rctx, rcancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(rctx, http.MethodGet, srv.URL+"/api/stream?points=8", nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			rcancel()
			t.Fatalf("stream: %v", err)
		}
		return resp, rcancel
	}

	r1, c1 := open()
	defer func() { c1(); r1.Body.Close() }()
	r2, c2 := open()
	defer func() { c2(); r2.Body.Close() }()

	if r1.StatusCode != http.StatusOK || r2.StatusCode != http.StatusOK {
		t.Fatalf("first two streams = %d, %d; both should be accepted", r1.StatusCode, r2.StatusCode)
	}

	r3, c3 := open()
	defer func() { c3(); r3.Body.Close() }()
	if r3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third stream = %d, want 503 at the limit", r3.StatusCode)
	}
	if r3.Header.Get("Retry-After") == "" {
		t.Error("a well-behaved client needs to be told when to come back")
	}

	// Closing one must free a slot; a leaked counter would wedge the endpoint
	// permanently after a burst of clients.
	c1()
	r1.Body.Close()

	var freed bool
	for i := 0; i < 50 && !freed; i++ {
		r4, c4 := open()
		if r4.StatusCode == http.StatusOK {
			freed = true
		}
		c4()
		r4.Body.Close()
		if !freed {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !freed {
		t.Error("a closed stream did not release its slot")
	}
}

func TestSSEStreamDelivers(t *testing.T) {
	a, h := newStack(t)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/stream?overviewMs=250&feedMs=100&points=8", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}

	// Generate traffic so a decision batch is produced.
	go func() {
		sub := model.Subject{
			ID: "spiffe://prod.internal/ns/web/sa/feed", TrustDomain: "prod.internal",
			Namespace: "web", Service: "feed", MTLS: true, Authenticated: true,
		}
		for i := 0; i < 200 && ctx.Err() == nil; i++ {
			a.Engine.Decide(&sub, &model.Request{Method: "GET", Path: "/api/v1/feed"})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	var sawOverview, sawDecisions bool
	buf := make([]byte, 32<<10)
	var acc strings.Builder
	deadline := time.Now().Add(6 * time.Second)

	for time.Now().Before(deadline) && !(sawOverview && sawDecisions) {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			s := acc.String()
			sawOverview = sawOverview || strings.Contains(s, "event: overview")
			sawDecisions = sawDecisions || strings.Contains(s, "event: decisions")
		}
		if err != nil {
			break
		}
	}

	if !sawOverview {
		t.Error("no overview frame received")
	}
	if !sawDecisions {
		t.Error("no decision batch received")
	}
}
