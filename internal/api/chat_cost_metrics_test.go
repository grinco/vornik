// Coverage for the chat-proxy cost-recording path. The operator
// needs assurance that every third-party /v1/chat/completions,
// /api/chat, and /api/generate call books cost — and that
// internal swarm calls don't double-book through this path.

package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// recordingUsageRepo captures every TaskLLMUsage row passed to
// Record so the assertions can inspect what landed. Sized for
// the chat-proxy use case — never more than a few writes per
// test. Read-side methods (List, Sum, Aggregate*, etc.) are
// no-ops because the chat-proxy path never exercises them.
type recordingUsageRepo struct {
	mu   sync.Mutex
	rows []persistence.TaskLLMUsage
	err  error // when non-nil, Record returns this so the warn-log path runs
}

func (r *recordingUsageRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.rows = append(r.rows, *u)
	return nil
}

func (r *recordingUsageRepo) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	return r.Record(ctx, u)
}

func (r *recordingUsageRepo) List(_ context.Context, _ persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return nil, nil
}
func (r *recordingUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (r *recordingUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (r *recordingUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}
func (r *recordingUsageRepo) SumCost(_ context.Context, _, _ time.Time) (float64, error) {
	return 0, nil
}
func (r *recordingUsageRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) AggregateByProject(_ context.Context, _, _ time.Time, _ int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) AggregateBySource(_ context.Context, _, _ time.Time, _ string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TimeSeriesByDay(_ context.Context, _, _ time.Time, _ string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TopTasks(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (r *recordingUsageRepo) TaskCostBreakdown(_ context.Context, _ string) ([]persistence.StepSpend, error) {
	return nil, nil
}

func (r *recordingUsageRepo) recorded() []persistence.TaskLLMUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]persistence.TaskLLMUsage, len(r.rows))
	copy(out, r.rows)
	return out
}

// waitForUsageRows polls the (mutex-guarded) recorder until it holds at
// least `want` rows or ~2s elapses, then returns a snapshot. Usage/audit
// telemetry is recorded off the hot path — a detached goroutine that runs
// after the handler returns (see ChatCompletions / OllamaChat) — so a
// synchronous read immediately after the handler can race the deferred
// write. For paths that still record synchronously this returns instantly.
func waitForUsageRows(t *testing.T, repo *recordingUsageRepo, want int) []persistence.TaskLLMUsage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows := repo.recorded()
		if len(rows) >= want || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stubChatProviderWithUsage returns a canned ChatResponse with
// non-zero Usage so the recorder branch fires. Distinct from the
// openaiStub which leaves Usage zero (skipping the recorder).
type stubChatProviderWithUsage struct {
	model       string
	promptToks  int
	completToks int
	respModel   string
}

func (s stubChatProviderWithUsage) build() *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: s.respModel}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Message: chat.Message{Role: "assistant", Content: "ok"},
	})
	resp.Usage.PromptTokens = s.promptToks
	resp.Usage.CompletionTokens = s.completToks
	resp.Usage.TotalTokens = s.promptToks + s.completToks
	return resp
}

func (s stubChatProviderWithUsage) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	return s.build(), nil
}
func (s stubChatProviderWithUsage) CompleteWithTools(_ context.Context, _ []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return s.build(), nil
}
func (s stubChatProviderWithUsage) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.build(), nil
}
func (s stubChatProviderWithUsage) Model() string              { return s.model }
func (s stubChatProviderWithUsage) SetMetrics(_ *chat.Metrics) {}

