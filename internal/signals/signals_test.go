package signals

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

/* ── P-square quantile estimator ────────────────────────────────────────── */

func TestP2QuantileAccuracy(t *testing.T) {
	// A lognormal body with a heavy tail, which is what request latency
	// actually looks like and the case a naive estimator gets wrong.
	rng := rand.New(rand.NewPCG(1, 2))
	const n = 200000
	samples := make([]float64, 0, n)
	est := NewP2Quantile(0.95)

	for i := 0; i < n; i++ {
		v := 20 * math.Exp(0.4*rng.NormFloat64())
		if rng.Float64() < 0.02 {
			v *= 5
		}
		samples = append(samples, v)
		est.Add(v)
	}

	sort.Float64s(samples)
	exact := samples[int(0.95*float64(len(samples)))]
	got := est.Value()

	// P-square is an approximation; a couple of percent on a heavy-tailed
	// distribution is the accuracy it advertises and all the router needs.
	if rel := math.Abs(got-exact) / exact; rel > 0.03 {
		t.Errorf("p95 = %.2f, exact = %.2f (%.1f%% off)", got, exact, rel*100)
	}
}

func TestP2QuantileSmallSamples(t *testing.T) {
	e := NewP2Quantile(0.5)
	if e.Value() != 0 {
		t.Error("an empty estimator should report zero")
	}

	// Below five observations the estimator interpolates exactly, so it is
	// usable from the first sample rather than only after warm-up.
	for _, v := range []float64{10, 20, 30} {
		e.Add(v)
	}
	if got := e.Value(); math.Abs(got-20) > 1e-9 {
		t.Errorf("median of {10,20,30} = %v, want 20", got)
	}

	e.Add(40)
	if got := e.Value(); math.Abs(got-25) > 1e-9 {
		t.Errorf("median of {10,20,30,40} = %v, want 25", got)
	}
}

func TestP2QuantileIgnoresGarbage(t *testing.T) {
	e := NewP2Quantile(0.9)
	e.Add(math.NaN())
	e.Add(math.Inf(1))
	if e.Count() != 0 {
		t.Error("NaN and Inf must not be recorded")
	}
}

func TestRollingQuantileForgets(t *testing.T) {
	now := int64(0)
	clock := func() int64 { return now }
	r := NewRollingQuantile(0.5, int64(time.Second), clock)

	for i := 0; i < 50; i++ {
		r.Add(100)
	}
	if got := r.Value(); math.Abs(got-100) > 1 {
		t.Fatalf("want ~100, got %v", got)
	}

	// Two full windows later, with new data, the old samples must be gone.
	now += int64(2 * time.Second)
	for i := 0; i < 50; i++ {
		r.Add(10)
	}
	now += int64(1200 * time.Millisecond)
	for i := 0; i < 50; i++ {
		r.Add(10)
	}
	if got := r.Value(); got > 20 {
		t.Errorf("stale samples still dominate: %v", got)
	}
}

func TestEWMADecaysOnWallClock(t *testing.T) {
	now := int64(0)
	e := NewEWMA(int64(time.Second), func() int64 { return now })

	if e.Initialized() {
		t.Error("a fresh EWMA should not report initialised")
	}
	e.Add(1)
	if got := e.Value(); got != 1 {
		t.Fatalf("first observation should be adopted directly, got %v", got)
	}

	// One half-life of elapsed time moves the average halfway to the new
	// value, regardless of how many samples arrived.
	now += int64(time.Second)
	e.Add(0)
	if got := e.Value(); math.Abs(got-0.5) > 0.01 {
		t.Errorf("after one half-life want ~0.5, got %v", got)
	}
}

/* ── Series ─────────────────────────────────────────────────────────────── */

