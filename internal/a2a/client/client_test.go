package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubAgent impersonates a vornik A2A inbound: POST /tasks returns a
// task id + path-only stream URL; GET the stream emits the scripted
// SSE frames.
func stubAgent(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId":    "T1",
			"status":    "submitted",
			"streamUrl": "/tasks/T1",
		})
	})
	mux.HandleFunc("/tasks/T1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			if fl != nil {
				fl.Flush()
			}
		}
	})
	return httptest.NewServer(mux)
}

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// The vornik inbound delivers the task's answer as an `event: artifact`
// frame carrying the ResultEnvelope (a2a-expert-federation-design §7).
// The pre-extraction consumer ignored artifact frames, so the answer
// never reached the caller. This is the regression that pins the fix.
func TestClient_Call_ReadsArtifactAnswer(t *testing.T) {
	srv := stubAgent(t, []string{
		frame("status", `{"taskId":"T1","state":"working","final":false}`),
		frame("artifact", `{"taskId":"T1","payload":{"answer":"grounded product answer","citations":["doc-1"]}}`),
		frame("status", `{"taskId":"T1","state":"completed","final":true}`),
	})
	defer srv.Close()

	res, err := New().Call(context.Background(), CallRequest{
		AgentURL: srv.URL,
		Text:     "how do I configure a project?",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.State != "completed" {
		t.Fatalf("state = %q, want completed", res.State)
	}
	if !strings.Contains(res.Answer, "grounded product answer") {
		t.Fatalf("answer = %q, want it to contain the artifact answer text", res.Answer)
	}
}

// Back-compat: third-party partners deliver text via `event: message`;
// the client must still surface that (the a2a_call step relies on it).
func TestClient_Call_ReadsMessageText(t *testing.T) {
	srv := stubAgent(t, []string{
		frame("status", `{"taskId":"T1","state":"working","final":false}`),
		frame("message", `{"text":"partner reply text"}`),
		frame("status", `{"taskId":"T1","state":"completed","final":true}`),
	})
	defer srv.Close()

	res, err := New().Call(context.Background(), CallRequest{
		AgentURL: srv.URL,
		Text:     "scrape this",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(res.Answer, "partner reply text") {
		t.Fatalf("answer = %q, want message text", res.Answer)
	}
}

func TestClient_Call_FailedStateReturnsError(t *testing.T) {
	srv := stubAgent(t, []string{
		frame("status", `{"taskId":"T1","state":"failed","final":true}`),
	})
	defer srv.Close()

	_, err := New().Call(context.Background(), CallRequest{
		AgentURL: srv.URL, Text: "x", Timeout: 5 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error on failed terminal state")
	}
}
