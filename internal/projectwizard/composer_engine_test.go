package projectwizard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"vornik.io/vornik/internal/llmspend"

	"github.com/prometheus/client_golang/prometheus"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func tier3Reply(t *testing.T, msg string, ready bool, bundle *ComposedBundle) chatReply {
	t.Helper()
	env := Envelope{Message: msg, Tier: 3, ReadyToCommit: ready, Bundle: bundle}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal tier-3 envelope: %v", err)
	}
	return chatReply{content: string(b)}
}

func downgradedReply(t *testing.T, msg, suggested string) chatReply {
	t.Helper()
	env := Envelope{Message: msg, Tier: 1, ReadyToCommit: false, SuggestedTemplate: suggested}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return chatReply{content: string(b)}
}

// wireComposer attaches the test archetype/prior fixtures a tier-3
// turn needs to actually materialize+validate.
func wireComposer(w *Wizard) {
	w.RoleLibrary = testArchetypes()
	w.Priors = testPriors()
}

// --- Tier arbitration ---

func TestConverse_TierArbitration_AnchorForcesDowngradeViaRetry(t *testing.T) {
	w, _, chatStub := newWizardForTest(
		tier3Reply(t, "Here's a full custom build.", false, validComposedBundle()),
		downgradedReply(t, "Using the ai-news-digest template instead.", "ai-news-digest"),
	)
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "I want a daily AI news digest emailed to me")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier == 3 || res.Envelope.Bundle != nil {
		t.Fatalf("expected the anchor gate to force a non-tier-3 result, got tier=%d bundle=%v", res.Envelope.Tier, res.Envelope.Bundle)
	}
	if res.Envelope.Message != "Using the ai-news-digest template instead." {
		t.Errorf("expected the RETRY's envelope to be what's returned, got %q", res.Envelope.Message)
	}
	if got := chatStub.calls.Load(); got != 2 {
		t.Errorf("expected exactly one corrective retry (2 total calls), got %d", got)
	}
}

func TestConverse_TierArbitration_RetryStillTier3_FailsClosed(t *testing.T) {
	w, store, chatStub := newWizardForTest(
		tier3Reply(t, "Here's a full custom build.", false, validComposedBundle()),
		tier3Reply(t, "Still building it fully custom.", false, validComposedBundle()),
	)
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "I want a daily AI news digest emailed to me")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier == 3 || res.Envelope.Bundle != nil {
		t.Fatalf("expected a fail-closed downgrade, got tier=%d bundle=%v", res.Envelope.Tier, res.Envelope.Bundle)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("fail-closed envelope must not be ready_to_commit")
	}
	if !strings.Contains(res.Envelope.Message, "confirm a from-scratch automation") {
		t.Errorf("expected the fail-closed sentinel phrase in the message, got %q", res.Envelope.Message)
	}
	if got := chatStub.calls.Load(); got != 2 {
		t.Errorf("expected exactly one corrective retry, got %d calls", got)
	}
	sess, _ := store.Get(context.Background(), res.SessionID)
	if sess.Tier3Turns != 0 {
		t.Errorf("a rejected tier-3 attempt must not count against the turn cap, got %d", sess.Tier3Turns)
	}
	if sess.Tier3Unlocked {
		t.Error("fail-closed bounce must not itself unlock — only an explicit affirmative reply does")
	}
}

func TestConverse_TierArbitration_ExplicitUnlockSkipsAnchorGate(t *testing.T) {
	w, store, chatStub := newWizardForTest(
		tier3Reply(t, "Here's a full custom build.", false, validComposedBundle()),
		tier3Reply(t, "Still building it fully custom.", false, validComposedBundle()),
		tier3Reply(t, "Building your custom automation.", true, validComposedBundle()),
	)
	wireComposer(w)

	// Turn 1: anchor fires, retry still tier-3 → fail closed.
	res1, err := w.Converse(context.Background(), "", "op_1", "I want a daily AI news digest emailed to me")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if res1.Envelope.Tier == 3 {
		t.Fatalf("turn 1 should have failed closed, got tier=%d", res1.Envelope.Tier)
	}

	// Turn 2: explicit affirmative unlocks — the THIRD scripted reply
	// (tier-3, valid) should now be accepted with NO further retry.
	res2, err := w.Converse(context.Background(), res1.SessionID, "op_1", "yes, build it from scratch")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Envelope.Tier != 3 || res2.Envelope.Bundle == nil {
		t.Fatalf("expected the unlocked session to accept tier-3, got tier=%d bundle=%v message=%q", res2.Envelope.Tier, res2.Envelope.Bundle, res2.Envelope.Message)
	}
	if got := chatStub.calls.Load(); got != 3 {
		t.Errorf("expected exactly 3 total LLM calls (2 for turn 1 + 1 for the unlocked turn 2), got %d", got)
	}
	sess, _ := store.Get(context.Background(), res2.SessionID)
	if !sess.Tier3Unlocked {
		t.Error("expected the session to be marked unlocked")
	}
	if sess.Tier3Turns != 1 {
		t.Errorf("expected the accepted tier-3 turn to count, got Tier3Turns=%d", sess.Tier3Turns)
	}
}

