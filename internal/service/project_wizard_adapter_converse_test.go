package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectwizard"
)

// fakeWizardSessionStore is a minimal in-memory
// projectwizard.SessionStore — just enough for Converse to run a
// single fresh-session turn. Mirrors the fakeSessionStore in
// projectwizard/wizard_test.go (unexported there, so duplicated
// rather than shared across packages).
type fakeWizardSessionStore struct {
	mu   sync.Mutex
	rows map[string]*persistence.ProjectWizardSession
}

func newFakeWizardSessionStore() *fakeWizardSessionStore {
	return &fakeWizardSessionStore{rows: map[string]*persistence.ProjectWizardSession{}}
}

func (f *fakeWizardSessionStore) Insert(_ context.Context, s *persistence.ProjectWizardSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *s
	f.rows[s.ID] = &clone
	return nil
}

func (f *fakeWizardSessionStore) Get(_ context.Context, id string) (*persistence.ProjectWizardSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	clone := *r
	return &clone, nil
}

func (f *fakeWizardSessionStore) Update(_ context.Context, s *persistence.ProjectWizardSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[s.ID]; !ok {
		return persistence.ErrNotFound
	}
	clone := *s
	f.rows[s.ID] = &clone
	return nil
}

func (f *fakeWizardSessionStore) ListByOperator(_ context.Context, operatorID string, pageSize int) ([]*persistence.ProjectWizardSession, error) {
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

func (f *fakeWizardSessionStore) CommitTo(_ context.Context, sessionID, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok {
		return persistence.ErrNotFound
	}
	row.CommittedProjectID = &projectID
	now := time.Now().UTC()
	row.CommittedAt = &now
	return nil
}

func (f *fakeWizardSessionStore) Cancel(_ context.Context, sessionID, operatorID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok {
		return persistence.ErrNotFound
	}
	if row.OperatorID != operatorID {
		return persistence.ErrNotFound
	}
	now := time.Now().UTC()
	row.CancelledAt = &now
	return nil
}

// fakeWizardChatProvider returns one scripted content string on
// every call — enough to drive a single Converse turn. Mirrors
// fakeChatProvider in projectwizard/wizard_test.go.
type fakeWizardChatProvider struct {
	calls   atomic.Int32
	content string
}

func (f *fakeWizardChatProvider) Complete(_ context.Context, _ []chat.Message) (*chat.ChatResponse, error) {
	f.calls.Add(1)
	resp := &chat.ChatResponse{Model: "fake"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: f.content}, FinishReason: "stop"})
	return resp, nil
}

func (f *fakeWizardChatProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeWizardChatProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeWizardChatProvider) Model() string            { return "fake" }
func (f *fakeWizardChatProvider) SetMetrics(*chat.Metrics) {}

// envelopeWithComposition is a canned wizard-v2 turn: a complete
// composition (template + params + one addon) with ready_to_commit
// true. The compose pipeline itself isn't wired in this fixture (no
// Templates source), so Compose fails and the wizard appends a
// "(composition: ...)" note to the message and forces
// ready_to_commit=false — but per wizard.go's Converse, the
// Envelope.Composition field on the returned envelope is untouched by
// that failure path, so it's still the right fixture for a mapping
// test that only cares about the adapter's field-for-field mirror.
const envelopeWithComposition = `{
	"message": "Here is your build.",
	"ready_to_commit": true,
	"composition": {
		"template": "python-scraper",
		"params": {"schedule": "daily"},
		"addons": [
			{"type": "schedule", "interval": "24h", "goal": "scrape the site"}
		]
	}
}`

func TestProjectWizardAdapter_Converse_MirrorsCompositionOntoAPIEnvelope(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	adapter := newProjectWizardAdapter(wiz)
	require.NotNil(t, adapter)

	res, err := adapter.Converse(context.Background(), "", "op_1", "build me a daily scraper")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Envelope)

	comp := res.Envelope.Composition
	require.NotNil(t, comp, "expected composition to be mirrored onto the API envelope")
	require.Equal(t, "python-scraper", comp.Template)
	require.Equal(t, []any{"daily"}, comp.Params["schedule"])
	require.Len(t, comp.Addons, 1)
	require.Equal(t, "schedule", comp.Addons[0]["type"])
	require.Equal(t, "24h", comp.Addons[0]["interval"])
}

