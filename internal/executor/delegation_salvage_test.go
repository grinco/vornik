package executor

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestSalvageDelegatedTasksFromMessage_FencedJSON covers the recoverable half
// of incident task_20260709102613_79c570a868fefedb (headmatch issue #34,
// 2026-07-09): the issue-fix `decompose` lead dumped its delegatedTasks plan as
// a fenced ```json block inside its free-text `message` (its file_write to
// result.json failed), leaving the structured field empty so the engine
// scheduled zero subtasks. When the dumped block is COMPLETE, salvage recovers
// it from the prose (fence + trailing content tolerated) so the subtasks run.
// (In the live incident the block was also truncated past the output cap —
// unrecoverable here by design; the prompt/maxTokens tuning + the loud guard
// cover that half.)
func TestSalvageDelegatedTasksFromMessage_FencedJSON(t *testing.T) {
	msg := "I need to output the result directly rather than writing to a file. " +
		"Based on my analysis, here is my triage:\n\n" +
		"## Delegated Tasks (SEQUENTIAL execution)\n\n" +
		"```json\n" +
		`{"delegationMode":"SEQUENTIAL","delegatedTasks":[` +
		`{"workflow":"issue-subtask","prompt":"Implement Task 1 (mic calibration)."},` +
		`{"workflow":"issue-subtask","prompt":"Implement Task 2 (FR loading)."}` +
		"]}\n```\n\nThat completes the triage; each subtask is self-contained.\n"
	result := &agentStepResult{Message: msg}

	if !salvageDelegatedTasksFromMessage(result) {
		t.Fatalf("expected salvage to fire for a message carrying a fenced delegatedTasks block")
	}
	if len(result.DelegatedTasks) != 2 {
		t.Fatalf("expected 2 salvaged delegatedTasks, got %d", len(result.DelegatedTasks))
	}
	if result.DelegationMode != "SEQUENTIAL" {
		t.Fatalf("expected salvaged delegationMode=SEQUENTIAL, got %q", result.DelegationMode)
	}
	if result.DelegatedTasks[0].Workflow != "issue-subtask" || result.DelegatedTasks[0].Prompt == "" {
		t.Fatalf("salvaged subtask fields not populated: %+v", result.DelegatedTasks[0])
	}
}

func TestSalvageDelegatedTasksFromMessage_NoOpAndNegativeCases(t *testing.T) {
	cases := []struct {
		name      string
		result    *agentStepResult
		wantFired bool
		wantLen   int
		wantMode  string
	}{
		{
			name:      "already populated is a no-op",
			result:    &agentStepResult{DelegatedTasks: []delegatedTaskSpec{{Workflow: "x", Prompt: "p"}}, Message: `{"delegatedTasks":[{"workflow":"y","prompt":"q"}]}`},
			wantFired: false,
			wantLen:   1,
		},
		{
			name:      "prose without a delegatedTasks object",
			result:    &agentStepResult{Message: "I analysed the issue but could not proceed."},
			wantFired: false,
			wantLen:   0,
		},
		{
			name:      "empty message",
			result:    &agentStepResult{Message: "   "},
			wantFired: false,
			wantLen:   0,
		},
		{
			name:      "object present but delegatedTasks empty",
			result:    &agentStepResult{Message: `{"delegationMode":"SEQUENTIAL","delegatedTasks":[]}`},
			wantFired: false,
			wantLen:   0,
		},
		{
			name:      "bare object with trailing prose",
			result:    &agentStepResult{Message: `prefix {"delegatedTasks":[{"workflow":"issue-subtask","prompt":"do it"}]} suffix prose } with braces`},
			wantFired: true,
			wantLen:   1,
		},
		{
			// The live fefedb shape: an opening ```json fence, then the object
			// truncated mid-array past the output-token cap (no closing brace).
			// Unrecoverable — must fail cleanly (no panic, no partial parse).
			name:      "truncated json block past the output cap",
			result:    &agentStepResult{Message: "```json\n{\"delegationMode\":\"SEQUENTIAL\",\"delegatedTasks\":[{\"workflow\":\"issue-subtask\",\"prompt\":\"Implement Task 1 with a very long spec that gets cut o"},
			wantFired: false,
			wantLen:   0,
		},
		{
			name:      "existing delegationMode is preserved",
			result:    &agentStepResult{DelegationMode: "PARALLEL", Message: `{"delegationMode":"SEQUENTIAL","delegatedTasks":[{"workflow":"issue-subtask","prompt":"do it"}]}`},
			wantFired: true,
			wantLen:   1,
			wantMode:  "PARALLEL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := salvageDelegatedTasksFromMessage(tc.result)
			if got != tc.wantFired {
				t.Fatalf("fired=%v, want %v", got, tc.wantFired)
			}
			if len(tc.result.DelegatedTasks) != tc.wantLen {
				t.Fatalf("delegatedTasks len=%d, want %d", len(tc.result.DelegatedTasks), tc.wantLen)
			}
			if tc.wantMode != "" && tc.result.DelegationMode != tc.wantMode {
				t.Fatalf("delegationMode=%q, want %q", tc.result.DelegationMode, tc.wantMode)
			}
		})
	}
}

