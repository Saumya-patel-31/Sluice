package signals

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// GridZone describes an electricity market whose carbon intensity a cloud
// region draws from. Zone identifiers follow Electricity Maps' scheme so the
// live adapter can query them directly without a translation table.
type GridZone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Country string `json:"country"`
	// BaseIntensity is the annual mean in gCO2e per kWh.
	BaseIntensity float64 `json:"baseIntensity"`
	// SolarShare is roughly the fraction of midday demand met by solar. It
	// sets the depth of the midday carbon trough in the synthetic model —
	// a grid with high solar penetration is dramatically cleaner at noon than
	// at 8pm, and that swing is the whole reason time-shifted routing pays.
	SolarShare float64 `json:"solarShare"`
	// UTCOffset is the zone's standard-time offset in hours, used to place
	// the diurnal curve in local time.
	UTCOffset float64 `json:"utcOffset"`
}

// gridZones is the bundled zone dataset. Base intensities are approximate
// recent annual averages compiled from public grid-operator disclosures; they
// are a starting point for cold starts and offline deployments, and are
// superseded by live readings wherever an Electricity Maps token is supplied.
var gridZones = map[string]GridZone{
	"US-MIDA-PJM":  {ID: "US-MIDA-PJM", Name: "PJM Mid-Atlantic", Country: "US", BaseIntensity: 355, SolarShare: 0.06, UTCOffset: -5},
	"US-MIDW-MISO": {ID: "US-MIDW-MISO", Name: "MISO Midwest", Country: "US", BaseIntensity: 415, SolarShare: 0.04, UTCOffset: -6},
	"US-CAL-CISO":  {ID: "US-CAL-CISO", Name: "California ISO", Country: "US", BaseIntensity: 220, SolarShare: 0.30, UTCOffset: -8},
	"US-NW-BPAT":   {ID: "US-NW-BPAT", Name: "Bonneville (Pacific NW)", Country: "US", BaseIntensity: 100, SolarShare: 0.03, UTCOffset: -8},
	"US-SW-AZPS":   {ID: "US-SW-AZPS", Name: "Arizona Public Service", Country: "US", BaseIntensity: 400, SolarShare: 0.20, UTCOffset: -7},
	"US-CAR-SCEG":  {ID: "US-CAR-SCEG", Name: "Carolinas", Country: "US", BaseIntensity: 350, SolarShare: 0.09, UTCOffset: -5},
	"CA-QC":        {ID: "CA-QC", Name: "Québec", Country: "CA", BaseIntensity: 30, SolarShare: 0.00, UTCOffset: -5},
	"IE":           {ID: "IE", Name: "Ireland", Country: "IE", BaseIntensity: 285, SolarShare: 0.02, UTCOffset: 0},
	"GB":           {ID: "GB", Name: "Great Britain", Country: "GB", BaseIntensity: 225, SolarShare: 0.05, UTCOffset: 0},
	"DE":           {ID: "DE", Name: "Germany", Country: "DE", BaseIntensity: 380, SolarShare: 0.14, UTCOffset: 1},
	"NL":           {ID: "NL", Name: "Netherlands", Country: "NL", BaseIntensity: 270, SolarShare: 0.15, UTCOffset: 1},
	"BE":           {ID: "BE", Name: "Belgium", Country: "BE", BaseIntensity: 165, SolarShare: 0.08, UTCOffset: 1},
	"FR":           {ID: "FR", Name: "France", Country: "FR", BaseIntensity: 55, SolarShare: 0.04, UTCOffset: 1},
	"SE":           {ID: "SE", Name: "Sweden", Country: "SE", BaseIntensity: 25, SolarShare: 0.01, UTCOffset: 1},
	"FI":           {ID: "FI", Name: "Finland", Country: "FI", BaseIntensity: 70, SolarShare: 0.01, UTCOffset: 2},
	"JP-TK":        {ID: "JP-TK", Name: "Tokyo (TEPCO)", Country: "JP", BaseIntensity: 465, SolarShare: 0.11, UTCOffset: 9},
	"KR":           {ID: "KR", Name: "South Korea", Country: "KR", BaseIntensity: 440, SolarShare: 0.05, UTCOffset: 9},
	"SG":           {ID: "SG", Name: "Singapore", Country: "SG", BaseIntensity: 490, SolarShare: 0.02, UTCOffset: 8},
	"AU-NSW":       {ID: "AU-NSW", Name: "New South Wales", Country: "AU", BaseIntensity: 545, SolarShare: 0.18, UTCOffset: 10},
	"IN-WE":        {ID: "IN-WE", Name: "India West", Country: "IN", BaseIntensity: 630, SolarShare: 0.11, UTCOffset: 5.5},
	"IN-SO":        {ID: "IN-SO", Name: "India South", Country: "IN", BaseIntensity: 560, SolarShare: 0.14, UTCOffset: 5.5},
	"BR-CS":        {ID: "BR-CS", Name: "Brazil Central-South", Country: "BR", BaseIntensity: 100, SolarShare: 0.06, UTCOffset: -3},
	"ZA":           {ID: "ZA", Name: "South Africa", Country: "ZA", BaseIntensity: 710, SolarShare: 0.05, UTCOffset: 2},
	"AE":           {ID: "AE", Name: "United Arab Emirates", Country: "AE", BaseIntensity: 490, SolarShare: 0.09, UTCOffset: 4},
}