func TestSeriesRingBuffer(t *testing.T) {
	s := NewSeries(4)
	base := time.Unix(0, 0)
	for i := 0; i < 10; i++ {
		s.Add(base.Add(time.Duration(i)*time.Second), float64(i))
	}
	pts := s.Points()
	if len(pts) != 4 {
		t.Fatalf("want 4 retained points, got %d", len(pts))
	}
	if pts[0].V != 6 || pts[3].V != 9 {
		t.Errorf("want the most recent four, got %v..%v", pts[0].V, pts[3].V)
	}
	if last, ok := s.Last(); !ok || last.V != 9 {
		t.Errorf("Last = %v", last)
	}

	empty := NewSeries(3)
	if _, ok := empty.Last(); ok {
		t.Error("an empty series has no last point")
	}
	if got := empty.Downsample(5); len(got) != 0 {
		t.Error("downsampling an empty series should be empty")
	}
}

func TestSeriesDownsampleKeepsEnds(t *testing.T) {
	s := NewSeries(100)
	base := time.Unix(0, 0)
	for i := 0; i < 100; i++ {
		s.Add(base.Add(time.Duration(i)*time.Second), float64(i))
	}
	got := s.Downsample(10)
	if len(got) != 10 {
		t.Fatalf("want 10 points, got %d", len(got))
	}
	if got[0].V != 0 || got[9].V != 99 {
		t.Errorf("downsampling must preserve both ends, got %v..%v", got[0].V, got[9].V)
	}
}

/* ── Health and circuit breaking ────────────────────────────────────────── */

func TestBreakerTripsOnConsecutiveFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := DefaultBreakerConfig()
	h := NewHealthTracker(cfg, time.Second, func() time.Time { return now })

	for i := 0; i < cfg.ConsecutiveFailures-1; i++ {
		h.Observe(false)
	}
	if h.State().State != BreakerClosed {
		t.Fatal("breaker tripped early")
	}

	h.Observe(false)
	st := h.State()
	if st.State != BreakerOpen {
		t.Fatalf("want open after %d consecutive failures, got %s", cfg.ConsecutiveFailures, st.State)
	}
	if st.TrafficMultiplier != 0 {
		t.Errorf("an open breaker must take no traffic, got %v", st.TrafficMultiplier)
	}
}

func TestBreakerRecoveryPath(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := DefaultBreakerConfig()
	h := NewHealthTracker(cfg, time.Second, func() time.Time { return now })

	for i := 0; i < cfg.ConsecutiveFailures; i++ {
		h.Observe(false)
	}
	if h.State().State != BreakerOpen {
		t.Fatal("expected open")
	}

	// Reading the state after the open interval promotes it to half-open.
	now = now.Add(cfg.OpenDuration + time.Second)
	st := h.State()
	if st.State != BreakerHalfOpen {
		t.Fatalf("want half-open, got %s", st.State)
	}
	if st.TrafficMultiplier != cfg.HalfOpenShare {
		t.Errorf("half-open should carry a trickle, got %v", st.TrafficMultiplier)
	}

	for i := 0; i < cfg.HalfOpenSuccesses; i++ {
		h.Observe(true)
	}
	if got := h.State().State; got != BreakerClosed {
		t.Fatalf("want closed after %d successes, got %s", cfg.HalfOpenSuccesses, got)
	}
}

func TestBreakerBacksOffOnRepeatedFailure(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := DefaultBreakerConfig()
	h := NewHealthTracker(cfg, time.Second, func() time.Time { return now })

	for i := 0; i < cfg.ConsecutiveFailures; i++ {
		h.Observe(false)
	}
	firstRetry := h.State().RetryAt

	now = now.Add(cfg.OpenDuration + time.Second)
	_ = h.State()    // promote to half-open
	h.Observe(false) // the probe fails

	st := h.State()
	if st.State != BreakerOpen {
		t.Fatalf("a failed probe must re-open, got %s", st.State)
	}
	// A persistently sick region should be probed less often, not hammered on
	// a fixed cadence.
	if !st.RetryAt.After(firstRetry.Add(cfg.OpenDuration)) {
		t.Error("the open interval should have backed off")
	}
}