// TestFailEmptyDelegation pins the routing decision: with an on_fail wired the
// guard routes there (terminate=false); without one it unwinds
// (terminate=true). guardErr is always non-nil and names the offending step.
func TestFailEmptyDelegation(t *testing.T) {
	e := &Executor{} // nil outcomeRepo → finalizePendingOutcome is a no-op
	exec := &persistence.Execution{ID: "e1"}

	t.Run("routes to on_fail when wired", func(t *testing.T) {
		next, terminate, err := e.failEmptyDelegation(context.Background(), exec, "decompose",
			registry.WorkflowStep{DelegatedWorkflow: "issue-subtask", OnFail: "failed"})
		if terminate {
			t.Fatalf("expected terminate=false when on_fail is wired")
		}
		if next != "failed" {
			t.Fatalf("expected next=failed, got %q", next)
		}
		if err == nil {
			t.Fatalf("expected a non-nil guard error")
		}
	})

	t.Run("unwinds when no on_fail", func(t *testing.T) {
		next, terminate, err := e.failEmptyDelegation(context.Background(), exec, "decompose",
			registry.WorkflowStep{DelegatedWorkflow: "issue-subtask"})
		if !terminate || next != "" || err == nil {
			t.Fatalf("expected terminate=true, empty next, non-nil err; got next=%q terminate=%v err=%v", next, terminate, err)
		}
	})
}

// TestEmptyDelegationGuardTripped pins the loud guard: a fresh (non-resume)
// step that pins delegated_workflow but emits zero delegatedTasks must trip so
// the workflow routes to on_fail with an accurate cause instead of silently
// advancing to a review that sees an empty diff (incident ...fefedb).
func TestEmptyDelegationGuardTripped(t *testing.T) {
	delegating := registry.WorkflowStep{DelegatedWorkflow: "issue-subtask"}
	plain := registry.WorkflowStep{}
	withTasks := &agentStepResult{DelegatedTasks: []delegatedTaskSpec{{Workflow: "issue-subtask", Prompt: "p"}}}
	empty := &agentStepResult{}

	cases := []struct {
		name                string
		step                registry.WorkflowStep
		result              *agentStepResult
		routeAlreadyHandled bool
		want                bool
	}{
		{name: "fresh delegating step with no subtasks trips", step: delegating, result: empty, want: true},
		{name: "delegating step that emitted subtasks does not trip", step: delegating, result: withTasks, want: false},
		{name: "resume never trips (children already ran)", step: delegating, result: empty, routeAlreadyHandled: true, want: false},
		{name: "non-delegating step never trips", step: plain, result: empty, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyDelegationGuardTripped(tc.step, tc.result, tc.routeAlreadyHandled); got != tc.want {
				t.Fatalf("emptyDelegationGuardTripped=%v, want %v", got, tc.want)
			}
		})
	}
}
