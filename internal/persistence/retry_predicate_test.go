package persistence

import "testing"

// Six sites decided "does this task get another attempt?" and every one of them
// asked only about the budget:
//
//	internal/scheduler/scheduler.go:1042, :1074
//	internal/executor/executor.go:2886
//	internal/executor/workflow.go:3420, :3572 (taskWillRetry), :3962
//
// None consulted the failure class, so a permanent failure — a GitHub 404 on a
// PR that does not exist — burned the whole budget. Design
// https://docs.vornik.io

func TestTaskShouldRetry_BudgetSemanticsUnchanged(t *testing.T) {
	cases := []struct {
		name        string
		attempt     int
		maxAttempts int
		want        bool
	}{
		{"budget remains", 1, 3, true},
		{"last attempt", 3, 3, false},
		{"past budget", 4, 3, false},
		{"single attempt", 1, 1, false},
		// MaxAttempts == 0 means retries are disabled. taskWillRetry already
		// treated it as "this is the only attempt"; the shared predicate must
		// keep that, or a legacy row with an unset budget would retry forever.
		{"zero max is disabled", 1, 0, false},
		{"negative max is disabled", 1, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Empty class = the overwhelming majority of failures. This is C5:
			// everything that is not a terminal class behaves exactly as before.
			if got := TaskShouldRetry(tc.attempt, tc.maxAttempts, ""); got != tc.want {
				t.Errorf("TaskShouldRetry(%d, %d, \"\") = %v, want %v",
					tc.attempt, tc.maxAttempts, got, tc.want)
			}
		})
	}
}

// TestTaskShouldRetry_TerminalClassSkipsTheBudget — C1, and the whole point.
func TestTaskShouldRetry_TerminalClassSkipsTheBudget(t *testing.T) {
	if TaskShouldRetry(1, 3, TaskFailureClassForgeTargetUnavailable) {
		t.Error("a terminal class must not spend an attempt even with budget remaining — " +
			"three attempts in five seconds against a PR that does not exist is the incident this fixes")
	}
}

// TestTaskShouldRetry_EveryOtherClassStillRetries — C5, asserted over the whole
// vocabulary rather than a sample. If a future change adds a class to the
// terminal set without meaning to, this fails.
func TestTaskShouldRetry_EveryOtherClassStillRetries(t *testing.T) {
	all := []string{
		TaskFailureClassLLMError, TaskFailureClassTimeout, TaskFailureClassToolError,
		TaskFailureClassInvalidOutput, TaskFailureClassMergeFailed, TaskFailureClassGateFailed,
		TaskFailureClassBudgetBlocked, TaskFailureClassRateLimited, TaskFailureClassWorkflowRole,
		TaskFailureClassWorkflowCfg, TaskFailureClassOrphaned, TaskFailureClassCancelled,
		TaskFailureClassRuntimeError, TaskFailureClassUnknown, TaskFailureClassLeaseExpired,
		TaskFailureClassWorkflowDrift, TaskFailureClassStuckExecution,
		TaskFailureClassToolIterationLimit, TaskFailureClassSecretLeak, TaskFailureClassChildFailed,
	}
	for _, class := range all {
		if !TaskShouldRetry(1, 3, class) {
			t.Errorf("class %q stopped retrying — only the terminal set may do that, and adding "+
				"to it is a deliberate act with a playbook entry, not a side effect", class)
		}
	}
}

// TestTaskShouldRetry_ChildFailedIsNotTerminal — the bubble-up path stamps
// CHILD_FAILED unconditionally (workflow.go:3966), flattening a child's
// permanent forge failure. The parent must still retry on its own budget: its
// work may succeed on a re-run, and inheriting a child's permanence would
// strand a recoverable parent.
func TestTaskShouldRetry_ChildFailedIsNotTerminal(t *testing.T) {
	if IsTerminalFailureClass(TaskFailureClassChildFailed) {
		t.Error("CHILD_FAILED must never be terminal — a child's permanence is flattened into it, " +
			"and treating it as terminal would strand parents whose re-run could succeed")
	}
	if !TaskShouldRetry(1, 3, TaskFailureClassChildFailed) {
		t.Error("a parent with budget remaining must retry after a child failure")
	}
}

// TestIsTerminalFailureClass_IsANarrowAllowList — the guard against the set
// becoming a dumping ground. Membership is asserted exactly, so growing it is a
// deliberate edit to a test rather than a quiet addition.
func TestIsTerminalFailureClass_IsANarrowAllowList(t *testing.T) {
	want := map[string]bool{TaskFailureClassForgeTargetUnavailable: true}

	for class := range want {
		if !IsTerminalFailureClass(class) {
			t.Errorf("%q must be terminal", class)
		}
	}
	// Unknown / empty / arbitrary must all fall through to the budget.
	for _, class := range []string{"", "  ", "NOT_A_CLASS", "forge_target_unavailable"} {
		if IsTerminalFailureClass(class) {
			t.Errorf("IsTerminalFailureClass(%q) = true; the set must be an exact allow-list. "+
				"Note the lowercase spelling is the STEP vocabulary, which is disjoint from this one", class)
		}
	}
}