func TestBreakerIgnoresEarlyNoise(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := DefaultBreakerConfig()
	cfg.ConsecutiveFailures = 0 // rate-based only
	h := NewHealthTracker(cfg, time.Millisecond, func() time.Time { return now })

	// A single failure against a fresh backend reads as a 100% error rate.
	// MinObservations exists so that does not eject it.
	h.Observe(false)
	if h.State().State != BreakerClosed {
		t.Error("one failed probe on a new backend must not eject it")
	}
}

func TestForceOpenAndReset(t *testing.T) {
	h := NewHealthTracker(DefaultBreakerConfig(), time.Second, time.Now)
	h.ForceOpen()
	if h.State().State != BreakerOpen {
		t.Fatal("ForceOpen should eject immediately, for a maintenance drain")
	}
	h.Reset()
	if h.State().State != BreakerClosed {
		t.Fatal("Reset should close the breaker")
	}
}

/* ── Carbon ─────────────────────────────────────────────────────────────── */

func TestDiurnalIntensityFollowsSolar(t *testing.T) {
	high := gridZones["US-CAL-CISO"] // ~30% solar
	low := gridZones["US-NW-BPAT"]   // hydro, almost no solar

	// Local noon and local 8pm for a UTC-8 zone.
	noon := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 6, 2, 4, 0, 0, 0, time.UTC)

	highNoon, highEve := DiurnalIntensity(high, noon), DiurnalIntensity(high, evening)
	lowNoon, lowEve := DiurnalIntensity(low, noon), DiurnalIntensity(low, evening)

	if highNoon >= highEve {
		t.Errorf("a solar-heavy grid should be cleaner at midday: noon %.0f, evening %.0f", highNoon, highEve)
	}
	// The swing is the signal a follow-the-sun policy trades on, so a
	// high-solar grid must vary more than a hydro one.
	highSwing := (highEve - highNoon) / highEve
	lowSwing := (lowEve - lowNoon) / lowEve
	if highSwing <= lowSwing {
		t.Errorf("solar penetration should widen the daily swing: %.3f vs %.3f", highSwing, lowSwing)
	}
	if DiurnalIntensity(low, noon) <= 0 {
		t.Error("intensity must stay positive")
	}
}

func TestZoneLookupAndUnknownIsNotClean(t *testing.T) {
	z, ok := ZoneFor(model.Backend{Cloud: model.CloudAWS, Region: "eu-north-1"})
	if !ok || z.ID != "SE" {
		t.Fatalf("AWS eu-north-1 should map to the Swedish grid, got %q", z.ID)
	}

	// AWS us-east-1 and Azure eastus are both on PJM. Routing between those
	// clouds does nothing for emissions, and the model has to know that.
	aws, _ := ZoneFor(model.Backend{Cloud: model.CloudAWS, Region: "us-east-1"})
	azure, _ := ZoneFor(model.Backend{Cloud: model.CloudAzure, Region: "eastus"})
	if aws.ID != azure.ID {
		t.Errorf("co-located regions should share a grid zone: %q vs %q", aws.ID, azure.ID)
	}

	// An unmapped region must not read as clean, or it wins traffic by virtue
	// of missing data.
	unknown, ok := ZoneFor(model.Backend{Cloud: model.CloudAWS, Region: "mars-west-1"})
	if ok {
		t.Error("expected the lookup to report the region as unmapped")
	}
	if unknown.BaseIntensity < 300 {
		t.Errorf("an unknown grid should be assumed dirty, got %.0f", unknown.BaseIntensity)
	}

	explicit, ok := ZoneFor(model.Backend{Cloud: model.CloudAWS, Region: "anything", GridZone: "FR"})
	if !ok || explicit.ID != "FR" {
		t.Error("an explicit GridZone should win over the region mapping")
	}
}

func TestCarbonModelArithmetic(t *testing.T) {
	m := DefaultCarbonModel()
	// 0.015 kWh/GB × 1.09 PUE × 55 gCO2e/kWh
	got := m.GramsPerGB(model.CloudGCP, 55)
	want := 0.015 * 1.09 * 55
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("GramsPerGB = %v, want %v", got, want)
	}
	if m.PUEFor("unknown-cloud") <= 1 {
		t.Error("an unknown provider should still get a plausible PUE")
	}
}

