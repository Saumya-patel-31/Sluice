package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
)

func testBackends() []model.Backend {
	return []model.Backend{
		{ID: "aws-us-east-1", Cloud: model.CloudAWS, Region: "us-east-1", Jurisdiction: "US", Tier: "primary", Enabled: true},
		{ID: "gcp-europe-west1", Cloud: model.CloudGCP, Region: "europe-west1", Jurisdiction: "EU", Tier: "primary", Enabled: true},
		{ID: "azure-northeurope", Cloud: model.CloudAzure, Region: "northeurope", Jurisdiction: "EU", Tier: "burst", Enabled: true},
		{ID: "aws-ap-south-1", Cloud: model.CloudAWS, Region: "ap-south-1", Jurisdiction: "IN", Tier: "burst", Enabled: true},
	}
}

func prodSubject() *model.Subject {
	return &model.Subject{
		ID:            "spiffe://prod.internal/ns/payments/sa/checkout",
		TrustDomain:   "prod.internal",
		Namespace:     "payments",
		Service:       "checkout",
		MTLS:          true,
		Authenticated: true,
		Claims:        map[string]string{"residency": "eu"},
	}
}

func evalDefault(t *testing.T, sub *model.Subject, req *model.Request) Result {
	t.Helper()
	set := MustCompileDefault()
	return set.Evaluate(Input{
		Subject:        sub,
		Request:        req,
		Candidates:     testBackends(),
		Now:            time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC),
		BaseObjectives: model.Vector{0.25, 0.25, 0.25, 0.25},
	})
}

func TestDefaultDocumentCompiles(t *testing.T) {
	set := MustCompileDefault()
	if set.Len() == 0 {
		t.Fatal("default document produced no policies")
	}
	// Evaluation order must be priority-ascending.
	prev := -1 << 31
	for _, p := range set.Policies() {
		if p.Priority < prev {
			t.Fatalf("policies out of order: %q has priority %d after %d", p.Name, p.Priority, prev)
		}
		prev = p.Priority
	}
	if set.Hash() == "" {
		t.Fatal("expected a non-empty set hash")
	}
}

func TestAnonymousIsDenied(t *testing.T) {
	anon := model.Anonymous()
	res := evalDefault(t, &anon, &model.Request{Method: "GET", Path: "/api/v1/feed"})
	if res.Verdict != model.VerdictDeny {
		t.Fatalf("want deny for anonymous traffic, got %s", res.Verdict)
	}
	if !strings.Contains(res.DenyReason, "unauthenticated") {
		t.Fatalf("unexpected deny reason: %q", res.DenyReason)
	}
}

func TestAuthenticatedWorkloadIsAllowed(t *testing.T) {
	res := evalDefault(t, prodSubject(), &model.Request{Method: "GET", Path: "/api/v1/search"})
	if res.Verdict != model.VerdictAllow {
		t.Fatalf("want allow, got %s (%s)", res.Verdict, res.DenyReason)
	}
	if len(res.Eligible) != 4 {
		t.Fatalf("want all 4 backends eligible, got %v", res.Eligible)
	}
}

func TestPaymentsWithoutMTLSIsDenied(t *testing.T) {
	sub := prodSubject()
	sub.MTLS = false
	res := evalDefault(t, sub, &model.Request{Method: "POST", Path: "/api/payments/charge"})
	if res.Verdict != model.VerdictDeny {
		t.Fatalf("want deny, got %s", res.Verdict)
	}
	if !strings.Contains(res.DenyReason, "mutual TLS") {
		t.Fatalf("unexpected deny reason: %q", res.DenyReason)
	}
}

func TestResidencyConstraintPrunesNonEU(t *testing.T) {
	res := evalDefault(t, prodSubject(), &model.Request{
		Method: "POST", Path: "/api/v1/profile", DataClass: model.DataPII,
	})
	if res.Verdict != model.VerdictAllow {
		t.Fatalf("want allow, got %s (%s)", res.Verdict, res.DenyReason)
	}
	for _, id := range res.Eligible {
		if id == "aws-us-east-1" || id == "aws-ap-south-1" {
			t.Fatalf("non-EU backend %q survived the residency constraint", id)
		}
	}
	if len(res.Eligible) != 2 {
		t.Fatalf("want the 2 EU backends, got %v", res.Eligible)
	}
	if reason := res.Pruned["aws-us-east-1"]; !strings.Contains(reason, "GDPR") {
		t.Fatalf("expected a GDPR prune reason, got %q", reason)
	}
}

