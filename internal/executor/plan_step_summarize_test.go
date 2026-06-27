package executor

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/verifier"
)

// Phase 32 regression — `summarizedMessageIDs` walks every `note`
// message's metadata.summarized_message_ids and builds a hide-set
// the prompt builder filters by. Two failure modes to lock down:
//
//   1. Summarized messages must drop out of the rendered window
//      (the bug it prevents: lead re-reads originals, double-counts).
//   2. The summary `note` itself must STAY visible (otherwise the
//      lead loses the compressed view too).

func TestSummarizedMessageIDs_HidesSummarizedOriginals(t *testing.T) {
	now := time.Now().UTC()
	messages := []*persistence.TaskMessage{
		{ID: "msg-1", MessageKind: persistence.TaskMessageKindMessage, Content: "old 1", CreatedAt: now},
		{ID: "msg-2", MessageKind: persistence.TaskMessageKindMessage, Content: "old 2", CreatedAt: now},
		{ID: "msg-3", MessageKind: persistence.TaskMessageKindMessage, Content: "kept", CreatedAt: now},
		{
			ID:          "note-1",
			MessageKind: persistence.TaskMessageKindNote,
			Content:     "summary of msg-1 + msg-2",
			Metadata:    []byte(`{"kind":"thread_summary","summarized_message_ids":["msg-1","msg-2"]}`),
			CreatedAt:   now,
		},
	}

	hide := summarizedMessageIDs(messages)

	if !hide["msg-1"] || !hide["msg-2"] {
		t.Errorf("msg-1 + msg-2 must be hidden; got %v", hide)
	}
	if hide["msg-3"] {
		t.Errorf("msg-3 must NOT be hidden")
	}
	if hide["note-1"] {
		t.Errorf("the summary note itself must NOT be in the hide-set")
	}
}

func TestSummarizedMessageIDs_MultipleNotesAggregate(t *testing.T) {
	messages := []*persistence.TaskMessage{
		{ID: "a", MessageKind: persistence.TaskMessageKindNote,
			Metadata: []byte(`{"summarized_message_ids":["x","y"]}`)},
		{ID: "b", MessageKind: persistence.TaskMessageKindNote,
			Metadata: []byte(`{"summarized_message_ids":["z"]}`)},
	}
	hide := summarizedMessageIDs(messages)
	if !hide["x"] || !hide["y"] || !hide["z"] {
		t.Errorf("notes from multiple summaries must aggregate; got %v", hide)
	}
}

func TestSummarizedMessageIDs_NonNoteIgnored(t *testing.T) {
	// A directive or message that happens to carry a similar
	// metadata field must NOT contribute to the hide-set.
	messages := []*persistence.TaskMessage{
		{ID: "a", MessageKind: persistence.TaskMessageKindDirective,
			Metadata: []byte(`{"summarized_message_ids":["should-not-hide"]}`)},
	}
	hide := summarizedMessageIDs(messages)
	if hide["should-not-hide"] {
		t.Errorf("only `note` messages contribute to the hide-set; got %v", hide)
	}
}

func TestSummarizedMessageIDs_MalformedMetadataSkipped(t *testing.T) {
	// Bad JSON / wrong shape on a note must not crash the prompt
	// build — the whole prompt-rendering path is best-effort.
	messages := []*persistence.TaskMessage{
		{ID: "a", MessageKind: persistence.TaskMessageKindNote, Metadata: []byte(`not-json`)},
		{ID: "b", MessageKind: persistence.TaskMessageKindNote, Metadata: nil},
		{ID: "c", MessageKind: persistence.TaskMessageKindNote,
			Metadata: []byte(`{"summarized_message_ids":["x"]}`)},
	}
	hide := summarizedMessageIDs(messages)
	if !hide["x"] {
		t.Errorf("valid summary must still contribute despite malformed siblings")
	}
}

func TestBuildPlanningPrompt_FiltersSummarizedAndShowsNote(t *testing.T) {
	now := time.Now().UTC()
	messages := []*persistence.TaskMessage{
		{ID: "old-msg", AuthorKind: "operator", MessageKind: persistence.TaskMessageKindMessage, Content: "ANCIENT_CONTENT", CreatedAt: now},
		{ID: "summary", AuthorKind: "lead", MessageKind: persistence.TaskMessageKindNote,
			Content:   "DIGESTED_SUMMARY",
			Metadata:  []byte(`{"summarized_message_ids":["old-msg"]}`),
			CreatedAt: now,
		},
		{ID: "recent", AuthorKind: "operator", MessageKind: persistence.TaskMessageKindMessage, Content: "RECENT_LINE", CreatedAt: now},
	}

	out := buildPlanningPromptWithContext("brief here", nil, nil, messages, nil)

	if strings.Contains(out, "ANCIENT_CONTENT") {
		t.Errorf("the summarized original must NOT appear verbatim in the prompt")
	}
	if !strings.Contains(out, "DIGESTED_SUMMARY") {
		t.Errorf("the summary `note` must remain in the prompt to preserve context")
	}
	if !strings.Contains(out, "RECENT_LINE") {
		t.Errorf("non-summarized recent messages must still render")
	}
	// The compression hint should also fire to tell the lead what happened.
	if !strings.Contains(out, "compressed by summarize_thread") {
		t.Errorf("prompt should note the summarize_thread compression count")
	}
}

