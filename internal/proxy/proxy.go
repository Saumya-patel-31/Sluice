// Package proxy is Sluice's native data plane: a reverse proxy that
// terminates mutual TLS, authorises each request against the policy engine,
// forwards it to the backend the router selected, and feeds the observed
// latency and byte count back into the signals the next decision will use.
//
// Envoy is the intended data plane for a real deployment, driven through the
// ext_authz service. This exists because a control plane that cannot be run
// end to end on its own is a control plane nobody can evaluate: `sluiced
// --proxy :9443` gives a complete, working system with one process and no
// sidecar.
package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/saumyapatel/sluice/internal/authz"
	"github.com/saumyapatel/sluice/internal/config"
	"github.com/saumyapatel/sluice/internal/identity"
	"github.com/saumyapatel/sluice/internal/model"
	"github.com/saumyapatel/sluice/internal/router"
	"github.com/saumyapatel/sluice/internal/signals"
)

// Config configures the data plane.
type Config struct {
	Listen string
	TLS    config.TLSConfig
	Engine *router.Engine
	Store  *signals.Store
	Log    *slog.Logger
	// InsecureUpstream skips upstream certificate verification, for demos
	// against self-signed synthetic backends.
	InsecureUpstream bool
	// TrustXFCC believes X-Forwarded-Client-Cert when TLS did not produce a
	// verified peer. Only safe behind a proxy that sets it and strips any
	// inbound copy.
	TrustXFCC bool
}

// Proxy is the data-plane server.
type Proxy struct {
	cfg      Config
	log      *slog.Logger
	rp       *httputil.ReverseProxy
	srv      *http.Server
	tlsOn    bool
	inFlight atomic.Int64
}

type ctxKey int

const (
	ctxTarget ctxKey = iota
	ctxDecision
)

// New builds a data-plane proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Engine == nil {
		return nil, errors.New("proxy: an engine is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	p := &Proxy{cfg: cfg, log: cfg.Log}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: cfg.InsecureUpstream}, //nolint:gosec // opt-in
	}

	p.rp = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(ctxTarget).(*url.URL)
			if target == nil {
				return
			}
			pr.SetURL(target)
			// Preserve the client's Host so upstream virtual hosting and
			// certificate matching behave the way the caller expects.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()

			if d, _ := pr.In.Context().Value(ctxDecision).(*model.Decision); d != nil {
				pr.Out.Header.Set(authz.HeaderDecision, d.ID)
				pr.Out.Header.Set(authz.HeaderBackend, d.ChosenBackend)
				pr.Out.Header.Set(authz.HeaderCloud, string(d.Cloud))
				pr.Out.Header.Set(authz.HeaderRegion, d.Region)
				pr.Out.Header.Set(authz.HeaderRoute, d.RouteID)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			d, _ := r.Context().Value(ctxDecision).(*model.Decision)
			backend := "unknown"
			if d != nil {
				backend = d.ChosenBackend
			}
			p.log.Warn("upstream request failed", "backend", backend, "err", err)
			// The failure is reported to the engine by ServeHTTP, which knows
			// the elapsed time; here we only shape the client's response.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "upstream unreachable",
				"backend": backend,
			})
		},
	}

	tlsCfg, err := buildTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	p.tlsOn = tlsCfg != nil

	p.srv = &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return p, nil
}

// Server returns the underlying HTTP server.
func (p *Proxy) Server() *http.Server { return p.srv }

// TLSEnabled reports whether the listener terminates TLS.
func (p *Proxy) TLSEnabled() bool { return p.tlsOn }

// InFlight returns the number of requests currently being proxied.
func (p *Proxy) InFlight() int64 { return p.inFlight.Load() }

