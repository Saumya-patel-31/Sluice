// Package app wires Sluice's components into a running control plane.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/config"
	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/policy"
	"github.com/Saumya-patel-31/sluice/internal/router"
	"github.com/Saumya-patel-31/sluice/internal/signals"
	"github.com/Saumya-patel-31/sluice/internal/sim"
	"github.com/Saumya-patel-31/sluice/internal/telemetry"
)

// App is a fully wired control plane.
type App struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Version string

	Store     *signals.Store
	Engine    *router.Engine
	Ledger    *telemetry.Ledger
	Registry  *telemetry.Registry
	Rollup    *telemetry.Rollup
	Collector *Collector

	Pricing *signals.PricingService
	Carbon  *signals.CarbonService
	Prober  *signals.Prober

	// Fleet and Generator are non-nil only in demo mode.
	Fleet     *sim.Fleet
	Generator *sim.Generator

	StartedAt time.Time

	mu           sync.RWMutex
	policyPath   string
	policyMod    time.Time
	lastSummary  telemetry.Summary
	lastSampleAt time.Time

	closers []func() error
}

// New builds the control plane from configuration. Nothing is started yet.
func New(cfg *config.Config, log *slog.Logger, version string) (*App, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}

	carbonModel := signals.DefaultCarbonModel()
	if cfg.Carbon.EnergyKWhPerGB > 0 {
		carbonModel.EnergyKWhPerGB = cfg.Carbon.EnergyKWhPerGB
	}
	for cloud, pue := range cfg.Carbon.PUE {
		carbonModel.PUE[model.Cloud(cloud)] = pue
	}

	storeCfg := signals.DefaultStoreConfig()
	storeCfg.Carbon = carbonModel
	storeCfg.HistoryPoints = cfg.Ledger.RollupPoints

	a := &App{
		Cfg:       cfg,
		Log:       log,
		Version:   version,
		Store:     signals.NewStore(storeCfg),
		Ledger:    telemetry.NewLedger(cfg.Ledger.Capacity),
		Registry:  telemetry.NewRegistry(),
		Rollup:    telemetry.NewRollup(cfg.Ledger.RollupPoints),
		StartedAt: time.Now(),
	}
	a.Collector = NewCollector(a.Registry, version)

	backends := cfg.Backends

	// In demo mode the synthetic fleet is created first, because each backend's
	// address becomes the ephemeral port its upstream landed on. Everything
	// downstream — probes, the proxy, the traffic generator — then talks to
	// real sockets rather than to a mock.
	if cfg.Demo.Enabled {
		fleet, err := sim.NewFleet(backends, log)
		if err != nil {
			return nil, err
		}
		a.Fleet = fleet
		fleet.Start()
		a.closers = append(a.closers, fleet.Close)

		for i := range backends {
			backends[i].Address = fleet.Addr(backends[i].ID)
		}
		log.Info("demo fleet started", "upstreams", len(backends))
	}

	for _, b := range backends {
		a.Store.Register(b)
	}

	engineCfg := router.DefaultConfig()
	engineCfg.ControlInterval = cfg.Router.ControlInterval()
	engineCfg.PolicyCacheSize = cfg.Policy.CacheSize
	engineCfg.PolicyCacheTTL = cfg.Policy.CacheTTL()
	engineCfg.DefaultRequestBytes = cfg.Router.DefaultRequestBytes
	engineCfg.DefaultObjectives = cfg.Router.DefaultObjectives
	engineCfg.Plan = router.PlanConfig{
		Temperature:      cfg.Router.Temperature,
		Smoothing:        cfg.Router.Smoothing,
		Deadband:         cfg.Router.Deadband,
		MinWeight:        cfg.Router.MinWeight,
		ExplorationFloor: cfg.Router.ExplorationFloor,
	}

	a.Engine = router.NewEngine(a.Store, engineCfg, log)
	a.Engine.AddSink(a.Ledger)
	a.Engine.AddSink(a.Collector)

	for _, r := range cfg.Routes {
		a.Engine.UpsertRoute(r)
	}

	if err := a.loadPolicy(cfg.Policy.File); err != nil {
		return nil, err
	}

	// Signal sources. In demo mode the bundled list prices and the modeled
	// carbon curve are wrapped so they drift and respond to injected
	// incidents; otherwise they are used directly.
	var basePricer signals.Pricer = &signals.StaticPricer{Overrides: cfg.Pricing.Overrides}
	pricing := signals.NewPricingService(a.Store, log, cfg.Pricing.Overrides, cfg.Pricing.Live)
	pricing.Interval = cfg.Pricing.RefreshInterval()
	if a.Fleet != nil {
		pricing.Providers = []signals.Pricer{&sim.DriftingPricer{Fleet: a.Fleet, Base: basePricer}}
		pricing.Interval = 2 * time.Second
	}
	a.Pricing = pricing

	carbon := signals.NewCarbonService(a.Store, log, cfg.Carbon.ElectricityMapsToken, time.Now)
	carbon.Interval = cfg.Carbon.RefreshInterval()
	if a.Fleet != nil {
		carbon.Sources = []signals.CarbonSource{
			&sim.DriftingCarbon{Fleet: a.Fleet, Base: &signals.ModeledCarbon{}},
		}
		carbon.Interval = 2 * time.Second
	}
	a.Carbon = carbon

	prober := signals.NewProber(a.Store, log)
	prober.Interval = cfg.Probe.Interval()
	prober.Timeout = cfg.Probe.Timeout()
	prober.Path = cfg.Probe.Path
	prober.InsecureSkipVerify = cfg.Probe.InsecureSkipVerify
	a.Prober = prober

	if a.Fleet != nil {
		a.Generator = &sim.Generator{
			Engine: a.Engine,
			Fleet:  a.Fleet,
			RPS:    cfg.Demo.RPS,
			Log:    log,
		}
	}

	return a, nil
}

