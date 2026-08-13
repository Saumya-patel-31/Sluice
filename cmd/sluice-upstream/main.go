// Command sluice-upstream is a synthetic origin server.
//
// It stands in for a real regional deployment in the container and Kubernetes
// demos, where the in-process simulator is not available because Sluice is
// running as a control plane against genuinely separate hosts. Latency and
// error behaviour are configurable so a compose file can describe a region
// that is fast and dirty, or slow and clean, and the router has something real
// to measure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	var (
		addr      = flag.String("listen", envOr("LISTEN", ":8080"), "listen address")
		name      = flag.String("name", envOr("REGION", "unnamed"), "region name reported in responses")
		latencyMs = flag.Float64("latency-ms", envFloat("LATENCY_MS", 25), "median response latency")
		errorRate = flag.Float64("error-rate", envFloat("ERROR_RATE", 0.001), "fraction of requests that fail")
		jitter    = flag.Float64("jitter", envFloat("JITTER", 0.22), "lognormal spread of the latency distribution")
	)
	flag.Parse()

	var served, failed atomic.Int64
	base := time.Duration(*latencyMs * float64(time.Millisecond))

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// A probe traverses the same network as a request but does not pay the
		// full processing cost, so it sees roughly half the latency.
		time.Sleep(sample(base/2, *jitter))
		if rand.Float64() < *errorRate {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Sluice-Backend", *name)
		_, _ = io.WriteString(w, "ok")
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"region":%q,"served":%d,"failed":%d,"latencyMs":%v,"errorRate":%v}`,
			*name, served.Load(), failed.Load(), *latencyMs, *errorRate)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sample(base, *jitter))
		served.Add(1)

		if rand.Float64() < *errorRate {
			failed.Add(1)
			http.Error(w, "upstream failure", http.StatusBadGateway)
			return
		}

		size := 8 << 10
		if v := r.Header.Get("X-Sluice-Size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 8<<20 {
				size = n
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Sluice-Backend", *name)
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(body(size))
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("sluice-upstream %s listening on %s (%.0fms, %.2f%% errors)",
			*name, *addr, *latencyMs, *errorRate*100)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	log.Printf("sluice-upstream %s stopped after %d requests", *name, served.Load())
}

// sample draws from a lognormal body with an occasional heavy tail, so p95 is
// meaningfully worse than p50 the way it is on a real service.
func sample(base time.Duration, spread float64) time.Duration {
	if base <= 0 {
		return 0
	}
	v := float64(base) * math.Exp(spread*rand.NormFloat64())
	if rand.Float64() < 0.02 {
		v *= 2.5 + 5*rand.Float64()
	}
	return time.Duration(math.Max(v, float64(time.Millisecond)))
}

var payload []byte

func body(n int) []byte {
	if len(payload) < n {
		payload = make([]byte, n)
		for i := range payload {
			payload[i] = byte('a' + i%26)
		}
	}
	return payload[:n]
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}
