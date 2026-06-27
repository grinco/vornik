package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// fakeOperatorProfileRepo serves a fixed profile (or error) on
// every Get. Records the operator_id the dispatcher asked for
// so tests can assert the right lookup key.
type fakeOperatorProfileRepo struct {
	profile     *persistence.OperatorProfile
	err         error
	lastLookup  string
	lookupCount atomic.Int64
}

func (f *fakeOperatorProfileRepo) Get(_ context.Context, operatorID string) (*persistence.OperatorProfile, error) {
	f.lookupCount.Add(1)
	f.lastLookup = operatorID
	if f.err != nil {
		return nil, f.err
	}
	if f.profile == nil {
		return nil, persistence.ErrNotFound
	}
	return f.profile, nil
}

func (f *fakeOperatorProfileRepo) Upsert(_ context.Context, _ *persistence.OperatorProfile) error {
	return nil
}
func (f *fakeOperatorProfileRepo) Delete(_ context.Context, _ string) error { return nil }
func (f *fakeOperatorProfileRepo) List(_ context.Context, _ int) ([]*persistence.OperatorProfile, error) {
	return nil, nil
}

// promptCapturingProvider records the system prompt of every
// chat call. Returns a fixed "all done" response so the agent
// loop terminates after one iteration.
type promptCapturingProvider struct {
	systemPrompts []string
}

