// LATENCY GUARD for the chat-proxy hot path.
//
// EXPECTED RESULT ON CURRENT CODE: FAILS (fails_until_fix).
//
// What this proves: the external /v1/chat/completions handler
// (Server.ChatCompletions in chat_proxy.go) currently runs its
// telemetry sinks SYNCHRONOUSLY, before the response is flushed to
// the client:
//
//	chat_proxy.go:285  s.recordChatAPIUsage(...)   -> llmUsageRepo.Record  (Postgres INSERT)
//	chat_proxy.go:290  s.recordChatAPIAudit(...)   -> chatAuditRepo writes (Postgres INSERT x2)
//	chat_proxy.go:311  s.emitLLMCallFinished(...)  -> live pub/sub
//	chat_proxy.go:314  w.WriteHeader(http.StatusOK)  <-- response only starts HERE
//
// Because Record() runs at line 285 and WriteHeader runs at line 314,
// a slow usage/audit sink blocks the client's bytes for the full
// duration of those DB writes. These taps are recent (Feature #3
// llm_call_*; commits d186434e / a5bea199 / 48cbb204) and are the
// prime suspect for the perceived "the endpoint got slower" lag —
// every EXTERNAL caller (no X-Vornik-Task-ID / X-Vornik-Execution-ID
// header) pays the cost of two Postgres inserts on the response path.
//
// THE SEAM (seam_for_slow_sink): the injectable
// persistence.TaskLLMUsageRepository wired via
// WithLLMUsageRepository. recordChatAPIUsage calls repo.Record on the
// hot path, so a Record that blocks on a channel stands in for a slow
// Postgres write deterministically (no sleeps, no real DB, no real
// network). The recorder's own doc comment even claims "The chat
// response is already on its way to the client; we don't fail the
// request because the cost row didn't land." — this test pins that
// stated contract, which the current ORDERING violates.
//
// THE FIX this test guards: move recordChatAPIUsage /
// recordChatAPIAudit / emitLLMCallFinished OFF the hot path — fire
// them after WriteHeader+Encode (or in a detached goroutine) so the
// client gets its bytes regardless of how slow the telemetry sink is.
// Once that lands, this test PASSES.
//
// Reuses package helpers: recordingUsageRepo (chat_cost_metrics_test.go),
// stubChatProviderWithUsage (chat_cost_metrics_test.go),
// NewServer/WithChatProvider/WithLLMUsageRepository (api.go).
// All new symbols are prefixed "lg" per the file's assignment.

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// lgBlockingUsageRepo is the deliberately-slow sink. Record blocks
// until the test closes `release`, standing in for a Postgres INSERT
// that takes a long time. It also signals (via recordEntered) the
// instant the handler enters Record, so the test can prove ordering:
// on the current synchronous code the handler enters Record BEFORE it
// writes the response, so the response never lands while Record is
// parked. Embeds recordingUsageRepo so every other
// TaskLLMUsageRepository method is inherited (no re-declaration).
type lgBlockingUsageRepo struct {
	recordingUsageRepo
	recordEntered chan struct{} // closed-once when Record is first entered
	release       chan struct{} // Record returns only after this is closed
	once          sync.Once
}

func (r *lgBlockingUsageRepo) Record(ctx context.Context, u *persistence.TaskLLMUsage) error {
	r.once.Do(func() { close(r.recordEntered) })
	<-r.release // park here, simulating a slow DB write
	return r.recordingUsageRepo.Record(ctx, u)
}

// lgSignalingResponseWriter wraps an httptest.ResponseRecorder and
// closes `wroteHeader` the moment the handler flushes response status
// to the client (WriteHeader, or an implicit WriteHeader via Write).
// This is the safe, race-free way to observe "the client got its
// response" while the handler goroutine is still running — reading the
// ResponseRecorder's fields directly from another goroutine would
// race. We only signal via a channel and never touch recorder state
// until the handler goroutine has returned.
type lgSignalingResponseWriter struct {
	rec         *httptest.ResponseRecorder
	wroteHeader chan struct{}
	once        sync.Once
}

func (w *lgSignalingResponseWriter) Header() http.Header { return w.rec.Header() }

func (w *lgSignalingResponseWriter) WriteHeader(code int) {
	w.signal()
	w.rec.WriteHeader(code)
}

func (w *lgSignalingResponseWriter) Write(b []byte) (int, error) {
	w.signal() // implicit 200 if WriteHeader wasn't called
	return w.rec.Write(b)
}

