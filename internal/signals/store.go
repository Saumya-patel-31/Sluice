package signals

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// Quote is a single measured or published value together with its provenance.
//
// Provenance is not decoration. A routing decision made against a six-hour-old
// price list is a different kind of claim than one made against a live API
// response, and an operator looking at a surprising decision needs to be able
// to tell those apart. Every number the router consumes carries its source and
// age.
type Quote struct {
	Value  float64       `json:"value"`
	Source string        `json:"source"`
	AsOf   time.Time     `json:"asOf"`
	TTL    time.Duration `json:"ttlSeconds"`
}

// Stale reports whether the quote is older than its TTL.
func (q Quote) Stale(now time.Time) bool {
	if q.AsOf.IsZero() {
		return true
	}
	if q.TTL <= 0 {
		return false
	}
	return now.Sub(q.AsOf) > q.TTL
}

// Age returns how long ago the quote was produced.
func (q Quote) Age(now time.Time) time.Duration {
	if q.AsOf.IsZero() {
		return 0
	}
	return now.Sub(q.AsOf)
}

// CarbonModel converts bytes moved into grams of CO2-equivalent.
//
// The conversion is deliberately explicit and configurable because the
// underlying literature is not settled. Published estimates for the energy
// intensity of fixed-line data transfer span roughly 0.004 to 0.06 kWh/GB
// depending on methodology, vintage, and how much of the access network is
// attributed to the transfer. Sluice defaults to a mid-range figure and
// reports it in the API so a reader can see exactly what assumption produced
// the carbon numbers on the dashboard, and override it.
type CarbonModel struct {
	// EnergyKWhPerGB is the network energy intensity of moving one GB.
	EnergyKWhPerGB float64 `json:"energyKwhPerGb"`
	// PUE is power usage effectiveness per cloud provider — datacentre
	// overhead multiplied on top of IT load. Providers publish these.
	PUE map[model.Cloud]float64 `json:"pue"`
}

// DefaultCarbonModel returns the shipped defaults with published PUE figures.
func DefaultCarbonModel() CarbonModel {
	return CarbonModel{
		EnergyKWhPerGB: 0.015,
		PUE: map[model.Cloud]float64{
			model.CloudAWS:    1.15,
			model.CloudGCP:    1.09,
			model.CloudAzure:  1.17,
			model.CloudOnPrem: 1.60,
		},
	}
}

// PUEFor returns the PUE for a cloud, defaulting to the industry average when
// the provider is unknown.
func (c CarbonModel) PUEFor(cloud model.Cloud) float64 {
	if p, ok := c.PUE[cloud]; ok && p > 0 {
		return p
	}
	return 1.5
}

// GramsPerGB converts a grid intensity in gCO2e/kWh into gCO2e per GB moved.
func (c CarbonModel) GramsPerGB(cloud model.Cloud, gridIntensity float64) float64 {
	return c.EnergyKWhPerGB * c.PUEFor(cloud) * gridIntensity
}

// -----------------------------------------------------------------------------
// Per-backend state
// -----------------------------------------------------------------------------

// backendState is the mutable signal state for one backend.
type backendState struct {
	backend model.Backend

	mu    sync.RWMutex
	price Quote
	grid  Quote

	latency *LatencyTracker
	health  *HealthTracker

	// Cumulative accounting, updated from real data-plane traffic.
	bytesOut      atomic.Int64
	requests      atomic.Int64
	errors        atomic.Int64
	spentMicroUSD atomic.Int64 // USD * 1e6, integer to avoid float drift
	gramsMilli    atomic.Int64 // grams * 1e3

	history    map[model.Dimension]*Series
	rpsHist    *Series
	weightHist *Series
}