// Start launches every background loop. It returns immediately; the loops run
// until ctx is cancelled.
func (a *App) Start(ctx context.Context) {
	go a.Pricing.Run(ctx)
	go a.Carbon.Run(ctx)
	go a.Prober.Run(ctx)
	go a.controlLoop(ctx)

	if a.Cfg.Policy.Watch && a.policyPath != "" {
		go a.watchPolicy(ctx)
	}
	if a.Generator != nil {
		go a.Generator.Run(ctx)
		if a.Cfg.Demo.AutoIncidents {
			go sim.AutoIncidents(ctx, a.Fleet, a.Cfg.Backends, a.Log)
		}
	}
}

// Close releases resources owned by the app.
func (a *App) Close() error {
	var firstErr error
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// controlLoop recomputes traffic distributions and refreshes derived
// telemetry. It runs the recompute itself rather than calling Engine.Run so
// that the loop's own cost is measured and the rollup samples land on exactly
// the same cadence as the plans they describe.
func (a *App) controlLoop(ctx context.Context) {
	interval := a.Cfg.Router.ControlInterval()
	t := time.NewTicker(interval)
	defer t.Stop()

	a.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.tick()
		}
	}
}

func (a *App) tick() {
	start := time.Now()
	a.Engine.Recompute()
	elapsed := time.Since(start)

	snap := a.Engine.Snapshot()
	a.Collector.ObserveState(a.Engine, snap, elapsed)
	a.sampleRollup(snap, start)
}

