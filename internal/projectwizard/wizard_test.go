package projectwizard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// fakeSessionStore is an in-memory ProjectWizardSessionRepository.
// Concurrency-safe so the test set could be `-parallel`'d.
type fakeSessionStore struct {
	mu    sync.Mutex
	rows  map[string]*persistence.ProjectWizardSession
	errOn string // optional: method name to fail (Insert/Get/Update)
}

func newFakeStore() *fakeSessionStore {
	return &fakeSessionStore{rows: map[string]*persistence.ProjectWizardSession{}}
}

func (f *fakeSessionStore) Insert(_ context.Context, s *persistence.ProjectWizardSession) error {
	if f.errOn == "Insert" {
		return errors.New("insert failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *s
	f.rows[s.ID] = &clone
	return nil
}

func (f *fakeSessionStore) Get(_ context.Context, id string) (*persistence.ProjectWizardSession, error) {
	if f.errOn == "Get" {
		return nil, errors.New("get failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	clone := *r
	return &clone, nil
}

func (f *fakeSessionStore) Update(_ context.Context, s *persistence.ProjectWizardSession) error {
	if f.errOn == "Update" {
		return errors.New("update failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[s.ID]; !ok {
		return persistence.ErrNotFound
	}
	clone := *s
	f.rows[s.ID] = &clone
	return nil
}

func (f *fakeSessionStore) ListByOperator(_ context.Context, operatorID string, pageSize int) ([]*persistence.ProjectWizardSession, error) {
	if f.errOn == "ListByOperator" {
		return nil, errors.New("list failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*persistence.ProjectWizardSession, 0)
	for _, r := range f.rows {
		if r != nil && r.OperatorID == operatorID {
			clone := *r
			out = append(out, &clone)
		}
	}
	if pageSize > 0 && len(out) > pageSize {
		out = out[:pageSize]
	}
	return out, nil
}

func (f *fakeSessionStore) CommitTo(_ context.Context, sessionID, projectID string) error {
	if f.errOn == "CommitTo" {
		return errors.New("commit failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok {
		return persistence.ErrNotFound
	}
	if row.CommittedProjectID != nil {
		return persistence.ErrInvalidTransition
	}
	row.CommittedProjectID = &projectID
	now := time.Now().UTC()
	row.CommittedAt = &now
	return nil
}

func (f *fakeSessionStore) Cancel(_ context.Context, sessionID, operatorID string) error {
	if f.errOn == "Cancel" {
		return errors.New("cancel failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok {
		return persistence.ErrNotFound
	}
	if row.OperatorID != operatorID {
		return persistence.ErrNotFound
	}
	if row.CommittedProjectID != nil {
		return persistence.ErrInvalidTransition
	}
	if row.CancelledAt != nil {
		// Idempotent: already cancelled.
		return nil
	}
	now := time.Now().UTC()
	row.CancelledAt = &now
	return nil
}

// fakeChatProvider returns scripted envelope responses keyed on
// call index. Models a one-shot scripted LLM for deterministic
// assertions; the real chat router gets exercised via integration
// tests.
type fakeChatProvider struct {
	calls   atomic.Int32
	replies []chatReply
}

type chatReply struct {
	content string
	err     error
}

func (f *fakeChatProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.replies) {
		return &chat.ChatResponse{}, nil
	}
	r := f.replies[idx]
	if r.err != nil {
		return nil, r.err
	}
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: r.content}, FinishReason: "stop"})
	resp.Usage.PromptTokens = 200
	resp.Usage.CompletionTokens = 50
	resp.Usage.TotalTokens = 250
	return resp, nil
}
func (f *fakeChatProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeChatProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	panic("not used")
}
func (f *fakeChatProvider) Model() string            { return "fake" }
func (f *fakeChatProvider) SetMetrics(*chat.Metrics) {}

// canned envelope JSONs for fixture replies.
const envelopeAskQuestion = `{"message":"What sites should I track?","ready_to_commit":false,"open_questions":["news.example.com","another.com"]}`
const envelopeProposeDraft = `{"message":"Here's a draft.","ready_to_commit":false,"proposal":{"raw":{"projectId":"news","displayName":"News"}}}`
const envelopeProposeReady = `{"message":"Looks good. Ready to commit?","ready_to_commit":true,"proposal":{"raw":{"projectId":"news","displayName":"News"}}}`
const envelopeMissingID = `{"message":"Draft.","ready_to_commit":true,"proposal":{"raw":{"displayName":"News"}}}`

func newWizardForTest(replies ...chatReply) (*Wizard, *fakeSessionStore, *fakeChatProvider) {
	store := newFakeStore()
	chatStub := &fakeChatProvider{replies: replies}
	w := &Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	return w, store, chatStub
}

func TestConverse_HappyPath_QuestionThenDraft(t *testing.T) {
	w, store, _ := newWizardForTest(
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeProposeDraft},
	)
	res1, err := w.Converse(context.Background(), "", "op_1", "I want a news feed")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if res1.SessionID == "" {
		t.Fatal("expected fresh session ID")
	}
	if res1.Envelope.Message != "What sites should I track?" {
		t.Errorf("envelope wrong: %+v", res1.Envelope)
	}
	if res1.Envelope.Proposal != nil {
		t.Error("turn 1 should not have proposal")
	}

	// Session row should carry the transcript with 2 turns
	// (user + assistant).
	stored, _ := store.Get(context.Background(), res1.SessionID)
	var transcript []Turn
	_ = json.Unmarshal(stored.Transcript, &transcript)
	if len(transcript) != 2 {
		t.Errorf("expected 2-turn transcript after turn 1, got %d", len(transcript))
	}

	// Turn 2 — operator answers, LLM proposes draft.
	res2, err := w.Converse(context.Background(), res1.SessionID, "op_1", "news.example.com please")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.SessionID != res1.SessionID {
		t.Errorf("session id changed across turns")
	}
	if res2.Envelope.Proposal == nil || res2.Envelope.Proposal.Raw["projectId"] != "news" {
		t.Errorf("turn 2 should have proposal with projectId=news, got %+v", res2.Envelope.Proposal)
	}
	if res2.Envelope.ReadyToCommit {
		t.Error("turn 2 should not be ready_to_commit (LLM said false)")
	}
	// Stored row should now reflect the proposal.
	stored, _ = store.Get(context.Background(), res2.SessionID)
	if len(stored.CurrentProposal) == 0 {
		t.Error("expected stored proposal")
	}
}

func TestConverse_CrossOperatorReturnsNotFound(t *testing.T) {
	w, _, _ := newWizardForTest(
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeProposeDraft},
	)
	res, err := w.Converse(context.Background(), "", "op_owner", "I want a news feed")
	if err != nil {
		t.Fatalf("owner turn: %v", err)
	}
	_, err = w.Converse(context.Background(), res.SessionID, "op_attacker", "steal this session")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-operator converse, got %v", err)
	}
}

func TestConverse_ValidatorRejectionForcesReadyFalse(t *testing.T) {
	w, _, _ := newWizardForTest(
		chatReply{content: envelopeMissingID},
	)
	res, err := w.Converse(context.Background(), "", "op_1", "build me a thing")
	if err != nil {
		t.Fatalf("converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("expected ready_to_commit=false on validator failure")
	}
	if !strings.Contains(res.Envelope.Message, "validation") {
		t.Errorf("expected validation note in message: %q", res.Envelope.Message)
	}
}

func TestConverse_EmptyMessageRejected(t *testing.T) {
	w, _, _ := newWizardForTest()
	if _, err := w.Converse(context.Background(), "", "op_1", "  "); err == nil {
		t.Error("expected empty-message error")
	}
}

func TestConverse_MissingOperatorRejected(t *testing.T) {
	w, _, _ := newWizardForTest()
	if _, err := w.Converse(context.Background(), "", "", "hi"); err == nil {
		t.Error("expected missing-operator error")
	}
}

func TestConverse_NotFullyWiredErrors(t *testing.T) {
	w := &Wizard{}
	if _, err := w.Converse(context.Background(), "", "op_1", "hi"); err == nil {
		t.Error("expected not-wired error")
	}
}

func TestConverse_SessionResumesAcrossTurns(t *testing.T) {
	w, _, chatStub := newWizardForTest(
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeProposeReady},
	)
	res1, _ := w.Converse(context.Background(), "", "op_1", "news feed")
	if chatStub.calls.Load() != 1 {
		t.Errorf("expected 1 chat call after turn 1, got %d", chatStub.calls.Load())
	}
	res2, _ := w.Converse(context.Background(), res1.SessionID, "op_1", "yes go ahead")
	if res2.SessionID != res1.SessionID {
		t.Errorf("session id mismatch")
	}
	if !res2.Envelope.ReadyToCommit {
		t.Error("turn 2 should signal ready_to_commit=true (validator passes minimal draft)")
	}
}

func TestConverse_CommittedSessionRejected(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	res, err := w.Converse(context.Background(), "", "op_1", "hi")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	// Simulate commit on the stored row.
	stored, _ := store.Get(context.Background(), res.SessionID)
	projectID := "news"
	stored.CommittedProjectID = &projectID
	_ = store.Update(context.Background(), stored)

	if _, err := w.Converse(context.Background(), res.SessionID, "op_1", "again"); !errors.Is(err, ErrSessionCommitted) {
		t.Errorf("expected ErrSessionCommitted, got %v", err)
	}
}

func TestConverse_TurnCapEnforced(t *testing.T) {
	replies := make([]chatReply, 6)
	for i := range replies {
		replies[i] = chatReply{content: envelopeAskQuestion}
	}
	w, _, _ := newWizardForTest(replies...)
	w.MaxTurns = 2
	res, err := w.Converse(context.Background(), "", "op_1", "first")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := w.Converse(context.Background(), res.SessionID, "op_1", "second"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if _, err := w.Converse(context.Background(), res.SessionID, "op_1", "third"); !errors.Is(err, ErrTurnsExhausted) {
		t.Errorf("expected ErrTurnsExhausted, got %v", err)
	}
}

func TestConverse_LLMErrorPropagates(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{err: errors.New("provider down")})
	if _, err := w.Converse(context.Background(), "", "op_1", "hi"); err == nil {
		t.Error("expected LLM error to bubble up")
	}
}

func TestParseEnvelope_HandlesFencedJSON(t *testing.T) {
	wrapped := "```json\n" + envelopeAskQuestion + "\n```"
	env, err := parseEnvelope(wrapped)
	if err != nil {
		t.Fatalf("parseEnvelope: %v", err)
	}
	if env.Message == "" {
		t.Error("expected message after unwrapping fences")
	}
}

func TestParseEnvelope_RejectsEmptyMessage(t *testing.T) {
	_, err := parseEnvelope(`{"message":"  ","ready_to_commit":false}`)
	if err == nil {
		t.Error("expected error on empty message")
	}
}

func TestPermissiveValidator(t *testing.T) {
	v := permissiveValidator{}
	if v.Validate(nil) == nil {
		t.Error("nil proposal should fail")
	}
	if v.Validate(&ProjectYAML{Raw: map[string]any{}}) == nil {
		t.Error("empty raw should fail")
	}
	if v.Validate(&ProjectYAML{Raw: map[string]any{"displayName": "x"}}) == nil {
		t.Error("missing projectId should fail")
	}
	if err := v.Validate(&ProjectYAML{Raw: map[string]any{"projectId": "ok"}}); err != nil {
		t.Errorf("valid proposal should pass: %v", err)
	}
}