// regionGrid maps each cloud region to the electricity market it draws from.
// Two providers in the same metro share a zone — AWS us-east-1 and Azure
// eastus are both on PJM — which is exactly the kind of correlation a
// carbon-aware router has to know about, since "failing over to the other
// cloud" in Northern Virginia does nothing for emissions.
var regionGrid = map[model.Cloud]map[string]string{
	model.CloudAWS: {
		"us-east-1": "US-MIDA-PJM", "us-east-2": "US-MIDW-MISO",
		"us-west-1": "US-CAL-CISO", "us-west-2": "US-NW-BPAT",
		"ca-central-1": "CA-QC", "eu-west-1": "IE", "eu-west-2": "GB",
		"eu-central-1": "DE", "eu-north-1": "SE",
		"ap-northeast-1": "JP-TK", "ap-southeast-1": "SG", "ap-southeast-2": "AU-NSW",
		"ap-south-1": "IN-WE", "sa-east-1": "BR-CS", "af-south-1": "ZA",
		"me-south-1": "AE",
	},
	model.CloudGCP: {
		"us-central1": "US-MIDW-MISO", "us-east1": "US-CAR-SCEG", "us-west1": "US-NW-BPAT",
		"northamerica-northeast1": "CA-QC",
		"europe-west1":            "BE", "europe-west4": "NL", "europe-north1": "FI",
		"asia-south1": "IN-WE", "asia-southeast1": "SG", "asia-northeast1": "JP-TK",
		"southamerica-east1": "BR-CS", "australia-southeast1": "AU-NSW",
	},
	model.CloudAzure: {
		"eastus": "US-MIDA-PJM", "eastus2": "US-MIDA-PJM",
		"westus2": "US-NW-BPAT", "westus3": "US-SW-AZPS", "centralus": "US-MIDW-MISO",
		"northeurope": "IE", "westeurope": "NL", "uksouth": "GB",
		"francecentral": "FR", "swedencentral": "SE",
		"southeastasia": "SG", "japaneast": "JP-TK", "australiaeast": "AU-NSW",
		"koreacentral": "KR", "centralindia": "IN-WE", "brazilsouth": "BR-CS",
	},
}

// ZoneFor resolves the grid zone for a backend, preferring an explicit
// GridZone on the backend over the bundled region mapping.
func ZoneFor(b model.Backend) (GridZone, bool) {
	if b.GridZone != "" {
		if z, ok := gridZones[b.GridZone]; ok {
			return z, true
		}
	}
	if m, ok := regionGrid[b.Cloud]; ok {
		if id, ok := m[b.Region]; ok {
			return gridZones[id], true
		}
	}
	// An unmapped region is assumed to be on a middling fossil-heavy grid.
	// As with pricing, unknown must not read as clean.
	return GridZone{ID: "unknown", Name: "Unmapped grid", BaseIntensity: 450, SolarShare: 0.05}, false
}

// Zones returns the bundled zone dataset.
func Zones() map[string]GridZone {
	out := make(map[string]GridZone, len(gridZones))
	for k, v := range gridZones {
		out[k] = v
	}
	return out
}

// DiurnalIntensity models a grid's carbon intensity at an instant from its
// annual mean, solar penetration and local clock.
//
// Two effects dominate the real curve and both are reproduced here: demand
// peaks in the early evening (and again, more weakly, in the morning) when the
// dirtiest peaker plants come online, and solar suppresses intensity around
// midday in proportion to how much of it a grid has built. The result is that
// a high-solar grid like CAISO swings far more across a day than a
// hydro-dominated one like Bonneville — which is the signal a follow-the-sun
// routing policy is actually trading on.
func DiurnalIntensity(z GridZone, t time.Time) float64 {
	localHour := math.Mod(float64(t.UTC().Hour())+float64(t.UTC().Minute())/60+z.UTCOffset+24, 24)

	// Solar output: a half-sine from 06:00 to 18:00 local, peaking at noon.
	solar := 0.0
	if localHour > 6 && localHour < 18 {
		solar = math.Sin(math.Pi * (localHour - 6) / 12)
	}

	// Demand shape: evening peak at 19:00, softer morning peak at 08:00.
	evening := 0.30 * math.Exp(-math.Pow(localHour-19, 2)/8)
	morning := 0.15 * math.Exp(-math.Pow(localHour-8, 2)/6)
	demand := 0.88 + evening + morning

	v := z.BaseIntensity * demand * (1 - z.SolarShare*solar)
	if v < 5 {
		v = 5
	}
	return v
}

// -----------------------------------------------------------------------------
// Carbon sources
// -----------------------------------------------------------------------------

// CarbonSource resolves grid carbon intensity in gCO2e per kWh.
type CarbonSource interface {
	Name() string
	Intensity(ctx context.Context, b model.Backend) (float64, bool, error)
}