func TestConverse_TierArbitration_MaxTierPin_NoRetry(t *testing.T) {
	w, _, chatStub := newWizardForTest(
		tier3Reply(t, "Here's a full custom build.", false, validComposedBundle()),
	)
	wireComposer(w)
	w.Composer = config.ComposerConfig{MaxTier: 2, MaxTier3Turns: 10}

	res, err := w.Converse(context.Background(), "", "op_1", "build me anything, totally unrelated to any template")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier == 3 {
		t.Fatalf("composer.max_tier=2 must block tier-3 output, got tier=%d", res.Envelope.Tier)
	}
	if got := chatStub.calls.Load(); got != 1 {
		t.Errorf("an operator pin must not trigger a retry (no anchor judgment involved), got %d calls", got)
	}
	if !strings.Contains(res.Envelope.Message, "max_tier") {
		t.Errorf("expected the pin to be named in the message, got %q", res.Envelope.Message)
	}
}

func TestConverse_TierArbitration_MaxTier3TurnsCap(t *testing.T) {
	replies := []chatReply{
		tier3Reply(t, "build 1", true, validComposedBundle()),
		tier3Reply(t, "build 2", true, validComposedBundle()),
		tier3Reply(t, "build 3 should be capped", false, validComposedBundle()),
	}
	w, store, chatStub := newWizardForTest(replies...)
	wireComposer(w)
	w.Composer = config.ComposerConfig{MaxTier: 3, MaxTier3Turns: 2}

	sessionID := ""
	for i := 0; i < 2; i++ {
		res, err := w.Converse(context.Background(), sessionID, "op_1", "build me something unrelated to any template")
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		sessionID = res.SessionID
		if res.Envelope.Tier != 3 {
			t.Fatalf("turn %d: expected tier-3 to be accepted (under the cap), got tier=%d msg=%q", i, res.Envelope.Tier, res.Envelope.Message)
		}
	}
	sess, _ := store.Get(context.Background(), sessionID)
	if sess.Tier3Turns != 2 {
		t.Fatalf("expected 2 accepted tier-3 turns, got %d", sess.Tier3Turns)
	}

	// Third tier-3 attempt should be capped — no retry, immediate downgrade.
	res3, err := w.Converse(context.Background(), sessionID, "op_1", "one more totally custom build please")
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if res3.Envelope.Tier == 3 {
		t.Fatalf("expected the per-session tier-3 cap to block a third tier-3 turn, got tier=%d", res3.Envelope.Tier)
	}
	if got := chatStub.calls.Load(); got != 3 {
		t.Errorf("cap hit must not trigger a retry, expected 3 total calls, got %d", got)
	}
}

// --- Bundle pipeline (staged validation + guardrails) wired through Converse ---

func TestConverse_TierThree_ValidBundle_ReadyToCommit(t *testing.T) {
	w, store, _ := newWizardForTest(
		tier3Reply(t, "Here is your automation.", true, validComposedBundle()),
	)
	wireComposer(w)
	// Weak-match description so the anchor gate doesn't fire.
	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier != 3 || res.Envelope.Bundle == nil {
		t.Fatalf("expected an accepted tier-3 turn, got tier=%d msg=%q", res.Envelope.Tier, res.Envelope.Message)
	}
	if !res.Envelope.ReadyToCommit {
		t.Errorf("expected ready_to_commit to survive a valid, non-autonomous bundle: %q", res.Envelope.Message)
	}
	sess, _ := store.Get(context.Background(), res.SessionID)
	if len(sess.Bundle) == 0 {
		t.Error("expected the validated bundle to be persisted on the session")
	}
	if sess.Tier3Turns != 1 {
		t.Errorf("expected Tier3Turns incremented, got %d", sess.Tier3Turns)
	}
}

