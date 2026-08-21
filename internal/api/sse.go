package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Saumya-patel-31/sluice/internal/model"
)

// handleStream serves the dashboard's live feed over server-sent events.
//
// SSE rather than WebSockets: the traffic is entirely one-directional, it
// survives ordinary HTTP proxies without an upgrade negotiation, and browsers
// reconnect on their own. The dashboard needs no send channel, so a
// bidirectional transport would only add failure modes.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	// Reads are unauthenticated, so this is a resource-exhaustion path that
	// needs no credentials: every subscriber holds a goroutine and a buffered
	// channel until its connection closes. Refusing past the ceiling keeps a
	// runaway client from starving the control loop of memory, and 503 with
	// Retry-After tells a well-behaved one what to do about it.
	if n := s.streams.Add(1); n > s.maxStreams {
		s.streams.Add(-1)
		s.log.Warn("event stream refused: subscriber limit reached",
			"limit", s.maxStreams, "peer", peerIP(r), "requestId", RequestID(r.Context()))
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "too_many_streams",
			"detail": "this control plane is already serving its maximum of " +
				strconv.FormatInt(s.maxStreams, 10) + " event streams",
			"limit": s.maxStreams,
		})
		return
	}
	defer s.streams.Add(-1)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer, or events arrive in clumps.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	decisions, unsubscribe := s.app.Ledger.Subscribe(512)
	defer unsubscribe()

	overviewEvery := time.Duration(queryInt(r, "overviewMs", 1000, 250, 10000)) * time.Millisecond
	feedEvery := time.Duration(queryInt(r, "feedMs", 300, 100, 5000)) * time.Millisecond
	seriesPoints := queryInt(r, "points", 120, 8, 600)
	// A burst of decisions must not turn into a burst of frames. Batching
	// bounds how much the browser has to parse per second regardless of
	// whether the router is handling ten requests a second or ten thousand.
	batchMax := queryInt(r, "batch", 40, 1, 200)

	overviewTick := time.NewTicker(overviewEvery)
	defer overviewTick.Stop()
	feedTick := time.NewTicker(feedEvery)
	defer feedTick.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	var sb strings.Builder
	send := func(event string, payload any) bool {
		sb.Reset()
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		sb.WriteString("event: ")
		sb.WriteString(event)
		sb.WriteString("\ndata: ")
		sb.Write(b)
		sb.WriteString("\n\n")
		if _, err := fmt.Fprint(w, sb.String()); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send("overview", s.buildOverview(seriesPoints)) {
		return
	}

	pending := make([]map[string]any, 0, batchMax)
	// dropped counts decisions discarded because the batch was already full.
	// Reporting it lets the UI say "showing a sample" honestly rather than
	// implying the feed is complete.
	var dropped int

	for {
		select {
		case <-ctx.Done():
			return

		case d := <-decisions:
			if d == nil {
				continue
			}
			if len(pending) < batchMax {
				pending = append(pending, briefDecision(d))
			} else {
				dropped++
			}

		case <-feedTick.C:
			if len(pending) == 0 && dropped == 0 {
				continue
			}
			ok := send("decisions", map[string]any{
				"decisions": pending,
				"sampled":   dropped > 0,
				"dropped":   dropped,
			})
			pending = pending[:0]
			dropped = 0
			if !ok {
				return
			}

		case <-overviewTick.C:
			if !send("overview", s.buildOverview(seriesPoints)) {
				return
			}

		case <-heartbeat.C:
			// A bare comment keeps intermediaries from reaping an idle
			// connection during a quiet period.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// compile-time assurance that briefDecision keeps accepting the ledger's type.
var _ = func(d *model.Decision) map[string]any { return briefDecision(d) }
