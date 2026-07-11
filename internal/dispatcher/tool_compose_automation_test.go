package dispatcher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/outputguard"
)

// stubComposerBridge is a scripted dispatcher.ComposerBridge double.
// It records every call's (operatorID, conversationID, message) so
// tests can assert open-or-continue behaviour, and returns either a
// canned success tuple or a canned error.
type stubComposerBridge struct {
	mu    sync.Mutex
	calls []composerCall

	planText   string
	previewURL string
	needsInput bool
	err        error
}

type composerCall struct {
	operatorID     string
	conversationID string
	message        string
}

func (s *stubComposerBridge) ComposeTurn(_ context.Context, operatorID, conversationID, message string) (string, string, bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, composerCall{operatorID, conversationID, message})
	s.mu.Unlock()
	return s.planText, s.previewURL, s.needsInput, s.err
}

func (s *stubComposerBridge) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func newComposeAutomationExecutor(bridge ComposerBridge, enabled bool) *ToolExecutor {
	return &ToolExecutor{
		composer:        bridge,
		composerEnabled: enabled,
		logger:          zerolog.Nop(),
	}
}

// TestComposeAutomation_NilBridge — no bridge wired at all (composer
// subsystem entirely absent) must degrade to a clean message, never a
// panic, regardless of composerEnabled.
func TestComposeAutomation_NilBridge(t *testing.T) {
	te := newComposeAutomationExecutor(nil, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"scrape prices daily"}`, 1)
	if !strings.Contains(res.Content, "isn't configured") {
		t.Errorf("expected not-configured message, got %q", res.Content)
	}
}

// TestComposeAutomation_DisabledDuringSoak — bridge is wired (the
// Wizard exists) but composer.enabled is false (the Phase 3 soak
// default) — the tool must refuse gracefully without ever calling
// ComposeTurn.
func TestComposeAutomation_DisabledDuringSoak(t *testing.T) {
	bridge := &stubComposerBridge{}
	te := newComposeAutomationExecutor(bridge, false)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"scrape prices daily"}`, 1)
	if !strings.Contains(res.Content, "isn't enabled yet") {
		t.Errorf("expected soak refusal message, got %q", res.Content)
	}
	if bridge.callCount() != 0 {
		t.Errorf("ComposeTurn must not be called while disabled; got %d calls", bridge.callCount())
	}
}

// TestComposeAutomation_NoOperatorID — a synthesised/internal turn
// (no WithOperatorID stamped on ctx) must refuse rather than silently
// attribute the composer session to an empty operator id.
func TestComposeAutomation_NoOperatorID(t *testing.T) {
	bridge := &stubComposerBridge{}
	te := newComposeAutomationExecutor(bridge, true)

	res := te.composeAutomation(context.Background(), `{"request":"scrape prices daily"}`, 0)
	if !strings.Contains(res.Content, "not initiated by an identified operator") {
		t.Errorf("expected operator-identity refusal, got %q", res.Content)
	}
	if bridge.callCount() != 0 {
		t.Errorf("ComposeTurn must not be called without an operator id; got %d calls", bridge.callCount())
	}
}

// TestComposeAutomation_EmptyRequest — blank `request` is rejected
// before ever reaching the bridge (never a wasted Converse call / LLM
// spend on an empty ask).
func TestComposeAutomation_EmptyRequest(t *testing.T) {
	bridge := &stubComposerBridge{}
	te := newComposeAutomationExecutor(bridge, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"   "}`, 1)
	if !strings.Contains(res.Content, "`request` is required") {
		t.Errorf("expected empty-request refusal, got %q", res.Content)
	}
	if bridge.callCount() != 0 {
		t.Errorf("ComposeTurn must not be called with an empty request; got %d calls", bridge.callCount())
	}
}

// TestComposeAutomation_InvalidJSON — malformed tool-call arguments
// produce a content-string refusal, never a panic.
func TestComposeAutomation_InvalidJSON(t *testing.T) {
	te := newComposeAutomationExecutor(&stubComposerBridge{}, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{not json`, 1)
	if !strings.Contains(res.Content, "invalid arguments") {
		t.Errorf("expected invalid-arguments message, got %q", res.Content)
	}
}

// TestComposeAutomation_ReadyPlan_IncludesPreviewLink — a tier-3
// bundle-ready turn (needsInput=false) must surface the plan text AND
// the preview/commit link, tagged first-party for the output guard.
func TestComposeAutomation_ReadyPlan_IncludesPreviewLink(t *testing.T) {
	bridge := &stubComposerBridge{
		planText:   "Plan:\n1. Scrape the site\n2. Email a summary",
		previewURL: "https://vornik.example.com/ui/projects/new/wizard?session=pw_1",
		needsInput: false,
	}
	te := newComposeAutomationExecutor(bridge, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"scrape the site daily and email me a summary"}`, 1)
	if !strings.Contains(res.Content, "1. Scrape the site") {
		t.Errorf("plan steps missing from content: %q", res.Content)
	}
	if !strings.Contains(res.Content, bridge.previewURL) {
		t.Errorf("preview URL missing from content: %q", res.Content)
	}
	if res.Provenance != outputguard.ProvenanceFirstParty {
		t.Errorf("expected first-party provenance, got %v", res.Provenance)
	}
}

// TestComposeAutomation_ClarifyingTurn_NoLink — a still-gathering turn
// (needsInput=true, empty previewURL) must surface ONLY the
// clarifying text — no dangling/empty link line.
func TestComposeAutomation_ClarifyingTurn_NoLink(t *testing.T) {
	bridge := &stubComposerBridge{
		planText:   "What schedule should this run on?",
		previewURL: "",
		needsInput: true,
	}
	te := newComposeAutomationExecutor(bridge, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"scrape the site"}`, 1)
	if res.Content != "What schedule should this run on?" {
		t.Errorf("content = %q, want the clarifying question verbatim (no link)", res.Content)
	}
	if strings.Contains(res.Content, "http") {
		t.Errorf("clarifying turn must not include a link: %q", res.Content)
	}
}

