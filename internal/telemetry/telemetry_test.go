package telemetry

import (
	"strings"
	"testing"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

func TestCounterAndGaugeExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("sluice_decisions_total", "Routing decisions.", "verdict")
	c.With("allow").Add(3)
	c.With("deny").Inc()

	g := r.Gauge("sluice_backend_weight", "Traffic share.", "backend")
	g.With("aws-us-east-1").Set(0.42)

	out := r.String()

	for _, want := range []string{
		"# HELP sluice_decisions_total Routing decisions.",
		"# TYPE sluice_decisions_total counter",
		`sluice_decisions_total{verdict="allow"} 3`,
		`sluice_decisions_total{verdict="deny"} 1`,
		"# TYPE sluice_backend_weight gauge",
		`sluice_backend_weight{backend="aws-us-east-1"} 0.42`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}

	// Counters must not go backwards, even if a caller tries.
	c.With("allow").Add(-5)
	if got := c.With("allow").Value(); got != 3 {
		t.Errorf("counter accepted a negative delta: %v", got)
	}
}

func TestHistogramExposition(t *testing.T) {
	r := NewRegistry()
	h := r.Histogram("sluice_decision_seconds", "Decision latency.",
		[]float64{0.001, 0.01, 0.1}, "route")

	for _, v := range []float64{0.0005, 0.005, 0.05, 0.5} {
		h.With("default").Observe(v)
	}

	out := r.String()
	for _, want := range []string{
		"# TYPE sluice_decision_seconds histogram",
		`sluice_decision_seconds_bucket{route="default",le="0.001"} 1`,
		`sluice_decision_seconds_bucket{route="default",le="0.01"} 2`,
		`sluice_decision_seconds_bucket{route="default",le="0.1"} 3`,
		`sluice_decision_seconds_bucket{route="default",le="+Inf"} 4`,
		`sluice_decision_seconds_count{route="default"} 4`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}

	hh := h.With("default")
	if hh.Count() != 4 {
		t.Errorf("count = %d, want 4", hh.Count())
	}
	if got := hh.Sum(); got < 0.5554 || got > 0.5556 {
		t.Errorf("sum = %v, want ~0.5555", got)
	}

	// A sample exactly on a boundary belongs in that bucket.
	h2 := r.Histogram("sluice_exact", "Boundary check.", []float64{1, 2})
	h2.With().Observe(1)
	if !strings.Contains(r.String(), `sluice_exact_bucket{le="1"} 1`) {
		t.Error("a sample equal to a bucket bound must fall inside it")
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("sluice_denies_total", "Denials.", "reason")
	c.With(`policy "pii-eu" said no` + "\n" + `path\to`).Inc()

	out := r.String()
	want := `sluice_denies_total{reason="policy \"pii-eu\" said no\npath\\to"} 1`
	if !strings.Contains(out, want) {
		t.Errorf("label value not escaped\ngot:\n%s", out)
	}
}

func TestGaugeVecReset(t *testing.T) {
	r := NewRegistry()
	g := r.Gauge("sluice_weight", "Share.", "backend")
	g.With("a").Set(1)
	if !strings.Contains(r.String(), `sluice_weight{backend="a"}`) {
		t.Fatal("expected the series to be present")
	}
	g.Reset()
	if strings.Contains(r.String(), `sluice_weight{backend="a"}`) {
		t.Error("Reset must drop stale series so removed backends stop reporting")
	}
}

func TestRegistryRejectsConflictingRegistration(t *testing.T) {
	r := NewRegistry()
	r.Counter("x", "help", "a")

	defer func() {
		if recover() == nil {
			t.Error("re-registering a metric with different labels should panic")
		}
	}()
	r.Counter("x", "help", "b")
}

// -----------------------------------------------------------------------------
// Ledger
// -----------------------------------------------------------------------------

func decision(id string, v model.Verdict, cloud model.Cloud, usd, grams float64) *model.Decision {
	d := &model.Decision{
		ID: id, Timestamp: time.Now(), RouteID: "default", Verdict: v,
		Subject: model.Subject{ID: "spiffe://prod.internal/ns/api/sa/gw"},
		Request: model.Request{Method: "GET", Path: "/api/v1/items"},
	}
	if v == model.VerdictAllow {
		d.Cloud = cloud
		d.ChosenBackend = string(cloud) + "-primary"
		d.SavedUSD, d.SavedGrams = usd, grams
	} else {
		d.DenyReason = "unauthenticated request rejected"
	}
	return d
}

func TestLedgerRetentionAndEviction(t *testing.T) {
	l := NewLedger(3)
	for i, id := range []string{"a", "b", "c", "d"} {
		l.Record(decision(id, model.VerdictAllow, model.CloudAWS, float64(i), 0))
	}

	if l.Retained() != 3 {
		t.Fatalf("retained = %d, want 3", l.Retained())
	}
	if _, ok := l.Get("a"); ok {
		t.Error("the oldest entry should have been evicted from the index")
	}
	if _, ok := l.Get("d"); !ok {
		t.Error("the newest entry should be retrievable")
	}

	recent := l.Recent(Filter{Limit: 10})
	if len(recent) != 3 || recent[0].ID != "d" {
		t.Fatalf("Recent should return newest-first, got %d entries starting %q",
			len(recent), recent[0].ID)
	}

	// Lifetime totals must survive eviction.
	s := l.Summary()
	if s.Total != 4 {
		t.Errorf("total = %d, want 4 (lifetime, not retained)", s.Total)
	}
	if s.SavedUSD != 0+1+2+3 {
		t.Errorf("savedUsd = %v, want 6", s.SavedUSD)
	}
}

func TestLedgerFilters(t *testing.T) {
	l := NewLedger(50)
	l.Record(decision("1", model.VerdictAllow, model.CloudAWS, 0.5, 10))
	l.Record(decision("2", model.VerdictAllow, model.CloudGCP, 0.1, 90))
	l.Record(decision("3", model.VerdictDeny, "", 0, 0))

	if got := l.Recent(Filter{Verdict: "deny", Limit: 10}); len(got) != 1 || got[0].ID != "3" {
		t.Errorf("verdict filter returned %v", ids(got))
	}
	if got := l.Recent(Filter{Cloud: "gcp", Limit: 10}); len(got) != 1 || got[0].ID != "2" {
		t.Errorf("cloud filter returned %v", ids(got))
	}
	if got := l.Recent(Filter{MinSavedUSD: 0.3, Limit: 10}); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("savings filter returned %v", ids(got))
	}
	if got := l.Recent(Filter{Subject: "NS/API", Limit: 10}); len(got) != 3 {
		t.Errorf("subject substring match should be case-insensitive, got %v", ids(got))
	}
	if got := l.Recent(Filter{Path: "items", Limit: 10}); len(got) != 3 {
		t.Errorf("path filter returned %v", ids(got))
	}
	if got := l.Recent(Filter{Limit: 2}); len(got) != 2 {
		t.Errorf("limit not applied, got %d", len(got))
	}
}

