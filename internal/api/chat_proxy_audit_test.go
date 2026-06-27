// Tests for recordChatAPIAudit — the chat-proxy + ollama-proxy
// audit-log writer. Pins the internal-agent skip, the principal
// resolution (User-Agent → chat_id), the system-prompt SavePrompt
// path, and the message extraction (system / last user).
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// stubChatAuditRepoAPI captures every Insert + SavePrompt so tests
// can pin the row shape without touching a real DB.
type stubChatAuditRepoAPI struct {
	mu       sync.Mutex
	inserts  []*persistence.ChatAuditEntry
	prompts  map[string]string
	insertEr error
}

func newStubChatAuditRepoAPI() *stubChatAuditRepoAPI {
	return &stubChatAuditRepoAPI{prompts: map[string]string{}}
}

func (s *stubChatAuditRepoAPI) Insert(_ context.Context, e *persistence.ChatAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertEr != nil {
		return s.insertEr
	}
	s.inserts = append(s.inserts, e)
	return nil
}
func (s *stubChatAuditRepoAPI) List(_ context.Context, _ persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return nil, nil
}
func (s *stubChatAuditRepoAPI) SavePrompt(_ context.Context, hash, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts[hash] = body
	return nil
}
func (s *stubChatAuditRepoAPI) GetPrompt(_ context.Context, hash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.prompts[hash]; ok {
		return v, nil
	}
	return "", persistence.ErrNotFound
}

func (s *stubChatAuditRepoAPI) snapshot() []*persistence.ChatAuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*persistence.ChatAuditEntry(nil), s.inserts...)
}

// newAuditTestServer builds a minimal Server suitable for recorder
// unit tests. Only chatAuditRepo + logger need to be wired; the
// pricing path is left empty so computeChatCallCost returns 0
// (matches deployments without pricing.yaml).
func newAuditTestServer(repo persistence.ChatAuditRepository) *Server {
	return &Server{logger: zerolog.Nop(), chatAuditRepo: repo}
}

// TestRecordChatAPIAudit_NilRepoNoOp — without wiring the recorder
// is a no-op (proxy calls still run; just no audit row). Pin so
// future refactors don't accidentally panic on the nil-repo path.
func TestRecordChatAPIAudit_NilRepoNoOp(t *testing.T) {
	s := newAuditTestServer(nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &chat.ChatResponse{Model: "test-model"}
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m", nil, resp, 0)
}

// TestRecordChatAPIAudit_NilResponse — nil response is silently
// dropped (the caller decided not to fail the request on a
// transient provider error; audit isn't a place to second-guess).
func TestRecordChatAPIAudit_NilResponse(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m", nil, nil, 0)
	if got := len(repo.snapshot()); got != 0 {
		t.Errorf("nil response should record nothing; got %d row(s)", got)
	}
}

// TestRecordChatAPIAudit_InternalAgentSkipped — X-Vornik-Task-ID
// header marks the caller as a workflow-step agent container.
// Those calls are already audited at step granularity; we skip
// the chat-audit row to keep that surface focused on
// conversational traffic.
func TestRecordChatAPIAudit_InternalAgentSkipped(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-Vornik-Task-ID", "task_internal_1")
	resp := &chat.ChatResponse{
		Model: "m",
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{},
	}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{})
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m", nil, resp, 0)
	if got := len(repo.snapshot()); got != 0 {
		t.Errorf("internal agent call should be skipped; got %d row(s)", got)
	}

	// X-Vornik-Execution-ID is the alternate signal — same skip.
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2.Header.Set("X-Vornik-Execution-ID", "exec_x")
	s.recordChatAPIAudit(context.Background(), r2, time.Now(), "m", nil, resp, 0)
	if got := len(repo.snapshot()); got != 0 {
		t.Errorf("X-Vornik-Execution-ID should also skip; got %d row(s)", got)
	}
}

// TestRecordChatAPIAudit_HappyPath — external call lands one row
// with all the fields populated: chat_id = "api:<UA>", role =
// external_api, model, prompt hash, user message, response text,
// iteration = 1, duration > 0.
func TestRecordChatAPIAudit_HappyPath(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("User-Agent", "OpenWebUI/0.5.7")

	messages := []chat.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "first turn"},
		{Role: "assistant", Content: "reply 1"},
		{Role: "user", Content: "latest turn"},
	}
	resp := buildChatResponse("minimax-m2", "the answer", 50, 25)

	startedAt := time.Now().Add(-100 * time.Millisecond)
	s.recordChatAPIAudit(context.Background(), r, startedAt, "minimax-m2", messages, resp, 0.0042)

	rows := repo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ChatID != "api:OpenWebUI/0.5.7" {
		t.Errorf("ChatID = %q, want api:OpenWebUI/0.5.7", row.ChatID)
	}
	if row.RoleUsed != "external_api" {
		t.Errorf("RoleUsed = %q, want external_api", row.RoleUsed)
	}
	if row.Model != "minimax-m2" {
		t.Errorf("Model = %q, want minimax-m2", row.Model)
	}
	if row.UserMessage != "latest turn" {
		t.Errorf("UserMessage = %q, want latest turn (latest user message wins)", row.UserMessage)
	}
	if row.Response != "the answer" {
		t.Errorf("Response = %q, want the answer", row.Response)
	}
	if row.SystemPromptHash == "" {
		t.Error("SystemPromptHash empty — should be sha256 of the system prompt")
	}
	if got := repo.prompts[row.SystemPromptHash]; got != "you are helpful" {
		t.Errorf("SavePrompt body = %q, want 'you are helpful'", got)
	}
	if row.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", row.Iterations)
	}
	if row.DurationMs <= 0 {
		t.Errorf("DurationMs = %d, want >0 (recorded against startedAt)", row.DurationMs)
	}
	if row.CostUSD != 0.0042 {
		t.Errorf("CostUSD = %f, want 0.0042", row.CostUSD)
	}
}