// TestComposeAutomation_BridgeError — a hard bridge/wizard error
// (not one of the graceful sentinel cases) surfaces as chat text, not
// a panic or a swallowed failure.
func TestComposeAutomation_BridgeError(t *testing.T) {
	bridge := &stubComposerBridge{err: errors.New("boom")}
	te := newComposeAutomationExecutor(bridge, true)
	ctx := WithOperatorID(context.Background(), "telegram:1")

	res := te.composeAutomation(ctx, `{"request":"scrape the site"}`, 1)
	if !strings.Contains(res.Content, "boom") {
		t.Errorf("expected bridge error surfaced in content, got %q", res.Content)
	}
}

// TestComposeAutomation_OpenOrContinue_SameConversationKey — a
// second compose_automation call from the SAME originating channel
// session must present the bridge with the SAME conversationID as the
// first call, so the bridge (which maps conversationID -> wizard
// session id) continues the SAME draft instead of opening a new one
// every turn.
func TestComposeAutomation_OpenOrContinue_SameConversationKey(t *testing.T) {
	bridge := &stubComposerBridge{planText: "ok", needsInput: true}
	te := newComposeAutomationExecutor(bridge, true)

	ctx := WithOperatorID(context.Background(), "telegram:1")
	ctx = withOriginatingChannel(ctx, "telegram", "555")

	te.composeAutomation(ctx, `{"request":"scrape the site daily"}`, 1)
	te.composeAutomation(ctx, `{"request":"every morning at 8am"}`, 1)

	if bridge.callCount() != 2 {
		t.Fatalf("expected 2 ComposeTurn calls, got %d", bridge.callCount())
	}
	first := bridge.calls[0]
	second := bridge.calls[1]
	if first.conversationID != second.conversationID {
		t.Errorf("conversationID drifted across turns: %q vs %q", first.conversationID, second.conversationID)
	}
	if first.conversationID != "telegram:555" {
		t.Errorf("conversationID = %q, want %q", first.conversationID, "telegram:555")
	}
	if first.operatorID != "telegram:1" || second.operatorID != "telegram:1" {
		t.Errorf("operatorID not threaded through: %q / %q", first.operatorID, second.operatorID)
	}
}

// TestComposerConversationID exercises the derivation helper directly
// across its precedence rules: originating-channel context first,
// legacy chatID fallback second, empty when neither is present.
func TestComposerConversationID(t *testing.T) {
	if got := composerConversationID(context.Background(), 0); got != "" {
		t.Errorf("no context/chatID: got %q, want empty", got)
	}
	if got := composerConversationID(context.Background(), 42); got != "telegram:42" {
		t.Errorf("chatID fallback: got %q, want telegram:42", got)
	}
	ctx := withOriginatingChannel(context.Background(), "email", "thread-abc")
	if got := composerConversationID(ctx, 42); got != "email:thread-abc" {
		t.Errorf("originating-channel precedence: got %q, want email:thread-abc", got)
	}
}

// TestAgent_SetComposerBridge_NilSafe — a nil *Agent receiver must not
// panic (mirrors every other Set* wiring method's defensive guard).
func TestAgent_SetComposerBridge_NilSafe(_ *testing.T) {
	var a *Agent
	a.SetComposerBridge(&stubComposerBridge{}, true)
}

// TestAgent_SetComposerBridge_PropagatesToToolExecutor — the
// late-binding setter (same chicken-and-egg pattern as
// SetEmailSender: the container builds the composer bridge after the
// dispatcher Agent itself) must reach the ToolExecutor the compose_automation
// handler actually reads, not just the Agent's own field.
func TestAgent_SetComposerBridge_PropagatesToToolExecutor(t *testing.T) {
	a := NewAgent(nil, nil, nil, nil, nil)
	bridge := &stubComposerBridge{planText: "ok", needsInput: true}

	a.SetComposerBridge(bridge, true)

	if a.composer != ComposerBridge(bridge) {
		t.Error("Agent.composer not set")
	}
	if !a.composerEnabled {
		t.Error("Agent.composerEnabled not set")
	}
	if a.toolExecutor.composer != ComposerBridge(bridge) {
		t.Error("ToolExecutor.composer not propagated")
	}
	if !a.toolExecutor.composerEnabled {
		t.Error("ToolExecutor.composerEnabled not propagated")
	}
}

// TestComposeAutomationDescriptor_RegisteredInDispatcherTools mirrors
// the "must appear in DispatcherTools()" convention every other tool
// pins (see tools_test.go, tool_update_operator_profile_test.go).
func TestComposeAutomationDescriptor_RegisteredInDispatcherTools(t *testing.T) {
	for _, tl := range DispatcherTools() {
		if tl.Function.Name == composeAutomationName {
			return
		}
	}
	t.Fatalf("%s tool not registered in DispatcherTools()", composeAutomationName)
}
