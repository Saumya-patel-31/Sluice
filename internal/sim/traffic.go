package sim

import (
	"context"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// Decider is the slice of the routing engine the generator needs. Keeping it
// narrow means the generator can be pointed at a stub in tests without
// standing up a control plane.
type Decider interface {
	Decide(*model.Subject, *model.Request) *model.Decision
	ObserveResult(backendID string, rtt time.Duration, ok bool, bytesOut int64)
}

// Profile is one class of synthetic caller.
type Profile struct {
	Name string
	// Share is the profile's relative frequency; shares are normalised.
	Share   float64
	Subject model.Subject
	Method  string
	// Paths are drawn uniformly, so a profile can span several endpoints.
	Paths     []string
	DataClass model.DataClass
	// Bytes is the response size this caller expects, which is what turns a
	// per-GB egress price into a per-request cost.
	Bytes    int64
	SourceIP string
	Headers  map[string]string
}

// DefaultProfiles is a traffic mix chosen to exercise every decision outcome:
// authorised interactive traffic, batch that trades latency for savings,
// personal data with a residency constraint, and two classes that must be
// refused.
func DefaultProfiles() []Profile {
	prod := func(ns, svc string, mtls bool) model.Subject {
		return model.Subject{
			ID:          "spiffe://prod.internal/ns/" + ns + "/sa/" + svc,
			TrustDomain: "prod.internal", Namespace: ns, Service: svc,
			Issuer: "CN=sluice-demo-ca", MTLS: mtls, Authenticated: true,
		}
	}

	return []Profile{
		{
			Name: "feed", Share: 30, Subject: prod("web", "feed-gateway", true),
			Method: "GET", Paths: []string{"/api/v1/feed", "/api/v1/feed/trending"},
			DataClass: model.DataInternal, Bytes: 8 << 10, SourceIP: "10.2.4.11",
		},
		{
			Name: "search", Share: 14, Subject: prod("web", "search-api", true),
			Method: "GET", Paths: []string{"/api/v1/search"},
			DataClass: model.DataInternal, Bytes: 6 << 10, SourceIP: "10.2.4.19",
		},
		{
			Name: "checkout", Share: 10, Subject: prod("payments", "checkout", true),
			Method: "POST", Paths: []string{"/api/payments/charge", "/api/payments/authorize"},
			DataClass: model.DataRegulated, Bytes: 2 << 10, SourceIP: "10.3.1.4",
		},
		{
			Name: "etl", Share: 18, Subject: prod("data", "etl", true),
			Method: "POST", Paths: []string{"/batch/reindex", "/batch/rollup", "/batch/export"},
			DataClass: model.DataInternal, Bytes: 256 << 10, SourceIP: "10.6.0.30",
		},
		{
			// EU personal data. Authorised, but the residency constraint means
			// only European regions may serve it.
			Name: "profile-eu", Share: 12,
			Subject: func() model.Subject {
				s := prod("identity", "profile-api", true)
				s.Claims = map[string]string{"residency": "eu", "tier": "gold"}
				return s
			}(),
			Method: "GET", Paths: []string{"/api/v1/profile", "/api/v1/profile/preferences"},
			DataClass: model.DataPII, Bytes: 4 << 10, SourceIP: "10.2.9.7",
		},
		{
			Name: "staging-canary", Share: 6,
			Subject: model.Subject{
				ID: "spiffe://staging.internal/ns/web/sa/canary", TrustDomain: "staging.internal",
				Namespace: "web", Service: "canary", MTLS: true, Authenticated: true,
			},
			Method: "GET", Paths: []string{"/api/v1/items"},
			DataClass: model.DataInternal, Bytes: 8 << 10, SourceIP: "10.9.0.2",
		},
		{
			// Unauthenticated. Must be refused by the baseline policy.
			Name: "anonymous-scan", Share: 6, Subject: model.Anonymous(),
			Method: "GET", Paths: []string{"/api/v1/feed", "/api/v1/users", "/admin/flags"},
			DataClass: model.DataInternal, Bytes: 2 << 10, SourceIP: "203.0.113.44",
		},
		{
			// Authenticated but off the corporate network, reaching for the
			// admin surface. Refused by the CIDR policy, not by authentication.
			Name: "admin-offnet", Share: 4, Subject: prod("ops", "console", true),
			Method: "GET", Paths: []string{"/admin/flags", "/admin/backends"},
			DataClass: model.DataConfidential, Bytes: 2 << 10, SourceIP: "198.51.100.23",
		},
	}
}

// Generator drives synthetic traffic through the engine and out to the
// synthetic upstreams, feeding the observed results back in.
type Generator struct {
	Engine   Decider
	Fleet    *Fleet
	Profiles []Profile
	// RPS is the mean aggregate request rate.
	RPS float64
	// Concurrency bounds in-flight synthetic requests, so a brownout produces
	// queueing rather than unbounded goroutine growth.
	Concurrency int
	Log         *slog.Logger

	client *http.Client
	cum    []float64
	total  float64

	issued  atomic.Uint64
	allowed atomic.Uint64
	denied  atomic.Uint64
	failed  atomic.Uint64

	once sync.Once
}

func (g *Generator) init() {
	g.once.Do(func() {
		if len(g.Profiles) == 0 {
			g.Profiles = DefaultProfiles()
		}
		// No default substitution for RPS. A caller that asked for zero wants
		// a quiet system — for a test that drives its own traffic, or an
		// operator watching an idle fleet — and silently generating 60 req/s
		// instead makes both impossible.
		if g.Concurrency <= 0 {
			g.Concurrency = 96
		}
		if g.Log == nil {
			g.Log = slog.Default()
		}
		g.client = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
		}
		g.cum = make([]float64, len(g.Profiles))
		for i, p := range g.Profiles {
			share := p.Share
			if share <= 0 {
				share = 1
			}
			g.total += share
			g.cum[i] = g.total
		}
	})
}

