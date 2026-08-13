package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRequestIDReusesInboundCorrelation(t *testing.T) {
	var seen string
	h := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	t.Run("an explicit request id is kept", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set(HeaderRequestID, "caller-supplied-id")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if seen != "caller-supplied-id" {
			t.Errorf("context id = %q", seen)
		}
		if rec.Header().Get(HeaderRequestID) != "caller-supplied-id" {
			t.Error("the id should be echoed so the caller can correlate")
		}
	})

	t.Run("a W3C traceparent supplies the trace id", func(t *testing.T) {
		// Minting a fresh id here would produce logs that cannot be joined to
		// the caller's trace, which is the join an operator actually needs.
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		h.ServeHTTP(httptest.NewRecorder(), req)

		if seen != "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Errorf("trace id = %q", seen)
		}
	})

	t.Run("a malformed traceparent falls back to a generated id", func(t *testing.T) {
		for _, bad := range []string{"garbage", "00-short-00f067aa0ba902b7-01", "00-zzzz2f3577b34da6a3ce929d0e0e4736-x-01"} {
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req.Header.Set("traceparent", bad)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if seen == "" || seen == bad {
				t.Errorf("traceparent %q produced id %q", bad, seen)
			}
		}
	})

	t.Run("an absurdly long id is not propagated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Header.Set(HeaderRequestID, strings.Repeat("x", 500))
		h.ServeHTTP(httptest.NewRecorder(), req)
		if len(seen) > 128 {
			t.Errorf("unbounded caller input reached the logs: %d chars", len(seen))
		}
	})
}

func TestRecoverPanicsReturnsJSON(t *testing.T) {
	h := withRequestID(recoverPanics(discardLogger(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler bug")
	})))

	rec := httptest.NewRecorder()
	// The point: this must not propagate and kill the connection.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "requestId") {
		t.Error("the response should carry the correlation id so the caller can quote it")
	}
}

// http.ErrAbortHandler is the standard library's way of dropping a connection
// deliberately. Swallowing it would turn an intentional abort into a 500.
func TestRecoverPanicsRepanicsOnAbort(t *testing.T) {
	h := recoverPanics(discardLogger(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Errorf("recovered %v, want it to propagate", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestAccessLogPreservesStreaming(t *testing.T) {
	// Wrapping the ResponseWriter must not hide its Flusher, or the event
	// stream buffers and the dashboard goes silent.
	var flushed bool
	h := accessLog(discardLogger(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("the wrapped writer lost its Flusher")
			return
		}
		_, _ = w.Write([]byte("data: hi\n\n"))
		f.Flush()
		flushed = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stream", nil))
	if !flushed {
		t.Fatal("handler could not flush")
	}
	if rec.Body.Len() == 0 {
		t.Error("nothing reached the client")
	}
}

func TestStatusWriterRecordsOutcome(t *testing.T) {
	sw := &statusWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}

	// A handler that writes without calling WriteHeader has implicitly sent
	// 200; the recorder has to agree.
	_, _ = sw.Write([]byte("hello"))
	if sw.status != http.StatusOK || sw.written != 5 {
		t.Errorf("status=%d written=%d", sw.status, sw.written)
	}

	// The first WriteHeader wins, as it does in net/http.
	sw2 := &statusWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	sw2.WriteHeader(http.StatusTeapot)
	sw2.WriteHeader(http.StatusInternalServerError)
	if sw2.status != http.StatusTeapot {
		t.Errorf("status = %d, want the first write to win", sw2.status)
	}
}

func TestAuthenticatorRejectsAndCounts(t *testing.T) {
	a := NewAuthenticator("right", false, false, discardLogger())
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := a.Middleware(ok)

	call := func(method, token string) int {
		req := httptest.NewRequest(method, "/api/policy", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(http.MethodGet, ""); got != http.StatusNoContent {
		t.Errorf("unauthenticated read = %d, want 204", got)
	}
	if got := call(http.MethodPut, ""); got != http.StatusUnauthorized {
		t.Errorf("unauthenticated write = %d, want 401", got)
	}
	if got := call(http.MethodPut, "right"); got != http.StatusNoContent {
		t.Errorf("authenticated write = %d, want 204", got)
	}
	if a.Denied() != 1 {
		t.Errorf("denied = %d, want 1", a.Denied())
	}

	// With no token configured at all, writes are refused rather than open.
	none := NewAuthenticator("", false, false, discardLogger())
	rec := httptest.NewRecorder()
	none.Middleware(ok).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/policy", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("no configured token = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "SLUICE_API_TOKEN") {
		t.Error("the refusal should say how to enable writes")
	}

	// The explicit escape hatch opens them again.
	open := NewAuthenticator("", false, true, discardLogger())
	rec2 := httptest.NewRecorder()
	open.Middleware(ok).ServeHTTP(rec2, httptest.NewRequest(http.MethodPut, "/api/policy", nil))
	if rec2.Code != http.StatusNoContent {
		t.Errorf("explicit opt-in = %d, want 204", rec2.Code)
	}
}

func TestAuthenticatorReadGate(t *testing.T) {
	a := NewAuthenticator("right", true, false, discardLogger())
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("read with the gate on = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/overview", nil)
	req.Header.Set("X-Sluice-Token", "right")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("authenticated read = %d, want 204", rec2.Code)
	}
}