// TestRecordChatAPIAudit_NoUserAgent — caller didn't set
// User-Agent; chat_id falls back to "api:anonymous" so audit
// queries still group consistently.
func TestRecordChatAPIAudit_NoUserAgent(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := buildChatResponse("m", "r", 10, 5)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m", nil, resp, 0)

	rows := repo.snapshot()
	if len(rows) != 1 || rows[0].ChatID != "api:anonymous" {
		t.Errorf("ChatID = %q, want api:anonymous", rows[0].ChatID)
	}
}

// TestRecordChatAPIAudit_NoSystemPrompt — request with no
// system-role message: SystemPromptHash stays empty and SavePrompt
// is not called.
func TestRecordChatAPIAudit_NoSystemPrompt(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	messages := []chat.Message{{Role: "user", Content: "hi"}}
	resp := buildChatResponse("m", "hello", 5, 3)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m", messages, resp, 0)

	rows := repo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].SystemPromptHash != "" {
		t.Errorf("SystemPromptHash = %q, want empty", rows[0].SystemPromptHash)
	}
	if len(repo.prompts) != 0 {
		t.Errorf("SavePrompt called %d times, want 0 (no system prompt to save)", len(repo.prompts))
	}
}

// TestRecordChatAPIAudit_EffectiveModelFallback — when the
// response has no Model (some providers omit), the audit row
// falls back to the requested model so the audit surface still
// shows what was being asked for.
func TestRecordChatAPIAudit_EffectiveModelFallback(t *testing.T) {
	repo := newStubChatAuditRepoAPI()
	s := newAuditTestServer(repo)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := buildChatResponse("", "x", 1, 1) // Model intentionally empty
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "requested-model", nil, resp, 0)

	rows := repo.snapshot()
	if len(rows) != 1 || rows[0].Model != "requested-model" {
		t.Errorf("Model = %q, want requested-model (fallback when resp.Model empty)", rows[0].Model)
	}
}

// TestExtractAuditContent — system and user message extraction.
// Latest user wins; first system wins (multi-system is rare but
// possible; the convention matches dispatcher).
func TestExtractAuditContent(t *testing.T) {
	msgs := []chat.Message{
		{Role: "system", Content: "first sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "system", Content: "second sys"},
		{Role: "user", Content: "u2 — latest"},
	}
	sys, usr := extractAuditContent(msgs)
	if sys != "first sys" {
		t.Errorf("system = %q, want first sys", sys)
	}
	if usr != "u2 — latest" {
		t.Errorf("user = %q, want u2 — latest", usr)
	}
}

// TestExtractAssistantText — empty resp, empty choices, populated
// choice.
func TestExtractAssistantText(t *testing.T) {
	if got := extractAssistantText(nil); got != "" {
		t.Errorf("nil resp: got %q", got)
	}
	if got := extractAssistantText(&chat.ChatResponse{}); got != "" {
		t.Errorf("empty choices: got %q", got)
	}
	resp := buildChatResponse("m", "the answer", 1, 1)
	if got := extractAssistantText(resp); got != "the answer" {
		t.Errorf("got %q, want the answer", got)
	}
}

// TestTruncateForAuditAPI — short stays intact, long gets a
// "…(truncated)" suffix, limits below the suffix length hard-chop.
func TestTruncateForAuditAPI(t *testing.T) {
	if got := truncateForAuditAPI("abc", 100); got != "abc" {
		t.Errorf("short: got %q", got)
	}
	long := strings.Repeat("x", 600)
	got := truncateForAuditAPI(long, 500)
	if len(got) != 500 {
		t.Errorf("length = %d, want 500", len(got))
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("missing truncation suffix: %q", got[len(got)-30:])
	}
}

// buildChatResponse synthesises a chat.ChatResponse with one
// choice carrying the supplied text. Used across these tests.
func buildChatResponse(model, text string, promptTokens, completionTokens int) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: model}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		Index:   0,
		Message: chat.Message{Role: "assistant", Content: text},
	})
	resp.Usage.PromptTokens = promptTokens
	resp.Usage.CompletionTokens = completionTokens
	return resp
}

func (s *stubChatAuditRepoAPI) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}