// TestBuildPlanningPrompt_RecoveryModeBanner — when the recovery
// context is set, the prompt must (a) lead with a high-salience
// RECOVERY MODE banner, (b) include the failure shape so the lead
// can map it to its per-class playbook, and (c) prune `continue`
// from the 4-shape outcome menu so the model can't accidentally
// pick the forbidden outcome. Guards the 2026-05-26 fix for
// T-0833 recovery contract violation.
func TestBuildPlanningPrompt_RecoveryModeBanner(t *testing.T) {
	rc := &RecoveryContext{
		FailedStep:    "research",
		FailureClass:  "agent_error",
		FailureReason: "produced_files \"artifacts/out/report.md\": file does not exist",
		BlockedURLs: []verifier.BlockedURL{
			{URL: "https://example.com/x", Reason: "captcha"},
		},
	}
	out := buildPlanningPromptWithContext("base prompt", nil, nil, nil, rc)

	if !strings.Contains(out, "RECOVERY MODE") {
		t.Error("recovery banner missing — model needs the high-salience cue at the top")
	}
	if !strings.Contains(out, "research") || !strings.Contains(out, "agent_error") {
		t.Error("recovery banner must include failed_step + failure_class for the lead's per-class playbook")
	}
	if !strings.Contains(out, "captcha") {
		t.Error("blocked_urls list must surface in the banner so the lead can propose source swaps")
	}
	if !strings.Contains(out, "continue") {
		// "continue" should appear ONLY in the forbidden-callout
		// (the warning that says DO NOT emit it), not as an option.
		// Sanity: at least the warning is present.
		t.Error("prompt must explicitly warn against continue in recovery mode")
	}
	if strings.Contains(out, "FOUR outcome shapes") {
		t.Error("recovery-mode menu must enumerate THREE shapes, not four — continue is pruned")
	}
	if !strings.Contains(out, "THREE outcome shapes") {
		t.Error("recovery-mode menu must say THREE shapes are available")
	}
	// The original menu listed `(1) Continue with a plan` — that
	// header must NOT appear in recovery mode.
	if strings.Contains(out, "(1) Continue with a plan") {
		t.Error("the continue option header must be pruned from recovery-mode menu")
	}
}

// TestRecoveryModeResponseSchema_ConstrainsOutcomeEnum — the
// long-term fix's contract: when recovery mode is active, the JSON
// schema sent to the LLM gateway constrains `outcome` to the
// three-value enum that excludes `continue`. Providers that honour
// json_schema (OpenAI strict, Bedrock Converse) refuse to emit
// `outcome=continue` at the structured-output decoder. This is the
// strongest layer in the defense-in-depth stack — banner (1) +
// menu pruning (2) + corrective-hint retry (3) + schema enforcement
// (this).
func TestRecoveryModeResponseSchema_ConstrainsOutcomeEnum(t *testing.T) {
	schema := recoveryModeResponseSchema()

	// Top-level shape — outcome is required.
	required, _ := schema["required"].([]string)
	foundOutcome := false
	for _, r := range required {
		if r == "outcome" {
			foundOutcome = true
		}
	}
	if !foundOutcome {
		t.Errorf("schema must mark outcome as required (got required=%v)", required)
	}

	// outcome property: type=string, enum excludes continue.
	props, _ := schema["properties"].(map[string]any)
	outcomeProp, _ := props["outcome"].(map[string]any)
	enum, _ := outcomeProp["enum"].([]string)
	seen := map[string]bool{}
	for _, v := range enum {
		seen[v] = true
	}
	if seen["continue"] {
		t.Errorf("schema enum must NOT include `continue` — that's the whole point. got %v", enum)
	}
	for _, want := range []string{"checkpoint", "external_wait", "closure_request"} {
		if !seen[want] {
			t.Errorf("schema enum missing valid outcome %q (got %v)", want, enum)
		}
	}
	if len(enum) != 3 {
		t.Errorf("schema enum should have exactly 3 entries, got %d (%v)", len(enum), enum)
	}
}

// TestRecoveryContractCorrectiveHint_FormatAndContent — the
// corrective hint appended to the retry prompt must name what
// went wrong, identify the failed step + class, and show the
// exact JSON shape the lead should emit. Pins the contract so a
// future hint-text edit can't drop the critical fields.
func TestRecoveryContractCorrectiveHint_FormatAndContent(t *testing.T) {
	rc := &RecoveryContext{
		FailedStep:    "research",
		FailureClass:  "agent_error",
		FailureReason: "missing artifact",
	}
	hint := recoveryContractCorrectiveHint(rc, "continue")
	mustContain := []string{
		"CORRECTION",
		"outcome=continue",
		"INVALID in recovery mode",
		"research",
		"agent_error",
		`"outcome":"checkpoint"`,
		`"kind":"decision"`,
		"abort with explanation",
		"recovery-mode playbook",
	}
	for _, want := range mustContain {
		if !strings.Contains(hint, want) {
			t.Errorf("corrective hint missing required substring %q\nhint = %s", want, hint)
		}
	}
}

// TestBuildPlanningPrompt_NormalModeKeepsAllFourShapes — counterpart
// of the recovery-mode test: without a RecoveryContext, the full
// four-shape menu (with `continue` as option 1) renders unchanged.
// Pins the non-recovery surface so a future template edit can't
// silently drop the legacy default.
func TestBuildPlanningPrompt_NormalModeKeepsAllFourShapes(t *testing.T) {
	out := buildPlanningPromptWithContext("base prompt", nil, nil, nil, nil)
	if strings.Contains(out, "RECOVERY MODE") {
		t.Error("RECOVERY MODE banner must NOT appear when recoveryCtx is nil")
	}
	if !strings.Contains(out, "FOUR outcome shapes") {
		t.Error("normal-mode menu must enumerate FOUR shapes including continue")
	}
	if !strings.Contains(out, "(1) Continue with a plan") {
		t.Error("normal-mode menu must list `(1) Continue with a plan`")
	}
}
