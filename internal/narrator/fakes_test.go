package narrator

import (
	"context"
	"errors"
	"sync"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// fakeSub is a controllable Subscriber: the test pushes LiveEvents on
// Events and the narrator's Run loop consumes them via SubscribeAll.
type fakeSub struct {
	Events    chan livepubsub.LiveEvent
	cancelled bool
}

func newFakeSub() *fakeSub {
	return &fakeSub{Events: make(chan livepubsub.LiveEvent, 256)}
}

func (f *fakeSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return f.Events, func() { f.cancelled = true }, nil
}

func (f *fakeSub) push(executionID, kind string, payload any) {
	f.Events <- livepubsub.LiveEvent{ExecutionID: executionID, Kind: kind, Payload: payload}
}

// orderedRecorder is a shared append-only log used to prove
// persist-then-publish ordering across fakeStore + fakePub.
type orderedRecorder struct {
	mu  sync.Mutex
	log []string
}

func (o *orderedRecorder) record(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.log = append(o.log, s)
}

func (o *orderedRecorder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.log))
	copy(out, o.log)
	return out
}

// fakeStore is an in-memory Store. failInsert, when true, makes
// Insert return an error (simulating a persist failure / "crash
// between persist and publish" — the caller of Insert would never
// reach the Publish call in that case, since emitLine returns early).
type fakeStore struct {
	mu         sync.Mutex
	rows       []*persistence.ExecutionNarration
	seqByExec  map[string]int64
	recorder   *orderedRecorder
	failInsert bool
}

func newFakeStore(rec *orderedRecorder) *fakeStore {
	return &fakeStore{seqByExec: map[string]int64{}, recorder: rec}
}

func (s *fakeStore) Insert(_ context.Context, row *persistence.ExecutionNarration) (int64, error) {
	if s.failInsert {
		return 0, errors.New("fakeStore: simulated persist failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.seqByExec[row.ExecutionID]
	s.seqByExec[row.ExecutionID] = seq + 1
	cp := *row
	cp.Seq = seq
	s.rows = append(s.rows, &cp)
	if s.recorder != nil {
		s.recorder.record("store:" + row.ExecutionID)
	}
	return seq, nil
}

func (s *fakeStore) all() []*persistence.ExecutionNarration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*persistence.ExecutionNarration, len(s.rows))
	copy(out, s.rows)
	return out
}

// fakePub is an in-memory EventPublisher.
type fakePub struct {
	mu       sync.Mutex
	payloads []livepubsub.NarrationLinePayload
	recorder *orderedRecorder
}

func newFakePub(rec *orderedRecorder) *fakePub {
	return &fakePub{recorder: rec}
}

func (p *fakePub) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	if kind != livepubsub.KindNarrationLine {
		return 0
	}
	np, ok := payload.(livepubsub.NarrationLinePayload)
	if !ok {
		return 0
	}
	p.mu.Lock()
	p.payloads = append(p.payloads, np)
	p.mu.Unlock()
	if p.recorder != nil {
		p.recorder.record("publish:" + executionID)
	}
	return np.Seq
}

func (p *fakePub) all() []livepubsub.NarrationLinePayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]livepubsub.NarrationLinePayload, len(p.payloads))
	copy(out, p.payloads)
	return out
}

// fakeExecutions resolves a fixed set of executions by ID.
type fakeExecutions struct {
	mu   sync.Mutex
	byID map[string]*persistence.Execution
}

func newFakeExecutions() *fakeExecutions {
	return &fakeExecutions{byID: map[string]*persistence.Execution{}}
}

//nolint:unparam // reusable test fake; every current test happens to seed proj-1, but the fake API stays general
func (f *fakeExecutions) set(id, projectID, taskID string, status persistence.ExecutionStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id] = &persistence.Execution{ID: id, ProjectID: projectID, TaskID: taskID, Status: status}
}

func (f *fakeExecutions) unset(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, id)
}

func (f *fakeExecutions) Get(_ context.Context, id string) (*persistence.Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.byID[id]; ok {
		cp := *e
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

// fakeProvider is a minimal chat.Provider stand-in. Complete returns
// replies in order (or the last one repeated once exhausted); Prompts
// records every user-message body sent, so tests can assert the
// untrusted-field delimiting shape without needing a real LLM.
type fakeProvider struct {
	mu      sync.Mutex
	replies []string
	err     error
	calls   int
	Prompts []string
	model   string
}

func (f *fakeProvider) Complete(_ context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range messages {
		if m.Role == "user" {
			f.Prompts = append(f.Prompts, m.Content)
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	idx := f.calls
	if idx >= len(f.replies) {
		idx = len(f.replies) - 1
	}
	f.calls++
	if idx < 0 {
		return &chat.ChatResponse{}, nil
	}
	resp := &chat.ChatResponse{Model: "fake-model"}
	resp.Choices = []struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{
		{Message: chat.Message{Role: "assistant", Content: f.replies[idx]}},
	}
	resp.Usage.PromptTokens = 10
	resp.Usage.CompletionTokens = 5
	return resp, nil
}

func (f *fakeProvider) CompleteWithTools(ctx context.Context, messages []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	return f.Complete(ctx, messages)
}

func (f *fakeProvider) CompleteWithToolsStream(ctx context.Context, messages []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return f.Complete(ctx, messages)
}

func (f *fakeProvider) Model() string { return f.model }

func (f *fakeProvider) SetMetrics(_ *chat.Metrics) {}

var _ chat.Provider = (*fakeProvider)(nil)

// fakeUsageRecorder captures every task_llm_usage row Record sees.
type fakeUsageRecorder struct {
	mu   sync.Mutex
	rows []*persistence.TaskLLMUsage
}

func (r *fakeUsageRecorder) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, u)
	return nil
}

// Upsert satisfies llmspend.UsageRepo; the narrator only calls Record.
func (r *fakeUsageRecorder) Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error {
	return r.Record(ctx, u)
}

func (r *fakeUsageRecorder) all() []*persistence.TaskLLMUsage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*persistence.TaskLLMUsage, len(r.rows))
	copy(out, r.rows)
	return out
}

// fakePricing charges a fixed cost per call regardless of tokens —
// keeps cost-cap tests independent of token-count arithmetic.
type fakePricing struct{ perCall float64 }

func (p fakePricing) CostUSD(_ string, _, _ int) float64 {
	return p.perCall
}
