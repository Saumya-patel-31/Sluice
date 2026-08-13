package config

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/signals"
)

func TestStripComments(t *testing.T) {
	src := `{
  // a line comment
  "a": 1, // trailing
  /* a block
     comment */
  "b": "// not a comment",
  "c": "/* also not */",
  "d": "he said \"// hi\""
}`
	got := string(StripComments([]byte(src)))

	for _, want := range []string{`"a": 1`, `"b": "// not a comment"`, `"c": "/* also not */"`} {
		if !strings.Contains(got, want) {
			t.Errorf("stripping damaged a string literal; missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "a line comment") || strings.Contains(got, "a block") {
		t.Errorf("comments survived:\n%s", got)
	}
	// Offsets are preserved so encoding/json error positions still point at
	// the right line.
	if len(got) != len(src) {
		t.Errorf("length changed: %d then %d", len(src), len(got))
	}
	if strings.Count(got, "\n") != strings.Count(src, "\n") {
		t.Error("line count changed; reported error lines would be wrong")
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("the shipped default must validate: %v", err)
	}
	if len(cfg.Backends) != 10 {
		t.Errorf("want 10 demo backends, got %d", len(cfg.Backends))
	}

	// The topology only demonstrates anything if no single backend wins on
	// every objective.
	var cheapest, fastest string
	bestPrice, bestLatency := 1e9, 1e9
	for _, b := range cfg.Backends {
		p, _ := priceOf(b)
		if p < bestPrice {
			bestPrice, cheapest = p, b.ID
		}
		if l := simLatency(b); l < bestLatency {
			bestLatency, fastest = l, b.ID
		}
	}
	if cheapest == fastest {
		t.Errorf("%q is both cheapest and fastest, so the demo shows no tradeoff", cheapest)
	}
}

func priceOf(b model.Backend) (float64, bool) {
	return signals.ListPrice(b.Cloud, b.Region)
}

func simLatency(b model.Backend) float64 {
	v, err := strconv.ParseFloat(b.Label("sim.latencyMs"), 64)
	if err != nil || v <= 0 {
		return math.Inf(1)
	}
	return v
}

func TestNormalizeRejectsBadConfig(t *testing.T) {
	cases := map[string]func(*Config){
		"duplicate backend id": func(c *Config) {
			c.Backends = append(c.Backends, c.Backends[0])
		},
		"unknown cloud": func(c *Config) {
			c.Backends[0].Cloud = "digitalocean"
		},
		"backend has no id": func(c *Config) {
			c.Backends[0].ID = ""
		},
		"duplicate route id": func(c *Config) {
			c.Routes = append(c.Routes, c.Routes[0])
		},
		"unknown backend": func(c *Config) {
			c.Routes[0].BackendIDs = []string{"does-not-exist"}
		},
		// A configuration with no catch-all silently denies every path outside
		// the configured prefixes, which is the sort of failure that only
		// shows up in production.
		"no catch-all route": func(c *Config) {
			for i := range c.Routes {
				if c.Routes[i].PathPrefix == "/" {
					c.Routes[i].PathPrefix = "/somewhere"
				}
			}
		},
	}

	for name, mutate := range cases {
		cfg := Default()
		mutate(cfg)
		if err := cfg.Normalize(); err == nil {
			t.Errorf("%s: expected Normalize to reject this", name)
		}
	}
}

func TestNormalizeAppliesDefaults(t *testing.T) {
	cfg := &Config{
		Backends: []model.Backend{{ID: "b1", Cloud: model.CloudAWS, Region: "us-east-1", Enabled: true}},
		Routes:   []model.Route{{ID: "default", PathPrefix: "/"}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	// Loopback, not every interface: the default posture of a component that
	// can rewrite authorisation policy should be "not reachable".
	if cfg.Listen.API != "127.0.0.1:8080" {
		t.Errorf("listen = %q, want a loopback default", cfg.Listen.API)
	}
	if cfg.Backends[0].Bias != 1 {
		t.Errorf("bias should default to neutral, got %v", cfg.Backends[0].Bias)
	}
	if cfg.Backends[0].Tier != "primary" {
		t.Errorf("tier = %q", cfg.Backends[0].Tier)
	}
	if cfg.Backends[0].DisplayName == "" {
		t.Error("display name should be derived")
	}
	if cfg.Routes[0].Objectives.Sum() == 0 {
		t.Error("route objectives should fall back to the router defaults")
	}
	if cfg.Routes[0].Temperature <= 0 {
		t.Error("temperature should be defaulted")
	}
}

// The control-plane API can replace the document that authorises every
// request in the fleet. A configuration that would expose that to a network
// without a credential must not be startable by accident.
func TestAPIPostureFailsClosed(t *testing.T) {
	exposed := func() *Config {
		c := Default()
		c.Listen.API = "0.0.0.0:8080"
		return c
	}

	if err := exposed().Normalize(); err == nil {
		t.Fatal("a network-reachable API with no token must refuse to start")
	} else if !strings.Contains(err.Error(), "API_TOKEN") {
		t.Errorf("the error should say how to fix it, got: %v", err)
	}

	withToken := exposed()
	withToken.API.Token = "secret"
	if err := withToken.Normalize(); err != nil {
		t.Errorf("a token should satisfy the check: %v", err)
	}

	// The unsafe configuration has to be spelled out, not reached by omission.
	explicit := exposed()
	explicit.API.AllowAnonymousMutations = true
	if err := explicit.Normalize(); err != nil {
		t.Errorf("an explicit opt-in should be accepted: %v", err)
	}

	// Loopback needs no token: there is no boundary to cross.
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		c := Default()
		c.Listen.API = addr
		if err := c.Normalize(); err != nil {
			t.Errorf("%s should not require a token: %v", addr, err)
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"127.0.0.1":      true,
		"0.0.0.0:8080":   false,
		":8080":          false,
		"[::]:8080":      false,
		"10.0.0.5:8080":  false,
		"":               false,
		// An unresolvable hostname must read as routable. Guessing "loopback"
		// here would silently open the write API.
		"sluice.internal:8080": false,
	}
	for addr, want := range cases {
		if got := LoopbackOnly(addr); got != want {
			t.Errorf("LoopbackOnly(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	env := map[string]string{
		"SLUICE_LISTEN_API":                 "0.0.0.0:9000",
		"SLUICE_API_TOKEN":                  "from-env",
		"SLUICE_ELECTRICITY_MAPS_TOKEN":     "grid-token",
		"SLUICE_PRICING_LIVE":               "true",
		"SLUICE_CARBON_ENERGY_KWH_PER_GB":   "0.03",
		"SLUICE_TLS_TRUST_DOMAINS":          "prod.internal, staging.internal",
		"SLUICE_API_REQUIRE_AUTH_FOR_READS": "1",
	}
	cfg := Default()
	if err := cfg.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}

	if cfg.Listen.API != "0.0.0.0:9000" || cfg.API.Token != "from-env" {
		t.Errorf("listen=%q token=%q", cfg.Listen.API, cfg.API.Token)
	}
	if cfg.Carbon.ElectricityMapsToken != "grid-token" {
		t.Error("the grid token was ignored — the k8s manifest sets this and it must take effect")
	}
	if !cfg.Pricing.Live || !cfg.API.RequireAuthForReads {
		t.Error("boolean overrides not applied")
	}
	if cfg.Carbon.EnergyKWhPerGB != 0.03 {
		t.Errorf("energy intensity = %v", cfg.Carbon.EnergyKWhPerGB)
	}
	if len(cfg.TLS.TrustDomains) != 2 || cfg.TLS.TrustDomains[1] != "staging.internal" {
		t.Errorf("trust domains = %v", cfg.TLS.TrustDomains)
	}

	// A malformed value must fail loudly rather than silently keeping the
	// default, which is how a "live pricing" flag quietly stays off.
	bad := Default()
	err := bad.ApplyEnv(func(k string) string {
		if k == "SLUICE_PRICING_LIVE" {
			return "yes-please"
		}
		return ""
	})
	if err == nil {
		t.Error("a malformed boolean should be rejected")
	}
}

// Secrets belong in a mounted file rather than the process environment, where
// /proc/<pid>/environ and every crash dump can read them.
func TestApplyEnvReadsSecretFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	err := cfg.ApplyEnv(func(k string) string {
		switch k {
		case "SLUICE_API_TOKEN_FILE":
			return path
		case "SLUICE_API_TOKEN":
			return "env-token"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.API.Token != "file-token" {
		t.Errorf("the _FILE form should win and be trimmed, got %q", cfg.API.Token)
	}

	missing := Default()
	if err := missing.ApplyEnv(func(k string) string {
		if k == "SLUICE_API_TOKEN_FILE" {
			return filepath.Join(dir, "absent")
		}
		return ""
	}); err == nil {
		t.Error("an unreadable secret file must be an error, not an empty token")
	}
}

// --print-config is what an operator pastes into a ticket.
func TestRenderRedactsSecrets(t *testing.T) {
	cfg := Default()
	cfg.API.Token = "super-secret"
	cfg.Carbon.ElectricityMapsToken = "grid-secret"

	var buf strings.Builder
	if err := cfg.Render(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "super-secret") || strings.Contains(out, "grid-secret") {
		t.Fatal("--print-config leaked a credential")
	}
	if !strings.Contains(out, "<redacted>") {
		t.Error("expected the redaction marker so the reader knows a value exists")
	}
	// The original must not be mutated by printing it.
	if cfg.API.Token != "super-secret" {
		t.Error("Render mutated the live configuration")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sluice.jsonc")

	// A misspelled key silently keeping the default is exactly the failure
	// that only shows up on the bill, so it must be an error.
	body := `{
  // note the typo
  "rooter": { "temperature": 0.5 }
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an unknown-field error")
	} else if !strings.Contains(err.Error(), "rooter") {
		t.Errorf("the error should name the offending key: %v", err)
	}
}

func TestLoadAcceptsCommentedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sluice.jsonc")
	body := `{
  /* Routing configuration is full of choices whose reasoning matters. */
  "listen": { "api": ":9999" },
  "backends": [
    { "id": "b1", "cloud": "aws", "region": "us-east-1", "enabled": true } // one region
  ],
  "routes": [
    { "id": "default", "pathPrefix": "/" }
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen.API != ":9999" {
		t.Errorf("listen = %q", cfg.Listen.API)
	}
	if len(cfg.Backends) != 1 {
		t.Errorf("backends = %d", len(cfg.Backends))
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.jsonc")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestDurationHelpers(t *testing.T) {
	cfg := Default()
	if got := cfg.Router.ControlInterval().Milliseconds(); got != 1000 {
		t.Errorf("control interval = %dms", got)
	}
	if got := cfg.Probe.Timeout().Milliseconds(); got != 2000 {
		t.Errorf("probe timeout = %dms", got)
	}
	// Zero means "use the default", not "zero".
	empty := PolicyConfig{}
	if empty.CacheTTL() <= 0 {
		t.Error("an unset cache TTL should fall back to a sane default")
	}
}
