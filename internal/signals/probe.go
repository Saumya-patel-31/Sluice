package signals

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// Prober actively measures backend latency and liveness.
//
// Active probing is what makes weight-based routing recoverable. Once the
// router sheds a backend to zero weight there is no organic traffic left to
// tell it whether the backend came back, so the only evidence available is
// synthetic. Probes run against every backend regardless of its current
// weight, including fully ejected ones.
type Prober struct {
	Store *Store
	Log   *slog.Logger

	// Interval is the base probe period per backend.
	Interval time.Duration
	// Timeout bounds a single probe.
	Timeout time.Duration
	// Path is the health endpoint appended to each backend address.
	Path string
	// Concurrency bounds simultaneous probes across all backends.
	Concurrency int
	// Jitter is the fraction of Interval to randomise each cycle by, in
	// [0,1). Without it every backend is probed on the same tick forever,
	// which turns the control plane into a synchronised load spike.
	Jitter float64
	// InsecureSkipVerify disables upstream certificate verification. Intended
	// for the self-signed certificates used in local demos.
	InsecureSkipVerify bool

	once   sync.Once
	client *http.Client
}

// NewProber returns a Prober with sane defaults bound to a store.
func NewProber(store *Store, log *slog.Logger) *Prober {
	return &Prober{
		Store:       store,
		Log:         log,
		Interval:    2 * time.Second,
		Timeout:     2 * time.Second,
		Path:        "/healthz",
		Concurrency: 32,
		Jitter:      0.25,
	}
}

func (p *Prober) init() {
	p.once.Do(func() {
		if p.Interval <= 0 {
			p.Interval = 2 * time.Second
		}
		if p.Timeout <= 0 {
			p.Timeout = 2 * time.Second
		}
		if p.Concurrency <= 0 {
			p.Concurrency = 32
		}
		if p.Path == "" {
			p.Path = "/healthz"
		}
		p.client = &http.Client{
			Timeout: p.Timeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: p.Timeout}).DialContext,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: p.InsecureSkipVerify}, //nolint:gosec // opt-in, demo only
			},
			// Probes measure the backend, not the redirect chain.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
}

// Run probes every registered backend on the configured interval until ctx is
// cancelled.
func (p *Prober) Run(ctx context.Context) {
	p.init()
	sem := make(chan struct{}, p.Concurrency)

	for {
		start := time.Now()
		backends := p.Store.Backends()

		var wg sync.WaitGroup
		for _, b := range backends {
			if !b.Enabled || b.Address == "" {
				continue
			}
			wg.Add(1)
			go func(b model.Backend) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				p.ProbeOnce(ctx, b)
			}(b)
		}
		wg.Wait()

		// Sleep the remainder of the interval, jittered, so probe cycles do
		// not drift into lockstep with each other or with the control loop.
		delay := p.Interval - time.Since(start)
		if delay < 0 {
			delay = 0
		}
		if p.Jitter > 0 {
			delay += time.Duration(rand.Float64() * p.Jitter * float64(p.Interval))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// ProbeOnce issues a single health probe and records the result.
func (p *Prober) ProbeOnce(ctx context.Context, b model.Backend) {
	p.init()

	url := strings.TrimSuffix(b.Address, "/") + p.Path
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		p.Store.ObserveProbe(b.ID, 0, false)
		return
	}
	req.Header.Set("User-Agent", "sluice-prober/1")

	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		p.Store.ObserveProbe(b.ID, 0, false)
		if p.Log != nil {
			p.Log.Debug("probe failed", "backend", b.ID, "err", err)
		}
		return
	}
	// The body must be drained for the connection to be reusable, and its
	// read time is part of the latency the client would experience.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	rtt := time.Since(start)

	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	p.Store.ObserveProbe(b.ID, rtt, ok)
}