// pick draws a profile by share.
func (g *Generator) pick() *Profile {
	r := mrand.Float64() * g.total
	for i, c := range g.cum {
		if r < c {
			return &g.Profiles[i]
		}
	}
	return &g.Profiles[len(g.Profiles)-1]
}

// Run drives traffic until ctx is cancelled. A rate of zero or less generates
// nothing and returns immediately.
func (g *Generator) Run(ctx context.Context) {
	g.init()
	if g.RPS <= 0 {
		return
	}
	sem := make(chan struct{}, g.Concurrency)
	start := time.Now()

	// Re-evaluate the interval every tick so the rate can follow a slow wave
	// without restarting the loop.
	for {
		rate := g.currentRate(time.Since(start))
		interval := time.Duration(float64(time.Second) / rate)
		if interval < 100*time.Microsecond {
			interval = 100 * time.Microsecond
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		select {
		case sem <- struct{}{}:
		default:
			// At the concurrency ceiling. Dropping the request rather than
			// queueing it is what a real client under load would do, and it
			// keeps the generator from becoming the bottleneck being measured.
			continue
		}
		go func() {
			defer func() { <-sem }()
			g.fire(ctx)
		}()
	}
}

// currentRate shapes the request rate into a slow wave with a little noise, so
// the charts show a system under varying load rather than a flat line.
func (g *Generator) currentRate(elapsed time.Duration) float64 {
	base := g.RPS
	wave := 1 + 0.35*math.Sin(2*math.Pi*elapsed.Seconds()/240)
	noise := 1 + 0.08*(mrand.Float64()-0.5)
	r := base * wave * noise
	if r < 1 {
		r = 1
	}
	return r
}

// fire makes one decision and, when allowed, actually performs the request.
func (g *Generator) fire(ctx context.Context) {
	p := g.pick()
	g.issued.Add(1)

	sub := p.Subject
	req := &model.Request{
		Method:         p.Method,
		Path:           p.Paths[mrand.IntN(len(p.Paths))],
		Host:           "api.sluice.internal",
		SourceIP:       p.SourceIP,
		DataClass:      p.DataClass,
		EstimatedBytes: p.Bytes,
		Headers:        p.Headers,
	}

	d := g.Engine.Decide(&sub, req)
	if d.Verdict != model.VerdictAllow {
		g.denied.Add(1)
		return
	}
	g.allowed.Add(1)

	addr := g.Fleet.Addr(d.ChosenBackend)
	if addr == "" {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, p.Method, addr+req.Path, nil)
	if err != nil {
		return
	}
	hreq.Header.Set("X-Sluice-Size", strconv.FormatInt(p.Bytes, 10))
	hreq.Header.Set("X-Sluice-Decision", d.ID)

	started := time.Now()
	resp, err := g.client.Do(hreq)
	if err != nil {
		g.failed.Add(1)
		// A transport failure is still a data point: the engine has to learn
		// that this backend is not serving, or the breaker will never trip.
		g.Engine.ObserveResult(d.ChosenBackend, time.Since(started), false, 0)
		return
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	rtt := time.Since(started)

	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if !ok {
		g.failed.Add(1)
	}
	g.Engine.ObserveResult(d.ChosenBackend, rtt, ok, n)
}

// GeneratorStats reports what the generator has produced.
type GeneratorStats struct {
	Issued  uint64 `json:"issued"`
	Allowed uint64 `json:"allowed"`
	Denied  uint64 `json:"denied"`
	Failed  uint64 `json:"failed"`
}

// Stats returns lifetime generator counters.
func (g *Generator) Stats() GeneratorStats {
	return GeneratorStats{
		Issued:  g.issued.Load(),
		Allowed: g.allowed.Load(),
		Denied:  g.denied.Load(),
		Failed:  g.failed.Load(),
	}
}

// -----------------------------------------------------------------------------
// Automatic incidents
// -----------------------------------------------------------------------------

// AutoIncidents periodically injects faults so an unattended demo shows the
// router reacting rather than sitting at a steady state.
func AutoIncidents(ctx context.Context, fleet *Fleet, backends []model.Backend, log *slog.Logger) {
	if len(backends) == 0 {
		return
	}
	kinds := []struct {
		kind IncidentKind
		note string
		mag  func() float64
	}{
		{IncidentBrownout, "regional brownout: latency degraded without failures",
			func() float64 { return 3 + 5*mrand.Float64() }},
		{IncidentOutage, "regional outage: upstream returning errors",
			func() float64 { return 0.6 + 0.35*mrand.Float64() }},
		{IncidentPriceSpike, "egress price spike: volume tier rolled over",
			func() float64 { return 1.8 + 1.7*mrand.Float64() }},
		{IncidentCarbonSpike, "grid intensity spike: renewables output collapsed",
			func() float64 { return 1.6 + 1.4*mrand.Float64() }},
	}

	for {
		wait := time.Duration(40+mrand.IntN(70)) * time.Second
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		b := backends[mrand.IntN(len(backends))]
		k := kinds[mrand.IntN(len(kinds))]
		_, err := fleet.Inject(Incident{
			Kind:      k.kind,
			BackendID: b.ID,
			Magnitude: k.mag(),
			EndsAt:    time.Now().Add(time.Duration(45+mrand.IntN(75)) * time.Second),
			Note:      k.note,
		})
		if err != nil && log != nil {
			log.Warn("auto incident failed", "err", err)
		}
	}
}
