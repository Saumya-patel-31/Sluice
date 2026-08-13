package signals

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/saumyapatel/sluice/internal/model"
)

// PriceTableAsOf records when the bundled list prices were last transcribed
// from provider documentation. It is surfaced in the API so nobody mistakes a
// bundled figure for a live one.
const PriceTableAsOf = "2025-11-01"

// egressListPrices holds published internet-egress list prices in USD per GB
// at the first paid tier (after each provider's free allowance, below the
// first volume-discount break).
//
// These are a floor, not a truth: real bills depend on committed-use
// discounts, private pricing agreements, and volume tiers that only the
// account owner can see. The bundled table exists so the router has a
// defensible starting point on a cold start and in air-gapped deployments;
// LivePricer overrides it wherever an API is reachable.
var egressListPrices = map[model.Cloud]map[string]float64{
	model.CloudAWS: {
		"us-east-1":      0.090,
		"us-east-2":      0.090,
		"us-west-1":      0.090,
		"us-west-2":      0.090,
		"ca-central-1":   0.090,
		"eu-west-1":      0.090,
		"eu-west-2":      0.090,
		"eu-central-1":   0.090,
		"eu-north-1":     0.090,
		"ap-northeast-1": 0.114,
		"ap-southeast-1": 0.120,
		"ap-southeast-2": 0.114,
		"ap-south-1":     0.1093,
		"sa-east-1":      0.150,
		"af-south-1":     0.154,
		"me-south-1":     0.117,
	},
	model.CloudGCP: {
		"us-central1":             0.120,
		"us-east1":                0.120,
		"us-west1":                0.120,
		"northamerica-northeast1": 0.120,
		"europe-west1":            0.120,
		"europe-west4":            0.120,
		"europe-north1":           0.120,
		"asia-south1":             0.120,
		"asia-southeast1":         0.120,
		"asia-northeast1":         0.120,
		"southamerica-east1":      0.120,
		"australia-southeast1":    0.190,
	},
	model.CloudAzure: {
		"eastus":        0.087,
		"eastus2":       0.087,
		"westus2":       0.087,
		"westus3":       0.087,
		"centralus":     0.087,
		"northeurope":   0.087,
		"westeurope":    0.087,
		"uksouth":       0.087,
		"francecentral": 0.087,
		"swedencentral": 0.087,
		"southeastasia": 0.120,
		"japaneast":     0.120,
		"australiaeast": 0.120,
		"koreacentral":  0.120,
		"centralindia":  0.1093,
		"brazilsouth":   0.181,
	},
	model.CloudOnPrem: {},
}

// fallbackEgressPrice is used for a region absent from the table. It is
// deliberately pessimistic: an unknown region should not look cheap and win
// traffic by virtue of missing data.
const fallbackEgressPrice = 0.15

// ListPrice returns the bundled list price for a cloud region.
func ListPrice(cloud model.Cloud, region string) (float64, bool) {
	if m, ok := egressListPrices[cloud]; ok {
		if p, ok := m[region]; ok {
			return p, true
		}
	}
	return fallbackEgressPrice, false
}

// Pricer resolves an egress price for a backend. Returning ok=false means
// "no opinion", and the next provider in the chain is consulted.
type Pricer interface {
	Name() string
	Price(ctx context.Context, b model.Backend) (price float64, ok bool, err error)
}

// -----------------------------------------------------------------------------
// Static pricer
// -----------------------------------------------------------------------------

// StaticPricer serves the bundled list-price table, with optional per-backend
// overrides for accounts with negotiated rates.
type StaticPricer struct {
	// Overrides is keyed by backend ID and wins over the bundled table. This
	// is where a real deployment expresses its committed-use discount.
	Overrides map[string]float64
}

// Name identifies the provider.
func (p *StaticPricer) Name() string { return "list-price:" + PriceTableAsOf }

// Price returns the override or bundled price for the backend.
func (p *StaticPricer) Price(_ context.Context, b model.Backend) (float64, bool, error) {
	if v, ok := p.Overrides[b.ID]; ok {
		return v, true, nil
	}
	v, _ := ListPrice(b.Cloud, b.Region)
	return v, true, nil
}

// -----------------------------------------------------------------------------
// Azure Retail Prices API
// -----------------------------------------------------------------------------

// AzureRetailPricer queries the Azure Retail Prices API, which is public and
// requires no authentication. It is the only one of the three major providers
// that will quote you a price without credentials, so it is wired up by
// default and serves as the reference implementation for the live-pricing
// path.
type AzureRetailPricer struct {
	// Endpoint defaults to the public Azure retail prices API.
	Endpoint string
	Client   *http.Client
	Log      *slog.Logger

	mu      sync.RWMutex
	cache   map[string]float64
	fetched time.Time
}

