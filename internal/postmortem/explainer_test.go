package postmortem

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// makeChatResponse constructs a ChatResponse with one choice
// + the given content + token counts. Wraps the inline-
// anonymous Choices/Usage structs that ChatResponse uses (no
// named Choice / Usage types) so each test stays terse.
func makeChatResponse(content string, promptTokens, completionTokens int) *chat.ChatResponse {
	resp := &chat.ChatResponse{
		Choices: []struct {
			Index        int          `json:"index"`
			Message      chat.Message `json:"message"`
			FinishReason string       `json:"finish_reason"`
		}{
			{Message: chat.Message{Role: "assistant", Content: content}, FinishReason: "stop"},
		},
	}
	resp.Usage.PromptTokens = promptTokens
	resp.Usage.CompletionTokens = completionTokens
	resp.Usage.TotalTokens = promptTokens + completionTokens
	return resp
}

// stubChat returns a fixed response. Production wires a real
// chat.Provider (claude/codex/HTTP); this is enough to verify
// the explainer's prompt assembly + cost accounting + repo
// persistence without standing up an LLM.
type stubChat struct {
	resp     *chat.ChatResponse
	err      error
	captured []chat.Message
	model    string
}

func (s *stubChat) Complete(_ context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	s.captured = msgs
	return s.resp, s.err
}

func (s *stubChat) CompleteWithTools(_ context.Context, msgs []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	s.captured = msgs
	return s.resp, s.err
}

func (s *stubChat) CompleteWithToolsStream(_ context.Context, msgs []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	s.captured = msgs
	return s.resp, s.err
}

func (s *stubChat) Model() string              { return s.model }
func (s *stubChat) SetMetrics(_ *chat.Metrics) {}

// recordingPostMortemRepo is the test repo. Captures the row
// passed to Record so tests can assert on summary/model/cost.
type recordingPostMortemRepo struct {
	cached  *persistence.TaskPostMortem
	written *persistence.TaskPostMortem
	getErr  error
}

func (r *recordingPostMortemRepo) Record(_ context.Context, pm *persistence.TaskPostMortem) error {
	r.written = pm
	return nil
}

func (r *recordingPostMortemRepo) Get(_ context.Context, _ string) (*persistence.TaskPostMortem, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	if r.cached != nil {
		return r.cached, nil
	}
	return nil, persistence.ErrNotFound
}

// stubTaskRepo returns one fixed task; everything else is no-op.
type stubTaskRepo struct {
	persistence.TaskRepository
	task *persistence.Task
}

func (s *stubTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return s.task, nil
}

// TestExplainer_ReturnsCachedRowOnSecondCall — the caching
// promise: a second Generate without forceRefresh skips the
// LLM entirely and returns the stored row. Without this
// guarantee the failed-task UI's auto-load on every render
// would re-bill the operator on every page navigation.
func TestExplainer_ReturnsCachedRowOnSecondCall(t *testing.T) {
	cached := &persistence.TaskPostMortem{
		TaskID:    "task-1",
		ProjectID: "p1",
		Summary:   "previously generated",
	}
	repo := &recordingPostMortemRepo{cached: cached}
	chatStub := &stubChat{}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: repo,
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	res, err := exp.Generate(context.Background(), "task-1", false)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Cached)
	assert.Equal(t, "previously generated", res.PostMortem.Summary)
	assert.Empty(t, chatStub.captured, "cached path must not call the LLM")
}

// TestExplainer_ForceRefreshSkipsCache — the operator's
// "Regenerate" button. force_refresh=true on the form means
// re-fire the LLM regardless of the cached row, and the new
// summary overwrites the prior one (Record is upsert).
func TestExplainer_ForceRefreshSkipsCache(t *testing.T) {
	cached := &persistence.TaskPostMortem{
		TaskID:  "task-1",
		Summary: "old explainer",
	}
	repo := &recordingPostMortemRepo{cached: cached}
	chatStub := &stubChat{
		resp: makeChatResponse("fresh explainer", 100, 30),
	}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: repo,
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	res, err := exp.Generate(context.Background(), "task-1", true)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Cached)
	assert.Equal(t, "fresh explainer", res.PostMortem.Summary)
	require.NotNil(t, repo.written)
	assert.Equal(t, "fresh explainer", repo.written.Summary)
	assert.Equal(t, 100, repo.written.PromptTokens)
	assert.Equal(t, 30, repo.written.CompletionTokens)
}

// TestExplainer_PromptIncludesEvidence — the LLM must see
// the task's status, error, and prompt. Without this content
// the model has no grounding and produces useless summaries.
func TestExplainer_PromptIncludesEvidence(t *testing.T) {
	errMsg := "schema violation: role \"strategist\" missing required keys: [proposals]"
	errClass := "INVALID_OUTPUT"
	task := &persistence.Task{
		ID:             "task-X",
		ProjectID:      "ibkr-trader",
		Status:         persistence.TaskStatusFailed,
		Attempt:        3,
		MaxAttempts:    3,
		LastError:      &errMsg,
		LastErrorClass: &errClass,
		Payload:        []byte(`{"taskType":"trading","context":{"prompt":"Run one trading tick on the watchlist."}}`),
	}
	chatStub := &stubChat{
		resp: makeChatResponse("summary", 0, 0),
	}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: task},
		PostMortems: &recordingPostMortemRepo{},
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	_, err := exp.Generate(context.Background(), "task-X", false)
	require.NoError(t, err)
	require.Len(t, chatStub.captured, 2, "system + user message")
	user := chatStub.captured[1].Content
	assert.Contains(t, user, "task-X")
	assert.Contains(t, user, "ibkr-trader")
	assert.Contains(t, user, "FAILED")
	assert.Contains(t, user, "schema violation")
	assert.Contains(t, user, "INVALID_OUTPUT")
	assert.Contains(t, user, "Attempt: 3/3")
	assert.Contains(t, user, "Run one trading tick")
}

// TestExplainer_PropagatesLLMError — a transient LLM failure
// must NOT be silently swallowed; the operator deserves to
// see "post-mortem failed: <reason>" in the UI rather than
// have the button quietly do nothing.
func TestExplainer_PropagatesLLMError(t *testing.T) {
	chatStub := &stubChat{err: errors.New("upstream 503 from chat provider")}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: &recordingPostMortemRepo{},
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	_, err := exp.Generate(context.Background(), "task-1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream 503")
}

// TestExplainer_RefusesEmptyResponse — some providers occasion-
// ally return empty content (rate-limited path returns 200 +
// no choices, etc). The explainer must NOT persist an empty
// summary; the operator can retry.
func TestExplainer_RefusesEmptyResponse(t *testing.T) {
	chatStub := &stubChat{
		resp: makeChatResponse("   ", 0, 0),
	}
	repo := &recordingPostMortemRepo{}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: repo,
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	_, err := exp.Generate(context.Background(), "task-1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty summary")
	assert.Nil(t, repo.written, "empty summary must not be persisted")
}
