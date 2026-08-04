package consult

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
)

// stubExpert is a minimal A2A inbound: POST /tasks → task id + path stream URL;
// GET the stream → an artifact answer + terminal status. It records the submit
// body so tests can assert the outbound metadata (hop counter).
type stubExpert struct {
	mu         sync.Mutex
	lastBody   []byte
	statusCode int // 0 = normal; non-zero = fail /tasks with this code
	answer     string
	server     *httptest.Server
}

func newStubExpert(t *testing.T, answer string) *stubExpert {
	t.Helper()
	s := &stubExpert{answer: answer}
	mux := http.NewServeMux()
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		s.mu.Lock()
		s.lastBody = body
		code := s.statusCode
		s.mu.Unlock()
		if code != 0 {
			http.Error(w, "expert down", code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"taskId": "T1", "status": "submitted", "streamUrl": "/tasks/T1"})
	})
	mux.HandleFunc("/tasks/T1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		frames := []string{
			"event: status\ndata: {\"taskId\":\"T1\",\"state\":\"working\",\"final\":false}\n\n",
			"event: artifact\ndata: {\"taskId\":\"T1\",\"payload\":{\"answer\":\"" + s.answer + "\"}}\n\n",
			"event: status\ndata: {\"taskId\":\"T1\",\"state\":\"completed\",\"final\":true}\n\n",
		}
		for _, f := range frames {
			_, _ = w.Write([]byte(f))
			if fl != nil {
				fl.Flush()
			}
		}
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubExpert) submittedMetadata() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req struct {
		Metadata map[string]any `json:"metadata"`
	}
	_ = json.Unmarshal(s.lastBody, &req)
	return req.Metadata
}

func newProviderFor(peers map[string]config.A2APeer, consult config.A2AConsultConfig, hops TaskHopLookup) *Provider {
	return New(peers, consult, client.New(), hops)
}

func TestProvider_Tools_OnePerPeer(t *testing.T) {
	p := newProviderFor(map[string]config.A2APeer{
		"vornik_architecture": {URL: "https://h/a", Description: "Vornik architecture expert."},
		"vornik_config":       {URL: "https://h/c"},
	}, config.A2AConsultConfig{}, nil)

	tools := p.Tools("any")
	names := map[string]string{}
	for _, tl := range tools {
		names[tl.Function.Name] = tl.Function.Description
	}
	if _, ok := names["mcp__consult__vornik_architecture"]; !ok {
		t.Fatalf("missing consult tool for vornik_architecture; got %v", names)
	}
	if names["mcp__consult__vornik_architecture"] != "Vornik architecture expert." {
		t.Errorf("description should come from peer config, got %q", names["mcp__consult__vornik_architecture"])
	}
	if _, ok := names["mcp__consult__vornik_config"]; !ok {
		t.Error("missing consult tool for vornik_config (no description → generated fallback)")
	}
}

func TestProvider_Owns(t *testing.T) {
	p := newProviderFor(nil, config.A2AConsultConfig{}, nil)
	if !p.Owns("mcp__consult__vornik_architecture") {
		t.Error("should own mcp__consult__ names")
	}
	if p.Owns("mcp__pagedrop__publish") || p.Owns("memory_search") {
		t.Error("must not own non-consult names")
	}
}

func TestProvider_Execute_HappyPath(t *testing.T) {
	expert := newStubExpert(t, "Vornik uses queue-backed leasing.")
	p := newProviderFor(map[string]config.A2APeer{
		"vornik_architecture": {URL: expert.server.URL, InsecureHTTP: true},
	}, config.A2AConsultConfig{}, nil)

	out, err := p.Execute(context.Background(), "proj", "mcp__consult__vornik_architecture", `{"question":"how does leasing work?"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "queue-backed leasing") {
		t.Fatalf("result missing the answer: %q", out)
	}
	if !strings.Contains(out, "Consulted vornik_architecture") {
		t.Errorf("result should carry a provenance prefix: %q", out)
	}
}

func TestProvider_Execute_EmptyQuestion(t *testing.T) {
	p := newProviderFor(map[string]config.A2APeer{"x": {URL: "https://h/a"}}, config.A2AConsultConfig{}, nil)
	out, err := p.Execute(context.Background(), "proj", "mcp__consult__x", `{"question":"  "}`)
	if err != nil {
		t.Fatalf("should return a string result, not a Go error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "question") {
		t.Errorf("empty question should yield a clear result: %q", out)
	}
}

func TestProvider_Execute_ExpertUnavailable(t *testing.T) {
	expert := newStubExpert(t, "")
	expert.statusCode = http.StatusServiceUnavailable
	p := newProviderFor(map[string]config.A2APeer{
		"x": {URL: expert.server.URL, InsecureHTTP: true},
	}, config.A2AConsultConfig{}, nil)

	out, err := p.Execute(context.Background(), "proj", "mcp__consult__x", `{"question":"q"}`)
	if err != nil {
		t.Fatalf("expert failure must be a clean string result, not a Go error/panic: %v", err)
	}
	if !strings.Contains(out, "did not return") || !strings.Contains(strings.ToLower(out), "stale") {
		t.Errorf("expert-unavailable should yield a clear result that discourages a stale-memory fallback: %q", out)
	}
}

func TestProvider_Execute_MaxCallsPerTask(t *testing.T) {
	expert := newStubExpert(t, "ok")
	p := newProviderFor(map[string]config.A2APeer{
		"x": {URL: expert.server.URL, InsecureHTTP: true},
	}, config.A2AConsultConfig{MaxCallsPerTask: 2}, nil)

	ctx := context.WithValue(context.Background(), mcp.TaskIDHeaderKey{}, "task-1")
	for i := 0; i < 2; i++ {
		if out, _ := p.Execute(ctx, "proj", "mcp__consult__x", `{"question":"q"}`); strings.Contains(strings.ToLower(out), "budget") {
			t.Fatalf("call %d should be allowed, got budget error: %q", i+1, out)
		}
	}
	out, _ := p.Execute(ctx, "proj", "mcp__consult__x", `{"question":"q"}`)
	if !strings.Contains(strings.ToLower(out), "budget") {
		t.Fatalf("3rd call for the same task must hit the budget cap: %q", out)
	}
}

// stubHops reports a fixed inbound hop count.
type stubHops struct{ n int }

func (s stubHops) InboundConsultHops(_ context.Context, _ string) int { return s.n }

func TestProvider_Execute_HopMetadataPropagated(t *testing.T) {
	expert := newStubExpert(t, "ok")
	p := newProviderFor(map[string]config.A2APeer{
		"x": {URL: expert.server.URL, InsecureHTTP: true},
	}, config.A2AConsultConfig{}, stubHops{n: 1}) // inbound 1 → outbound 2

	ctx := context.WithValue(context.Background(), mcp.TaskIDHeaderKey{}, "task-1")
	if _, err := p.Execute(ctx, "proj", "mcp__consult__x", `{"question":"q"}`); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	md := expert.submittedMetadata()
	if md == nil {
		t.Fatal("expert received no metadata")
	}
	// JSON numbers decode to float64.
	if got, _ := md[config.ConsultHopHeader].(float64); int(got) != 2 {
		t.Fatalf("hop header = %v, want inbound(1)+1 = 2", md[config.ConsultHopHeader])
	}
}