func TestConverse_TierThree_GuardrailViolation_BouncesWithoutMutating(t *testing.T) {
	bundle := validComposedBundle()
	// Give the researcher role a tool outside its archetype allowlist.
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "researcher", "allowedTools": []any{"file_read", "run_shell"}},
		map[string]any{"name": "writer", "archetypeId": "writer"},
	}
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("a guardrail violation must force ready_to_commit=false")
	}
	if !strings.Contains(res.Envelope.Message, "run_shell") {
		t.Errorf("expected the violation to be named in the message, got %q", res.Envelope.Message)
	}
	sess, _ := store.Get(context.Background(), res.SessionID)
	if len(sess.Bundle) != 0 {
		t.Error("a violating bundle must NEVER be persisted, mutated or not")
	}
	if sess.Tier3Turns != 0 {
		t.Error("a rejected bundle must not count against the turn cap")
	}
}

func TestConverse_TierThree_StagedValidationFailure_Bounces(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Project["defaultWorkflowId"] = "does-not-exist"
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("staged validation failure must force ready_to_commit=false")
	}
	if !strings.Contains(res.Envelope.Message, "registry:") {
		t.Errorf("expected the staged-validation stage to be named, got %q", res.Envelope.Message)
	}
	sess, _ := store.Get(context.Background(), res.SessionID)
	if len(sess.Bundle) != 0 {
		t.Error("an invalid bundle must not be persisted")
	}
}

func TestConverse_TierThree_ScheduleConfirmationGate(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Project["autonomy"] = map[string]any{"enabled": true, "goal": "daily digest", "pollInterval": "24h"}
	bundle.Plan.Schedule = "Runs every day"
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier != 3 || res.Envelope.Bundle == nil {
		t.Fatalf("expected the bundle itself to validate, got tier=%d msg=%q", res.Envelope.Tier, res.Envelope.Message)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("an autonomy bundle must never be ready_to_commit without a stored schedule confirmation")
	}
	if !strings.Contains(res.Envelope.Message, "schedule") {
		t.Errorf("expected a schedule-confirmation note, got %q", res.Envelope.Message)
	}

	// Once the session records a matching confirmation, a subsequent
	// identical bundle turn is no longer blocked by the gate.
	sess, _ := store.Get(context.Background(), res.SessionID)
	now := sess.UpdatedAt
	sess.ScheduleConfirmedAt = &now
	sess.ScheduleConfirmedCron = "24h"
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatalf("seed confirmation: %v", err)
	}

	w2, _, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w2)
	w2.Sessions = store
	res2, err := w2.Converse(context.Background(), res.SessionID, "op_1", "yes that schedule works")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !res2.Envelope.ReadyToCommit {
		t.Errorf("expected ready_to_commit once the schedule confirmation matches, got %q", res2.Envelope.Message)
	}
}

// TestConverse_TierThree_ScheduleConfirmationGate_EquivalentFormMatches is
// the safe direction of the companion-review's schedule-comparison
// finding: a confirmation recorded in one duration spelling ("1440m")
// must still satisfy the gate against a bundle that expresses the
// semantically-identical schedule differently ("24h") — raw string
// equality would otherwise force a spurious, annoying re-confirmation
// prompt every time formatting merely differs.
func TestConverse_TierThree_ScheduleConfirmationGate_EquivalentFormMatches(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Project["autonomy"] = map[string]any{"enabled": true, "goal": "daily digest", "pollInterval": "24h"}
	bundle.Plan.Schedule = "Runs every day"
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	sess, _ := store.Get(context.Background(), res.SessionID)
	now := sess.UpdatedAt
	sess.ScheduleConfirmedAt = &now
	sess.ScheduleConfirmedCron = "1440m" // same cadence as "24h", different spelling
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatalf("seed confirmation: %v", err)
	}

	w2, _, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w2)
	w2.Sessions = store
	res2, err := w2.Converse(context.Background(), res.SessionID, "op_1", "yes that schedule works")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !res2.Envelope.ReadyToCommit {
		t.Errorf("expected ready_to_commit — \"1440m\" and \"24h\" are the same cadence, got %q", res2.Envelope.Message)
	}
}