// ServeHTTP authorises and forwards one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/sluice/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	sub := p.subjectFor(r)
	if sub.Authenticated && !identity.TrustDomainAllowed(sub, p.cfg.TLS.TrustDomains) {
		writeDenial(w, http.StatusForbidden,
			"identity presents an untrusted SPIFFE domain: "+sub.TrustDomain, "")
		return
	}

	req := p.requestFor(r)
	d := p.cfg.Engine.Decide(&sub, req)

	if d.Verdict != model.VerdictAllow {
		status := http.StatusForbidden
		if d.Verdict == model.VerdictNoCapacity {
			status = http.StatusServiceUnavailable
		}
		writeDenial(w, status, d.DenyReason, d.ID)
		return
	}

	backend, ok := p.cfg.Engine.Snapshot().ByID(d.ChosenBackend)
	if !ok || backend.Backend.Address == "" {
		writeDenial(w, http.StatusServiceUnavailable,
			"selected backend has no reachable address: "+d.ChosenBackend, d.ID)
		return
	}
	target, err := url.Parse(backend.Backend.Address)
	if err != nil {
		writeDenial(w, http.StatusInternalServerError,
			"backend address is malformed: "+backend.Backend.Address, d.ID)
		return
	}

	w.Header().Set(authz.HeaderDecision, d.ID)
	w.Header().Set(authz.HeaderBackend, d.ChosenBackend)
	w.Header().Set(authz.HeaderCloud, string(d.Cloud))
	w.Header().Set(authz.HeaderRegion, d.Region)

	release := p.cfg.Store.TrackInFlight(d.ChosenBackend)
	p.inFlight.Add(1)
	defer func() {
		release()
		p.inFlight.Add(-1)
	}()

	ctx := context.WithValue(r.Context(), ctxTarget, target)
	ctx = context.WithValue(ctx, ctxDecision, d)

	// The counting writer is what closes the loop: the bytes it observes are
	// the bytes the egress bill will be computed from, so cost attribution
	// comes from measurement rather than from the caller's declared size.
	cw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
	started := time.Now()
	p.rp.ServeHTTP(cw, r.WithContext(ctx))
	elapsed := time.Since(started)

	success := cw.status < 500
	p.cfg.Engine.ObserveResult(d.ChosenBackend, elapsed, success, cw.written)
}

// subjectFor derives identity from the TLS handshake, falling back to a
// trusted forwarded-certificate header.
func (p *Proxy) subjectFor(r *http.Request) model.Subject {
	if sub := identity.FromTLS(r.TLS); sub.Authenticated {
		return sub
	}
	if p.cfg.TrustXFCC {
		if xfcc := r.Header.Get("x-forwarded-client-cert"); xfcc != "" {
			if sub := identity.FromXFCC(xfcc); sub.Authenticated {
				return sub
			}
		}
	}
	return model.Anonymous()
}

func (p *Proxy) requestFor(r *http.Request) *model.Request {
	req := &model.Request{
		Method:    r.Method,
		Path:      r.URL.Path,
		Host:      r.Host,
		SourceIP:  remoteIP(r),
		SourceGeo: r.Header.Get(authz.HeaderGeo),
		DataClass: model.DataClass(strings.ToLower(r.Header.Get(authz.HeaderDataClass))),
		Headers:   make(map[string]string, len(r.Header)),
	}
	for k, v := range r.Header {
		if len(v) > 0 {
			req.Headers[strings.ToLower(k)] = v[0]
		}
	}
	if v := r.Header.Get(authz.HeaderBytes); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			req.EstimatedBytes = n
		}
	}
	return req
}

// remoteIP returns the peer address.
//
// X-Forwarded-For is deliberately ignored. This proxy is the edge; a caller
// that can set that header could otherwise present itself as coming from the
// corporate range and satisfy a CIDR policy it should fail.
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeDenial(w http.ResponseWriter, status int, reason, decisionID string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if decisionID != "" {
		w.Header().Set(authz.HeaderDecision, decisionID)
	}
	w.WriteHeader(status)
	body := map[string]string{"error": http.StatusText(status), "reason": reason}
	if decisionID != "" {
		body["decisionId"] = decisionID
	}
	_ = json.NewEncoder(w).Encode(body)
}

// countingWriter records the response status and the bytes written.
type countingWriter struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (c *countingWriter) WriteHeader(status int) {
	if c.wrote {
		return
	}
	c.wrote = true
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.wrote = true
	}
	n, err := c.ResponseWriter.Write(b)
	c.written += int64(n)
	return n, err
}

// Flush forwards to the wrapped writer so streaming responses are not
// buffered by the byte counter.
func (c *countingWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets ReverseProxy reach the underlying writer for hijacking and
// other optional interfaces.
func (c *countingWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// buildTLS assembles the listener's TLS configuration, returning nil when no
// certificate is configured (plain HTTP, for local development).
func buildTLS(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("proxy: loading server certificate: %w", err)
	}

	out := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}

	if cfg.ClientCAFile != "" {
		pem, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("proxy: reading client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("proxy: client CA file %s contains no certificates", cfg.ClientCAFile)
		}
		out.ClientCAs = pool
		// RequireAndVerifyClientCert, not VerifyClientCertIfGiven: under zero
		// trust an unauthenticated connection is refused at the handshake
		// rather than admitted and then denied by policy, so an unauthorised
		// peer never reaches the decision path at all.
		out.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return out, nil
}
