package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
)

// Config is the complete runtime configuration.
type Config struct {
	// Listen holds the bind addresses for each surface. They are separate so
	// the control API and the data path can be exposed on different networks
	// — the dashboard on an internal interface, the proxy on the edge.
	Listen ListenConfig `json:"listen"`

	Backends []model.Backend `json:"backends"`
	Routes   []model.Route   `json:"routes"`

	API     APIConfig     `json:"api"`
	Policy  PolicyConfig  `json:"policy"`
	Pricing PricingConfig `json:"pricing"`
	Carbon  CarbonConfig  `json:"carbon"`
	Router  RouterConfig  `json:"router"`
	Probe   ProbeConfig   `json:"probe"`
	Ledger  LedgerConfig  `json:"ledger"`
	TLS     TLSConfig     `json:"tls"`
	Demo    DemoConfig    `json:"demo"`
}

// ListenConfig holds bind addresses.
type ListenConfig struct {
	// API serves the dashboard, the REST API and /metrics.
	API string `json:"api"`
	// Proxy is the native data-plane listener. Empty disables it.
	Proxy string `json:"proxy"`
	// Authz serves the Envoy ext_authz HTTP endpoint. Empty disables it.
	Authz string `json:"authz"`
}

// APIConfig controls access to the control-plane API.
//
// This surface can replace the entire authorisation policy with one request.
// An unauthenticated write API on a component whose job is zero-trust
// authorisation is not a rough edge, it is the whole product defeated, so the
// posture here is fail-at-deploy rather than fail-at-exploit: a configuration
// that would expose writes to a network refuses to start.
type APIConfig struct {
	// Token is required in an Authorization: Bearer header (or X-Sluice-Token)
	// on every mutating request. Supply it through SLUICE_API_TOKEN or
	// SLUICE_API_TOKEN_FILE rather than in a committed file.
	Token string `json:"token,omitempty"`
	// RequireAuthForReads extends the token requirement to read endpoints.
	// /healthz and /readyz are always exempt so a kubelet can probe them.
	RequireAuthForReads bool `json:"requireAuthForReads"`
	// AllowAnonymousMutations is the explicit escape hatch for a demo bound to
	// a routable address. It exists so the unsafe configuration has to be
	// spelled out rather than reached by forgetting something.
	AllowAnonymousMutations bool `json:"allowAnonymousMutations"`
	// MaxEventStreams caps concurrent dashboard subscribers. Each holds a
	// goroutine and a buffered channel for the life of the connection, and
	// reads need no credential, so an unbounded count is a
	// resource-exhaustion path open to anyone who can reach the port.
	MaxEventStreams int `json:"maxEventStreams"`
}

// PolicyConfig points at the policy document.
type PolicyConfig struct {
	// File is a path to a .sluice policy document. Empty uses the built-in
	// default set.
	File string `json:"file"`
	// CacheSize and CacheTTLSeconds bound the authorisation cache.
	CacheSize       int     `json:"cacheSize"`
	CacheTTLSeconds float64 `json:"cacheTtlSeconds"`
	// Watch reloads the file when it changes on disk.
	Watch bool `json:"watch"`
}

// PricingConfig controls egress price resolution.
type PricingConfig struct {
	// Live enables provider pricing APIs. Only Azure's is reachable without
	// credentials; the others fall back to the bundled list prices.
	Live bool `json:"live"`
	// Overrides is keyed by backend ID, in USD per GB. This is where a
	// negotiated rate or committed-use discount goes.
	Overrides map[string]float64 `json:"overrides"`
	// RefreshSeconds is the price refresh interval.
	RefreshSeconds float64 `json:"refreshSeconds"`
}

// CarbonConfig controls grid-intensity resolution and the emissions model.
type CarbonConfig struct {
	// ElectricityMapsToken enables live grid readings. Without it Sluice uses
	// its bundled zone dataset shaped by a diurnal model.
	ElectricityMapsToken string `json:"electricityMapsToken"`
	// EnergyKWhPerGB is the network energy intensity assumption. Published
	// estimates range roughly 0.004-0.06; the default is 0.015.
	EnergyKWhPerGB float64 `json:"energyKwhPerGb"`
	// PUE overrides the per-provider datacentre overhead multipliers.
	PUE map[string]float64 `json:"pue"`
	// RefreshSeconds is the grid-intensity refresh interval.
	RefreshSeconds float64 `json:"refreshSeconds"`
}

// RouterConfig tunes the allocation pipeline.
type RouterConfig struct {
	// ControlIntervalMs is how often traffic weights are recomputed.
	ControlIntervalMs float64 `json:"controlIntervalMs"`
	// Temperature controls how sharply score differences become traffic
	// share. Small values approach winner-take-all.
	Temperature float64 `json:"temperature"`
	// Smoothing is the fraction of each newly computed target folded in per
	// cycle, in (0,1]. Lower values react more slowly and churn less.
	Smoothing float64 `json:"smoothing"`
	// Deadband is how far the distribution must move before it is pushed to
	// the data plane.
	Deadband float64 `json:"deadband"`
	// MinWeight zeroes shares below it.
	MinWeight float64 `json:"minWeight"`
	// ExplorationFloor guarantees each eligible backend a minimum share so
	// its signals stay fresh.
	ExplorationFloor float64 `json:"explorationFloor"`
	// DefaultObjectives applies to routes that do not set their own.
	DefaultObjectives model.Vector `json:"defaultObjectives"`
	// DefaultRequestBytes is the assumed response size when a caller does
	// not declare one.
	DefaultRequestBytes int64 `json:"defaultRequestBytes"`
}

