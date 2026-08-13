// Command sluiced is the Sluice control plane: it resolves egress prices,
// grid carbon intensity and latency for every registered backend, evaluates
// zero-trust policy on each request, and publishes a traffic distribution that
// trades cost and emissions against a latency SLO.
//
// Run it with no arguments for a self-contained demo:
//
//	sluiced --demo
//
// which starts ten synthetic regions across three clouds, drives traffic
// through them, and serves the dashboard on http://localhost:8080.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/saumyapatel/sluice/internal/api"
	"github.com/saumyapatel/sluice/internal/app"
	"github.com/saumyapatel/sluice/internal/authz"
	"github.com/saumyapatel/sluice/internal/config"
	"github.com/saumyapatel/sluice/internal/proxy"
)

// version is overridden at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sluiced:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "", "path to a JSONC configuration file (default: built-in demo topology)")
		policyPath = flag.String("policy", "", "path to a .sluice policy document (default: built-in policy set)")
		listen     = flag.String("listen", "", "override the API and dashboard listen address")
		authzAddr  = flag.String("authz", "", "override the Envoy ext_authz listen address (empty disables)")
		proxyAddr  = flag.String("proxy", "", "override the native data-plane listen address (empty disables)")
		demo       = flag.Bool("demo", false, "run the built-in simulator: synthetic regions, traffic and incidents")
		noDemo     = flag.Bool("no-demo", false, "disable the simulator even if the configuration enables it")
		livePrices = flag.Bool("live-prices", false, "query provider pricing APIs (only Azure's works without credentials)")
		logLevel   = flag.String("log-level", "info", "debug, info, warn or error")
		showVer    = flag.Bool("version", false, "print the version and exit")
		dumpConf   = flag.Bool("print-config", false, "print the effective configuration as JSON and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("sluice %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}

	log := newLogger(*logLevel)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	// Flags override the file, which overrides the defaults.
	if *listen != "" {
		cfg.Listen.API = *listen
	}
	if *authzAddr != "" {
		cfg.Listen.Authz = *authzAddr
	}
	if *proxyAddr != "" {
		cfg.Listen.Proxy = *proxyAddr
	}
	if *policyPath != "" {
		cfg.Policy.File = *policyPath
	}
	if *demo {
		cfg.Demo.Enabled = true
	}
	if *noDemo {
		cfg.Demo.Enabled = false
	}
	if *livePrices {
		cfg.Pricing.Live = true
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}

	if *dumpConf {
		return cfg.Save("/dev/stdout")
	}

	a, err := app.New(cfg, log, version)
	if err != nil {
		return err
	}
	defer func() {
		if err := a.Close(); err != nil {
			log.Warn("shutdown cleanup reported an error", "err", err)
		}
	}()

	// Signals cancel the root context, which every background loop watches.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a.Start(ctx)

	servers := []*namedServer{
		{
			name: "api",
			srv: &http.Server{
				Addr:              cfg.Listen.API,
				Handler:           api.New(a).Handler(),
				ReadHeaderTimeout: 10 * time.Second,
				// No write timeout: the SSE stream is a long-lived response,
				// and a write deadline would sever the dashboard on a timer.
				IdleTimeout: 120 * time.Second,
			},
		},
	}

	if cfg.Listen.Authz != "" {
		servers = append(servers, &namedServer{
			name: "ext_authz",
			srv: &http.Server{
				Addr:              cfg.Listen.Authz,
				Handler:           authz.New(a.Engine, log).Handler(),
				ReadHeaderTimeout: 5 * time.Second,
				WriteTimeout:      5 * time.Second,
			},
		})
	}

	if cfg.Listen.Proxy != "" {
		p, err := proxy.New(proxy.Config{
			Listen:           cfg.Listen.Proxy,
			TLS:              cfg.TLS,
			Engine:           a.Engine,
			Store:            a.Store,
			Log:              log,
			InsecureUpstream: cfg.Probe.InsecureSkipVerify,
		})
		if err != nil {
			return err
		}
		servers = append(servers, &namedServer{name: "proxy", srv: p.Server(), tls: p.TLSEnabled()})
	}

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *namedServer) {
			log.Info("listening", "surface", s.name, "addr", s.srv.Addr, "tls", s.tls)
			var err error
			if s.tls {
				err = s.srv.ListenAndServeTLS("", "")
			} else {
				err = s.srv.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", s.name, err)
			}
		}(s)
	}

	banner(log, cfg, version)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		stop()
		return err
	}

	// Drain in-flight requests before exiting, so a rolling restart does not
	// drop the connections the router just authorised.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("graceful shutdown timed out", "surface", s.name, "err", err)
		}
	}
	log.Info("stopped")
	return nil
}

type namedServer struct {
	name string
	srv  *http.Server
	tls  bool
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	return config.Load(path)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func banner(log *slog.Logger, cfg *config.Config, version string) {
	addr := cfg.Listen.API
	if addr != "" && addr[0] == ':' {
		addr = "localhost" + addr
	}
	log.Info("sluice is up",
		"version", version,
		"dashboard", "http://"+addr,
		"metrics", "http://"+addr+"/metrics",
		"backends", len(cfg.Backends),
		"routes", len(cfg.Routes),
		"demo", cfg.Demo.Enabled,
	)
}