// ModeledCarbon serves the bundled zone dataset shaped by the diurnal model.
// This is the default source: it needs no credentials, no network, and
// produces a curve with the right shape and roughly the right magnitude.
type ModeledCarbon struct {
	Now func() time.Time
}

// Name identifies the source.
func (m *ModeledCarbon) Name() string { return "modeled:diurnal" }

// Intensity returns the modeled intensity for the backend's grid zone.
func (m *ModeledCarbon) Intensity(_ context.Context, b model.Backend) (float64, bool, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	z, _ := ZoneFor(b)
	return DiurnalIntensity(z, now()), true, nil
}

// ElectricityMapsSource reads live grid intensity from the Electricity Maps
// API. It requires an auth token; without one the service silently falls back
// to the modeled source.
type ElectricityMapsSource struct {
	Token    string
	Endpoint string
	Client   *http.Client
	Log      *slog.Logger
	// TTL is how long a zone reading is reused. Electricity Maps updates
	// hourly for most zones, so polling faster only burns quota.
	TTL time.Duration

	mu    sync.RWMutex
	cache map[string]carbonReading
}

type carbonReading struct {
	value   float64
	fetched time.Time
}

// Name identifies the source.
func (e *ElectricityMapsSource) Name() string { return "electricitymaps" }

func (e *ElectricityMapsSource) ttl() time.Duration {
	if e.TTL > 0 {
		return e.TTL
	}
	return 20 * time.Minute
}

// Intensity returns the live intensity for the backend's grid zone.
func (e *ElectricityMapsSource) Intensity(ctx context.Context, b model.Backend) (float64, bool, error) {
	if e.Token == "" {
		return 0, false, nil
	}
	z, mapped := ZoneFor(b)
	if !mapped {
		return 0, false, nil
	}

	e.mu.RLock()
	r, ok := e.cache[z.ID]
	e.mu.RUnlock()
	if ok && time.Since(r.fetched) < e.ttl() {
		return r.value, true, nil
	}

	v, err := e.fetch(ctx, z.ID)
	if err != nil {
		if e.Log != nil {
			e.Log.Warn("electricity maps fetch failed", "zone", z.ID, "err", err)
		}
		return 0, false, err
	}

	e.mu.Lock()
	if e.cache == nil {
		e.cache = make(map[string]carbonReading)
	}
	e.cache[z.ID] = carbonReading{value: v, fetched: time.Now()}
	e.mu.Unlock()
	return v, true, nil
}

func (e *ElectricityMapsSource) fetch(ctx context.Context, zone string) (float64, error) {
	base := e.Endpoint
	if base == "" {
		base = "https://api.electricitymap.org/v3"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/carbon-intensity/latest?zone="+zone, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("auth-token", e.Token)

	c := e.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("electricity maps: status %s", resp.Status)
	}

	var out struct {
		Zone            string  `json:"zone"`
		CarbonIntensity float64 `json:"carbonIntensity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.CarbonIntensity <= 0 {
		return 0, fmt.Errorf("electricity maps: zone %q returned no intensity", zone)
	}
	return out.CarbonIntensity, nil
}

// -----------------------------------------------------------------------------
// Carbon service
// -----------------------------------------------------------------------------

// CarbonService refreshes grid intensity into the Store on a cadence.
type CarbonService struct {
	Store    *Store
	Sources  []CarbonSource
	Interval time.Duration
	TTL      time.Duration
	Log      *slog.Logger
}

// NewCarbonService wires live Electricity Maps first (when a token is set)
// with the modeled dataset as the backstop.
func NewCarbonService(store *Store, log *slog.Logger, emToken string, now func() time.Time) *CarbonService {
	var sources []CarbonSource
	if emToken != "" {
		sources = append(sources, &ElectricityMapsSource{Token: emToken, Log: log})
	}
	sources = append(sources, &ModeledCarbon{Now: now})
	return &CarbonService{
		Store:    store,
		Sources:  sources,
		Interval: time.Minute,
		TTL:      30 * time.Minute,
		Log:      log,
	}
}

// Refresh resolves and stores intensity for every registered backend.
func (s *CarbonService) Refresh(ctx context.Context) error {
	now := time.Now()
	for _, b := range s.Store.Backends() {
		var (
			v      float64
			source string
			found  bool
		)
		for _, src := range s.Sources {
			x, ok, err := src.Intensity(ctx, b)
			if err != nil || !ok {
				continue
			}
			v, source, found = x, src.Name(), true
			break
		}
		if !found {
			z, _ := ZoneFor(b)
			v, source = z.BaseIntensity, "zone-average"
		}
		s.Store.SetGridIntensity(b.ID, Quote{Value: v, Source: source, AsOf: now, TTL: s.TTL})
	}
	return nil
}

// Run refreshes immediately and then on the configured interval.
func (s *CarbonService) Run(ctx context.Context) {
	if err := s.Refresh(ctx); err != nil && s.Log != nil {
		s.Log.Warn("initial carbon refresh failed", "err", err)
	}
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil && s.Log != nil {
				s.Log.Warn("carbon refresh failed", "err", err)
			}
		}
	}
}