type azurePriceResponse struct {
	Items []struct {
		RetailPrice   float64 `json:"retailPrice"`
		UnitOfMeasure string  `json:"unitOfMeasure"`
		ArmRegionName string  `json:"armRegionName"`
		MeterName     string  `json:"meterName"`
		ProductName   string  `json:"productName"`
		Type          string  `json:"type"`
	} `json:"Items"`
	NextPageLink string `json:"NextPageLink"`
}

// Name identifies the provider.
func (p *AzureRetailPricer) Name() string { return "azure-retail-api" }

func (p *AzureRetailPricer) endpoint() string {
	if p.Endpoint != "" {
		return p.Endpoint
	}
	return "https://prices.azure.com/api/retail/prices"
}

func (p *AzureRetailPricer) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Price returns the live Azure bandwidth price for the backend's region.
func (p *AzureRetailPricer) Price(ctx context.Context, b model.Backend) (float64, bool, error) {
	if b.Cloud != model.CloudAzure {
		return 0, false, nil
	}

	p.mu.RLock()
	v, cached := p.cache[b.Region]
	fresh := time.Since(p.fetched) < time.Hour
	p.mu.RUnlock()
	if cached && fresh {
		return v, true, nil
	}

	price, err := p.fetchRegion(ctx, b.Region)
	if err != nil {
		// A live-pricing failure must not fail the decision. Fall through to
		// the next provider (ultimately the bundled table) and let the quote's
		// Source field record that the number is not live.
		if p.Log != nil {
			p.Log.Warn("azure retail price fetch failed", "region", b.Region, "err", err)
		}
		return 0, false, err
	}

	p.mu.Lock()
	if p.cache == nil {
		p.cache = make(map[string]float64)
	}
	p.cache[b.Region] = price
	p.fetched = time.Now()
	p.mu.Unlock()

	return price, true, nil
}

func (p *AzureRetailPricer) fetchRegion(ctx context.Context, region string) (float64, error) {
	filter := fmt.Sprintf(
		"serviceName eq 'Bandwidth' and armRegionName eq '%s' and priceType eq 'Consumption'",
		strings.ReplaceAll(region, "'", ""),
	)
	u := p.endpoint() + "?" + url.Values{
		"$filter":      {filter},
		"api-version":  {"2023-01-01-preview"},
		"currencyCode": {"USD"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("azure retail prices: unexpected status %s", resp.Status)
	}

	var out azurePriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("azure retail prices: decode: %w", err)
	}

	// The Bandwidth service returns many meters (inter-region, CDN, routing
	// preference). Take the cheapest positive per-GB internet-egress meter,
	// which is the tier a first-GB transfer actually lands in.
	best := 0.0
	for _, it := range out.Items {
		if it.RetailPrice <= 0 || !strings.Contains(it.UnitOfMeasure, "GB") {
			continue
		}
		name := strings.ToLower(it.MeterName)
		if !strings.Contains(name, "data transfer out") && !strings.Contains(name, "egress") {
			continue
		}
		if best == 0 || it.RetailPrice < best {
			best = it.RetailPrice
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("azure retail prices: no egress meter for region %q", region)
	}
	return best, nil
}

// -----------------------------------------------------------------------------
// Pricing service
// -----------------------------------------------------------------------------

// PricingService refreshes egress prices into the Store on a fixed cadence,
// consulting providers in priority order until one has an opinion.
type PricingService struct {
	Store     *Store
	Providers []Pricer
	Interval  time.Duration
	TTL       time.Duration
	Log       *slog.Logger
}

// NewPricingService wires the default chain: live Azure API first, bundled
// list prices as the backstop.
func NewPricingService(store *Store, log *slog.Logger, overrides map[string]float64, live bool) *PricingService {
	var providers []Pricer
	if live {
		providers = append(providers, &AzureRetailPricer{Log: log})
	}
	providers = append(providers, &StaticPricer{Overrides: overrides})
	return &PricingService{
		Store:     store,
		Providers: providers,
		Interval:  15 * time.Minute,
		TTL:       2 * time.Hour,
		Log:       log,
	}
}

// Refresh resolves and stores a price for every registered backend.
func (s *PricingService) Refresh(ctx context.Context) error {
	now := time.Now()
	for _, b := range s.Store.Backends() {
		var (
			price  float64
			source string
			found  bool
		)
		for _, p := range s.Providers {
			v, ok, err := p.Price(ctx, b)
			if err != nil || !ok {
				continue
			}
			price, source, found = v, p.Name(), true
			break
		}
		if !found {
			price, source = fallbackEgressPrice, "fallback"
		}
		s.Store.SetPrice(b.ID, Quote{Value: price, Source: source, AsOf: now, TTL: s.TTL})
	}
	return nil
}

// Run refreshes immediately and then on the configured interval until ctx is
// cancelled.
func (s *PricingService) Run(ctx context.Context) {
	if err := s.Refresh(ctx); err != nil && s.Log != nil {
		s.Log.Warn("initial price refresh failed", "err", err)
	}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil && s.Log != nil {
				s.Log.Warn("price refresh failed", "err", err)
			}
		}
	}
}
