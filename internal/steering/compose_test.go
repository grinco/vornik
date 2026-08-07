package steering

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

type stubCheckpointReader struct{ cp *persistence.TaskMessage }

func (s stubCheckpointReader) GetOpenCheckpoint(_ context.Context, _ string) (*persistence.TaskMessage, error) {
	return s.cp, nil
}

func decisionCheckpoint(question string, opts ...string) *persistence.TaskMessage {
	list := make([]map[string]string, 0, len(opts))
	for _, o := range opts {
		list = append(list, map[string]string{"id": o, "label": strings.ToUpper(o[:1]) + o[1:]})
	}
	meta, _ := json.Marshal(map[string]any{"kind": "decision", "question": question, "options": list})
	return &persistence.TaskMessage{ID: "cp1", Content: question, Metadata: meta}
}

func notifierWithCheckpoint(cp *persistence.TaskMessage) *Notifier {
	return New(nil, nil, nil, stubCheckpointReader{cp: cp}, "https://vornik.example", true, zerolog.Nop())
}

func task() *persistence.Task {
	return &persistence.Task{ID: "task_20260806212011_77b90a7d0e1d0e47", ProjectID: "companion-janka"}
}

// The reported bug: a decision checkpoint reached Slack with neither the
// question nor its options, because both lived only in Buttons — a field only
// Telegram reads. The options must be IN THE TEXT.
func TestComposeText_CarriesQuestionAndNumberedOptions(t *testing.T) {
	n := notifierWithCheckpoint(decisionCheckpoint("Which blind type?", "roller", "roman", "venetian"))
	got := n.composeTextWithHint(context.Background(), task(),
		string(persistence.TaskStatusAwaitingInput), "/vornik answer 77b9 <number>")

	if !strings.Contains(got, "Which blind type?") {
		t.Errorf("question missing:\n%s", got)
	}
	for _, want := range []string{"1. Roller", "2. Roman", "3. Venetian"} {
		if !strings.Contains(got, want) {
			t.Errorf("option %q missing (1-based, labels):\n%s", want, got)
		}
	}
	if !strings.Contains(got, "/vornik answer 77b9 ") {
		t.Errorf("reply instruction must name the ref from the id's RANDOM suffix:\n%s", got)
	}
	if !strings.Contains(got, "https://vornik.example/ui/tasks/") {
		t.Errorf("UI link missing:\n%s", got)
	}
}

// Free-text checkpoints have no options; the operator still needs to know how
// to reply.
func TestComposeText_FreeTextCheckpointStillTellsYouHowToAnswer(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{"kind": "action_required"})
	cp := &persistence.TaskMessage{ID: "cp1", Content: "Measure the window and post the width.", Metadata: meta}
	got := notifierWithCheckpoint(cp).composeTextWithHint(context.Background(), task(),
		string(persistence.TaskStatusAwaitingInput), "/vornik answer 77b9 <your answer>")

	if !strings.Contains(got, "Measure the window") {
		t.Errorf("question missing:\n%s", got)
	}
	if !strings.Contains(got, "/vornik answer 77b9 <your answer>") {
		t.Errorf("free-text reply form missing:\n%s", got)
	}
}

// Truncation must cut the QUESTION, never the actionable tail. Cutting from the
// end — the obvious implementation — would drop the reply instruction and leave
// the operator a prompt they cannot act on, which is the original bug again.
func TestComposeText_TruncatesTheQuestionNotTheInstruction(t *testing.T) {
	huge := strings.Repeat("x", 10000)
	n := notifierWithCheckpoint(decisionCheckpoint(huge, "roller", "roman"))
	got := n.composeTextWithHint(context.Background(), task(),
		string(persistence.TaskStatusAwaitingInput), "/vornik answer 77b9 <number>")

	if len(got) > composeTextMaxBytes {
		t.Errorf("len = %d, want <= %d (Telegram's 4096 is the binding limit)", len(got), composeTextMaxBytes)
	}
	for _, want := range []string{"1. Roller", "2. Roman", "/vornik answer 77b9 ", "/ui/tasks/"} {
		if !strings.Contains(got, want) {
			t.Errorf("truncation cut the actionable tail — %q missing:\n…%s", want, got[max(0, len(got)-200):])
		}
	}
	if !strings.Contains(got, "…") {
		t.Error("a truncated question should be visibly elided")
	}
}

// Approval prompts share the grammar so operators learn one thing.
func TestComposeText_ApprovalRendersAsTwoOptions(t *testing.T) {
	got := notifierWithCheckpoint(nil).composeText(context.Background(), task(), string(persistence.TaskStatusAwaitingApproval))
	for _, want := range []string{"1. approve", "2. reject"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from the approval prompt:\n%s", want, got)
		}
	}
}

// No checkpoint reader wired (or no open checkpoint): fall back to the generic
// nudge rather than rendering an empty options block.
func TestComposeText_NoCheckpointFallsBackToTheGenericPrompt(t *testing.T) {
	got := notifierWithCheckpoint(nil).composeText(context.Background(), task(), string(persistence.TaskStatusAwaitingInput))
	if !strings.Contains(got, "needs your input") {
		t.Errorf("generic fallback missing:\n%s", got)
	}
	if strings.Contains(got, "1. ") {
		t.Errorf("must not render an options block with no checkpoint:\n%s", got)
	}
}

// The ref the text prints must be the one /vornik answer resolves.
func TestTaskRef_UsesTheRandomSuffixNotTheTimestamp(t *testing.T) {
	if got := TaskRef("task_20260806212011_77b90a7d0e1d0e47"); got != "77b9" {
		t.Errorf("TaskRef = %q, want %q (first 4 of the RANDOM suffix)", got, "77b9")
	}
	if got := TaskRef("weird-id"); got == "" {
		t.Error("TaskRef must degrade to something non-empty for an unexpected id shape")
	}
}

// Multiple vornik deployments share one Slack workspace, each answering its own
// per-project slash command (/vornik, /holy, …). A hardcoded "/vornik answer"
// would tell a /holy operator to invoke a DIFFERENT instance — one with its own
// database, where the 4-hex ref either matches nothing or, worse, collides with
// an unrelated waiting task and answers it. The reply protocol therefore belongs
// to the channel, not to this shared renderer.
func TestComposeText_ReplyInstructionComesFromTheChannel(t *testing.T) {
	n := notifierWithCheckpoint(decisionCheckpoint("Which blind type?", "roller", "roman"))

	got := n.composeTextWithHint(context.Background(), task(),
		string(persistence.TaskStatusAwaitingInput), "/holy answer 77b9 <number>")
	if !strings.Contains(got, "/holy answer 77b9 <number>") {
		t.Errorf("must print the deployment's OWN command:\n%s", got)
	}
	if strings.Contains(got, "/vornik answer") {
		t.Errorf("must not hardcode /vornik — another instance owns that command:\n%s", got)
	}
}

// Telegram answers by tapping a button and has no slash command; an empty hint
// must omit the reply line rather than print a Slack-shaped instruction that
// does not exist there.
func TestComposeText_EmptyHintOmitsTheReplyLine(t *testing.T) {
	n := notifierWithCheckpoint(decisionCheckpoint("Which blind type?", "roller"))
	got := n.composeTextWithHint(context.Background(), task(), string(persistence.TaskStatusAwaitingInput), "")

	if strings.Contains(got, "Reply:") {
		t.Errorf("no hint means no reply line:\n%s", got)
	}
	if !strings.Contains(got, "1. Roller") {
		t.Errorf("options must still render:\n%s", got)
	}
}
