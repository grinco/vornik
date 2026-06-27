package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// logStub is a Provider that returns a scripted response/error and
// optionally implements the optional interfaces, toggled by the
// embedded flags, so the forwarding tests can assert the decorator is
// transparent.
type logStub struct {
	resp        *ChatResponse
	err         error
	model       string
	withModelTo string // non-empty => implements ModelOverridable
	pingErr     error
	pinger      bool
	models      []ModelInfo
	lister      bool
}

func (s *logStub) Complete(_ context.Context, _ []Message) (*ChatResponse, error) {
	return s.resp, s.err
}
func (s *logStub) CompleteWithTools(_ context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	return s.resp, s.err
}
func (s *logStub) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	return s.resp, s.err
}
func (s *logStub) Model() string         { return s.model }
func (s *logStub) SetMetrics(_ *Metrics) {}

func okResponse(content string) *ChatResponse {
	r := &ChatResponse{}
	r.Choices = append(r.Choices, struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}{Index: 0, Message: Message{Role: "assistant", Content: content}, FinishReason: "stop"})
	r.Usage.PromptTokens = 11
	r.Usage.CompletionTokens = 7
	r.Usage.TotalTokens = 18
	return r
}

// parseLogLines splits a zerolog JSON buffer into one map per line.
func parseLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func findMsg(lines []map[string]any, msg string) map[string]any {
	for _, m := range lines {
		if m["message"] == msg {
			return m
		}
	}
	return nil
}