// TestConverse_TierThree_ScheduleConfirmationGate_ChangedScheduleForcesReconfirm
// is the DANGEROUS direction the companion review flagged: a session
// with a stale confirmation on record must never let a genuinely
// CHANGED schedule through just because a naive normalization made it
// look equal. "24h" confirmed, "48h" now proposed, must still block.
func TestConverse_TierThree_ScheduleConfirmationGate_ChangedScheduleForcesReconfirm(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Project["autonomy"] = map[string]any{"enabled": true, "goal": "daily digest", "pollInterval": "48h"}
	bundle.Plan.Schedule = "Runs every two days"
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	sess, _ := store.Get(context.Background(), res.SessionID)
	now := sess.UpdatedAt
	sess.ScheduleConfirmedAt = &now
	sess.ScheduleConfirmedCron = "24h" // stale confirmation for a DIFFERENT cadence
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatalf("seed confirmation: %v", err)
	}

	w2, _, _ := newWizardForTest(tier3Reply(t, "Here is your scheduled automation.", true, bundle))
	wireComposer(w2)
	w2.Sessions = store
	res2, err := w2.Converse(context.Background(), res.SessionID, "op_1", "yes that schedule works")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Envelope.ReadyToCommit {
		t.Error("a changed schedule (24h confirmed, 48h proposed) must never be waved through as ready_to_commit")
	}
	if !strings.Contains(res2.Envelope.Message, "schedule") {
		t.Errorf("expected a schedule-confirmation note, got %q", res2.Envelope.Message)
	}
}

// TestConverse_TierThree_EmptyWorkflowsBundle_Bounces exercises the
// full applyBundle pipeline (not just the shapeCheckBundle /
// stageBundleForValidation units in isolation) with a bundle carrying
// zero workflows, proving the shape-check defensive layer actually
// bounces the turn end-to-end rather than only failing in unit
// isolation.
func TestConverse_TierThree_EmptyWorkflowsBundle_Bounces(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Workflows = nil
	w, store, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Error("an empty-workflows bundle must never be ready_to_commit")
	}
	if !strings.Contains(res.Envelope.Message, "at least one workflow is required") {
		t.Errorf("expected the shape-check reason to be named, got %q", res.Envelope.Message)
	}
	sess, _ := store.Get(context.Background(), res.SessionID)
	if len(sess.Bundle) != 0 {
		t.Error("an empty-workflows bundle must never be persisted")
	}
	if sess.Tier3Turns != 0 {
		t.Error("a rejected bundle must not count against the turn cap")
	}
}

// --- Consecutive tier-3 validation-failure fallback (task 1.3, design §7 row 1) ---

// unrelatedDescription has zero token overlap with either testPriors()
// entry (verified against tokenize()'s <3-char-drop rule — in
// particular it avoids "with", which the trading-bot prior's blurb
// happens to contain and would otherwise make it (not ai-news-digest)
// anchorScore's top pick), so anchorScore ties both priors at 0 and
// picks testPriors()[0] ("ai-news-digest") by first-seen order — that
// determinism is what lets the fallback test assert on the specific
// nearest-template chosen. It also stays safely below
// anchorConfidenceThreshold, so the pre-existing tier-arbitration
// anchor gate never downgrades these turns in-place either.
const unrelatedDescription = "totally unrelated custom request that no known template can match"

