package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Authenticator gates the control-plane API.
//
// The threat it addresses is specific: PUT /api/policy replaces the document
// that authorises every request flowing through the data plane. One
// unauthenticated call to it turns a zero-trust router into an open one. Reads
// are comparatively harmless — they expose topology and cost figures — so the
// two are gated separately, and only writes are locked down by default.
type Authenticator struct {
	// tokenHash is the SHA-256 of the configured token. Comparing hashes of a
	// fixed width means the comparison time cannot vary with how many leading
	// characters a guess got right, which a length-varying compare of the raw
	// strings would leak.
	tokenHash [32]byte
	hasToken  bool

	requireForReads bool
	allowAnonWrites bool
	log             *slog.Logger

	denied atomic.Uint64
}

// NewAuthenticator builds the gate from configuration.
func NewAuthenticator(token string, requireForReads, allowAnonymousMutations bool, log *slog.Logger) *Authenticator {
	if log == nil {
		log = slog.Default()
	}
	a := &Authenticator{
		requireForReads: requireForReads,
		allowAnonWrites: allowAnonymousMutations,
		log:             log,
	}
	if token != "" {
		a.tokenHash = sha256.Sum256([]byte(token))
		a.hasToken = true
	}
	return a
}

// Denied returns how many requests have been rejected, exported so an operator
// can alert on someone probing the admin surface.
func (a *Authenticator) Denied() uint64 { return a.denied.Load() }

// Enabled reports whether a token is configured.
func (a *Authenticator) Enabled() bool { return a.hasToken }

// presented extracts a token from the request.
//
// Both an Authorization header and a dedicated header are accepted. The
// dedicated one exists because the dashboard is a browser and a
// long-lived Authorization header on same-origin fetches is awkward to manage;
// a query parameter is deliberately *not* accepted, because it would land in
// every access log and proxy trace along the path.
func presented(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if v, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(v)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Sluice-Token"))
}

func (a *Authenticator) valid(token string) bool {
	if !a.hasToken || token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1
}

// isMutation reports whether a request changes control-plane state.
func isMutation(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// Middleware enforces the policy on every /api request.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := presented(r)
		authed := a.valid(token)

		if isMutation(r) {
			switch {
			case authed:
			case a.allowAnonWrites:
				// Explicitly configured open write API. The startup banner and
				// the dashboard both say so loudly; nothing more to do here.
			case !a.hasToken:
				a.reject(w, r, http.StatusForbidden, "no_admin_token",
					"this control plane has no API token configured, so mutating requests are refused. "+
						"Set SLUICE_API_TOKEN to enable them.")
				return
			default:
				a.reject(w, r, http.StatusUnauthorized, "invalid_token",
					"a valid bearer token is required for mutating requests")
				return
			}
		} else if a.requireForReads && !authed {
			a.reject(w, r, http.StatusUnauthorized, "invalid_token",
				"this control plane requires a bearer token for all API access")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) reject(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	a.denied.Add(1)

	// Log the attempt without the token itself. A rejected credential is still
	// a credential, and one typo away from being a valid one in the log.
	a.log.Warn("control-plane API request refused",
		"code", code,
		"method", r.Method,
		"path", r.URL.Path,
		"peer", peerIP(r),
		"agent", truncate(r.UserAgent(), 80),
	)

	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sluice"`)
	}
	writeJSON(w, status, map[string]string{
		"error":  code,
		"detail": msg,
	})
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