func (p *promptCapturingProvider) Complete(_ context.Context, messages []chat.Message) (*chat.ChatResponse, error) {
	if len(messages) > 0 && messages[0].Role == "system" {
		p.systemPrompts = append(p.systemPrompts, messages[0].Content)
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"})
	return resp, nil
}

func (p *promptCapturingProvider) CompleteWithTools(_ context.Context, messages []chat.Message, _ []chat.Tool) (*chat.ChatResponse, error) {
	if len(messages) > 0 && messages[0].Role == "system" {
		p.systemPrompts = append(p.systemPrompts, messages[0].Content)
	}
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop"})
	return resp, nil
}

func (p *promptCapturingProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, _ chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, nil
}
func (p *promptCapturingProvider) Model() string              { return "stub" }
func (p *promptCapturingProvider) SetMetrics(_ *chat.Metrics) {}

// newTestAgentWithProfile constructs a minimal Agent for the
// operator-profile injection tests. nil profileRepo simulates
// the "repo not wired" branch. All other deps stay nil — the
// test exercises only the prompt-build path; tools etc. are
// out of scope.
func newTestAgentWithProfile(t *testing.T, provider chat.Provider, profileRepo persistence.OperatorProfileRepository) *Agent {
	t.Helper()
	opts := []AgentOption{
		WithMaxIterations(1),
	}
	if profileRepo != nil {
		opts = append(opts, WithOperatorProfileRepository(profileRepo))
	}
	return NewAgent(provider, nil, nil, nil, nil, opts...)
}

// TestAgentProcess_InjectsOperatorProfileWhenWired: when the
// agent has a profile repo AND the request carries an OperatorID
// AND a row exists, the system prompt sent to the chat provider
// MUST contain the <operator_profile> block.
func TestAgentProcess_InjectsOperatorProfileWhenWired(t *testing.T) {
	provider := &promptCapturingProvider{}
	repo := &fakeOperatorProfileRepo{
		profile: &persistence.OperatorProfile{
			OperatorID: "telegram:42",
			Structured: []byte(`{"tone":"terse"}`),
			Notes:      "operator prefers code blocks",
		},
	}
	agent := newTestAgentWithProfile(t, provider, repo)

	req := Request{
		OperatorID:       "telegram:42",
		LeadSystemPrompt: "you are the lead",
		Messages:         []chat.Message{{Role: "user", Content: "hi"}},
	}
	_ = agent.Process(context.Background(), req)

	if len(provider.systemPrompts) == 0 {
		t.Fatalf("no chat call was made")
	}
	got := provider.systemPrompts[0]
	if !strings.Contains(got, "<operator_profile>") {
		t.Errorf("system prompt missing <operator_profile>: %q", got)
	}
	if !strings.Contains(got, "tone: terse") {
		t.Errorf("system prompt missing structured key: %q", got)
	}
	if !strings.Contains(got, "operator prefers code blocks") {
		t.Errorf("system prompt missing notes: %q", got)
	}
	if repo.lastLookup != "telegram:42" {
		t.Errorf("repo lookup id = %q, want telegram:42", repo.lastLookup)
	}
}

// TestAgentProcess_NoProfileBlockWhenOperatorIDEmpty: a turn
// without an OperatorID (synthesised internal turns, autonomy
// loops) MUST NOT do a repo lookup or inject a block.
func TestAgentProcess_NoProfileBlockWhenOperatorIDEmpty(t *testing.T) {
	provider := &promptCapturingProvider{}
	repo := &fakeOperatorProfileRepo{
		profile: &persistence.OperatorProfile{OperatorID: "x", Notes: "x"},
	}
	agent := newTestAgentWithProfile(t, provider, repo)

	req := Request{
		LeadSystemPrompt: "lead",
		Messages:         []chat.Message{{Role: "user", Content: "hi"}},
	}
	_ = agent.Process(context.Background(), req)

	if repo.lookupCount.Load() != 0 {
		t.Errorf("repo Get called %d times for empty OperatorID; should be 0", repo.lookupCount.Load())
	}
	if len(provider.systemPrompts) > 0 && strings.Contains(provider.systemPrompts[0], "<operator_profile>") {
		t.Errorf("profile block injected for empty OperatorID: %q", provider.systemPrompts[0])
	}
}

// TestAgentProcess_NoBlockWhenRepoUnwired: an agent constructed
// without the profile repo wired (most tests + degraded boot)
// does not crash and does not inject the block.
func TestAgentProcess_NoBlockWhenRepoUnwired(t *testing.T) {
	provider := &promptCapturingProvider{}
	agent := newTestAgentWithProfile(t, provider, nil)

	req := Request{
		OperatorID:       "telegram:42",
		LeadSystemPrompt: "lead",
		Messages:         []chat.Message{{Role: "user", Content: "hi"}},
	}
	_ = agent.Process(context.Background(), req)

	if len(provider.systemPrompts) == 0 {
		t.Fatalf("no chat call")
	}
	if strings.Contains(provider.systemPrompts[0], "<operator_profile>") {
		t.Errorf("block injected without repo: %q", provider.systemPrompts[0])
	}
}

// TestAgentProcess_RepoErrorDegradesSilently: a DB blip on the
// profile read MUST NOT break the turn. The dispatcher omits
// the block + logs the error internally.
func TestAgentProcess_RepoErrorDegradesSilently(t *testing.T) {
	provider := &promptCapturingProvider{}
	repo := &fakeOperatorProfileRepo{err: errors.New("connection refused")}
	agent := newTestAgentWithProfile(t, provider, repo)

	req := Request{
		OperatorID:       "telegram:42",
		LeadSystemPrompt: "lead",
		Messages:         []chat.Message{{Role: "user", Content: "hi"}},
	}
	_ = agent.Process(context.Background(), req)
	// The dispatcher signals failure via Text / log rather than
	// an Error field on Result — confirm graceful degradation
	// via "chat call happened, block absent".
	if len(provider.systemPrompts) == 0 {
		t.Errorf("repo error blocked the chat call; should degrade gracefully")
	}
	if len(provider.systemPrompts) > 0 && strings.Contains(provider.systemPrompts[0], "<operator_profile>") {
		t.Errorf("block injected despite repo error: %q", provider.systemPrompts[0])
	}
}

// TestAgentProcess_NotFoundIsNotAnError: ErrNotFound from the
// repo (operator has no profile yet) is the expected "fresh
// operator" path; no block, no error, no log noise.
func TestAgentProcess_NotFoundIsNotAnError(t *testing.T) {
	provider := &promptCapturingProvider{}
	repo := &fakeOperatorProfileRepo{} // returns ErrNotFound by default
	agent := newTestAgentWithProfile(t, provider, repo)

	req := Request{
		OperatorID:       "telegram:42",
		LeadSystemPrompt: "lead",
		Messages:         []chat.Message{{Role: "user", Content: "hi"}},
	}
	_ = agent.Process(context.Background(), req)
	// Repo is consulted exactly once. No block injected.
	if repo.lookupCount.Load() != 1 {
		t.Errorf("repo Get called %d times, want 1", repo.lookupCount.Load())
	}
	if len(provider.systemPrompts) > 0 && strings.Contains(provider.systemPrompts[0], "<operator_profile>") {
		t.Errorf("block injected on ErrNotFound: %q", provider.systemPrompts[0])
	}
}