// BackendState is an immutable view of one backend's signals at a point in
// time. The router only ever sees this, never the live mutable state, so a
// decision cannot observe a half-updated signal set.
type BackendState struct {
	Backend model.Backend `json:"backend"`

	Egress        Quote `json:"egress"`        // USD per GB
	GridIntensity Quote `json:"gridIntensity"` // gCO2e per kWh
	CarbonPerGB   Quote `json:"carbonPerGb"`   // gCO2e per GB, derived
	LatencyP50    Quote `json:"latencyP50"`    // ms
	LatencyP95    Quote `json:"latencyP95"`    // ms
	ErrorRate     Quote `json:"errorRate"`     // 0..1

	Breaker      BreakerState `json:"breaker"`
	Healthy      bool         `json:"healthy"`
	InFlight     int64        `json:"inFlight"`
	Requests     int64        `json:"requests"`
	Errors       int64        `json:"errors"`
	BytesOut     int64        `json:"bytesOut"`
	SpentUSD     float64      `json:"spentUsd"`
	EmittedGrams float64      `json:"emittedGrams"`

	// Stale lists the signals whose quotes have outlived their TTL.
	Stale []string `json:"stale,omitempty"`
}

// Vector returns the raw objective vector, in the dimension order the router
// expects. All components are "lower is better".
func (s BackendState) Vector() model.Vector {
	var v model.Vector
	v[model.DimCost] = s.Egress.Value
	v[model.DimLatency] = s.LatencyP95.Value
	v[model.DimCarbon] = s.CarbonPerGB.Value
	v[model.DimReliability] = s.ErrorRate.Value
	return v
}

// Snapshot is a coherent read of every backend's signals.
type Snapshot struct {
	Taken    time.Time      `json:"taken"`
	Backends []BackendState `json:"backends"`
	Carbon   CarbonModel    `json:"carbonModel"`
}

// ByID returns the state for one backend.
func (s Snapshot) ByID(id string) (BackendState, bool) {
	for _, b := range s.Backends {
		if b.Backend.ID == id {
			return b, true
		}
	}
	return BackendState{}, false
}

// -----------------------------------------------------------------------------
// Store
// -----------------------------------------------------------------------------

// StoreConfig tunes retention and tracker behaviour.
type StoreConfig struct {
	// HistoryPoints is how many samples each per-dimension series retains.
	HistoryPoints int
	// LatencyWindow is the sliding window for the p95 estimator.
	LatencyWindow time.Duration
	// ErrorHalfLife is the decay half-life of the error-rate EWMA.
	ErrorHalfLife time.Duration
	// Breaker configures outlier ejection.
	Breaker BreakerConfig
	// Carbon is the bytes-to-emissions conversion model.
	Carbon CarbonModel
	// Now is injectable for tests. Defaults to time.Now.
	Now func() time.Time
}

// DefaultStoreConfig returns production-sane defaults.
func DefaultStoreConfig() StoreConfig {
	return StoreConfig{
		HistoryPoints: 720,
		LatencyWindow: 30 * time.Second,
		ErrorHalfLife: 20 * time.Second,
		Breaker:       DefaultBreakerConfig(),
		Carbon:        DefaultCarbonModel(),
		Now:           time.Now,
	}
}

// Store is the single source of truth for backend registration and every
// signal the router reads. Providers write into it; the router reads
// snapshots out of it.
type Store struct {
	cfg StoreConfig

	mu       sync.RWMutex
	backends map[string]*backendState
	order    []string
}

// NewStore returns an empty Store.
func NewStore(cfg StoreConfig) *Store {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HistoryPoints <= 0 {
		cfg.HistoryPoints = 720
	}
	if cfg.LatencyWindow <= 0 {
		cfg.LatencyWindow = 30 * time.Second
	}
	if cfg.ErrorHalfLife <= 0 {
		cfg.ErrorHalfLife = 20 * time.Second
	}
	if cfg.Carbon.EnergyKWhPerGB == 0 {
		cfg.Carbon = DefaultCarbonModel()
	}
	return &Store{cfg: cfg, backends: make(map[string]*backendState)}
}

// CarbonModel returns the store's bytes-to-emissions model.
func (s *Store) CarbonModel() CarbonModel { return s.cfg.Carbon }

func (s *Store) nowNanos() int64 { return s.cfg.Now().UnixNano() }