// ProbeConfig tunes active health probing.
type ProbeConfig struct {
	IntervalMs float64 `json:"intervalMs"`
	TimeoutMs  float64 `json:"timeoutMs"`
	Path       string  `json:"path"`
	// InsecureSkipVerify disables upstream certificate verification. Intended
	// for local demos with self-signed certificates.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`
}

// LedgerConfig bounds decision retention.
type LedgerConfig struct {
	// Capacity is how many decisions are retained with full explainability.
	Capacity int `json:"capacity"`
	// RollupPoints is how many aggregate samples the charts retain.
	RollupPoints int `json:"rollupPoints"`
}

// TLSConfig configures the data plane's mutual TLS.
type TLSConfig struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// ClientCAFile enables mTLS: peers must present a certificate chaining to
	// this CA, and their URI SAN becomes the request's SPIFFE identity.
	ClientCAFile string `json:"clientCaFile"`
	// TrustDomain restricts which SPIFFE trust domains are accepted.
	TrustDomains []string `json:"trustDomains"`
}

// DemoConfig controls the built-in simulator.
type DemoConfig struct {
	// Enabled starts synthetic upstreams and a traffic generator so the
	// system is alive on first run with no external dependencies.
	Enabled bool `json:"enabled"`
	// RPS is the aggregate synthetic request rate.
	RPS float64 `json:"rps"`
	// Seed makes a run reproducible. Zero picks a random seed.
	Seed uint64 `json:"seed"`
	// AutoIncidents periodically injects brownouts and price spikes.
	AutoIncidents bool `json:"autoIncidents"`
}

// Durations derived from the numeric fields.
func (p PolicyConfig) CacheTTL() time.Duration {
	return secondsToDuration(p.CacheTTLSeconds, 5*time.Second)
}

// RefreshInterval returns the price refresh period.
func (p PricingConfig) RefreshInterval() time.Duration {
	return secondsToDuration(p.RefreshSeconds, 15*time.Minute)
}

// RefreshInterval returns the carbon refresh period.
func (c CarbonConfig) RefreshInterval() time.Duration {
	return secondsToDuration(c.RefreshSeconds, time.Minute)
}

// ControlInterval returns the control-loop period.
func (r RouterConfig) ControlInterval() time.Duration {
	return millisToDuration(r.ControlIntervalMs, time.Second)
}

// Interval returns the probe period.
func (p ProbeConfig) Interval() time.Duration { return millisToDuration(p.IntervalMs, 2*time.Second) }

// Timeout returns the probe timeout.
func (p ProbeConfig) Timeout() time.Duration { return millisToDuration(p.TimeoutMs, 2*time.Second) }

func secondsToDuration(v float64, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return time.Duration(v * float64(time.Second))
}

func millisToDuration(v float64, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return time.Duration(v * float64(time.Millisecond))
}

// -----------------------------------------------------------------------------
// Loading
// -----------------------------------------------------------------------------

// Load reads a JSONC configuration file, overlays the environment, and applies
// defaults. It does not call Normalize — the caller applies flag overrides
// first, since flags have the highest precedence.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg := Default()
	dec := json.NewDecoder(strings.NewReader(string(StripComments(raw))))
	// Unknown fields are an error rather than a shrug: a misspelled key in a
	// routing configuration silently keeping the default is exactly the kind
	// of failure that only shows up on the bill.
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	if err := cfg.ApplyEnv(os.Getenv); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoopbackOnly reports whether a listen address is reachable only from the
// local host.
//
// An empty or wildcard host means every interface, which is what ":8080" and
// "0.0.0.0:8080" both mean and what a container almost always wants — so those
// are treated as exposed, not as unknown.
func LoopbackOnly(addr string) bool {
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "0.0.0.0", "::", "*":
		return false
	case "localhost":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname we cannot classify. Assume it is routable; guessing
		// "loopback" here would silently open the write API.
		return false
	}
	return ip.IsLoopback()
}

