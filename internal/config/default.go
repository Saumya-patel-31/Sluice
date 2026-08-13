package config

import "github.com/saumyapatel/sluice/internal/model"

// Default returns the shipped configuration: a ten-region, three-cloud
// topology chosen so that no single backend wins on every objective.
//
// The spread is the point, and one relationship in particular: the *fastest*
// region is gcp-us-central1, which also has the most expensive egress —
// Google's premium tier lists at $0.12/GB against Azure's $0.087, a 38%
// premium. A latency-only balancer will pay that premium on every byte and
// never know it did. That is precisely the case Sluice exists to catch, and a
// demo where the fastest region happened to also be the cheapest would
// demonstrate nothing.
//
// The other relationships: azure-francecentral has both the cheapest egress
// and by far the cleanest grid but sits 88ms away, so it wins batch traffic
// and gets shed by the interactive SLO. aws-us-east-1 and azure-eastus share
// the PJM grid, so failing over between those two clouds does nothing at all
// for emissions — a correlation the carbon model has to know about and the
// topology view makes visible.
func Default() *Config {
	return &Config{
		Listen: ListenConfig{
			API:   ":8080",
			Proxy: "",
			Authz: ":8081",
		},
		Backends: defaultBackends(),
		Routes:   defaultRoutes(),
		Policy: PolicyConfig{
			CacheSize:       8192,
			CacheTTLSeconds: 5,
			Watch:           true,
		},
		Pricing: PricingConfig{
			Live:           false,
			RefreshSeconds: 900,
		},
		Carbon: CarbonConfig{
			EnergyKWhPerGB: 0.015,
			RefreshSeconds: 60,
		},
		Router: RouterConfig{
			ControlIntervalMs:   1000,
			Temperature:         0.12,
			Smoothing:           0.35,
			Deadband:            0.04,
			MinWeight:           0.01,
			ExplorationFloor:    0.02,
			DefaultObjectives:   model.Vector{0.35, 0.35, 0.20, 0.10},
			DefaultRequestBytes: 64 << 10,
		},
		Probe: ProbeConfig{
			IntervalMs: 2000,
			TimeoutMs:  2000,
			Path:       "/healthz",
		},
		Ledger: LedgerConfig{Capacity: 2000, RollupPoints: 600},
		Demo:   DemoConfig{Enabled: true, RPS: 90, AutoIncidents: true},
	}
}