func (w *lgSignalingResponseWriter) signal() {
	w.once.Do(func() { close(w.wroteHeader) })
}

// TestChatCompletions_ResponseDoesNotWaitForSlowUsageSink is THE
// latency guard. It injects a usage sink whose Record blocks
// indefinitely and asserts the client's response is flushed WITHOUT
// waiting for that sink.
//
// ON CURRENT CODE THIS FAILS: recordChatAPIUsage (chat_proxy.go:285)
// calls the blocking Record before WriteHeader (chat_proxy.go:314),
// so wroteHeader never fires while Record is parked -> the test's
// "response flushed" wait times out -> t.Fatalf. That timeout IS the
// bug being demonstrated.
//
// AFTER THE FIX (telemetry moved off the hot path): WriteHeader fires
// before/independently of the parked Record, the wroteHeader channel
// closes promptly, and the test passes.
func TestChatCompletions_ResponseDoesNotWaitForSlowUsageSink(t *testing.T) {
	repo := &lgBlockingUsageRepo{
		recordEntered: make(chan struct{}),
		release:       make(chan struct{}),
	}
	// Non-zero usage so recordChatAPIUsage doesn't early-return on the
	// "no token counts" guard (chat_proxy.go:415) and actually reaches
	// repo.Record.
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 100, completToks: 50,
	}
	s := NewServer(
		WithChatProvider(prov),
		WithLLMUsageRepository(repo),
	)

	// External caller: no X-Vornik-Task-ID / X-Vornik-Execution-ID, so
	// the usage recorder is NOT skipped — this is exactly the traffic
	// class that regressed.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("User-Agent", "latency-guard/1.0")

	rec := httptest.NewRecorder()
	w := &lgSignalingResponseWriter{rec: rec, wroteHeader: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ChatCompletions(w, req)
	}()

	// Make sure the handler actually reached the sink before we judge
	// it. If the handler finishes without ever entering Record (e.g. a
	// future refactor that drops the recorder for external calls), the
	// recordEntered wait below would hang — so we bound it and treat a
	// timeout as "sink never invoked", which is fine for this guard's
	// purpose (the response-flush assertion still governs pass/fail).
	select {
	case <-repo.recordEntered:
		// Handler is now inside the (parked) slow sink. The whole point:
		// the client's response must already be on the wire by now.
	case <-w.wroteHeader:
		// Response flushed before the sink was even entered — this is
		// the desired post-fix behaviour. Fall through to the success
		// path below.
	case <-time.After(2 * time.Second):
		t.Fatalf("handler neither flushed the response nor entered the usage sink within 2s")
	}

	// THE ASSERTION: with the slow sink still parked (release NOT
	// closed), the response must already be flushed to the client.
	select {
	case <-w.wroteHeader:
		// PASS path (post-fix): the response was written without
		// waiting for the telemetry sink to complete.
	case <-time.After(500 * time.Millisecond):
		// FAIL path (current code): WriteHeader is downstream of the
		// synchronous Record at chat_proxy.go:285, so the client gets
		// nothing while Record is parked. This timeout proves the
		// latency bug.
		close(repo.release) // unblock the handler so the goroutine can exit
		<-done
		t.Fatalf("FAILS_UNTIL_FIX: chat response was NOT flushed to the client " +
			"while the usage/audit sink was still blocked. recordChatAPIUsage " +
			"(chat_proxy.go:285) runs synchronously BEFORE WriteHeader " +
			"(chat_proxy.go:314), so a slow Postgres INSERT stalls every external " +
			"caller. Fix: move recordChatAPIUsage/recordChatAPIAudit/emitLLMCallFinished " +
			"off the hot path (run them after the response is flushed).")
	}

	// Release the sink and let the handler goroutine finish cleanly so
	// the test doesn't leak a blocked goroutine.
	close(repo.release)
	<-done

	// Sanity: the response that reached the client is a normal 200 with
	// a body. Safe to read recorder state now that the handler goroutine
	// has returned.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Error("response body is empty; client got 200 with no payload")
	}

	// And the telemetry still eventually lands (best-effort, off the
	// hot path) — the fix must not DROP the cost row, only defer it.
	// Post-fix it runs in a detached goroutine that outlives the handler,
	// so poll the (mutex-guarded) recorder rather than assuming it's done
	// the instant <-done fires.
	deadline := time.Now().Add(2 * time.Second)
	for len(repo.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := len(repo.recorded()); got != 1 {
		t.Errorf("recorded %d usage rows, want 1 (telemetry must be deferred, not dropped)", got)
	}
}
