package narrator

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// TestLLM_RecordsUsageWithTaskAndExecutionID — design §5.1/§9: unlike
// memory.NarrativeWriter, the narrator's task_llm_usage rows carry
// BOTH TaskID and ExecutionID, role=task_narrator, and the new
// TaskLLMUsageSourceTaskNarrator source constant.
func TestLLM_RecordsUsageWithTaskAndExecutionID(t *testing.T) {
	fp := &fakeProvider{replies: []string{"Reading the pricing pages you gave me."}}
	usage := &fakeUsageRecorder{}
	h := newTestHarness(t, func(n *Narrator) {
		n.Client = fp
		n.LLMUsage = usage
		n.Pricing = fakePricing{perCall: 0.001}
	})
	seedRunningExecution(h)

	h.Sub.push(testExecID, livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "researcher"})
	h.awaitLine(2 * time.Second)

	rows := usage.all()
	if len(rows) != 1 {
		t.Fatalf("expected 1 task_llm_usage row, got %d", len(rows))
	}
	row := rows[0]
	if row.Role != "task_narrator" {
		t.Errorf("Role = %q, want task_narrator", row.Role)
	}
	if row.Source != persistence.TaskLLMUsageSourceTaskNarrator {
		t.Errorf("Source = %q, want %q", row.Source, persistence.TaskLLMUsageSourceTaskNarrator)
	}
	if row.TaskID == nil || *row.TaskID != "task-1" {
		t.Errorf("TaskID = %v, want task-1", row.TaskID)
	}
	if row.ExecutionID == nil || *row.ExecutionID != testExecID {
		t.Errorf("ExecutionID = %v, want %s", row.ExecutionID, testExecID)
	}
	if row.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want proj-1", row.ProjectID)
	}
}

// TestLLM_ModelOverride_UsesModelOverridable — the narrator model
// pin routes through chat.ModelOverridable.WithModel when the
// provider supports it (mirrors memory.pickModelForNarrative).
func TestLLM_ModelOverride_UsesModelOverridable(t *testing.T) {
	base := &fakeProvider{replies: []string{"line"}}
	ov := &overridableFakeProvider{fakeProvider: base}
	got := pickModel(ov, "cheap-model")
	pinned, ok := got.(*fakeProvider)
	if !ok {
		t.Fatalf("pickModel should return the pinned provider")
	}
	if pinned.model != "cheap-model" {
		t.Errorf("model = %q, want cheap-model", pinned.model)
	}
}

func TestLLM_ModelOverride_EmptyModelLeavesProviderUnchanged(t *testing.T) {
	base := &fakeProvider{}
	if pickModel(base, "") != base {
		t.Error("empty model should return the original provider")
	}
}

// overridableFakeProvider adds chat.ModelOverridable on top of
// fakeProvider for the pickModel test above.
type overridableFakeProvider struct {
	*fakeProvider
}

func (o *overridableFakeProvider) WithModel(model string) chat.Provider {
	return &fakeProvider{replies: o.replies, model: model}
}

var _ chat.ModelOverridable = (*overridableFakeProvider)(nil)