/* ── Pricing ────────────────────────────────────────────────────────────── */

func TestListPrices(t *testing.T) {
	if p, ok := ListPrice(model.CloudAWS, "us-east-1"); !ok || p != 0.090 {
		t.Errorf("AWS us-east-1 = %v (found %v)", p, ok)
	}
	// Azure is genuinely cheaper than AWS in the same metro, which is the
	// arbitrage the router exists to exploit.
	azure, _ := ListPrice(model.CloudAzure, "eastus")
	aws, _ := ListPrice(model.CloudAWS, "us-east-1")
	if azure >= aws {
		t.Errorf("expected Azure eastus (%v) below AWS us-east-1 (%v)", azure, aws)
	}

	// An unknown region must not look cheap.
	p, ok := ListPrice(model.CloudAWS, "nowhere-1")
	if ok {
		t.Error("expected the region to be reported as unknown")
	}
	if p < aws {
		t.Errorf("an unknown region should be priced pessimistically, got %v", p)
	}
}

func TestStaticPricerHonoursOverrides(t *testing.T) {
	p := &StaticPricer{Overrides: map[string]float64{"b1": 0.02}}
	got, ok, err := p.Price(t.Context(), model.Backend{ID: "b1", Cloud: model.CloudAWS, Region: "us-east-1"})
	if err != nil || !ok || got != 0.02 {
		t.Fatalf("negotiated rate should win: got %v, ok %v, err %v", got, ok, err)
	}
}

/* ── Store ──────────────────────────────────────────────────────────────── */

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(DefaultStoreConfig())
	s.Register(model.Backend{
		ID: "b1", Cloud: model.CloudAWS, Region: "us-east-1",
		Jurisdiction: "US", Tier: "primary", Enabled: true,
	})
	return s
}

func TestStoreSnapshotAndVector(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	s.SetPrice("b1", Quote{Value: 0.09, Source: "test", AsOf: now, TTL: time.Hour})
	s.SetGridIntensity("b1", Quote{Value: 355, Source: "test", AsOf: now, TTL: time.Hour})
	for i := 0; i < 20; i++ {
		s.ObserveProbe("b1", 25*time.Millisecond, true)
	}

	snap := s.Snapshot()
	b, ok := snap.ByID("b1")
	if !ok {
		t.Fatal("backend missing from snapshot")
	}

	v := b.Vector()
	if v[model.DimCost] != 0.09 {
		t.Errorf("cost = %v", v[model.DimCost])
	}
	if math.Abs(v[model.DimLatency]-25) > 3 {
		t.Errorf("latency = %v, want ~25", v[model.DimLatency])
	}
	wantCarbon := DefaultCarbonModel().GramsPerGB(model.CloudAWS, 355)
	if math.Abs(v[model.DimCarbon]-wantCarbon) > 1e-9 {
		t.Errorf("carbon = %v, want %v", v[model.DimCarbon], wantCarbon)
	}
	if len(b.Stale) != 0 {
		t.Errorf("fresh quotes should not be marked stale: %v", b.Stale)
	}
}

func TestStoreFlagsStaleQuotes(t *testing.T) {
	s := newTestStore(t)
	s.SetPrice("b1", Quote{Value: 0.09, Source: "test", AsOf: time.Now().Add(-2 * time.Hour), TTL: time.Minute})
	s.SetGridIntensity("b1", Quote{Value: 300, Source: "test", AsOf: time.Now(), TTL: time.Hour})

	b, _ := s.Snapshot().ByID("b1")
	if len(b.Stale) != 1 || b.Stale[0] != "egress" {
		t.Errorf("expected the egress quote to be flagged stale, got %v", b.Stale)
	}
}