// Register adds or replaces a backend. Signal history and tracker state are
// preserved across re-registration so a config reload does not blank out the
// latency distributions the router depends on.
func (s *Store) Register(b model.Backend) {
	if b.Bias == 0 {
		b.Bias = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.backends[b.ID]; ok {
		existing.mu.Lock()
		existing.backend = b
		existing.mu.Unlock()
		return
	}

	st := &backendState{
		backend:    b,
		latency:    NewLatencyTracker(s.cfg.LatencyWindow, s.nowNanos),
		health:     NewHealthTracker(s.cfg.Breaker, s.cfg.ErrorHalfLife, s.cfg.Now),
		history:    make(map[model.Dimension]*Series, model.NumDimensions),
		rpsHist:    NewSeries(s.cfg.HistoryPoints),
		weightHist: NewSeries(s.cfg.HistoryPoints),
	}
	for d := model.Dimension(0); d < model.NumDimensions; d++ {
		st.history[d] = NewSeries(s.cfg.HistoryPoints)
	}
	s.backends[b.ID] = st
	s.order = append(s.order, b.ID)
	sort.Strings(s.order)
}

// Remove deregisters a backend and discards its state.
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backends, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Backends returns the registered backend definitions, sorted by ID.
func (s *Store) Backends() []model.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]model.Backend, 0, len(s.order))
	for _, id := range s.order {
		st := s.backends[id]
		st.mu.RLock()
		out = append(out, st.backend)
		st.mu.RUnlock()
	}
	return out
}

// Backend returns one backend definition.
func (s *Store) Backend(id string) (model.Backend, bool) {
	s.mu.RLock()
	st, ok := s.backends[id]
	s.mu.RUnlock()
	if !ok {
		return model.Backend{}, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.backend, true
}

func (s *Store) get(id string) *backendState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backends[id]
}

// SetPrice records an egress price quote in USD per GB.
func (s *Store) SetPrice(backendID string, q Quote) {
	if st := s.get(backendID); st != nil {
		st.mu.Lock()
		st.price = q
		st.mu.Unlock()
	}
}

// SetGridIntensity records a grid carbon-intensity quote in gCO2e per kWh.
func (s *Store) SetGridIntensity(backendID string, q Quote) {
	if st := s.get(backendID); st != nil {
		st.mu.Lock()
		st.grid = q
		st.mu.Unlock()
	}
}

// ObserveProbe records an out-of-band health-probe result. Probes keep the
// latency distribution fresh for backends currently receiving little or no
// traffic — without them, a backend the router has shed to zero weight can
// never earn its way back, because there are no live samples to prove it
// recovered.
func (s *Store) ObserveProbe(backendID string, rtt time.Duration, ok bool) {
	st := s.get(backendID)
	if st == nil {
		return
	}
	if ok {
		st.latency.Observe(rtt)
	}
	st.health.Observe(ok)
}

// ObserveRequest records a real data-plane request: its latency, outcome, and
// the bytes it moved. Cost and emissions are accrued here so the dashboard's
// spend figures come from bytes actually transferred rather than from a
// projection.
func (s *Store) ObserveRequest(backendID string, rtt time.Duration, ok bool, bytesOut int64) {
	st := s.get(backendID)
	if st == nil {
		return
	}
	st.latency.Observe(rtt)
	st.health.Observe(ok)
	st.requests.Add(1)
	if !ok {
		st.errors.Add(1)
	}
	if bytesOut > 0 {
		st.bytesOut.Add(bytesOut)
		gb := float64(bytesOut) / (1 << 30)

		st.mu.RLock()
		price := st.price.Value
		grid := st.grid.Value
		cloud := st.backend.Cloud
		st.mu.RUnlock()

		st.spentMicroUSD.Add(int64(gb * price * 1e6))
		st.gramsMilli.Add(int64(gb * s.cfg.Carbon.GramsPerGB(cloud, grid) * 1e3))
	}
}

// TrackInFlight increments the in-flight counter and returns a release func.
func (s *Store) TrackInFlight(backendID string) func() {
	st := s.get(backendID)
	if st == nil {
		return func() {}
	}
	st.health.inFlight.Add(1)
	var once sync.Once
	return func() { once.Do(func() { st.health.inFlight.Add(-1) }) }
}