func ids(ds []*model.Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func TestLedgerSummary(t *testing.T) {
	l := NewLedger(50)
	l.Record(decision("1", model.VerdictAllow, model.CloudAWS, 0.5, 10))
	l.Record(decision("2", model.VerdictAllow, model.CloudGCP, -0.2, 90))
	l.Record(decision("3", model.VerdictDeny, "", 0, 0))
	l.Record(decision("4", model.VerdictDeny, "", 0, 0))

	s := l.Summary()
	if s.ByVerdict["allow"] != 2 || s.ByVerdict["deny"] != 2 {
		t.Errorf("verdict counts = %v", s.ByVerdict)
	}
	if s.ByCloud["aws"] != 1 || s.ByCloud["gcp"] != 1 {
		t.Errorf("cloud counts = %v", s.ByCloud)
	}
	// A negative saving must be reported, not floored at zero.
	if s.SavedUSD != 0.3 {
		t.Errorf("savedUsd = %v, want 0.3 (0.5 + -0.2)", s.SavedUSD)
	}
	if len(s.TopDenyReasons) != 1 || s.TopDenyReasons[0].Count != 2 {
		t.Errorf("deny reasons = %v", s.TopDenyReasons)
	}
}

func TestLedgerSubscribeDoesNotBlockRecording(t *testing.T) {
	l := NewLedger(50)
	ch, cancel := l.Subscribe(2)
	defer cancel()

	// Far more decisions than the subscriber buffer holds. Recording must
	// stay non-blocking; excess updates are dropped and counted.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			l.Record(decision("x", model.VerdictAllow, model.CloudAWS, 0, 0))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a slow subscriber")
	}

	if len(ch) == 0 {
		t.Error("the subscriber should have received something")
	}
	if l.Summary().DroppedToSubs == 0 {
		t.Error("drops should be counted so the loss is visible")
	}

	cancel()
	cancel() // must be idempotent
}

func TestSubscriberReceivesDecisions(t *testing.T) {
	l := NewLedger(10)
	ch, cancel := l.Subscribe(4)
	defer cancel()

	l.Record(decision("live", model.VerdictAllow, model.CloudAzure, 1, 2))
	select {
	case got := <-ch:
		if got.ID != "live" {
			t.Errorf("got %q", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("no decision delivered to the subscriber")
	}
}

// -----------------------------------------------------------------------------
// Rollup
// -----------------------------------------------------------------------------

func TestRollup(t *testing.T) {
	r := NewRollup(10)
	base := time.Unix(0, 0)
	for i := 0; i < 15; i++ {
		r.ObserveMany(base.Add(time.Duration(i)*time.Second), map[string]float64{
			"rps.aws": float64(i),
			"rps.gcp": float64(i * 2),
		})
	}

	if keys := r.Keys(); len(keys) != 2 || keys[0] != "rps.aws" {
		t.Fatalf("keys = %v", keys)
	}

	pts := r.Series("rps.aws", 100)
	if len(pts) != 10 {
		t.Fatalf("capacity should bound retention, got %d points", len(pts))
	}
	if pts[0].V != 5 || pts[len(pts)-1].V != 14 {
		t.Errorf("expected the most recent 10 samples, got %v..%v", pts[0].V, pts[len(pts)-1].V)
	}

	if got := r.Series("rps.aws", 4); len(got) != 4 {
		t.Errorf("downsample returned %d points, want 4", len(got))
	}
	if r.Series("missing", 10) != nil {
		t.Error("an unknown series should return nil")
	}
	if all := r.All(3); len(all) != 2 || len(all["rps.gcp"]) != 3 {
		t.Errorf("All returned %v", all)
	}
}
