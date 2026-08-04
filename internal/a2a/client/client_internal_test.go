package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolveStreamURL(t *testing.T) {
	cases := []struct {
		agent, stream, want string
		wantErr             bool
	}{
		{"https://h.example.com/a2a/v1/agents/p/w", "https://h.example.com/a2a/v1/agents/p/w/tasks/T", "https://h.example.com/a2a/v1/agents/p/w/tasks/T", false},
		{"http://localhost:8080/a2a/v1/agents/p/w", "/a2a/v1/agents/p/w/tasks/T", "http://localhost:8080/a2a/v1/agents/p/w/tasks/T", false},
		{"https://h.example.com/a2a/v1/agents/p/w", "https://evil.example.net/steal", "", true},
		{"https://h.example.com/a2a/v1/agents/p/w", "tasks/T", "", true},
	}
	for _, tc := range cases {
		got, err := resolveStreamURL(tc.agent, tc.stream)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveStreamURL(%q,%q) should error", tc.agent, tc.stream)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveStreamURL(%q,%q): %v", tc.agent, tc.stream, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveStreamURL(%q,%q) = %q, want %q", tc.agent, tc.stream, got, tc.want)
		}
	}
}

func TestTruncateForLog(t *testing.T) {
	if truncateForLog("abc", 10) != "abc" {
		t.Errorf("short string changed")
	}
	got := truncateForLog("abcdefghij", 5)
	if !strings.HasSuffix(got, "…") || len(got) >= 10 {
		t.Errorf("truncate: %q", got)
	}
}

func TestAnswerFromArtifact(t *testing.T) {
	cases := []struct {
		name, payload, want string
	}{
		{"answer field", `{"answer":"hi","citations":[]}`, "hi"},
		{"text field", `{"text":"yo"}`, "yo"},
		{"bare string", `"plain"`, "plain"},
		{"no known field returns json", `{"foo":1}`, `{"foo":1}`},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		if got := answerFromArtifact([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: answerFromArtifact(%q) = %q, want %q", tc.name, tc.payload, got, tc.want)
		}
	}
}

func stubStream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(handler)
}

func TestConsumeSSE_StreamCloseWithoutFinal_LastStatusWins(t *testing.T) {
	srv := stubStream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(frame("status", `{"taskId":"x","state":"working"}`)))
	})
	defer srv.Close()

	state, _, _, err := New().consumeSSE(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if state != "working" {
		t.Errorf("state on close: %q", state)
	}
}

func TestConsumeSSE_NoStatusFrameIsError(t *testing.T) {
	srv := stubStream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message\ndata: {}\n\n"))
	})
	defer srv.Close()

	_, _, _, err := New().consumeSSE(context.Background(), srv.URL, "")
	if err == nil {
		t.Error("close without status frames should error")
	}
}

func TestConsumeSSE_StreamHTTPError(t *testing.T) {
	srv := stubStream(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	})
	defer srv.Close()

	_, _, _, err := New().consumeSSE(context.Background(), srv.URL, "key")
	if err == nil || !strings.Contains(err.Error(), "stream HTTP 410") {
		t.Fatalf("expected stream HTTP 410, got %v", err)
	}
}

func TestConsumeSSE_ContextCancelMidStream(t *testing.T) {
	srv := stubStream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(frame("status", `{"taskId":"t","state":"working","final":false}`)))
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(40 * time.Millisecond); cancel() }()
	state, _, _, err := New().consumeSSE(ctx, srv.URL, "")
	if err == nil {
		t.Fatal("expected ctx-cancel error from stalled stream")
	}
	if state != "working" {
		t.Errorf("last-seen state = %q, want working", state)
	}
}

func TestConsumeSSE_FlushLastFrameOnClose(t *testing.T) {
	srv := stubStream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(": keepalive\n"))
		_, _ = w.Write([]byte(frame("status", `{"taskId":"t","state":"completed","final":false}`)))
		_, _ = w.Write([]byte("event: message\ndata: {\"text\":\"final words\"}\n"))
		if fl != nil {
			fl.Flush()
		}
		// close without a terminating blank line
	})
	defer srv.Close()

	state, answer, _, err := New().consumeSSE(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatalf("clean close after status frame should not error: %v", err)
	}
	if state != "completed" {
		t.Errorf("state = %q, want completed", state)
	}
	if answer != "final words" {
		t.Errorf("answer = %q, want 'final words'", answer)
	}
}

// TestConsumeSSE_EarlyReturnDoesNotLeakProducer is the regression for
// the 2026-06-04 bug-sweep finding: the SSE scanner goroutine sent
// lines into a 1-slot channel with a bare blocking send. An early
// return (ctx cancel / idle) with undrained lines in the bufio buffer
// left the producer parked on the channel SEND forever (one goroutine +
// its 4 MiB scanner buffer per affected call). The defer-closed
// consumerGone channel releases it.
func TestConsumeSSE_EarlyReturnDoesNotLeakProducer(t *testing.T) {
	var burst strings.Builder
	for i := 0; i < 70_000; i++ {
		burst.WriteString(frame("message", `{"text":"chunk-chunk-chunk"}`))
	}
	payload := burst.String()

	stop := make(chan struct{})
	srv := stubStream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payload))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-stop
	})
	defer srv.Close()

	before := runtime.NumGoroutine()
	const iterations = 4
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		_, _, _, _ = New().consumeSSE(ctx, srv.URL, "")
		cancel()
	}
	close(stop)

	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		if delta := runtime.NumGoroutine() - before; delta < 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines leaked: before=%d after=%d — SSE producer parked on channel send",
				before, runtime.NumGoroutine())
		}
	}
}

func TestClient_Call_OversizedSubmitResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxSubmitResponseBytes+1)))
	}))
	defer srv.Close()

	_, err := New().Call(context.Background(), CallRequest{AgentURL: srv.URL, Text: "x", Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "submit response exceeds") {
		t.Fatalf("expected oversized-submit error, got %v", err)
	}
}
