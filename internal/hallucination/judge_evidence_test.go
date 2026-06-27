package hallucination

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestFormatEvidence_HappyPath pins the structure the LLM judge sees:
// task header, summary, artifact list, tool-call audit (newest first,
// capped at 30 entries). A regression that drops any of those sections
// silently degrades judge accuracy — the model would grade on partial
// evidence without knowing it.
func TestFormatEvidence_HappyPath(t *testing.T) {
	j := &LLMJudge{MaxEvidenceBytes: 64 * 1024}
	size := int64(1234)
	payload, _ := json.Marshal(map[string]any{
		"taskType": "research",
		"context":  map[string]any{"prompt": "find the year janka was born"},
	})
	in := JudgeInput{
		Task: &persistence.Task{
			ID:        "task_x",
			ProjectID: "janka",
			Payload:   payload,
		},
		Artifacts: []*persistence.Artifact{
			{Name: "research.md", ArtifactClass: "output", SizeBytes: &size},
		},
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "web_fetch", ToolInput: `{"url":"https://example.org"}`, ToolOutput: "200 OK"},
		},
		LastResultText: "Janka was born in 1985 per the linked source.",
	}
	got := j.formatEvidence(in)

	for _, want := range []string{
		"Task ID: task_x",
		"Project: janka",
		"Type: research",
		"find the year janka was born",
		"Final summary text:",
		"Janka was born in 1985",
		"Artifacts:",
		"research.md",
		"Tool-call audit",
		"web_fetch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatEvidence output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestFormatEvidence_NoArtifactsRendersExplicit — empty artifact set
// must render as "(none)" so the judge can distinguish "no artifacts"
// from "I forgot to look." A silent drop here would let "I wrote
// research.md" claims pass on tasks that produced nothing.
func TestFormatEvidence_NoArtifactsRendersExplicit(t *testing.T) {
	j := &LLMJudge{MaxEvidenceBytes: 8192}
	in := JudgeInput{Task: &persistence.Task{ID: "t1", ProjectID: "p1"}}
	got := j.formatEvidence(in)
	if !strings.Contains(got, "(none)") {
		t.Errorf("expected '(none)' artifact placeholder, got:\n%s", got)
	}
}

// TestFormatEvidence_DefaultCapWhenUnset — MaxEvidenceBytes=0 must
// default to 8 KiB rather than producing unbounded output (cap=0
// means "no cap" in a careless implementation; we want "use the
// default").
func TestFormatEvidence_DefaultCapWhenUnset(t *testing.T) {
	j := &LLMJudge{} // MaxEvidenceBytes left zero
	big := strings.Repeat("x", 16*1024)
	in := JudgeInput{
		Task:           &persistence.Task{ID: "t", ProjectID: "p"},
		LastResultText: big,
	}
	got := j.formatEvidence(in)
	// Default cap = 8 KiB; expect overall output ≤ 8 KiB + the
	// "(truncated)" suffix string.
	if len(got) > 8*1024+64 {
		t.Errorf("default cap not enforced: output is %d bytes", len(got))
	}
}

// TestFormatEvidence_TruncatesPastCap — explicit cap must be
// honoured. The truncation marker is itself a contract: judges that
// see "…(truncated)" know they're missing evidence and can abstain.
func TestFormatEvidence_TruncatesPastCap(t *testing.T) {
	j := &LLMJudge{MaxEvidenceBytes: 256}
	in := JudgeInput{
		Task:           &persistence.Task{ID: "t", ProjectID: "p"},
		LastResultText: strings.Repeat("a", 2048),
	}
	got := j.formatEvidence(in)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("expected truncation marker, got tail %q", got[max(0, len(got)-20):])
	}
}

// TestFormatEvidence_AuditMostRecentFirst — audit entries are looped
// in reverse so the newest action is at the top. Old judge versions
// looped forward and the model's attention budget got blown on stale
// noise. Pin the ordering with two entries whose names are sortable.
func TestFormatEvidence_AuditMostRecentFirst(t *testing.T) {
	j := &LLMJudge{MaxEvidenceBytes: 8192}
	in := JudgeInput{
		Task: &persistence.Task{ID: "t", ProjectID: "p"},
		AuditEntries: []*persistence.ToolAuditEntry{
			{ToolName: "first_action", ToolInput: "{}", ToolOutput: "ok"},
			{ToolName: "second_action", ToolInput: "{}", ToolOutput: "ok"},
			{ToolName: "third_action", ToolInput: "{}", ToolOutput: "ok"},
		},
	}
	got := j.formatEvidence(in)
	idxThird := strings.Index(got, "third_action")
	idxFirst := strings.Index(got, "first_action")
	if idxThird == -1 || idxFirst == -1 {
		t.Fatalf("missing audit lines in:\n%s", got)
	}
	if idxThird >= idxFirst {
		t.Errorf("most-recent-first ordering broken: third at %d, first at %d", idxThird, idxFirst)
	}
}

// TestEvaluate_NotConfigured covers the pre-LLM abstain path:
// without a wired Client or Model, the judge MUST return abstain
// + zero metrics rather than calling out to a nil LLM. This is
// the gate that lets the daemon boot without a configured judge
// without exploding every task.
func TestEvaluate_NotConfigured(t *testing.T) {
	ctx := context.Background()

	t.Run("nil judge", func(t *testing.T) {
		var j *LLMJudge
		v, m, err := j.Evaluate(ctx, JudgeInput{})
		if err != nil {
			t.Errorf("nil judge unexpected error: %v", err)
		}
		if v == nil || v.Decision != persistence.JudgeVerdictAbstain {
			t.Errorf("expected abstain verdict, got %+v", v)
		}
		if m == nil {
			t.Error("expected non-nil metrics struct")
		}
	})

	t.Run("nil client", func(t *testing.T) {
		j := &LLMJudge{Model: "x"} // no Client
		v, m, _ := j.Evaluate(ctx, JudgeInput{})
		if v.Decision != persistence.JudgeVerdictAbstain {
			t.Errorf("expected abstain, got %s", v.Decision)
		}
		if m.PromptTokens != 0 {
			t.Errorf("expected zero-tokens metrics, got %+v", m)
		}
	})

}

// max is a tiny helper (Go 1.21+ has it but the helper makes the
// substring slice readable above).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