// sim.* labels seed the simulator's synthetic upstreams. They are ignored
// entirely when Sluice points at real backends.
func defaultBackends() []model.Backend {
	return []model.Backend{
		{
			ID: "aws-us-east-1", Cloud: model.CloudAWS, Region: "us-east-1",
			DisplayName: "AWS N. Virginia", Jurisdiction: "US", GridZone: "US-MIDA-PJM",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 400,
			Labels: map[string]string{"sim.latencyMs": "24", "sim.errorRate": "0.001", "geo": "us-east"},
		},
		{
			ID: "aws-us-west-2", Cloud: model.CloudAWS, Region: "us-west-2",
			DisplayName: "AWS Oregon", Jurisdiction: "US", GridZone: "US-NW-BPAT",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 350,
			Labels: map[string]string{"sim.latencyMs": "38", "sim.errorRate": "0.001", "geo": "us-west"},
		},
		{
			ID: "gcp-us-central1", Cloud: model.CloudGCP, Region: "us-central1",
			DisplayName: "Google Iowa", Jurisdiction: "US", GridZone: "US-MIDW-MISO",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 300,
			// The fastest region in the fleet, and the most expensive egress.
			Labels: map[string]string{"sim.latencyMs": "20", "sim.errorRate": "0.0015", "geo": "us-central"},
		},
		{
			ID: "azure-eastus", Cloud: model.CloudAzure, Region: "eastus",
			DisplayName: "Azure Virginia", Jurisdiction: "US", GridZone: "US-MIDA-PJM",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 320,
			// A few milliseconds behind GCP, at 28% lower egress cost.
			Labels: map[string]string{"sim.latencyMs": "28", "sim.errorRate": "0.002", "geo": "us-east"},
		},
		{
			ID: "gcp-europe-north1", Cloud: model.CloudGCP, Region: "europe-north1",
			DisplayName: "Google Finland", Jurisdiction: "EU", GridZone: "FI",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 220,
			Labels: map[string]string{"sim.latencyMs": "95", "sim.errorRate": "0.001", "geo": "eu-north"},
		},
		{
			ID: "azure-francecentral", Cloud: model.CloudAzure, Region: "francecentral",
			DisplayName: "Azure Paris", Jurisdiction: "EU", GridZone: "FR",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 240,
			Labels: map[string]string{"sim.latencyMs": "88", "sim.errorRate": "0.001", "geo": "eu-west"},
		},
		{
			ID: "azure-northeurope", Cloud: model.CloudAzure, Region: "northeurope",
			DisplayName: "Azure Ireland", Jurisdiction: "EU", GridZone: "IE",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 260,
			Labels: map[string]string{"sim.latencyMs": "78", "sim.errorRate": "0.0012", "geo": "eu-west"},
		},
		{
			ID: "gcp-europe-west1", Cloud: model.CloudGCP, Region: "europe-west1",
			DisplayName: "Google Belgium", Jurisdiction: "EU", GridZone: "BE",
			Tier: "primary", Bias: 1, Enabled: true, CapacityRPS: 230,
			Labels: map[string]string{"sim.latencyMs": "85", "sim.errorRate": "0.001", "geo": "eu-west"},
		},
		{
			ID: "azure-centralindia", Cloud: model.CloudAzure, Region: "centralindia",
			DisplayName: "Azure Pune", Jurisdiction: "IN", GridZone: "IN-WE",
			Tier: "burst", Bias: 1, Enabled: true, CapacityRPS: 140,
			Labels: map[string]string{"sim.latencyMs": "210", "sim.errorRate": "0.004", "geo": "apac"},
		},
		{
			ID: "aws-ap-southeast-1", Cloud: model.CloudAWS, Region: "ap-southeast-1",
			DisplayName: "AWS Singapore", Jurisdiction: "APAC", GridZone: "SG",
			Tier: "burst", Bias: 1, Enabled: true, CapacityRPS: 150,
			Labels: map[string]string{"sim.latencyMs": "185", "sim.errorRate": "0.003", "geo": "apac"},
		},
	}
}

func defaultRoutes() []model.Route {
	return []model.Route{
		{
			ID: "payments", DisplayName: "Payments API", PathPrefix: "/api/payments",
			// Money movement optimises for reliability and speed. Egress cost
			// on a payment authorisation is rounding error next to the cost of
			// a timeout, and the objectives should say so plainly.
			Objectives:   model.Vector{0.05, 0.45, 0.05, 0.45},
			LatencySLOMs: 45,
			Temperature:  0.08,
			RequireMTLS:  true,
		},
		{
			ID: "interactive", DisplayName: "Interactive API", PathPrefix: "/api/v1",
			Objectives:   model.Vector{0.15, 0.55, 0.15, 0.15},
			LatencySLOMs: 60,
			Temperature:  0.12,
		},
		{
			ID: "batch", DisplayName: "Batch & ETL", PathPrefix: "/batch",
			// Nothing is waiting on batch, so it spends its whole latency
			// budget buying cheaper and cleaner egress. This is where the
			// savings on the dashboard actually come from.
			Objectives:  model.Vector{0.45, 0.05, 0.40, 0.10},
			Temperature: 0.20,
		},
		{
			ID: "default", DisplayName: "Default", PathPrefix: "/",
			Objectives:   model.Vector{0.35, 0.35, 0.20, 0.10},
			LatencySLOMs: 120,
			Temperature:  0.14,
		},
	}
}
