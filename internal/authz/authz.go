// Package authz implements Envoy's external authorization contract over HTTP.
//
// Envoy is told to call this service before forwarding a request. A 2xx
// response authorises the request and Envoy copies the configured response
// headers onto the upstream call; any other status rejects it and Envoy
// returns this service's status and body to the client.
//
// The HTTP variant is used rather than the gRPC one deliberately. It needs no
// protobuf toolchain, no generated code and no third-party module, which keeps
// Sluice's dependency count at zero for a component that sits in the
// authorisation path of every request. The routing decision travels back as
// response headers, which Envoy's route table then matches on — so the data
// plane stays a stock Envoy with no custom filter to build or ship.
package authz

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Saumya-patel-31/sluice/internal/identity"
	"github.com/Saumya-patel-31/sluice/internal/model"
	"github.com/Saumya-patel-31/sluice/internal/router"
)

// Header names Envoy is configured to forward upstream. They are exported so
// the shipped Envoy bootstrap and this package cannot drift apart.
const (
	HeaderCloud      = "x-sluice-cloud"
	HeaderRegion     = "x-sluice-region"
	HeaderBackend    = "x-sluice-backend"
	HeaderUpstream   = "x-sluice-upstream"
	HeaderDecision   = "x-sluice-decision"
	HeaderRoute      = "x-sluice-route"
	HeaderScore      = "x-sluice-score"
	HeaderDenyReason = "x-sluice-deny-reason"

	// HeaderDataClass lets a caller declare payload sensitivity so residency
	// policy can act on it. It is a hint about the request, not an identity
	// claim, so accepting it from the caller is safe: the worst a liar can do
	// is subject their own traffic to stricter routing.
	HeaderDataClass = "x-sluice-data-class"
	// HeaderBytes lets a caller declare expected response size, which turns a
	// per-GB egress price into a per-request cost.
	HeaderBytes = "x-sluice-bytes"
	// HeaderGeo is a coarse client-origin hint.
	HeaderGeo = "x-sluice-geo"
)

// Server answers Envoy ext_authz checks.
type Server struct {
	engine *router.Engine
	log    *slog.Logger

	// TrustXFCC controls whether X-Forwarded-Client-Cert is believed. It must
	// be true only when this service is reachable solely from a proxy that
	// sets the header itself and strips any inbound copy.
	TrustXFCC bool
	// TrustedDomains restricts which SPIFFE trust domains are accepted before
	// policy even runs.
	TrustedDomains []string
	// PathPrefix is stripped from the incoming path. Envoy's ext_authz HTTP
	// service can be configured with a path prefix; if it is, the original
	// path arrives with that prefix attached and has to come back off before
	// route matching.
	PathPrefix string
}

// New returns an ext_authz server bound to a routing engine.
func New(engine *router.Engine, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{engine: engine, log: log, TrustXFCC: true}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", http.HandlerFunc(s.check))
	return mux
}

// check authorises one request.
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	sub := s.subjectFor(r)
	req := requestFor(r, s.PathPrefix)

	// Trust-domain filtering happens before policy so an identity from an
	// unknown certificate authority is rejected without its attributes ever
	// reaching an expression evaluator.
	if sub.Authenticated && !identity.TrustDomainAllowed(sub, s.TrustedDomains) {
		s.deny(w, nil, "identity presents an untrusted SPIFFE domain: "+sub.TrustDomain)
		return
	}

	d := s.engine.Decide(&sub, req)

	if d.Verdict != model.VerdictAllow {
		s.deny(w, d, d.DenyReason)
		return
	}

	h := w.Header()
	h.Set(HeaderCloud, string(d.Cloud))
	h.Set(HeaderRegion, d.Region)
	h.Set(HeaderBackend, d.ChosenBackend)
	h.Set(HeaderRoute, d.RouteID)
	h.Set(HeaderDecision, d.ID)
	if c, ok := d.Candidate(d.ChosenBackend); ok {
		h.Set(HeaderScore, strconv.FormatFloat(c.Score, 'f', 4, 64))
	}
	if b, ok := s.engine.Snapshot().ByID(d.ChosenBackend); ok {
		h.Set(HeaderUpstream, b.Backend.Address)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deny(w http.ResponseWriter, d *model.Decision, reason string) {
	if reason == "" {
		reason = "denied by policy"
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set(HeaderDenyReason, sanitizeHeader(reason))
	if d != nil {
		h.Set(HeaderDecision, d.ID)
		h.Set(HeaderRoute, d.RouteID)
	}

	status := http.StatusForbidden
	body := map[string]any{"error": "forbidden", "reason": reason}
	if d != nil {
		body["decisionId"] = d.ID
		// A request that policy permitted but for which no destination
		// survived is an availability failure, not an authorisation one, and
		// the status code has to say so or every dashboard will misattribute
		// the outage.
		if d.Verdict == model.VerdictNoCapacity {
			status = http.StatusServiceUnavailable
			body["error"] = "no eligible backend"
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// subjectFor derives the caller's identity from verified material only.
func (s *Server) subjectFor(r *http.Request) model.Subject {
	// A direct TLS handshake with a verified client certificate is the
	// strongest evidence available and wins over any header.
	if r.TLS != nil {
		if sub := identity.FromTLS(r.TLS); sub.Authenticated {
			return sub
		}
	}
	if s.TrustXFCC {
		if xfcc := r.Header.Get("x-forwarded-client-cert"); xfcc != "" {
			if sub := identity.FromXFCC(xfcc); sub.Authenticated {
				return sub
			}
		}
	}
	return model.Anonymous()
}

// requestFor projects an inbound HTTP request into the routing model.
func requestFor(r *http.Request, prefix string) *model.Request {
	path := r.URL.Path
	if prefix != "" {
		path = strings.TrimPrefix(path, prefix)
		if path == "" || path[0] != '/' {
			path = "/" + path
		}
	}
	// Envoy's ext_authz HTTP service forwards the original method and path,
	// but a deployment may instead pass them as headers; prefer those when
	// present so both wiring styles work.
	if v := r.Header.Get("x-envoy-original-path"); v != "" {
		path = v
	}
	method := r.Method
	if v := r.Header.Get("x-envoy-original-method"); v != "" {
		method = v
	}

	req := &model.Request{
		Method:    method,
		Path:      path,
		Host:      r.Host,
		SourceIP:  clientIP(r),
		SourceGeo: r.Header.Get(HeaderGeo),
		DataClass: model.DataClass(strings.ToLower(r.Header.Get(HeaderDataClass))),
		Headers:   make(map[string]string, len(r.Header)),
	}
	for k, v := range r.Header {
		if len(v) > 0 {
			req.Headers[strings.ToLower(k)] = v[0]
		}
	}
	if v := r.Header.Get(HeaderBytes); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			req.EstimatedBytes = n
		}
	}
	return req
}

// clientIP resolves the caller's address.
//
// X-Forwarded-For is only consulted for its leftmost entry when Envoy has
// already produced it, and Envoy is configured with a trusted hop count. A
// naive parse here would let any caller spoof a corporate source address and
// walk straight past a CIDR policy.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("x-envoy-external-address"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("x-forwarded-for"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sanitizeHeader strips characters that cannot appear in a header value.
// Deny reasons come from operator-authored policy messages, which can contain
// anything at all.
func sanitizeHeader(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}