// Normalize applies defaults and validates the configuration.
func (c *Config) Normalize() error {
	if c.Listen.API == "" {
		// Loopback rather than every interface. A control plane that binds
		// 0.0.0.0 by default is one nmap away from an open policy editor, and
		// the deployments that genuinely need a routable bind (containers) all
		// set it explicitly anyway.
		c.Listen.API = "127.0.0.1:8080"
	}
	if c.Ledger.Capacity <= 0 {
		c.Ledger.Capacity = 2000
	}
	if c.Ledger.RollupPoints <= 0 {
		c.Ledger.RollupPoints = 600
	}
	if c.Policy.CacheSize <= 0 {
		c.Policy.CacheSize = 8192
	}
	if c.API.MaxEventStreams <= 0 {
		c.API.MaxEventStreams = 64
	}
	if c.Probe.Path == "" {
		c.Probe.Path = "/healthz"
	}
	if c.Carbon.EnergyKWhPerGB <= 0 {
		c.Carbon.EnergyKWhPerGB = 0.015
	}
	if c.Router.Temperature <= 0 {
		c.Router.Temperature = 0.12
	}
	if c.Router.Smoothing <= 0 || c.Router.Smoothing > 1 {
		c.Router.Smoothing = 0.35
	}
	if c.Router.Deadband < 0 {
		c.Router.Deadband = 0.04
	}
	if c.Router.DefaultRequestBytes <= 0 {
		c.Router.DefaultRequestBytes = 64 << 10
	}
	if c.Router.DefaultObjectives.Sum() == 0 {
		c.Router.DefaultObjectives = model.Vector{0.35, 0.35, 0.20, 0.10}
	}

	seen := make(map[string]bool, len(c.Backends))
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.ID == "" {
			return fmt.Errorf("config: backend %d has no id", i)
		}
		if seen[b.ID] {
			return fmt.Errorf("config: duplicate backend id %q", b.ID)
		}
		seen[b.ID] = true
		if !b.Cloud.Valid() {
			return fmt.Errorf("config: backend %q has unknown cloud %q", b.ID, b.Cloud)
		}
		if b.Bias == 0 {
			b.Bias = 1
		}
		if b.DisplayName == "" {
			b.DisplayName = b.Cloud.Display() + " " + b.Region
		}
		if b.Tier == "" {
			b.Tier = "primary"
		}
	}

	if len(c.Routes) == 0 {
		return fmt.Errorf("config: at least one route is required")
	}
	routeIDs := make(map[string]bool, len(c.Routes))
	hasCatchAll := false
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.ID == "" {
			return fmt.Errorf("config: route %d has no id", i)
		}
		if routeIDs[r.ID] {
			return fmt.Errorf("config: duplicate route id %q", r.ID)
		}
		routeIDs[r.ID] = true
		if r.PathPrefix == "" || r.PathPrefix == "/" {
			hasCatchAll = true
		}
		if r.Objectives.Sum() == 0 {
			r.Objectives = c.Router.DefaultObjectives
		}
		if r.Temperature <= 0 {
			r.Temperature = c.Router.Temperature
		}
		if r.DisplayName == "" {
			r.DisplayName = r.ID
		}
		for _, id := range r.BackendIDs {
			if !seen[id] {
				return fmt.Errorf("config: route %q references unknown backend %q", r.ID, id)
			}
		}
	}
	if !hasCatchAll {
		return fmt.Errorf("config: no route matches \"/\"; requests outside every prefix would be denied")
	}

	return c.validateAPIPosture()
}

// validateAPIPosture refuses to start a configuration that would expose an
// unauthenticated write API to a network.
//
// Refusing at startup rather than returning 403 at request time is deliberate.
// A 403 is discovered by whoever probes for it; a failed rollout is discovered
// by the person deploying, while they are still holding the context to fix it.
func (c *Config) validateAPIPosture() error {
	if c.API.Token != "" || c.API.AllowAnonymousMutations {
		return nil
	}
	if LoopbackOnly(c.Listen.API) {
		return nil
	}
	return fmt.Errorf(
		"config: the API is bound to %s, which is reachable from the network, but no api.token is set.\n"+
			"        Anyone who can reach that address could replace the policy document.\n"+
			"        Set %sAPI_TOKEN (or %sAPI_TOKEN_FILE), bind the API to loopback,\n"+
			"        or pass --dev-insecure to accept an unauthenticated write API.",
		c.Listen.API, EnvPrefix, EnvPrefix)
}

// MutationsRequireToken reports whether writes will be gated by a token.
func (c *Config) MutationsRequireToken() bool { return c.API.Token != "" }

// Render writes the effective configuration as indented JSON.
//
// Named Render rather than WriteTo so it does not half-implement io.WriterTo,
// whose signature returns a byte count: a mismatch go vet flags, and one that
// would silently break anything type-asserting for that interface.
//
// The API token is redacted. `--print-config` is the command an operator runs
// when a deployment is misbehaving, usually into a ticket or a chat window,
// and a credential that leaks that way leaks permanently.
func (c *Config) Render(w io.Writer) error {
	redacted := *c
	if redacted.API.Token != "" {
		redacted.API.Token = "<redacted>"
	}
	if redacted.Carbon.ElectricityMapsToken != "" {
		redacted.Carbon.ElectricityMapsToken = "<redacted>"
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Without this, angle brackets and ampersands come out as < escapes.
	// This output is meant to be read and diffed by a person, not embedded in
	// a page.
	enc.SetEscapeHTML(false)
	return enc.Encode(&redacted)
}

// Save writes the configuration to a file, with secrets redacted.
func (c *Config) Save(path string) error {
	var buf bytes.Buffer
	if err := c.Render(&buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