// writePricingYAML writes a minimal pricing.yaml for the
// recorder tests and returns the path. Real pricing.yaml uses
// the same shape; we use a tiny entry so the test cost math is
// easy to verify.
func writePricingYAML(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/pricing.yaml"
	yaml := `models:
  test-model: { input: 2.00, output: 4.00 }
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write pricing.yaml: %v", err)
	}
	return path
}

// TestChatCompletions_RecordsExternalAPIUsage is the headline
// regression test for the operator's "I don't want to be blind
// to chat-API cost" brief. A third-party /v1/chat/completions
// call must produce exactly one TaskLLMUsage row with
// source=external_api, role=external_api, and a computed
// CostUSD from pricing.yaml.
func TestChatCompletions_RecordsExternalAPIUsage(t *testing.T) {
	pricingPath := writePricingYAML(t)
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model:       "test-model",
		respModel:   "test-model",
		promptToks:  1000,
		completToks: 500,
	}
	s := NewServer(
		WithChatProvider(prov),
		WithLLMUsageRepository(repo),
		WithPricingPath(pricingPath),
	)

	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("User-Agent", "openai-python/1.0")
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Source != persistence.TaskLLMUsageSourceExternalAPI {
		t.Errorf("source = %q, want external_api", row.Source)
	}
	if row.Role != "external_api" {
		t.Errorf("role = %q, want external_api", row.Role)
	}
	if row.ProjectID != "_external" {
		t.Errorf("project_id = %q, want _external (no header, no fallback)", row.ProjectID)
	}
	if row.PromptTokens != 1000 || row.CompletionTokens != 500 {
		t.Errorf("tokens = %d/%d, want 1000/500", row.PromptTokens, row.CompletionTokens)
	}
	// 1000 input @ $2/M + 500 output @ $4/M = $0.002 + $0.002 = $0.004
	if row.CostUSD < 0.0039 || row.CostUSD > 0.0041 {
		t.Errorf("cost = %v, want ~0.004 from pricing.yaml", row.CostUSD)
	}
	if row.SessionID == nil || *row.SessionID != "openai-python/1.0" {
		t.Errorf("session_id = %v, want User-Agent fingerprint", row.SessionID)
	}
	if row.TaskID != nil || row.ExecutionID != nil {
		t.Errorf("task/exec should be nil for external API rows, got %v/%v", row.TaskID, row.ExecutionID)
	}
}

// TestChatCompletions_SkipsUsageWhenAgentHeadersSet pins the
// critical de-duplication contract: when X-Vornik-Task-ID or
// X-Vornik-Execution-ID is set, the request is from an internal
// swarm agent which will flush its own workflow_step row via
// /api/v1/internal/llm-usage. Recording here would double-bill.
func TestChatCompletions_SkipsUsageWhenAgentHeadersSet(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 100, completToks: 50,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))

	for _, header := range []string{"X-Vornik-Task-ID", "X-Vornik-Execution-ID"} {
		t.Run(header, func(t *testing.T) {
			body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
			req.Header.Set(header, "some-id")
			rr := httptest.NewRecorder()
			s.ChatCompletions(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
		})
	}
	if len(repo.recorded()) != 0 {
		t.Errorf("agent-headered calls produced %d cost rows; expected 0 to avoid double-billing", len(repo.recorded()))
	}
}

// TestChatCompletions_UsesProjectHeaderForBilling — when the
// client supplies X-Vornik-Project-ID, the cost row attributes
// to that project instead of the "_external" fallback. The
// existing IDOR guards already validate the header against the
// API key's scope; here we just verify the recorder honours it.
func TestChatCompletions_UsesProjectHeaderForBilling(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 10, completToks: 5,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("X-Vornik-Project-ID", "tenant-acme")
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	if rows[0].ProjectID != "tenant-acme" {
		t.Errorf("project_id = %q, want tenant-acme (from header)", rows[0].ProjectID)
	}
}

// TestChatCompletions_ExternalBillingProjectFallback — when the
// daemon is configured with a fallback project and the request
// has no X-Vornik-Project-ID, rows land on the fallback. SaaS
// operators with a dedicated billing project use this to keep
// cost on one panel.
func TestChatCompletions_ExternalBillingProjectFallback(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 10, completToks: 5,
	}
	s := NewServer(
		WithChatProvider(prov),
		WithLLMUsageRepository(repo),
		WithExternalAPIBillingProjectID("external-traffic"),
	)
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	if rows[0].ProjectID != "external-traffic" {
		t.Errorf("project_id = %q, want external-traffic (fallback)", rows[0].ProjectID)
	}
}

// TestChatCompletions_SkipsUsageOnZeroTokens — when the provider
// can't return token counts (claude-cli, codex-cli surrogates),
// skip the row instead of writing zeroes that would skew
// $/call averages.
func TestChatCompletions_SkipsUsageOnZeroTokens(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		// zero tokens — the gate must skip
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if len(repo.recorded()) != 0 {
		t.Errorf("zero-token call recorded %d rows; expected 0", len(repo.recorded()))
	}
}

// TestChatCompletions_RecordsWithoutPricingWhenPathUnset — no
// pricing path means cost stays at 0 but the row still lands so
// operators see token volume even before they wire pricing.
func TestChatCompletions_RecordsWithoutPricingWhenPathUnset(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 100, completToks: 50,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].CostUSD != 0 {
		t.Errorf("cost = %v with no pricing path, want 0 (provider tokens still visible)", rows[0].CostUSD)
	}
	if rows[0].PromptTokens != 100 {
		t.Errorf("prompt_tokens = %d, want 100", rows[0].PromptTokens)
	}
}

// TestOllamaChat_RecordsExternalAPIUsage_NonStreaming pins the
// same cost-recording for the Ollama /api/chat non-streaming
// path.
func TestOllamaChat_RecordsExternalAPIUsage_NonStreaming(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 200, completToks: 100,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"model":"test-model","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Header.Set("User-Agent", "ollama-cli/0.5")
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].PromptTokens != 200 || rows[0].CompletionTokens != 100 {
		t.Errorf("tokens = %d/%d, want 200/100", rows[0].PromptTokens, rows[0].CompletionTokens)
	}
	if rows[0].SessionID == nil || *rows[0].SessionID != "ollama-cli/0.5" {
		t.Errorf("session_id = %v, want ollama-cli/0.5", rows[0].SessionID)
	}
}

// TestOllamaChat_RecordsExternalAPIUsage_Streaming pins the
// same recording on the streaming path. The recorder runs once
// against the final response, not per-chunk.
func TestOllamaChat_RecordsExternalAPIUsage_Streaming(t *testing.T) {
	repo := &recordingUsageRepo{}
	stub := &streamingStub{
		model:  "test-model",
		chunks: []string{"hello ", "world"},
		finalResp: func() *chat.ChatResponse {
			p := stubChatProviderWithUsage{respModel: "test-model", promptToks: 50, completToks: 25}
			return p.build()
		}(),
	}
	s := NewServer(WithChatProvider(stub), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("streaming /api/chat recorded %d rows, want exactly 1 (not per-chunk)", len(rows))
	}
	if rows[0].PromptTokens != 50 {
		t.Errorf("prompt_tokens = %d, want 50 (from final response)", rows[0].PromptTokens)
	}
}

// TestOllamaChat_SkipsUsageWhenAgentHeadersSet — same de-dup
// guard on the Ollama path. Internal agents talking to
// /api/chat (rare but possible if a swarm role is configured
// against Ollama-shaped clients) must not double-bill.
func TestOllamaChat_SkipsUsageWhenAgentHeadersSet(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 100, completToks: 50,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Header.Set("X-Vornik-Task-ID", "task_x")
	rr := httptest.NewRecorder()
	s.OllamaChat(rr, req)
	if len(repo.recorded()) != 0 {
		t.Errorf("internal agent /api/chat: recorded %d rows; expected 0", len(repo.recorded()))
	}
}

// TestOllamaGenerate_RecordsExternalAPIUsage covers the legacy
// single-prompt endpoint's cost path.
func TestOllamaGenerate_RecordsExternalAPIUsage(t *testing.T) {
	repo := &recordingUsageRepo{}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 75, completToks: 25,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"stream":false,"prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", body)
	rr := httptest.NewRecorder()
	s.OllamaGenerate(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	rows := waitForUsageRows(t, repo, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
}

// TestRecordChatAPIUsage_RepoErrorIsBestEffort — when the
// underlying repo errors, the request still completes and the
// warn-log path runs. Operators shouldn't see a 500 because the
// cost ledger had a transient hiccup.
func TestRecordChatAPIUsage_RepoErrorIsBestEffort(t *testing.T) {
	repo := &recordingUsageRepo{err: errInjected}
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 10, completToks: 5,
	}
	s := NewServer(WithChatProvider(prov), WithLLMUsageRepository(repo))
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 even when usage repo errored (best-effort recording)", rr.Code)
	}
}

// TestRecordChatAPIUsage_NoRepoIsNoOp — without a usage repo
// wired, the recorder must be a no-op rather than nil-deref.
// Mirrors the api package's "fields are optional" contract.
func TestRecordChatAPIUsage_NoRepoIsNoOp(t *testing.T) {
	prov := stubChatProviderWithUsage{
		model: "test-model", respModel: "test-model",
		promptToks: 10, completToks: 5,
	}
	s := NewServer(WithChatProvider(prov)) // no WithLLMUsageRepository
	body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rr := httptest.NewRecorder()
	s.ChatCompletions(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
	// no panic + no assertion needed — surviving the call is the proof
}

// errInjected is the canned error the repo stub returns when
// asked to simulate a write failure.
var errInjected = errUsageRepo{}

type errUsageRepo struct{}

func (errUsageRepo) Error() string { return "simulated usage repo failure" }