func TestConverse_TierThree_ConsecutiveValidationFailures_DowngradesOnThird(t *testing.T) {
	badBundle := validComposedBundle()
	badBundle.Project["defaultWorkflowId"] = "does-not-exist" // deterministic staged-validation failure
	replies := []chatReply{
		tier3Reply(t, "attempt 1", true, badBundle),
		tier3Reply(t, "attempt 2", true, badBundle),
		tier3Reply(t, "attempt 3", true, badBundle),
	}
	w, store, _ := newWizardForTest(replies...)
	wireComposer(w)

	sessionID := ""
	for i := 0; i < 2; i++ {
		res, err := w.Converse(context.Background(), sessionID, "op_1", unrelatedDescription)
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		sessionID = res.SessionID
		if res.Envelope.Tier != 3 || res.Envelope.Bundle == nil {
			t.Fatalf("turn %d: expected the turn to still be tier-3-ish (retrying), got tier=%d bundle=%v", i+1, res.Envelope.Tier, res.Envelope.Bundle)
		}
		if res.Envelope.ReadyToCommit {
			t.Fatalf("turn %d: a staged-validation failure must never be ready_to_commit", i+1)
		}
		sess, getErr := store.Get(context.Background(), sessionID)
		if getErr != nil {
			t.Fatalf("turn %d: Get: %v", i+1, getErr)
		}
		if want := i + 1; sess.Tier3ConsecutiveValidationFailures != want {
			t.Fatalf("turn %d: Tier3ConsecutiveValidationFailures = %d, want %d", i+1, sess.Tier3ConsecutiveValidationFailures, want)
		}
	}

	// Third consecutive failure trips the circuit breaker: downgrade to
	// the nearest template instead of another bounce.
	res3, err := w.Converse(context.Background(), sessionID, "op_1", unrelatedDescription)
	if err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if res3.Envelope.Tier == 3 || res3.Envelope.Bundle != nil {
		t.Fatalf("expected the 3rd consecutive failure to downgrade away from tier-3, got tier=%d bundle=%v", res3.Envelope.Tier, res3.Envelope.Bundle)
	}
	if res3.Envelope.ReadyToCommit {
		t.Error("a fallback downgrade must never be ready_to_commit")
	}
	if res3.Envelope.SuggestedTemplate != "ai-news-digest" {
		t.Errorf("expected the anchorScore-chosen nearest template, got %q", res3.Envelope.SuggestedTemplate)
	}
	if !strings.Contains(res3.Envelope.Message, "ai-news-digest") {
		t.Errorf("expected the nearest template named in the message, got %q", res3.Envelope.Message)
	}
	if !strings.Contains(res3.Envelope.Message, "trouble building this from scratch") {
		t.Errorf("expected the plain-language fallback framing, got %q", res3.Envelope.Message)
	}
	sess3, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Get after fallback: %v", err)
	}
	if sess3.Tier3ConsecutiveValidationFailures != 0 {
		t.Errorf("expected the counter reset once the fallback fires, got %d", sess3.Tier3ConsecutiveValidationFailures)
	}
	if len(sess3.Bundle) != 0 {
		t.Error("a downgraded turn must never persist a bundle")
	}
}

// TestConverse_TierThree_ConsecutiveValidationFailures_ResetsOnSuccess is
// the companion review's "2-fail -> success -> 2-fail must NOT trigger
// the fallback" case: a successful tier-3 composition in between two
// failure streaks resets the counter, so 4 total failures spread across
// a success never trips the 3-consecutive threshold.
func TestConverse_TierThree_ConsecutiveValidationFailures_ResetsOnSuccess(t *testing.T) {
	badBundle := validComposedBundle()
	badBundle.Project["defaultWorkflowId"] = "does-not-exist"
	goodBundle := validComposedBundle()
	replies := []chatReply{
		tier3Reply(t, "fail 1", true, badBundle),
		tier3Reply(t, "fail 2", true, badBundle),
		tier3Reply(t, "success", true, goodBundle),
		tier3Reply(t, "fail 3", true, badBundle),
		tier3Reply(t, "fail 4", true, badBundle),
	}
	w, store, _ := newWizardForTest(replies...)
	wireComposer(w)

	wantReady := []bool{false, false, true, false, false}
	sessionID := ""
	for i, ready := range wantReady {
		res, err := w.Converse(context.Background(), sessionID, "op_1", unrelatedDescription)
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		sessionID = res.SessionID
		if res.Envelope.ReadyToCommit != ready {
			t.Fatalf("turn %d: ReadyToCommit = %v, want %v (msg=%q)", i+1, res.Envelope.ReadyToCommit, ready, res.Envelope.Message)
		}
		if res.Envelope.Tier == 0 {
			t.Fatalf("turn %d: fallback must never fire — only 2 consecutive failures ever accumulate, got a downgrade (msg=%q)", i+1, res.Envelope.Message)
		}
	}
	sess, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sess.Tier3ConsecutiveValidationFailures != 2 {
		t.Errorf("expected 2 consecutive failures on record (fail,fail,success[reset],fail,fail), got %d", sess.Tier3ConsecutiveValidationFailures)
	}
}

// --- I2 (whole-branch review): composer_bundles_validated_total must
// count exactly the two staged-validation outcomes, never a
// guardrail/render/shape/bundle bounce that never reached staged
// validation, and never twice for one registry-invalid turn. ---

func TestConverse_TierThree_GuardrailBounce_RecordsNoBundleValidatedMetric(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Swarm["roles"] = []any{
		map[string]any{"name": "researcher", "archetypeId": "researcher", "allowedTools": []any{"file_read", "run_shell"}},
		map[string]any{"name": "writer", "archetypeId": "writer"},
	}
	w, _, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)
	w.Metrics = NewMetrics(prometheus.NewRegistry())

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Fatal("expected the guardrail violation to bounce the turn")
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultInvalid); got != 0 {
		t.Errorf("a guardrail bounce never reached staged validation — expected 0 invalid, got %v", got)
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultValid); got != 0 {
		t.Errorf("expected 0 valid, got %v", got)
	}
}