func TestProjectWizardAdapter_Converse_NoCompositionOnLegacyProposalTurn(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{
		content: `{"message":"Draft ready.","ready_to_commit":false,"proposal":{"raw":{"projectId":"news","displayName":"News"}}}`,
	}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	adapter := newProjectWizardAdapter(wiz)

	res, err := adapter.Converse(context.Background(), "", "op_1", "I want a news feed")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Envelope)
	require.Nil(t, res.Envelope.Composition, "v1 proposal-only turn should not carry a composition")
	require.Nil(t, res.Envelope.Bundle, "v1 proposal-only turn should not carry a bundle")
}

// envelopeWithBundle is a canned wizard tier-3 turn: a full composer
// bundle (project + swarm + one workflow + user-facing plan). The
// bundle pipeline itself isn't wired in this fixture (no
// LiveConfigDir/RoleLibrary), so applyBundle bounces the turn — but
// per wizard.go's applyBundle, envelope.Bundle is only cleared from
// session.Bundle on a rejection, never from the envelope itself, so
// this is still the right fixture for a mapping test that only cares
// about the adapter's bundle-mirroring, exactly like
// envelopeWithComposition above for Composition.
const envelopeWithBundle = `{
	"message": "Here is your custom automation.",
	"tier": 3,
	"ready_to_commit": true,
	"bundle": {
		"project": {"projectId": "custom-proj", "displayName": "Custom Project"},
		"swarm": {"swarmId": "custom-swarm"},
		"workflows": [{"workflowId": "wf1"}],
		"plan": {
			"steps": ["Step one", "Step two"],
			"schedule": "every 6 hours",
			"cost_band": "$1-5/day",
			"approvals": ["review before send"],
			"approvals_bypassed": ["auto-approve small changes"]
		}
	}
}`

func TestProjectWizardAdapter_Converse_MirrorsBundleOntoAPIEnvelope(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: envelopeWithBundle}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	adapter := newProjectWizardAdapter(wiz)
	require.NotNil(t, adapter)

	res, err := adapter.Converse(context.Background(), "", "op_1", "build me a fully custom automation")
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Envelope)

	bundle := res.Envelope.Bundle
	require.NotNil(t, bundle, "expected bundle to be mirrored onto the API envelope")

	project, ok := bundle["project"].(map[string]any)
	require.True(t, ok, "bundle.project should be a map, got %T", bundle["project"])
	require.Equal(t, "custom-proj", project["projectId"])

	swarm, ok := bundle["swarm"].(map[string]any)
	require.True(t, ok, "bundle.swarm should be a map, got %T", bundle["swarm"])
	require.Equal(t, "custom-swarm", swarm["swarmId"])

	workflows, ok := bundle["workflows"].([]any)
	require.True(t, ok, "bundle.workflows should be a slice, got %T", bundle["workflows"])
	require.Len(t, workflows, 1)

	plan, ok := bundle["plan"].(map[string]any)
	require.True(t, ok, "bundle.plan should be a map, got %T", bundle["plan"])
	require.Equal(t, []any{"Step one", "Step two"}, plan["steps"])
	require.Equal(t, "every 6 hours", plan["schedule"])
	require.Equal(t, "$1-5/day", plan["cost_band"])
	require.Equal(t, []any{"review before send"}, plan["approvals"])
	require.Equal(t, []any{"auto-approve small changes"}, plan["approvals_bypassed"])
}

// TestProjectWizardAdapter_ConfirmSchedule_Delegates verifies the
// api.ProjectWizard boundary (task 1.2a's schedule chip) delegates
// straight through to the underlying *projectwizard.Wizard and its
// error contract survives the adapter unchanged.
func TestProjectWizardAdapter_ConfirmSchedule_Delegates(t *testing.T) {
	store := newFakeWizardSessionStore()
	session := &persistence.ProjectWizardSession{ID: "pw_1", OperatorID: "op_1"}
	require.NoError(t, store.Insert(context.Background(), session))

	wiz := &projectwizard.Wizard{Sessions: store}
	adapter := newProjectWizardAdapter(wiz)
	require.NotNil(t, adapter)

	require.NoError(t, adapter.ConfirmSchedule(context.Background(), "pw_1", "op_1", "24h"))

	stored, err := store.Get(context.Background(), "pw_1")
	require.NoError(t, err)
	require.NotNil(t, stored.ScheduleConfirmedAt)
	require.Equal(t, "24h", stored.ScheduleConfirmedCron)

	// Not-found propagates unchanged through the adapter.
	err = adapter.ConfirmSchedule(context.Background(), "pw_missing", "op_1", "24h")
	require.ErrorIs(t, err, persistence.ErrNotFound)
}
