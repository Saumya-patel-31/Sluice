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

	if cfg.Listen.API != ":8080" {
		t.Errorf("listen = %q", cfg.Listen.API)
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