func TestConverse_TierThree_RegistryInvalid_RecordsExactlyOneInvalid(t *testing.T) {
	bundle := validComposedBundle()
	bundle.Project["defaultWorkflowId"] = "does-not-exist"
	w, _, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, bundle))
	wireComposer(w)
	w.Metrics = NewMetrics(prometheus.NewRegistry())

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.ReadyToCommit {
		t.Fatal("expected staged validation to fail")
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultInvalid); got != 1 {
		t.Errorf("expected exactly 1 invalid recording (pre-fix this double-counted to 2), got %v", got)
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultValid); got != 0 {
		t.Errorf("expected 0 valid, got %v", got)
	}
}

func TestConverse_TierThree_ValidBundle_RecordsExactlyOneValid(t *testing.T) {
	w, _, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, validComposedBundle()))
	wireComposer(w)
	w.Metrics = NewMetrics(prometheus.NewRegistry())

	res, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Envelope.Tier != 3 || res.Envelope.Bundle == nil {
		t.Fatalf("expected an accepted tier-3 turn, got tier=%d msg=%q", res.Envelope.Tier, res.Envelope.Message)
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultValid); got != 1 {
		t.Errorf("expected exactly 1 valid recording, got %v", got)
	}
	if got := bundleValidatedMetricValue(t, w.Metrics, bundleValidationResultInvalid); got != 0 {
		t.Errorf("expected 0 invalid, got %v", got)
	}
}

func TestConverse_TierThree_UsesAutomationComposerRole(t *testing.T) {
	rec := &captureRecorder{}
	w, _, _ := newWizardForTest(tier3Reply(t, "Here is your automation.", true, validComposedBundle()))
	wireComposer(w)
	w.Spend = llmspend.New(rec, nil, "project_wizard", RoleProjectWizard)

	_, err := w.Converse(context.Background(), "", "op_1", "totally unrelated custom request with no template match at all")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if len(rec.rows) != 1 {
		t.Fatalf("expected 1 usage row, got %d", len(rec.rows))
	}
	if rec.rows[0].Role != RoleAutomationComposer {
		t.Errorf("Role = %q, want %q", rec.rows[0].Role, RoleAutomationComposer)
	}
}

// --- schedule-equivalence contract (companion review finding 2) ---

func TestSchedulesEquivalent(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical strings", "24h", "24h", true},
		{"equivalent durations, different spelling", "24h", "1440m", true},
		{"equivalent durations, reversed args", "1440m", "24h", true},
		{"genuinely different durations", "24h", "48h", false},
		{"unparseable vs valid — errs safe (mismatch)", "1d", "24h", false},
		{"unparseable vs same unparseable string — still errs safe (mismatch)", "1d", "1d", false},
		{"both blank — nothing to parse, errs safe (mismatch)", "", "", false},
		{"blank vs valid", "", "24h", false},
		{"whitespace padding tolerated", " 24h ", "24h", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := schedulesEquivalent(c.a, c.b); got != c.want {
				t.Errorf("schedulesEquivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestScheduleConfirmed_UsesNormalizedComparison(t *testing.T) {
	now := time.Now()
	mb := &materializedBundle{Project: &registry.Project{Autonomy: registry.ProjectAutonomy{Enabled: true, PollInterval: "24h"}}}

	// Equivalent spelling: must be treated as confirmed.
	sess := &persistence.ProjectWizardSession{ScheduleConfirmedAt: &now, ScheduleConfirmedCron: "1440m"}
	if !scheduleConfirmed(sess, mb) {
		t.Error("expected \"1440m\" to satisfy a \"24h\" schedule confirmation (same cadence)")
	}

	// Genuinely different cadence: must NOT be treated as confirmed —
	// the dangerous direction the review flagged.
	sess = &persistence.ProjectWizardSession{ScheduleConfirmedAt: &now, ScheduleConfirmedCron: "48h"}
	if scheduleConfirmed(sess, mb) {
		t.Error("expected a genuinely different confirmed cadence (48h) to force re-confirmation against a 24h bundle")
	}

	// Unparseable confirmed value: must fail closed (never silently match).
	sess = &persistence.ProjectWizardSession{ScheduleConfirmedAt: &now, ScheduleConfirmedCron: "1d"}
	if scheduleConfirmed(sess, mb) {
		t.Error("expected an unparseable confirmed schedule to force re-confirmation rather than risk a false match")
	}
}