func TestStoreAccountsForRealTraffic(t *testing.T) {
	s := newTestStore(t)
	s.SetPrice("b1", Quote{Value: 0.10, Source: "test", AsOf: time.Now(), TTL: time.Hour})
	s.SetGridIntensity("b1", Quote{Value: 400, Source: "test", AsOf: time.Now(), TTL: time.Hour})

	const gb = 1 << 30
	s.ObserveRequest("b1", 30*time.Millisecond, true, gb)

	usd, grams, bytes, requests, errs := s.Totals()
	if bytes != gb || requests != 1 || errs != 0 {
		t.Errorf("counters = %d bytes, %d requests, %d errors", bytes, requests, errs)
	}
	// Spend comes from bytes actually transferred, not from a projection.
	if math.Abs(usd-0.10) > 1e-4 {
		t.Errorf("spend = %v, want ~0.10 for one GB at $0.10/GB", usd)
	}
	wantGrams := DefaultCarbonModel().GramsPerGB(model.CloudAWS, 400)
	if math.Abs(grams-wantGrams) > 0.01 {
		t.Errorf("emissions = %v, want %v", grams, wantGrams)
	}

	s.ObserveRequest("b1", 30*time.Millisecond, false, 0)
	if _, _, _, _, errs = s.Totals(); errs != 1 {
		t.Errorf("errors = %d, want 1", errs)
	}
}

func TestStoreRegistrationPreservesHistory(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 30; i++ {
		s.ObserveProbe("b1", 40*time.Millisecond, true)
	}
	before, _ := s.Snapshot().ByID("b1")

	// A config reload re-registers the same backend. Blanking its latency
	// distribution would leave the router deciding with no data.
	s.Register(model.Backend{ID: "b1", Cloud: model.CloudAWS, Region: "us-east-1", Enabled: true, Bias: 1})
	after, _ := s.Snapshot().ByID("b1")

	if math.Abs(before.LatencyP95.Value-after.LatencyP95.Value) > 1 {
		t.Errorf("re-registration lost the latency distribution: %v then %v",
			before.LatencyP95.Value, after.LatencyP95.Value)
	}
}

func TestStoreRemove(t *testing.T) {
	s := newTestStore(t)
	s.Remove("b1")
	if len(s.Backends()) != 0 {
		t.Error("Remove should deregister the backend")
	}
	if _, ok := s.Snapshot().ByID("b1"); ok {
		t.Error("a removed backend must not appear in snapshots")
	}
	// Writes against a removed backend must not panic.
	s.SetPrice("b1", Quote{Value: 1})
	s.ObserveRequest("b1", time.Millisecond, true, 10)
}

func TestStoreHistory(t *testing.T) {
	s := newTestStore(t)
	s.SetPrice("b1", Quote{Value: 0.09, AsOf: time.Now(), TTL: time.Hour})
	s.SetGridIntensity("b1", Quote{Value: 300, AsOf: time.Now(), TTL: time.Hour})
	for i := 0; i < 5; i++ {
		s.RecordSample(map[string]float64{"b1": 0.5}, map[string]float64{"b1": 12})
	}
	if got := s.History("b1", model.DimCost, 10); len(got) != 5 {
		t.Errorf("cost history has %d points, want 5", len(got))
	}
	if got := s.WeightHistory("b1", 10); len(got) != 5 || got[0].V != 0.5 {
		t.Errorf("weight history = %v", got)
	}
	if got := s.RPSHistory("b1", 10); len(got) != 5 || got[0].V != 12 {
		t.Errorf("rps history = %v", got)
	}
	if s.History("missing", model.DimCost, 10) != nil {
		t.Error("history for an unknown backend should be nil")
	}
}

func TestQuoteStaleness(t *testing.T) {
	now := time.Now()
	if !(Quote{}).Stale(now) {
		t.Error("a quote that was never set is stale")
	}
	if (Quote{Value: 1, AsOf: now.Add(-time.Hour)}).Stale(now) {
		t.Error("a quote with no TTL never goes stale")
	}
	if !(Quote{Value: 1, AsOf: now.Add(-time.Hour), TTL: time.Minute}).Stale(now) {
		t.Error("expected stale")
	}
}