func TestRegulatedTrafficRequiresPrimaryTier(t *testing.T) {
	sub := prodSubject()
	sub.Claims = nil // not an EU data subject, so only the tier rule applies
	res := evalDefault(t, sub, &model.Request{
		Method: "POST", Path: "/api/v1/ledger", DataClass: model.DataRegulated,
	})
	if res.Verdict != model.VerdictAllow {
		t.Fatalf("want allow, got %s (%s)", res.Verdict, res.DenyReason)
	}
	for _, id := range res.Eligible {
		if id == "azure-northeurope" || id == "aws-ap-south-1" {
			t.Fatalf("burst-tier backend %q survived the tier constraint", id)
		}
	}
}

func TestConstraintsEliminatingEverythingIsNotADeny(t *testing.T) {
	// PII from an EU subject, but no EU backend registered. The request was
	// authorised; there is simply nowhere legal to send it. That has to read
	// differently from a policy rejection.
	set := MustCompileDefault()
	res := set.Evaluate(Input{
		Subject: prodSubject(),
		Request: &model.Request{Method: "GET", Path: "/api/v1/profile", DataClass: model.DataPII},
		Candidates: []model.Backend{
			{ID: "aws-us-east-1", Cloud: model.CloudAWS, Region: "us-east-1", Jurisdiction: "US", Tier: "primary", Enabled: true},
		},
		Now: time.Now(),
	})
	if res.Verdict != model.VerdictNoCapacity {
		t.Fatalf("want no_capacity, got %s (%s)", res.Verdict, res.DenyReason)
	}
}

func TestPreferOverridesObjectives(t *testing.T) {
	res := evalDefault(t, prodSubject(), &model.Request{Method: "GET", Path: "/api/v1/feed"})
	if res.Objectives[model.DimLatency] != 0.70 {
		t.Fatalf("want latency weight 0.70 from the interactive policy, got %v", res.Objectives)
	}

	// Batch during the day picks up the cost/carbon profile but not the
	// overnight one, since the fixed clock is midday UTC.
	batch := evalDefault(t, prodSubject(), &model.Request{Method: "POST", Path: "/batch/reindex"})
	if batch.Objectives[model.DimCost] != 0.45 {
		t.Fatalf("want cost weight 0.45 for daytime batch, got %v", batch.Objectives)
	}
}

func TestOvernightBatchOverridesDaytimeBatch(t *testing.T) {
	set := MustCompileDefault()
	res := set.Evaluate(Input{
		Subject:    prodSubject(),
		Request:    &model.Request{Method: "POST", Path: "/batch/reindex"},
		Candidates: testBackends(),
		Now:        time.Date(2026, 3, 4, 23, 30, 0, 0, time.UTC),
	})
	// Both batch policies match; the higher priority number is evaluated last
	// and wins.
	if res.Objectives[model.DimCarbon] != 0.60 {
		t.Fatalf("want carbon weight 0.60 overnight, got %v", res.Objectives)
	}
}

func TestAdminRestrictedByCIDR(t *testing.T) {
	external := evalDefault(t, prodSubject(), &model.Request{
		Method: "GET", Path: "/admin/flags", SourceIP: "203.0.113.9",
	})
	if external.Verdict != model.VerdictDeny {
		t.Fatalf("want deny for off-corp admin access, got %s", external.Verdict)
	}

	internal := evalDefault(t, prodSubject(), &model.Request{
		Method: "GET", Path: "/admin/flags", SourceIP: "10.4.2.7",
	})
	if internal.Verdict != model.VerdictAllow {
		t.Fatalf("want allow from the corporate range, got %s (%s)", internal.Verdict, internal.DenyReason)
	}
}

