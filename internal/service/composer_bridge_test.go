package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectwizard"
)

// TestNewComposerBridge_NilWizardReturnsNilInterface — a missing
// Wizard (chat disabled / no sessions repo — see buildProjectWizard's
// gating) must produce a genuinely nil dispatcher.ComposerBridge, not
// a typed-nil *composerBridge wrapped in a non-nil interface, so the
// dispatcher's `te.composer == nil` check disables the tool cleanly.
func TestNewComposerBridge_NilWizardReturnsNilInterface(t *testing.T) {
	got := newComposerBridge(nil, "")
	if got != nil {
		t.Fatalf("expected nil interface, got %#v", got)
	}
}

// TestComposerBridge_ReadyPlan_FormatsStepsScheduleCostApprovals
// drives a real *projectwizard.Wizard through a tier-3 turn (reusing
// the envelopeWithBundle fixture from
// project_wizard_adapter_converse_test.go — same package, same
// canned bundle) and asserts the bridge renders the ComposedPlan as
// readable chat text: numbered steps, schedule, a cost estimate
// explicitly labelled an estimate, approvals, and ApprovalsBypassed
// rendered prominently — plus a preview URL pointing at the session.
func TestComposerBridge_ReadyPlan_FormatsStepsScheduleCostApprovals(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: envelopeWithBundle}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := newComposerBridge(wiz, "https://vornik.example.com")
	require.NotNil(t, bridge)

	planText, previewURL, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "build me a fully custom automation")
	require.NoError(t, err)
	require.False(t, needsInput, "a tier-3 turn with a bundle must not be reported as needing more input")

	require.Contains(t, planText, "1. Step one")
	require.Contains(t, planText, "2. Step two")
	require.Contains(t, planText, "Schedule: every 6 hours")
	require.Contains(t, planText, "Cost: $1-5/day")
	require.Contains(t, planText, "estimate only")
	require.Contains(t, planText, "review before send")
	require.Contains(t, planText, "⚠ Will proceed WITHOUT asking")
	require.Contains(t, planText, "auto-approve small changes")

	require.True(t, strings.HasPrefix(previewURL, "https://vornik.example.com/ui/projects/new/wizard?session="))
}

// TestComposerBridge_ClarifyingTurn_NoBundleYet drives a turn whose
// envelope carries no tier-3 bundle (reusing
// envelopeWithComposition — a wizard-v2 composition turn, tier 0) and
// asserts the bridge reports needsInput=true with the composer's own
// message as planText and no preview URL — there's nothing to commit
// yet.
func TestComposerBridge_ClarifyingTurn_NoBundleYet(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := newComposerBridge(wiz, "")

	planText, previewURL, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "build me a daily scraper")
	require.NoError(t, err)
	require.True(t, needsInput)
	require.Empty(t, previewURL)
	require.Contains(t, planText, "Here is your build.")
}

// TestComposerBridge_ClarifyingTurn_WithOpenQuestions asserts the
// composer's suggested quick-replies (open_questions) are folded into
// the clarifying text as a "you could reply" hint, matching the chip
// affordance the web wizard renders.
func TestComposerBridge_ClarifyingTurn_WithOpenQuestions(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: `{
		"message": "What schedule should this run on?",
		"ready_to_commit": false,
		"open_questions": ["every 6 hours", "daily", "weekly"]
	}`}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := newComposerBridge(wiz, "")

	planText, previewURL, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "scrape the site")
	require.NoError(t, err)
	require.True(t, needsInput)
	require.Empty(t, previewURL)
	require.Contains(t, planText, "What schedule should this run on?")
	require.Contains(t, planText, "every 6 hours / daily / weekly")
}

// TestComposerBridge_OpenOrContinue_SameConversationReusesSession —
// two ComposeTurn calls with the SAME (operatorID, conversationID)
// must drive the Converse loop against the SAME wizard session (one
// row in the store, not two), and a DIFFERENT conversationID for the
// same operator must open a second, independent session.
func TestComposerBridge_OpenOrContinue_SameConversationReusesSession(t *testing.T) {
	store := newFakeWizardSessionStore()
	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := newComposerBridge(wiz, "")

	_, _, _, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "build me a daily scraper")
	require.NoError(t, err)
	_, _, _, err = bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "every morning at 8am")
	require.NoError(t, err)

	rows, err := store.ListByOperator(context.Background(), "op_1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "same conversation must continue the SAME wizard session")

	// A distinct conversation for the same operator opens a second,
	// independent session.
	_, _, _, err = bridge.ComposeTurn(context.Background(), "op_1", "telegram:2", "build me something else")
	require.NoError(t, err)
	rows, err = store.ListByOperator(context.Background(), "op_1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2, "a different conversation must open a separate session")
}

// TestComposerBridge_CommittedSession_TransparentlyOpensFresh — when
// the mapped session was committed out from under the chat
// conversation (the operator finished it via the web UI), the next
// compose_automation turn for that same conversation must not
// permanently fail with ErrSessionCommitted — it should transparently
// start a fresh draft.
func TestComposerBridge_CommittedSession_TransparentlyOpensFresh(t *testing.T) {
	store := newFakeWizardSessionStore()
	committedID := "pw_committed"
	require.NoError(t, store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID:                 committedID,
		OperatorID:         "op_1",
		CommittedProjectID: strPtr("proj_1"),
	}))

	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := &composerBridge{wizard: wiz, sessions: map[string]string{"op_1|telegram:1": committedID}}

	planText, _, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "build me a daily scraper")
	require.NoError(t, err, "a committed-session collision must be handled transparently, not surfaced as an error")
	require.True(t, needsInput)
	require.NotEmpty(t, planText)

	newID := bridge.sessions["op_1|telegram:1"]
	require.NotEqual(t, committedID, newID, "the bridge must have opened a fresh session, not kept using the committed one")
}

