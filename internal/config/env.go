package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvPrefix namespaces every environment variable Sluice reads.
const EnvPrefix = "SLUICE_"

// ApplyEnv overlays environment variables on top of a loaded configuration.
//
// Precedence is file < environment < command-line flags. Secrets belong in the
// environment rather than in a file that gets committed, and listen addresses
// belong there because the same image runs in a container binding 0.0.0.0 and
// on a laptop binding loopback.
//
// Every secret also accepts a `_FILE` suffix — SLUICE_API_TOKEN_FILE — which
// reads the value from a path instead. That is the convention Docker secrets
// and Kubernetes projected volumes use, and it keeps credentials out of the
// process environment where `/proc/<pid>/environ` and every crash dump can
// see them.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	if getenv == nil {
		getenv = os.Getenv
	}

	str := func(key string) (string, bool) {
		if v := getenv(EnvPrefix + key); v != "" {
			return v, true
		}
		return "", false
	}

	// secret resolves KEY or KEY_FILE, preferring the file form.
	secret := func(key string) (string, bool, error) {
		if path := getenv(EnvPrefix + key + "_FILE"); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", false, fmt.Errorf("config: %s%s_FILE: %w", EnvPrefix, key, err)
			}
			return strings.TrimSpace(string(b)), true, nil
		}
		v, ok := str(key)
		return v, ok, nil
	}

	boolean := func(key string, dst *bool) error {
		v, ok := str(key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s%s must be a boolean, got %q", EnvPrefix, key, v)
		}
		*dst = b
		return nil
	}

	number := func(key string, dst *float64) error {
		v, ok := str(key)
		if !ok {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: %s%s must be a number, got %q", EnvPrefix, key, v)
		}
		*dst = f
		return nil
	}

	// Listen addresses.
	if v, ok := str("LISTEN_API"); ok {
		c.Listen.API = v
	}
	if v, ok := str("LISTEN_AUTHZ"); ok {
		c.Listen.Authz = v
	}
	if v, ok := str("LISTEN_PROXY"); ok {
		c.Listen.Proxy = v
	}

	// Secrets.
	tok, ok, err := secret("API_TOKEN")
	if err != nil {
		return err
	}
	if ok {
		c.API.Token = tok
	}

	emToken, ok, err := secret("ELECTRICITY_MAPS_TOKEN")
	if err != nil {
		return err
	}
	if ok {
		c.Carbon.ElectricityMapsToken = emToken
	}

	// Paths.
	if v, ok := str("POLICY_FILE"); ok {
		c.Policy.File = v
	}
	if v, ok := str("TLS_CERT_FILE"); ok {
		c.TLS.CertFile = v
	}
	if v, ok := str("TLS_KEY_FILE"); ok {
		c.TLS.KeyFile = v
	}
	if v, ok := str("TLS_CLIENT_CA_FILE"); ok {
		c.TLS.ClientCAFile = v
	}
	if v, ok := str("TLS_TRUST_DOMAINS"); ok {
		c.TLS.TrustDomains = splitList(v)
	}

	// Behaviour.
	if err := boolean("PRICING_LIVE", &c.Pricing.Live); err != nil {
		return err
	}
	if err := boolean("DEMO", &c.Demo.Enabled); err != nil {
		return err
	}
	if err := boolean("API_REQUIRE_AUTH_FOR_READS", &c.API.RequireAuthForReads); err != nil {
		return err
	}
	if err := boolean("API_ALLOW_ANONYMOUS_MUTATIONS", &c.API.AllowAnonymousMutations); err != nil {
		return err
	}
	if err := number("CARBON_ENERGY_KWH_PER_GB", &c.Carbon.EnergyKWhPerGB); err != nil {
		return err
	}
	if err := number("DEMO_RPS", &c.Demo.RPS); err != nil {
		return err
	}

	return nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// EnvDocs returns the recognised environment variables and what they do, for
// `--help` and the deployment documentation. Keeping this beside the parser
// means the two cannot drift.
func EnvDocs() [][2]string {
	return [][2]string{
		{EnvPrefix + "LISTEN_API", "dashboard, REST API and /metrics bind address"},
		{EnvPrefix + "LISTEN_AUTHZ", "Envoy ext_authz bind address (empty disables)"},
		{EnvPrefix + "LISTEN_PROXY", "native data-plane bind address (empty disables)"},
		{EnvPrefix + "API_TOKEN", "bearer token required for mutating API calls"},
		{EnvPrefix + "API_TOKEN_FILE", "read the API token from this path instead"},
		{EnvPrefix + "API_REQUIRE_AUTH_FOR_READS", "also require the token for read endpoints"},
		{EnvPrefix + "API_ALLOW_ANONYMOUS_MUTATIONS", "explicit opt-in to an unauthenticated write API"},
		{EnvPrefix + "ELECTRICITY_MAPS_TOKEN", "live grid carbon intensity"},
		{EnvPrefix + "ELECTRICITY_MAPS_TOKEN_FILE", "read the grid token from this path instead"},
		{EnvPrefix + "POLICY_FILE", "path to the .sluice policy document"},
		{EnvPrefix + "PRICING_LIVE", "query provider pricing APIs"},
		{EnvPrefix + "CARBON_ENERGY_KWH_PER_GB", "network energy intensity assumption"},
		{EnvPrefix + "TLS_CERT_FILE", "data-plane server certificate"},
		{EnvPrefix + "TLS_KEY_FILE", "data-plane server key"},
		{EnvPrefix + "TLS_CLIENT_CA_FILE", "CA that client certificates must chain to (enables mTLS)"},
		{EnvPrefix + "TLS_TRUST_DOMAINS", "comma-separated SPIFFE trust domains to accept"},
		{EnvPrefix + "DEMO", "run the built-in simulator"},
		{EnvPrefix + "DEMO_RPS", "synthetic request rate"},
	}
}
