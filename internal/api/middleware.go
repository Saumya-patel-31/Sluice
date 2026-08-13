package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

type ctxKey int

const ctxRequestID ctxKey = iota

// HeaderRequestID carries the correlation identifier in and out.
const HeaderRequestID = "X-Request-Id"

// RequestID returns the correlation identifier for a request, or "" if the
// middleware did not run.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// withRequestID attaches a correlation identifier to every request.
//
// It reuses an inbound X-Request-Id, or the trace-id from a W3C traceparent
// header, before generating one. A control plane that mints its own identifier
// unconditionally produces logs that cannot be joined to the caller's trace,
// which is exactly the join an operator needs when a decision looks wrong.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(HeaderRequestID))
		if id == "" {
			id = traceIDFromTraceparent(r.Header.Get("traceparent"))
		}
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// traceparent is version-traceid-spanid-flags; the trace id is the second
// field and is 32 hex characters.
func traceIDFromTraceparent(v string) string {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) < 3 || len(parts[1]) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return parts[1]
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A correlation id is not a security boundary; a time-derived
		// fallback is better than failing the request.
		return "req-" + time.Now().Format("150405.000000")
	}
	return hex.EncodeToString(b[:])
}

// statusWriter records what was actually sent, which the handler chain
// otherwise discards.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.wrote, s.status = true, code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote, s.status = true, http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

// Flush forwards to the wrapped writer. Without this the SSE stream would
// buffer, because wrapping hides the underlying Flusher.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets the standard library reach the original writer for the optional
// interfaces it probes for.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// accessLog records every request at a level chosen by outcome.
//
// Successful reads are logged at debug: on a dashboard polling once a second
// they are pure noise at info. Anything that failed, and anything that changed
// state, is logged at a level an operator will actually see — a control plane
// whose policy document can be replaced needs an audit trail of who replaced
// it and when.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SSE stream is a single request that lasts as long as the tab is
		// open. Logging it on completion would emit one line an hour later
		// with a meaningless duration, so it is logged on open instead.
		streaming := strings.HasSuffix(r.URL.Path, "/stream")
		if streaming {
			log.Info("event stream opened",
				"path", r.URL.Path, "peer", peerIP(r), "requestId", RequestID(r.Context()))
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"durationMs", float64(elapsed.Microseconds()) / 1000,
			"bytes", sw.written,
			"peer", peerIP(r),
			"requestId", RequestID(r.Context()),
		}

		switch {
		case sw.status >= 500:
			log.Error("control-plane request failed", attrs...)
		case sw.status >= 400:
			// 4xx on this surface is a client sending something wrong, or
			// somebody probing it. Either is worth seeing.
			log.Warn("control-plane request rejected", attrs...)
		case isMutation(r):
			log.Info("control-plane state changed", attrs...)
		case streaming:
			log.Info("event stream closed", attrs...)
		default:
			log.Debug("control-plane request", attrs...)
		}
	})
}

// recoverPanics turns a handler panic into a 500 instead of a dropped
// connection.
//
// net/http already recovers per connection, but it aborts the response without
// a status and writes an unstructured trace to stderr. For a component in the
// authorisation path, a panic needs to produce a real log line an alert can
// match on, and a response the caller can distinguish from a network failure.
func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A cancelled request unwinds through here on some paths; it is
			// not a bug and should not be logged as one.
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Error("control-plane handler panicked",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"requestId", RequestID(r.Context()),
				"stack", string(debug.Stack()),
			)
			// The handler may already have written; WriteHeader is then a
			// no-op and the client sees a truncated body, which is the best
			// available outcome.
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error":     "internal_error",
				"requestId": RequestID(r.Context()),
			})
		}()
		next.ServeHTTP(w, r)
	})
}
