package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestConsumeA2ASSEStream_EarlyReturnDoesNotLeakProducer is the
// regression for the 2026-06-04 bug-sweep finding: the SSE scanner
// goroutine sent lines into a 1-slot channel with a bare blocking
// send. resp.Body.Close() on the consumer's early return only
// releases a producer blocked in a socket READ — a producer parked on
// the channel SEND with undrained lines still in the bufio buffer
// stays parked forever (one goroutine + its 4 MiB scanner buffer per
// affected call).
//
// Reaching that park deterministically: the partner emits one large
// burst whose frame-by-frame drain takes far longer than the caller's
// ctx deadline. The ctx fires mid-drain; the consumer's select (ctx
// ready + scanCh ready, uniform choice) exits within a few
// iterations, leaving thousands of buffered lines behind — the
// producer parks on the next send. Post-fix the defer-closed
// consumerGone channel releases it.
func TestConsumeA2ASSEStream_EarlyReturnDoesNotLeakProducer(t *testing.T) {
	// ~70k frames ≈ 210k scanner lines ≈ well over 30ms of drain work.
	var burst strings.Builder
	for i := 0; i < 70_000; i++ {
		burst.WriteString(sseFrame("message", `{"text":"chunk-chunk-chunk"}`))
	}
	payload := burst.String()

	stop := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Keep the connection open: the leak is a parked SEND, not a
		// read EOF, and a hung partner is the canonical trigger.
		<-stop
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	before := runtime.NumGoroutine()

	const iterations = 4
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, _, _ = consumeA2ASSEStream(ctx, srv.URL+"/stream", "")
		cancel()
	}

	// Release the server handlers so only vornik-side goroutines can
	// remain above baseline in the settle check.
	close(stop)

	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if delta := runtime.NumGoroutine() - before; delta < 2 {
			return // settled at baseline — no per-call leak
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: before=%d after=%d (want growth < 2 after %d calls) — SSE producer parked on channel send",
				before, runtime.NumGoroutine(), iterations)
		}
	}
}
