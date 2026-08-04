package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// startFakeA2APartner spins up an httptest server impersonating
// a partner A2A agent. The submit endpoint records the inbound
// body + returns a task id + stream URL; the stream endpoint
// emits SSE frames the test scripts via the streamScript field.
type fakePartner struct {
	mu            sync.Mutex
	gotSubmitBody []byte
	gotAPIKey     string
	streamScript  []string // raw SSE frames to emit, one chunk per element
	streamDelay   time.Duration
	server        *httptest.Server
}

func newFakePartner(t *testing.T, script []string) *fakePartner {
	t.Helper()
	fp := &fakePartner{streamScript: script}
	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.gotAPIKey = r.Header.Get("X-API-Key")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		fp.gotSubmitBody = body
		fp.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId":    "task-fake-1",
			"status":    "submitted",
			"streamUrl": "/tasks/task-fake-1",
		})
	})
	mux.HandleFunc("/tasks/task-fake-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range fp.streamScript {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if fp.streamDelay > 0 {
				time.Sleep(fp.streamDelay)
			}
		}
	})
	fp.server = httptest.NewServer(mux)
	return fp
}

func (fp *fakePartner) URL() string { return fp.server.URL }
func (fp *fakePartner) Close()      { fp.server.Close() }

func sseFrame(event, jsonData string) string {
	return "event: " + event + "\ndata: " + jsonData + "\n\n"
}

func TestHandleA2ACallStep_HappyPath(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"working","final":false}`),
		sseFrame("message", `{"text":"hello operator"}`),
		sseFrame("status", `{"taskId":"task-fake-1","state":"completed","final":true}`),
	})
	defer partner.Close()

	e := &Executor{}
	step := &registry.WorkflowStep{
		Type:     "a2a_call",
		AgentURL: partner.URL(),
		Prompt:   "say hi",
	}
	res, err := e.handleA2ACallStep(context.Background(), "step1", step)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if res.State != "completed" {
		t.Errorf("final state: %q", res.State)
	}
	if res.Text != "hello operator" {
		t.Errorf("text: %q", res.Text)
	}
	if !strings.Contains(string(partner.gotSubmitBody), "say hi") {
		t.Errorf("submit body missing prompt: %s", partner.gotSubmitBody)
	}
}

func TestHandleA2ACallStep_PartnerFailed_ReturnsError(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"failed","final":true}`),
	})
	defer partner.Close()

	e := &Executor{}
	step := &registry.WorkflowStep{
		Type:     "a2a_call",
		AgentURL: partner.URL(),
		Prompt:   "test",
	}
	res, err := e.handleA2ACallStep(context.Background(), "step1", step)
	if err == nil {
		t.Fatal("expected error for partner failed state")
	}
	if res == nil || res.State != "failed" {
		t.Errorf("result on failure: %#v", res)
	}
}

func TestHandleA2ACallStep_InputRequiredIsNotHandledYet(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"input-required","final":true}`),
	})
	defer partner.Close()

	e := &Executor{}
	res, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "x",
	})
	if err == nil {
		t.Fatal("expected error for input-required (Phase C will handle)")
	}
	if res == nil || res.State != "input-required" {
		t.Errorf("result: %#v", res)
	}
}

func TestHandleA2ACallStep_AccumulatesMultipleTextParts(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("message", `{"text":"line 1"}`),
		sseFrame("message", `{"parts":[{"type":"text","text":"line 2"}]}`),
		sseFrame("status", `{"taskId":"task-fake-1","state":"completed","final":true}`),
	})
	defer partner.Close()

	e := &Executor{}
	res, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "go",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(res.Text, "line 1") || !strings.Contains(res.Text, "line 2") {
		t.Errorf("text accumulation lost a line: %q", res.Text)
	}
}

func TestHandleA2ACallStep_APIKeyFromEnv(t *testing.T) {
	partner := newFakePartner(t, []string{
		sseFrame("status", `{"taskId":"task-fake-1","state":"completed","final":true}`),
	})
	defer partner.Close()

	t.Setenv("TEST_PARTNER_KEY", "secret-key-42")
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "go", APIKeyEnv: "TEST_PARTNER_KEY",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if partner.gotAPIKey != "secret-key-42" {
		t.Errorf("partner got API key %q, want %q", partner.gotAPIKey, "secret-key-42")
	}
}

func TestHandleA2ACallStep_AgentURLRequired(t *testing.T) {
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", Prompt: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "agent_url is required") {
		t.Errorf("want agent_url required error, got %v", err)
	}
}

func TestHandleA2ACallStep_PromptRequired(t *testing.T) {
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: "https://partner.example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("want prompt required error, got %v", err)
	}
}

func TestHandleA2ACallStep_SubmitHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: srv.URL, Prompt: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("want HTTP 500 error, got %v", err)
	}
}

func TestHandleA2ACallStep_TimeoutHonoured(t *testing.T) {
	partner := newFakePartner(t, []string{
		// First status frame, then long delay — should hit ctx deadline.
		sseFrame("status", `{"taskId":"task-fake-1","state":"working","final":false}`),
	})
	partner.streamDelay = 2 * time.Second
	defer partner.Close()

	e := &Executor{}
	start := time.Now()
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL(), Prompt: "x",
		Timeout: "200ms",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout not honoured; elapsed %v", elapsed)
	}
}

func TestA2AHTTPClientRejectsCrossOriginRedirectWithAPIKey(t *testing.T) {
	var leakedKey string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	partner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/tasks", http.StatusFound)
	}))
	defer partner.Close()

	t.Setenv("TEST_PARTNER_KEY", "secret-key-42")
	e := &Executor{}
	_, err := e.handleA2ACallStep(context.Background(), "step1", &registry.WorkflowStep{
		Type: "a2a_call", AgentURL: partner.URL, Prompt: "x", APIKeyEnv: "TEST_PARTNER_KEY",
	})
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("want redirect surfaced as HTTP 302, got %v", err)
	}
	if leakedKey != "" {
		t.Fatalf("cross-origin redirect leaked X-API-Key: %q", leakedKey)
	}
}