func TestEvaluationErrorFailsClosed(t *testing.T) {
	set, err := Compile(`
policy "broken" {
  priority 10
  effect   allow
  when     request.path > 5
}`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := set.Evaluate(Input{
		Subject:    prodSubject(),
		Request:    &model.Request{Path: "/x"},
		Candidates: testBackends(),
	})
	if res.Verdict != model.VerdictDeny {
		t.Fatalf("a type error must fail closed, got %s", res.Verdict)
	}
	if !strings.Contains(res.DenyReason, "failing closed") {
		t.Fatalf("deny reason should say so: %q", res.DenyReason)
	}
	if res.Trace[0].Error == "" {
		t.Fatal("expected the trace to carry the evaluation error")
	}
}

func TestUnknownFieldIsACompileTimeError(t *testing.T) {
	set, err := Compile(`policy "typo" { effect allow when subject.trustdomain == "x" }`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	res := set.Evaluate(Input{Subject: prodSubject(), Request: &model.Request{}})
	if res.Verdict != model.VerdictDeny {
		t.Fatal("a misspelled attribute must not silently evaluate to false-then-allow")
	}
	if !strings.Contains(res.Trace[0].Error, "trust_domain") {
		t.Fatalf("the error should suggest the real field names, got %q", res.Trace[0].Error)
	}
}

// -----------------------------------------------------------------------------
// Expression-level tests
// -----------------------------------------------------------------------------

func evalExpr(t *testing.T, src string) Value {
	t.Helper()
	e, err := ParseExpr(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	env := BuildEnv(prodSubject(),
		&model.Request{Method: "GET", Path: "/api/v1/feed", SourceIP: "10.1.2.3",
			Headers: map[string]string{"X-Tenant": "acme"}},
		time.Date(2026, 3, 4, 23, 0, 0, 0, time.UTC))
	env.SetBackend(&model.Backend{ID: "b1", Cloud: model.CloudGCP, Region: "europe-west1",
		Jurisdiction: "EU", Labels: map[string]string{"az": "b"}, Enabled: true})
	v, err := e.Eval(env)
	if err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
	return v
}

func TestOperatorPrecedence(t *testing.T) {
	// 'and' binds tighter than 'or': false or (true and false) is false.
	if v := evalExpr(t, `false or true and false`); v.B {
		t.Fatal("want false; 'and' must bind tighter than 'or'")
	}
	if v := evalExpr(t, `(false or true) and false`); v.B {
		t.Fatal("parenthesised form should be false")
	}
	if v := evalExpr(t, `true or true and false`); !v.B {
		t.Fatal("want true")
	}
}

func TestStringOperators(t *testing.T) {
	cases := map[string]bool{
		`request.path startswith "/api"`:                            true,
		`request.path endswith "feed"`:                              true,
		`request.path contains "v1"`:                                true,
		`subject.id matches "spiffe://prod.internal/ns/payments/*"`: true,
		`subject.id matches "spiffe://prod.internal/ns/billing/*"`:  false,
		`backend.region in ["europe-west1", "us-east-1"]`:           true,
		`backend.region not in ["us-east-1"]`:                       true,
		`subject.claims["residency"] == "eu"`:                       true,
		`subject.claims["absent"] == null`:                          true,
		`request.headers["x-tenant"] == "acme"`:                     true,
		`ip_in_cidr(request.source_ip, "10.0.0.0/8")`:               true,
		`ip_in_cidr(request.source_ip, "192.168.0.0/16")`:           false,
		`has(backend.labels, "az")`:                                 true,
		`len(subject.service) == 8`:                                 true,
		`lower("ABC") == "abc"`:                                     true,
		`time.hour >= 22`:                                           true,
		`time.is_weekend`:                                           false,
	}
	for src, want := range cases {
		if got := evalExpr(t, src); got.B != want {
			t.Errorf("%s = %v, want %v", src, got.B, want)
		}
	}
}

func TestShortCircuitAvoidsTypeErrors(t *testing.T) {
	// The right side would be a type error, but 'and' must not reach it.
	e, err := ParseExpr(`false and request.path > 5`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	env := BuildEnv(prodSubject(), &model.Request{Path: "/x"}, time.Now())
	v, err := e.Eval(env)
	if err != nil {
		t.Fatalf("short-circuit should have avoided evaluation: %v", err)
	}
	if v.B {
		t.Fatal("want false")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"", "", true},
		{"", "x", false},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abd", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"*/ns/payments/*", "spiffe://p/ns/payments/sa/x", true},
		{"*/ns/payments/*", "spiffe://p/ns/billing/sa/x", false},
		{"*a*b*c*", "xxaxxbxxcxx", true},
		{"*a*b*c*", "xxaxxcxxbxx", false},
	}
	for _, c := range cases {
		if got := Glob(c.pattern, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestSyntaxErrorsCarryPosition(t *testing.T) {
	_, err := Compile("policy \"x\" {\n  effect allow\n  when subject.id ==\n}")
	if err == nil {
		t.Fatal("expected a syntax error")
	}
	var se *SyntaxError
	if !asSyntaxError(err, &se) {
		t.Fatalf("want a *SyntaxError, got %T: %v", err, err)
	}
	if se.Line != 4 {
		t.Errorf("want the error on line 4, got line %d (%s)", se.Line, se.Msg)
	}
}

func asSyntaxError(err error, out **SyntaxError) bool {
	if se, ok := err.(*SyntaxError); ok {
		*out = se
		return true
	}
	return false
}

func TestDuplicateClauseRejected(t *testing.T) {
	_, err := Compile(`policy "x" { effect allow when true when false }`)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want a duplicate-clause error, got %v", err)
	}
}

func TestEffectValidation(t *testing.T) {
	cases := map[string]string{
		`policy "a" { effect allow require backend.tier == "x" }`: "cannot have a 'require'",
		`policy "b" { effect constrain when true }`:               "need a 'require'",
		`policy "c" { effect prefer when true }`:                  "need a 'prefer'",
		`policy "d" { effect prefer prefer { bogus: 1 } }`:        "unknown objective",
		`policy "e" { effect teleport }`:                          "unknown effect",
	}
	for src, want := range cases {
		if _, err := Compile(src); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%s): want error containing %q, got %v", src, want, err)
		}
	}
}

// -----------------------------------------------------------------------------
// Cache
// -----------------------------------------------------------------------------

func TestCacheKeyDistinguishesInputs(t *testing.T) {
	base := Input{
		Subject:    prodSubject(),
		Request:    &model.Request{Method: "GET", Path: "/api/v1/feed"},
		Candidates: testBackends(),
	}
	k := CacheKey("h1", base)

	if CacheKey("h1", base) != k {
		t.Fatal("cache key must be stable for identical input")
	}
	if CacheKey("h2", base) == k {
		t.Fatal("a different policy document must produce a different key")
	}

	other := base
	other.Request = &model.Request{Method: "GET", Path: "/api/v1/other"}
	if CacheKey("h1", other) == k {
		t.Fatal("a different path must produce a different key")
	}

	noMTLS := *prodSubject()
	noMTLS.MTLS = false
	other = base
	other.Subject = &noMTLS
	if CacheKey("h1", other) == k {
		t.Fatal("mTLS status must be part of the key")
	}

	other = base
	other.Candidates = testBackends()[:2]
	if CacheKey("h1", other) == k {
		t.Fatal("the candidate set must be part of the key")
	}
}

func TestCacheRoundTripAndIsolation(t *testing.T) {
	c := NewCache(64, time.Minute)
	res := Result{
		Verdict:  model.VerdictAllow,
		Eligible: []string{"a", "b"},
		Pruned:   map[string]string{"c": "nope"},
		Trace:    []model.PolicyHit{{Policy: "p", Matched: true}},
	}
	c.Put(7, res)

	got, ok := c.Get(7)
	if !ok || got.Verdict != model.VerdictAllow {
		t.Fatal("expected a hit")
	}

	// Mutating the returned copy must not corrupt the cached entry.
	got.Eligible[0] = "mutated"
	got.Pruned["c"] = "mutated"

	again, _ := c.Get(7)
	if again.Eligible[0] != "a" || again.Pruned["c"] != "nope" {
		t.Fatal("cache returned a shared reference; entries must be cloned")
	}

	if _, ok := c.Get(8); ok {
		t.Fatal("unexpected hit for an absent key")
	}
	c.Purge()
	if _, ok := c.Get(7); ok {
		t.Fatal("purge should have emptied the cache")
	}
}

func TestCacheEvictsAndExpires(t *testing.T) {
	c := NewCache(cacheShards, time.Minute) // capacity 1 per shard
	// Two keys in the same shard: the second must evict the first.
	k1, k2 := uint64(0), uint64(cacheShards)
	c.Put(k1, Result{Verdict: model.VerdictAllow})
	c.Put(k2, Result{Verdict: model.VerdictDeny})
	if _, ok := c.Get(k1); ok {
		t.Fatal("expected the older entry to be evicted")
	}
	if _, ok := c.Get(k2); !ok {
		t.Fatal("expected the newer entry to be retained")
	}

	now := time.Now()
	c2 := NewCache(64, 10*time.Millisecond)
	c2.now = func() time.Time { return now }
	c2.Put(1, Result{Verdict: model.VerdictAllow})
	if _, ok := c2.Get(1); !ok {
		t.Fatal("expected a hit before expiry")
	}
	now = now.Add(time.Second)
	if _, ok := c2.Get(1); ok {
		t.Fatal("expected the entry to expire")
	}
}
