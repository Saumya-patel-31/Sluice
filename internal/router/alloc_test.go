package router

import (
	"testing"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// The architecture's central claim is that scoring is a control-loop concern:
// the 1 Hz loop normalises, weights, softmaxes, sheds against the SLO, caps and
// damps, and the request path only applies the plan it produced. Benchmarking
// that turned up something the claim glosses over — the request path is not
// O(1). It copies the candidate set twice per decision, once to hand policy the
// backends it may filter on and once to record the scores in the decision, so
// its allocation count grows with the fleet:
//
//	backends=3   17 allocations
//	backends=12  30
//	backends=48  66
//
// That is roughly a fixed 18 plus one per backend, and it is the price of the
// explainability the ledger provides: every decision carries every candidate it
// considered, with the score and the reason it was or was not chosen. At 48
// backends it costs tens of microseconds against a 20–90 ms round trip, so the
// trade is worth making — but it is a trade, and it should not silently get
// worse.
//
// Wall-clock is not asserted here. This runs on shared CI runners and on
// laptops with a container runtime alongside, where timings vary by 2x between
// consecutive runs; allocation counts do not vary at all. A regression that
// matters — a new map built per request, a copy that becomes quadratic, work
// migrating out of Recompute — shows up here first and deterministically.
func TestDecideAllocationsStayLinearInFleetSize(t *testing.T) {
	sub := benchSubject(0)
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}

	for _, tc := range []struct{ backends, budget int }{
		{3, 24},
		{12, 38},
		{48, 76},
	} {
		e := benchEngine(t, tc.backends)
		// Warm the policy decision cache; a cold miss is a different path and
		// is measured by BenchmarkDecideColdPolicyCache.
		if d := e.Decide(sub, req); d.Verdict != model.VerdictAllow {
			t.Fatalf("backends=%d: setup produced %s (%s)", tc.backends, d.Verdict, d.DenyReason)
		}

		got := int(testing.AllocsPerRun(2000, func() { e.Decide(sub, req) }))
		if got > tc.budget {
			t.Errorf("backends=%d: %d allocations per decision, budget %d\n"+
				"the request path has taken on work that belongs in Recompute, "+
				"or a per-request allocation has been added",
				tc.backends, got, tc.budget)
		}
		t.Logf("backends=%2d  %2d allocations per decision (budget %d)",
			tc.backends, got, tc.budget)
	}
}

// A denial must not cost more than an allow. If refusing a request were the
// expensive path, anyone who can reach the ext_authz endpoint could spend more
// of the control plane's budget by being unauthorised than by being a customer.
func TestDenyIsNotMoreExpensiveThanAllow(t *testing.T) {
	e := benchEngine(t, 12)
	req := &model.Request{Method: "GET", Path: "/api/v1/items", EstimatedBytes: 65536}

	allowSub := benchSubject(0)
	denySub := &model.Subject{ID: "anonymous"}

	if d := e.Decide(allowSub, req); d.Verdict != model.VerdictAllow {
		t.Fatalf("setup: %s (%s)", d.Verdict, d.DenyReason)
	}
	if d := e.Decide(denySub, req); d.Verdict != model.VerdictDeny {
		t.Fatalf("setup: an anonymous request should be denied, got %s", d.Verdict)
	}

	allow := testing.AllocsPerRun(2000, func() { e.Decide(allowSub, req) })
	deny := testing.AllocsPerRun(2000, func() { e.Decide(denySub, req) })

	if deny > allow {
		t.Errorf("deny allocates %.0f, allow %.0f: refusing a request costs more than serving one",
			deny, allow)
	}
	t.Logf("allow %.0f allocations, deny %.0f", allow, deny)
}