// TestComposerBridge_TooManySessions_FriendlyMessage — hitting the
// wizard's per-operator concurrent-session cap must surface as a
// graceful chat message (needsInput=true, no Go error propagated to
// the dispatcher tool), not a raw ErrTooManySessions bubbling up as
// ToolResult error text.
func TestComposerBridge_TooManySessions_FriendlyMessage(t *testing.T) {
	store := newFakeWizardSessionStore()
	require.NoError(t, store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID:         "pw_existing",
		OperatorID: "op_1",
	}))

	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions:          store,
		Chat:              chatStub,
		MaxTurns:          5,
		Timeout:           time.Second,
		MaxActiveSessions: 1,
	}
	bridge := newComposerBridge(wiz, "")

	// A fresh conversationID forces sessionID="" on the Converse call,
	// which enforces the cap against the operator's existing
	// uncommitted session.
	planText, previewURL, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:new", "build me a daily scraper")
	require.NoError(t, err)
	require.True(t, needsInput)
	require.Empty(t, previewURL)
	require.Contains(t, strings.ToLower(planText), "already have")
}

// TestComposerBridge_TurnsExhausted_FriendlyMessage — hitting the
// wizard's per-session turn cap (MaxTurns) must surface as a graceful
// chat message, mirroring the too-many-sessions case above, not a raw
// Go error.
func TestComposerBridge_TurnsExhausted_FriendlyMessage(t *testing.T) {
	store := newFakeWizardSessionStore()
	transcript, err := json.Marshal([]projectwizard.Turn{
		{Role: "user", Content: "first ask", CreatedAt: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID:         "pw_exhausted",
		OperatorID: "op_1",
		Transcript: transcript,
	}))

	chatStub := &fakeWizardChatProvider{content: envelopeWithComposition}
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     chatStub,
		MaxTurns: 1, // the pre-seeded session already used its one turn
		Timeout:  time.Second,
	}
	bridge := &composerBridge{wizard: wiz, sessions: map[string]string{"op_1|telegram:1": "pw_exhausted"}}

	planText, previewURL, needsInput, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "one more thing")
	require.NoError(t, err)
	require.True(t, needsInput)
	require.Empty(t, previewURL)
	require.Contains(t, strings.ToLower(planText), "turn limit")
}

// erroringChatProvider always fails Complete — drives Wizard.Converse
// into its generic (non-sentinel) error path.
type erroringChatProvider struct{ err error }

func (e *erroringChatProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, e.err
}
func (e *erroringChatProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return nil, e.err
}
func (e *erroringChatProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, e.err
}
func (e *erroringChatProvider) Model() string            { return "fake" }
func (e *erroringChatProvider) SetMetrics(*chat.Metrics) {}

// TestComposerBridge_GenericError_Propagates — a hard failure that
// isn't one of the graceful sentinel cases (ErrTooManySessions /
// ErrTurnsExhausted / a stale-session collision) must propagate as a
// Go error, letting the dispatcher tool surface it as chat text
// rather than the bridge swallowing it silently.
func TestComposerBridge_GenericError_Propagates(t *testing.T) {
	store := newFakeWizardSessionStore()
	wiz := &projectwizard.Wizard{
		Sessions: store,
		Chat:     &erroringChatProvider{err: errors.New("upstream unavailable")},
		MaxTurns: 5,
		Timeout:  time.Second,
	}
	bridge := newComposerBridge(wiz, "")

	_, _, _, err := bridge.ComposeTurn(context.Background(), "op_1", "telegram:1", "build me a daily scraper")
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream unavailable")
}

// TestFormatComposedPlanText_EmptyCostBandFallsBackToNotEstimated —
// an ungrounded/empty cost_band (e.g. a bundle rejected before the
// grounded-estimate step overwrote it) must never render as a blank
// "Cost: " line.
func TestFormatComposedPlanText_EmptyCostBandFallsBackToNotEstimated(t *testing.T) {
	got := formatComposedPlanText("msg", projectwizard.ComposedPlan{Steps: []string{"one"}})
	require.Contains(t, got, "Cost: (not estimated)")
}

func strPtr(s string) *string { return &s }

// TestComposerBridgePreviewURL exercises previewURL directly across
// the configured-base-URL and relative-fallback cases (trailing
// slash trimmed, path always the wizard page with ?session=).
func TestComposerBridgePreviewURL(t *testing.T) {
	b := &composerBridge{baseURL: strings.TrimRight("https://vornik.example.com/", "/")}
	require.Equal(t, "https://vornik.example.com/ui/projects/new/wizard?session=pw_1", b.previewURL("pw_1"))

	b2 := &composerBridge{baseURL: ""}
	require.Equal(t, "/ui/projects/new/wizard?session=pw_1", b2.previewURL("pw_1"))
}

func TestComposeAutomationSessionKeyIncludesOperatorAndConversation(t *testing.T) {
	b := &composerBridge{}
	if got := b.sessionKey("op_1", "telegram:1"); got != "op_1|telegram:1" {
		t.Errorf("sessionKey = %q", got)
	}
	// Different operator or different conversation must never collapse
	// onto the same key.
	if b.sessionKey("op_1", "telegram:1") == b.sessionKey("op_2", "telegram:1") {
		t.Error("different operators collided on the same session key")
	}
	if b.sessionKey("op_1", "telegram:1") == b.sessionKey("op_1", "telegram:2") {
		t.Error("different conversations collided on the same session key")
	}
}
