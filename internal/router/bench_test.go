package router

import (
	"fmt"
	"testing"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/policy"
	"github.com/Saumya-patel-31/sluice/internal/signals"
)

// The design claim this package rests on is that scoring is a control-loop
// concern and the request path only applies a plan the loop already computed.
// That is a claim about cost, and until these existed it was unmeasured — so
// "a routing decision belongs inline in a request" was an assertion.
//
// Run with:
//
//	go test ./internal/router -bench . -benchmem -run '^$'
//
// The number that matters is ns/op on BenchmarkDecideParallel, against the
// couple of hundred microseconds of network latency the decision is choosing
// between. If a decision ever costs a meaningful fraction of the round trip it
// is saving, the architecture is wrong.

func benchEngine(tb testing.TB, backends int) *Engine {
	tb.Helper()
	store := signals.NewStore(signals.DefaultStoreConfig())

	clouds := []model.Cloud{model.CloudAWS, model.CloudGCP, model.CloudAzure}
	jur := []string{"US", "EU", "EU"}
	for i := 0; i < backends; i++ {
		id := fmt.Sprintf("bench-%02d", i)
		store.Register(model.Backend{
			ID: id, Cloud: clouds[i%3], Region: fmt.Sprintf("r%02d", i),
			Jurisdiction: jur[i%3], Tier: "primary", Bias: 1, Enabled: true,
			Address: "http://" + id,
		})
		store.SetPrice(id, signals.Quote{Value: 0.08 + float64(i%7)*0.01, Source: "bench", AsOf: time.Now()})
		store.SetGridIntensity(id, signals.Quote{Value: float64(60 + i*13%400), Source: "bench", AsOf: time.Now()})
		// Enough samples that the p95 estimator has left its warm-up, or the
		// benchmark measures a different code path from production.
		for j := 0; j < 60; j++ {
			store.ObserveProbe(id, time.Duration(15+i*3%90)*time.Millisecond, true)
		}
	}

	e := NewEngine(store, DefaultConfig(), nil)
	for _, r := range []model.Route{
		{ID: "payments", DisplayName: "Payments", PathPrefix: "/api/payments",
			Objectives: model.Vector{0.05, 0.45, 0.05, 0.45}, LatencySLOMs: 45, Temperature: 0.08, RequireMTLS: true},
		{ID: "interactive", DisplayName: "Interactive", PathPrefix: "/api/v1",
			Objectives: model.Vector{0.15, 0.55, 0.15, 0.15}, LatencySLOMs: 60, Temperature: 0.12},
		{ID: "batch", DisplayName: "Batch", PathPrefix: "/batch",
			Objectives: model.Vector{0.45, 0.05, 0.40, 0.10}, Temperature: 0.20},
		{ID: "default", DisplayName: "Default", PathPrefix: "/",
			Objectives: model.Vector{0.35, 0.35, 0.20, 0.10}, LatencySLOMs: 120, Temperature: 0.14},
	} {
		e.UpsertRoute(r)
	}

	// The shipped policy document, not a trivial one: the request path
	// evaluates every rule whose effect could apply, and a benchmark against
	// an empty policy would flatter it.
	set, err := policy.Compile(benchPolicy)
	if err != nil {
		tb.Fatalf("compile bench policy: %v", err)
	}
	e.SetPolicy(set)
	e.Recompute()
	return e
}

const benchPolicy = `
policy "deny-unauthenticated" {
  priority 10
  effect   deny
  when     not subject.authenticated
  message  "unauthenticated request rejected"
}

policy "payments-requires-mtls" {
  priority 30
  effect   deny
  when     request.path startswith "/api/payments" and not subject.mtls
  message  "payments endpoints require mutual TLS"
}

policy "pii-stays-in-eu" {
  priority 100
  effect   constrain
  when     request.data_class == "pii" and subject.claims["residency"] == "eu"
  require  backend.jurisdiction == "EU"
  message  "GDPR residency"
}

policy "batch-favours-cost-and-carbon" {
  priority 210
  effect   prefer
  when     request.path startswith "/batch"
  prefer   { cost: 0.45, carbon: 0.40, latency: 0.05, reliability: 0.10 }
}

policy "allow-mesh" {
  priority 900
  effect   allow
  when     subject.authenticated and subject.trust_domain == "prod.internal"
}
`

func benchSubject(i int) *model.Subject {
	return &model.Subject{
		ID:          fmt.Sprintf("spiffe://prod.internal/ns/api/sa/gw-%d", i),
		TrustDomain: "prod.internal",
		Namespace:   "api",
		Service:     fmt.Sprintf("gw-%d", i),
		MTLS:        true, Authenticated: true,
	}
}

// The single-threaded floor.
func BenchmarkDecide(b *testing.B) {
	e := benchEngine(b, 3)
	sub := benchSubject(0)
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := e.Decide(sub, req); d.Verdict != model.VerdictAllow {
			b.Fatalf("unexpected verdict %s: %s", d.Verdict, d.DenyReason)
		}
	}
}

// The realistic one. Every request in a fleet arrives concurrently, and this is
// where a lock on the plan or the policy cache would show up — the request path
// reads a snapshot the control loop swaps under it.
func BenchmarkDecideParallel(b *testing.B) {
	e := benchEngine(b, 3)
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		sub := benchSubject(0)
		for pb.Next() {
			e.Decide(sub, req)
		}
	})
}

// A distinct subject per iteration defeats the policy decision cache, which is
// the honest worst case: a fleet with many callers, or a cache that has just
// been invalidated by a policy change.
func BenchmarkDecideColdPolicyCache(b *testing.B) {
	e := benchEngine(b, 3)
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}
	subs := make([]*model.Subject, 4096)
	for i := range subs {
		subs[i] = benchSubject(i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Decide(subs[i%len(subs)], req)
	}
}

// Denial should not be more expensive than allowing, or a hostile caller
// costs more to refuse than a legitimate one costs to serve.
func BenchmarkDecideDeny(b *testing.B) {
	e := benchEngine(b, 3)
	sub := &model.Subject{ID: "anonymous"}
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if d := e.Decide(sub, req); d.Verdict != model.VerdictDeny {
			b.Fatal("an unauthenticated request must be denied")
		}
	}
}

// How the request path scales with fleet size. Scoring is O(backends) per
// route, but it happens in Recompute, not here — so these should be close to
// flat, and a slope would mean work has leaked into the request path.
func BenchmarkDecideByFleetSize(b *testing.B) {
	for _, n := range []int{3, 12, 48} {
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			e := benchEngine(b, n)
			sub := benchSubject(0)
			req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.Decide(sub, req)
			}
		})
	}
}

// The control loop, which runs at 1 Hz. This is where the normalise, weight,
// softmax, SLO-shed, cap and damp work actually happens, for every route and
// every discovered objective profile. It has a whole second; the question is
// whether it is anywhere near needing one.
func BenchmarkRecompute(b *testing.B) {
	for _, n := range []int{3, 12, 48} {
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			e := benchEngine(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.Recompute()
			}
		})
	}
}