func TestLoggingProvider_MetadataAtInfo_NoContentWhenInfoLevel(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.InfoLevel)
	p := NewLoggingProvider(&logStub{resp: okResponse(`{"confidence":0.82}`), model: "minimax-m2"}, log)

	ctx := WithCallSite(context.Background(), "memetic.architect")
	if _, err := p.Complete(ctx, []Message{{Role: "user", Content: "hello"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	lines := parseLogLines(t, &buf)
	call := findMsg(lines, "llm call")
	if call == nil {
		t.Fatalf("missing 'llm call' INFO line; got %v", lines)
	}
	if call["call_site"] != "memetic.architect" {
		t.Errorf("call_site = %v, want memetic.architect", call["call_site"])
	}
	if call["model"] != "minimax-m2" {
		t.Errorf("model = %v, want minimax-m2", call["model"])
	}
	if call["total_tokens"].(float64) != 18 {
		t.Errorf("total_tokens = %v, want 18", call["total_tokens"])
	}
	// At INFO level, the response body must NOT be logged.
	if findMsg(lines, "llm response") != nil {
		t.Error("response body logged at INFO level; should be DEBUG-only")
	}
}

func TestLoggingProvider_ContentAtDebug(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.DebugLevel)
	const reply = `{"confidence":0.0}`
	p := NewLoggingProvider(&logStub{resp: okResponse(reply), model: "m"}, log)

	if _, err := p.Complete(context.Background(), []Message{{Role: "user", Content: "telemetry rollup…"}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	lines := parseLogLines(t, &buf)
	reqLine := findMsg(lines, "llm request")
	if reqLine == nil || !strings.Contains(reqLine["prompt"].(string), "telemetry rollup") {
		t.Errorf("expected DEBUG request line carrying the prompt; got %v", lines)
	}
	respLine := findMsg(lines, "llm response")
	if respLine == nil || respLine["response"] != reply {
		t.Errorf("expected DEBUG response line carrying %q; got %v", reply, lines)
	}
	// call_site defaults to "unknown" when unset.
	if call := findMsg(lines, "llm call"); call == nil || call["call_site"] != "unknown" {
		t.Errorf("unset call_site should log 'unknown'; got %v", lines)
	}
}

func TestLoggingProvider_ErrorLogsAtWarn(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.InfoLevel)
	sentinel := errors.New("upstream 502")
	p := NewLoggingProvider(&logStub{err: sentinel, model: "m"}, log)

	if _, err := p.Complete(context.Background(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel passthrough", err)
	}
	lines := parseLogLines(t, &buf)
	fail := findMsg(lines, "llm call failed")
	if fail == nil {
		t.Fatalf("missing failure line; got %v", lines)
	}
	if fail["level"] != "warn" {
		t.Errorf("failure level = %v, want warn", fail["level"])
	}
	if !strings.Contains(fail["error"].(string), "502") {
		t.Errorf("failure line should carry the error; got %v", fail["error"])
	}
}

// TestLoggingProvider_CallerCancelLogsAtDebugNotWarn covers the Janka
// first-eval-after-reload noise fix: a context.Canceled error from the
// inner provider is the CALLER's context being torn down (config
// reload / autonomy-loop restart / daemon shutdown), not an LLM
// failure. It must downgrade to a DEBUG "cancelled" line and NOT emit
// the WARN "llm call failed" that misleads the operator.
func TestLoggingProvider_CallerCancelLogsAtDebugNotWarn(t *testing.T) {
	var buf bytes.Buffer
	// Debug level so BOTH tiers can emit — proves the WARN line is
	// suppressed by classification, not by the level filter.
	log := zerolog.New(&buf).Level(zerolog.DebugLevel)
	p := NewLoggingProvider(&logStub{err: context.Canceled, model: "m"}, log)

	_, err := p.Complete(context.Background(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled passthrough", err)
	}
	lines := parseLogLines(t, &buf)
	if fail := findMsg(lines, "llm call failed"); fail != nil {
		t.Errorf("context.Canceled must NOT log 'llm call failed'; got %v", fail)
	}
	cancelled := findMsg(lines, "llm call cancelled (caller context done)")
	if cancelled == nil {
		t.Fatalf("missing cancelled DEBUG line; got %v", lines)
	}
	if cancelled["level"] != "debug" {
		t.Errorf("cancelled line level = %v, want debug", cancelled["level"])
	}
}

// TestLoggingProvider_DeadlineExceededStaysWarn confirms a real
// timeout (DeadlineExceeded) is still a failure — only the caller's
// explicit cancel is benign.
func TestLoggingProvider_DeadlineExceededStaysWarn(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.DebugLevel)
	p := NewLoggingProvider(&logStub{err: context.DeadlineExceeded, model: "m"}, log)

	if _, err := p.Complete(context.Background(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded passthrough", err)
	}
	lines := parseLogLines(t, &buf)
	fail := findMsg(lines, "llm call failed")
	if fail == nil {
		t.Fatalf("DeadlineExceeded must still log 'llm call failed'; got %v", lines)
	}
	if fail["level"] != "warn" {
		t.Errorf("DeadlineExceeded failure level = %v, want warn", fail["level"])
	}
	if findMsg(lines, "llm call cancelled (caller context done)") != nil {
		t.Error("DeadlineExceeded must NOT be downgraded to cancelled")
	}
}

// TestLoggingProvider_GenericErrorStaysWarn confirms an ordinary error
// (not a context cancel) keeps the WARN "llm call failed" path.
func TestLoggingProvider_GenericErrorStaysWarn(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.DebugLevel)
	p := NewLoggingProvider(&logStub{err: errors.New("boom"), model: "m"}, log)

	if _, err := p.Complete(context.Background(), nil); err == nil {
		t.Fatal("expected error passthrough")
	}
	lines := parseLogLines(t, &buf)
	fail := findMsg(lines, "llm call failed")
	if fail == nil || fail["level"] != "warn" {
		t.Fatalf("generic error must log WARN 'llm call failed'; got %v", lines)
	}
	if findMsg(lines, "llm call cancelled (caller context done)") != nil {
		t.Error("generic error must NOT be downgraded to cancelled")
	}
}

func TestLoggingProvider_TruncatesHugeBody(t *testing.T) {
	var buf bytes.Buffer
	log := zerolog.New(&buf).Level(zerolog.DebugLevel)
	huge := strings.Repeat("x", maxLoggedBodyBytes+500)
	p := NewLoggingProvider(&logStub{resp: okResponse(huge), model: "m"}, log)
	if _, err := p.Complete(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	resp := findMsg(parseLogLines(t, &buf), "llm response")
	body := resp["response"].(string)
	if !strings.Contains(body, "truncated 500 bytes") {
		t.Errorf("body not truncated with marker; len=%d", len(body))
	}
}

// --- optional-interface forwarding (decorator must be transparent) ---

func (s *logStub) WithModel(m string) Provider {
	if s.withModelTo == "" {
		return s // not actually overridable in this stub config
	}
	clone := *s
	clone.model = m
	return &clone
}
func (s *logStub) Ping(_ context.Context) error {
	if !s.pinger {
		return errors.New("not a pinger")
	}
	return s.pingErr
}
func (s *logStub) ListModels(_ context.Context) ([]ModelInfo, error) {
	if !s.lister {
		return nil, errors.New("not a lister")
	}
	return s.models, nil
}

func TestLoggingProvider_ForwardsOptionalInterfaces(t *testing.T) {
	log := zerolog.New(&bytes.Buffer{})
	stub := &logStub{model: "base", withModelTo: "yes", pinger: true,
		lister: true, models: []ModelInfo{{ID: "x"}}}
	p := NewLoggingProvider(stub, log)

	ov, ok := p.(ModelOverridable)
	if !ok {
		t.Fatal("LoggingProvider must implement ModelOverridable")
	}
	if got := ov.WithModel("pinned").Model(); got != "pinned" {
		t.Errorf("WithModel().Model() = %q, want pinned", got)
	}

	pg, ok := p.(Pinger)
	if !ok || pg.Ping(context.Background()) != nil {
		t.Errorf("Ping should forward and succeed; ok=%v", ok)
	}

	ml, ok := p.(ModelLister)
	if !ok {
		t.Fatal("LoggingProvider must implement ModelLister")
	}
	got, err := ml.ListModels(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "x" {
		t.Errorf("ListModels forward = %v, %v", got, err)
	}
}

func TestLoggingProvider_AggregatesThroughQueue(t *testing.T) {
	// LoggingProvider -> QueuedProvider -> Router must still surface the
	// per-sub-provider breakdown, so /api/v1/models keeps attribution.
	sub := &logStub{model: "r", lister: true, models: []ModelInfo{{ID: "a"}}}
	router, err := NewRouter(sub, nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	queued := NewQueuedProvider(router, 2)
	p := NewLoggingProvider(queued, zerolog.New(&bytes.Buffer{}))

	agg, ok := p.(ModelAggregator)
	if !ok {
		t.Fatal("LoggingProvider must implement ModelAggregator")
	}
	res, found := agg.ListModelsAggregated(context.Background())
	if !found {
		t.Fatalf("aggregation not found through the wrapped chain")
	}
	if len(res.Providers) == 0 {
		t.Errorf("expected a per-sub-provider breakdown; got %+v", res)
	}
}

func TestNewLoggingProvider_NilInnerReturnsNil(t *testing.T) {
	if NewLoggingProvider(nil, zerolog.Nop()) != nil {
		t.Error("nil inner should return nil")
	}
}