// RecordSample appends the current signal values to the history series. The
// control loop calls this on a fixed cadence so the charts have an even time
// base regardless of request volume.
func (s *Store) RecordSample(weights map[string]float64, rps map[string]float64) {
	now := s.cfg.Now()
	snap := s.Snapshot()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, bs := range snap.Backends {
		st := s.backends[bs.Backend.ID]
		if st == nil {
			continue
		}
		v := bs.Vector()
		for d := model.Dimension(0); d < model.NumDimensions; d++ {
			st.history[d].Add(now, v[d])
		}
		st.weightHist.Add(now, weights[bs.Backend.ID])
		st.rpsHist.Add(now, rps[bs.Backend.ID])
	}
}

// History returns the retained series for one backend and dimension,
// downsampled to at most n points.
func (s *Store) History(backendID string, d model.Dimension, n int) []Point {
	st := s.get(backendID)
	if st == nil {
		return nil
	}
	ser, ok := st.history[d]
	if !ok {
		return nil
	}
	return ser.Downsample(n)
}

// WeightHistory returns the traffic-share series for one backend.
func (s *Store) WeightHistory(backendID string, n int) []Point {
	st := s.get(backendID)
	if st == nil {
		return nil
	}
	return st.weightHist.Downsample(n)
}

// RPSHistory returns the request-rate series for one backend.
func (s *Store) RPSHistory(backendID string, n int) []Point {
	st := s.get(backendID)
	if st == nil {
		return nil
	}
	return st.rpsHist.Downsample(n)
}

// Snapshot returns a coherent immutable view of every backend's signals.
func (s *Store) Snapshot() Snapshot {
	now := s.cfg.Now()
	s.mu.RLock()
	ids := make([]string, len(s.order))
	copy(ids, s.order)
	states := make([]*backendState, 0, len(ids))
	for _, id := range ids {
		states = append(states, s.backends[id])
	}
	s.mu.RUnlock()

	out := Snapshot{Taken: now, Carbon: s.cfg.Carbon, Backends: make([]BackendState, 0, len(states))}
	for _, st := range states {
		st.mu.RLock()
		b := st.backend
		price := st.price
		grid := st.grid
		st.mu.RUnlock()

		p50, p95 := st.latency.Values()
		errRate := st.health.ErrorRate()
		breaker := st.health.State()

		carbon := Quote{
			Value:  s.cfg.Carbon.GramsPerGB(b.Cloud, grid.Value),
			Source: "derived:" + grid.Source,
			AsOf:   grid.AsOf,
			TTL:    grid.TTL,
		}
		latQuote := Quote{Source: "probe+traffic", AsOf: now, TTL: 2 * s.cfg.LatencyWindow}

		bs := BackendState{
			Backend:       b,
			Egress:        price,
			GridIntensity: grid,
			CarbonPerGB:   carbon,
			LatencyP50:    Quote{Value: p50, Source: latQuote.Source, AsOf: now, TTL: latQuote.TTL},
			LatencyP95:    Quote{Value: p95, Source: latQuote.Source, AsOf: now, TTL: latQuote.TTL},
			ErrorRate:     Quote{Value: errRate, Source: "traffic", AsOf: now, TTL: latQuote.TTL},
			Breaker:       breaker,
			Healthy:       breaker.State != BreakerOpen && b.Enabled,
			InFlight:      st.health.inFlight.Load(),
			Requests:      st.requests.Load(),
			Errors:        st.errors.Load(),
			BytesOut:      st.bytesOut.Load(),
			SpentUSD:      float64(st.spentMicroUSD.Load()) / 1e6,
			EmittedGrams:  float64(st.gramsMilli.Load()) / 1e3,
		}
		if price.Stale(now) {
			bs.Stale = append(bs.Stale, "egress")
		}
		if grid.Stale(now) {
			bs.Stale = append(bs.Stale, "gridIntensity")
		}
		out.Backends = append(out.Backends, bs)
	}
	return out
}

// Totals aggregates spend and emissions across all backends.
func (s *Store) Totals() (usd, grams float64, bytes, requests, errors int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, st := range s.backends {
		usd += float64(st.spentMicroUSD.Load()) / 1e6
		grams += float64(st.gramsMilli.Load()) / 1e3
		bytes += st.bytesOut.Load()
		requests += st.requests.Load()
		errors += st.errors.Load()
	}
	return
}
