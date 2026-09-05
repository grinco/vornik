package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmreplay"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// exchangeRecorderFake captures Record calls and signals each one, since the
// proxy records from its telemetry goroutine after the response is written.
type exchangeRecorderFake struct {
	mu   sync.Mutex
	rows []*persistence.LLMExchange
	got  chan struct{}
	err  error
}

func newExchangeRecorderFake() *exchangeRecorderFake {
	return &exchangeRecorderFake{got: make(chan struct{}, 8)}
}

func (f *exchangeRecorderFake) Record(_ context.Context, x *persistence.LLMExchange) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		x.Seq = len(f.rows) + 1
		f.rows = append(f.rows, x)
	}
	f.got <- struct{}{}
	return f.err
}

func (f *exchangeRecorderFake) ListByStep(context.Context, string, string) ([]*persistence.LLMExchange, error) {
	return nil, nil
}

func (f *exchangeRecorderFake) CountByExecution(context.Context, string) (int, error) { return 0, nil }

func (f *exchangeRecorderFake) wait(t *testing.T) *persistence.LLMExchange {
	t.Helper()
	select {
	case <-f.got:
	case <-time.After(3 * time.Second):
		t.Fatal("no exchange was recorded within 3s")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return nil
	}
	return f.rows[len(f.rows)-1]
}

// recordingRegistry stages one project with recording.llm_exchanges set as
// asked (registry.Registry has no programmatic project-set API).
func recordingRegistry(t *testing.T, projectID string, optIn bool) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s1.md"), []byte("---\nswarmId: \"s1\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "wf.md"), []byte("---\nworkflowId: \"wf\"\nentrypoint: \"run\"\nsteps:\n  run:\n    type: \"agent\"\n    prompt: \"do\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"), 0o644))
	rec := ""
	if optIn {
		rec = "recording:\n  llm_exchanges: true\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", projectID+".yaml"), []byte("projectId: \""+projectID+"\"\ndisplayName: \"P\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"wf\"\n"+rec), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

func exchangeTestServer(t *testing.T, optIn bool, stub *stubProvider) (*Server, *exchangeRecorderFake) {
	t.Helper()
	fake := newExchangeRecorderFake()
	s := &Server{
		logger:       zerolog.Nop(),
		chatProvider: stub,
		executionRepo: &mocks.MockExecutionRepository{GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, ProjectID: "proj", TaskID: "t1"}, nil
		}},
		projectRegistry: recordingRegistry(t, "proj", optIn),
		llmExchangeRepo: fake,
		exchangeRedactor: func(body string) (string, int) {
			return strings.ReplaceAll(body, "sk-secret", "[REDACTED]"), strings.Count(body, "sk-secret")
		},
	}
	return s, fake
}

func agentRequest(withStep bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(exchangeBody))
	req.Header.Set("X-Vornik-Execution-ID", "exec-1")
	if withStep {
		req.Header.Set("X-Vornik-Step-ID", "plan")
		req.Header.Set("X-Vornik-Iteration", "3")
	}
	return req
}

func okResponse() *chat.ChatResponse {
	r := &chat.ChatResponse{ID: "r1", Model: "real-model"}
	r.Choices = append(r.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "hi back"}, FinishReason: "stop"})
	r.Usage.PromptTokens, r.Usage.CompletionTokens = 11, 2
	return r
}

const exchangeBody = `{"model":"m","messages":[{"role":"user","content":"hello sk-secret"}],"tools":[]}`

// An opted-in execution's exchange is recorded from the decoded request:
// canonical form, hash of the STORED (redacted) bytes, step and iteration
// from the headers, model and usage from the provider's response.
func TestChatCompletions_RecordsExchangeWhenProjectOptedIn(t *testing.T) {
	s, fake := exchangeTestServer(t, true, &stubProvider{resp: okResponse()})
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(true))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	x := fake.wait(t)
	require.NotNil(t, x)
	require.Equal(t, "exec-1", x.ExecutionID)
	require.Equal(t, "plan", x.StepID)
	require.NotNil(t, x.Iteration)
	require.Equal(t, 3, *x.Iteration)
	require.Equal(t, "real-model", x.Model)
	require.Equal(t, 11, x.PromptTokens)
	require.Equal(t, 2, x.CompletionTokens)
	require.NotContains(t, x.RequestJSON, "sk-secret", "the store never sees the unredacted body")
	require.Contains(t, x.RequestJSON, "[REDACTED]")
	require.Equal(t, 1, x.Redactions)
	require.Equal(t, llmreplay.Hash([]byte(x.RequestJSON)), x.RequestHash, "the hash names the stored bytes")
	require.NotContains(t, x.RequestJSON, `"model"`, "the canonical form drops the model")
	require.Contains(t, x.ResponseJSON, "hi back")
}

func TestChatCompletions_RecordsProviderErrorAsTheResponse(t *testing.T) {
	s, fake := exchangeTestServer(t, true, &stubProvider{err: errors.New("upstream 429")})
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(true))
	require.NotEqual(t, http.StatusOK, w.Code)
	x := fake.wait(t)
	require.NotNil(t, x)
	require.Contains(t, x.ResponseJSON, `"error"`)
	require.Contains(t, x.ResponseJSON, "upstream 429")
}

func TestChatCompletions_DoesNotRecordWithoutOptInOrStepOrExecution(t *testing.T) {
	// Not opted in.
	s, fake := exchangeTestServer(t, false, &stubProvider{resp: okResponse()})
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(true))
	require.Equal(t, http.StatusOK, w.Code)
	// No step header on an opted-in project.
	s2, fake2 := exchangeTestServer(t, true, &stubProvider{resp: okResponse()})
	w = httptest.NewRecorder()
	s2.ChatCompletions(w, agentRequest(false))
	require.Equal(t, http.StatusOK, w.Code)
	// An external caller: no execution header at all.
	s3, fake3 := exchangeTestServer(t, true, &stubProvider{resp: okResponse()})
	w = httptest.NewRecorder()
	s3.ChatCompletions(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(exchangeBody)))
	require.Equal(t, http.StatusOK, w.Code)

	time.Sleep(150 * time.Millisecond) // the telemetry goroutine has run by now
	for i, f := range []*exchangeRecorderFake{fake, fake2, fake3} {
		f.mu.Lock()
		n := len(f.rows)
		f.mu.Unlock()
		require.Zero(t, n, "case %d must record nothing", i)
	}
}

// A store failure is a log line and a counter, never a failed completion.
func TestChatCompletions_StoreFailureDoesNotFailTheResponse(t *testing.T) {
	s, fake := exchangeTestServer(t, true, &stubProvider{resp: okResponse()})
	fake.err = errors.New("disk on fire")
	w := httptest.NewRecorder()
	s.ChatCompletions(w, agentRequest(true))
	require.Equal(t, http.StatusOK, w.Code)
	fake.wait(t)
}