// sampleRollup records the fleet-level aggregate series behind the dashboard's
// streaming charts.
func (a *App) sampleRollup(snap signals.Snapshot, now time.Time) {
	byBackend := a.Engine.BackendRPS()
	cloudRPS := map[string]float64{}
	var totalRPS float64
	for _, b := range snap.Backends {
		r := byBackend[b.Backend.ID]
		cloudRPS["rps."+string(b.Backend.Cloud)] += r
		totalRPS += r
	}
	cloudRPS["rps.total"] = totalRPS
	a.Rollup.ObserveMany(now, cloudRPS)

	summary := a.Ledger.Summary()

	a.mu.Lock()
	prev, prevAt := a.lastSummary, a.lastSampleAt
	a.lastSummary, a.lastSampleAt = summary, now
	a.mu.Unlock()

	if prevAt.IsZero() {
		return
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return
	}

	// Rates rather than totals: a cumulative savings line only ever goes up
	// and says nothing about whether the router is working right now. The
	// run-rate is what an operator actually watches.
	perHour := 3600 / elapsed
	a.Rollup.Observe("savings.usdPerHour", now, (summary.SavedUSD-prev.SavedUSD)*perHour)
	a.Rollup.Observe("savings.gramsPerHour", now, (summary.SavedGrams-prev.SavedGrams)*perHour)

	decisions := float64(summary.Total - prev.Total)
	denials := float64((summary.ByVerdict["deny"] + summary.ByVerdict["no_capacity"]) -
		(prev.ByVerdict["deny"] + prev.ByVerdict["no_capacity"]))
	if decisions > 0 {
		a.Rollup.Observe("deny.rate", now, denials/decisions)
	} else {
		a.Rollup.Observe("deny.rate", now, 0)
	}

	for routeID, plan := range a.Engine.Plans() {
		a.Rollup.Observe("p95."+routeID, now, plan.ProjectedP95)
	}
}

// -----------------------------------------------------------------------------
// Policy management
// -----------------------------------------------------------------------------

// loadPolicy compiles a policy document from disk, or the built-in default
// when no path is configured.
func (a *App) loadPolicy(path string) error {
	if path == "" {
		a.Engine.SetPolicy(policy.MustCompileDefault())
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	set, err := policy.Compile(string(src))
	if err != nil {
		return err
	}
	a.Engine.SetPolicy(set)

	a.mu.Lock()
	a.policyPath = path
	if st, err := os.Stat(path); err == nil {
		a.policyMod = st.ModTime()
	}
	a.mu.Unlock()
	return nil
}

// SetPolicySource compiles and installs a policy document supplied at runtime,
// persisting it when the configuration points at a file.
//
// The compile happens before anything is installed, so a document with a
// syntax error is rejected with its line number and the running policy set is
// left untouched. An operator cannot take authorisation down with a typo.
func (a *App) SetPolicySource(src string) error {
	set, err := policy.Compile(src)
	if err != nil {
		return err
	}
	a.Engine.SetPolicy(set)

	a.mu.RLock()
	path := a.policyPath
	a.mu.RUnlock()

	if path != "" {
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			return fmt.Errorf("policy: installed but not persisted: %w", err)
		}
		a.mu.Lock()
		if st, err := os.Stat(path); err == nil {
			a.policyMod = st.ModTime()
		}
		a.mu.Unlock()
	}
	return nil
}

// PolicyPath returns the configured policy file, or "" when using the default.
func (a *App) PolicyPath() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.policyPath
}

// watchPolicy reloads the policy file when its modification time changes.
//
// Polling rather than filesystem notifications: this needs to work identically
// on a developer laptop and inside a container reading a Kubernetes ConfigMap
// mount, and ConfigMap updates arrive as an atomic symlink swap that several
// notification APIs report inconsistently. A two-second poll is reliable
// everywhere and costs nothing.
func (a *App) watchPolicy(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.RLock()
			path, known := a.policyPath, a.policyMod
			a.mu.RUnlock()
			if path == "" {
				continue
			}
			st, err := os.Stat(path)
			if err != nil || !st.ModTime().After(known) {
				continue
			}
			if err := a.loadPolicy(path); err != nil {
				// Keep serving the previous set. A broken file on disk must
				// not be able to disarm the policy engine.
				a.Log.Error("policy reload failed; keeping the previous set",
					"path", path, "err", err)
				a.mu.Lock()
				a.policyMod = st.ModTime()
				a.mu.Unlock()
				continue
			}
			a.Log.Info("policy reloaded from disk", "path", path)
		}
	}
}

// Uptime returns how long the control plane has been running.
func (a *App) Uptime() time.Duration { return time.Since(a.StartedAt) }
