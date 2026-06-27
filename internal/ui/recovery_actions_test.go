package ui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestSteerPrefillFor covers the §8.6 failure-derived prefill: the
// steering textarea is seeded with a class-appropriate starter for
// the classes where that helps, and stays empty otherwise.
func TestSteerPrefillFor(t *testing.T) {
	// Classes that get a non-empty, class-specific prefill.
	withPrefill := []string{
		persistence.TaskFailureClassToolIterationLimit,
		persistence.TaskFailureClassInvalidOutput,
		persistence.TaskFailureClassInvalidOutputLoop,
		persistence.TaskFailureClassRateLimited,
	}
	for _, c := range withPrefill {
		p := SteerPrefillFor(c)
		if strings.TrimSpace(p) == "" {
			t.Errorf("class %q: expected a prefill, got empty", c)
		}
	}
	// invalid-output classes name the schema problem explicitly.
	if !strings.Contains(SteerPrefillFor(persistence.TaskFailureClassInvalidOutput), "schema") {
		t.Error("invalid-output prefill should mention schema validation")
	}
	// Unknown / generic classes get no prefill (operator types fresh).
	if SteerPrefillFor("UNKNOWN") != "" {
		t.Error("unknown class should have no prefill")
	}
	if SteerPrefillFor("") != "" {
		t.Error("empty class should have no prefill")
	}
}

// TestRecoveryActionsFor_EveryClassHasOnePrimary — every known
// failure class returns exactly one primary action. The first
// action's Variant pins the recommended path; the UI renders it
// with the accent treatment. A class with zero primaries would
// degrade silently; a class with two would split operator focus.
// Pins both at once.
func TestRecoveryActionsFor_EveryClassHasOnePrimary(t *testing.T) {
	allClasses := []string{
		persistence.TaskFailureClassLLMError,
		persistence.TaskFailureClassTimeout,
		persistence.TaskFailureClassToolError,
		persistence.TaskFailureClassInvalidOutput,
		persistence.TaskFailureClassInvalidOutputLoop,
		persistence.TaskFailureClassMergeFailed,
		persistence.TaskFailureClassGateFailed,
		persistence.TaskFailureClassBudgetBlocked,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassWorkflowRole,
		persistence.TaskFailureClassWorkflowCfg,
		persistence.TaskFailureClassWorkflowDrift,
		persistence.TaskFailureClassOrphaned,
		persistence.TaskFailureClassCancelled,
		persistence.TaskFailureClassRuntimeError,
		persistence.TaskFailureClassUnknown,
		persistence.TaskFailureClassLeaseExpired,
		persistence.TaskFailureClassStuckExecution,
		persistence.TaskFailureClassToolIterationLimit,
		persistence.TaskFailureClassSecretLeak,
		persistence.TaskFailureClassChildFailed,
		persistence.TaskFailureClassHallucinatedPlacement,
		"",                         // unclassified — default branch
		"NEW_CLASS_NOT_YET_MAPPED", // forward-compat
	}
	for _, class := range allClasses {
		t.Run("class="+class, func(t *testing.T) {
			actions := RecoveryActionsFor(class, "task_test")
			require.NotEmpty(t, actions, "every class must produce at least one action")
			primaries := 0
			for _, a := range actions {
				if a.Variant == "primary" {
					primaries++
				}
			}
			assert.Equal(t, 1, primaries,
				"class %q must have exactly one primary action; got %d",
				class, primaries)
		})
	}
}

// TestRecoveryActionsFor_EmptyTaskIDReturnsNil — defensive: a
// rendering bug that loses the task ID must not produce a form
// posting to /retry (which would 400). Empty taskID = no actions.
func TestRecoveryActionsFor_EmptyTaskIDReturnsNil(t *testing.T) {
	actions := RecoveryActionsFor(persistence.TaskFailureClassLLMError, "")
	assert.Nil(t, actions)
}

// TestRecoveryActionsFor_AlwaysIncludesClose — every failure class
// ends with the "Close — won't pursue" action (danger variant).
// This is the universal escape hatch — operator can always give up
// with explanation, and the audit captures that decision.
func TestRecoveryActionsFor_AlwaysIncludesClose(t *testing.T) {
	for _, class := range []string{
		persistence.TaskFailureClassLLMError,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassSecretLeak,
		"",
	} {
		actions := RecoveryActionsFor(class, "task_x")
		last := actions[len(actions)-1]
		assert.Equal(t, "danger", last.Variant, "class %q must end with danger-variant close action", class)
		assert.Contains(t, strings.ToLower(last.Label), "close")
		assert.Equal(t, "POST", last.Method)
	}
}

// TestRecoveryActionsFor_AlwaysIncludesSteer — every class has a
// "Steer + retry" action (either as the primary or as a secondary
// fallback). This is the universal escape hatch for operators who
// know what's wrong but bare retry will repeat the failure.
func TestRecoveryActionsFor_AlwaysIncludesSteer(t *testing.T) {
	for _, class := range []string{
		persistence.TaskFailureClassLLMError,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassToolIterationLimit,
		persistence.TaskFailureClassWorkflowRole,
		persistence.TaskFailureClassChildFailed,
		"",
	} {
		actions := RecoveryActionsFor(class, "task_x")
		found := false
		for _, a := range actions {
			if strings.Contains(strings.ToLower(a.Label), "steer") {
				found = true
				break
			}
		}
		assert.True(t, found, "class %q must offer a steer action somewhere", class)
	}
}

// TestRecoveryActionsFor_SafetyClassesDoNotPromoteRetry — secret
// leak and hallucinated placement are SAFETY classes — the operator
// MUST review the audit before any reattempt. The primary action
// must NOT be a one-click retry; it must direct to the audit page.
// Reversion of this rule is a real safety regression.
func TestRecoveryActionsFor_SafetyClassesDoNotPromoteRetry(t *testing.T) {
	for _, class := range []string{
		persistence.TaskFailureClassSecretLeak,
		persistence.TaskFailureClassHallucinatedPlacement,
	} {
		actions := RecoveryActionsFor(class, "task_x")
		require.NotEmpty(t, actions)
		primary := actions[0]
		assert.NotContains(t, strings.ToLower(primary.Label), "retry",
			"safety class %q: primary must NOT be retry — operator must inspect audit first", class)
		assert.Contains(t, strings.ToLower(primary.Label), "audit",
			"safety class %q: primary should send operator to the audit log", class)
	}
}

// TestRecoveryActionsFor_RateLimitedRecommendsRequeue — after the
// 2026-05-26 scraper backoff fix, the bare requeue path has
// materially better odds on RATE_LIMITED. The primary action should
// be requeue (not steer-first) because the in-process backoff
// absorbs most transients without operator intervention.
// Verb renamed from "retry" to "requeue" 2026-05-26 refactor B for
// consistency with the new task-level / execution-level distinction.
func TestRecoveryActionsFor_RateLimitedRecommendsRequeue(t *testing.T) {
	actions := RecoveryActionsFor(persistence.TaskFailureClassRateLimited, "task_x")
	require.NotEmpty(t, actions)
	primary := actions[0]
	assert.Contains(t, strings.ToLower(primary.Label), "requeue",
		"RATE_LIMITED: primary should be Requeue — the backoff layer handles transients now")
	assert.Equal(t, "POST", primary.Method)
}

// TestRecoveryActionsFor_BudgetBlockedSendsToSpend — retrying a
// budget-blocked task without first checking the cap is a waste of
// a click. Primary must send the operator to the spend / budget
// surface first.
func TestRecoveryActionsFor_BudgetBlockedSendsToSpend(t *testing.T) {
	actions := RecoveryActionsFor(persistence.TaskFailureClassBudgetBlocked, "task_x")
	require.NotEmpty(t, actions)
	primary := actions[0]
	assert.Equal(t, "GET", primary.Method)
	assert.Contains(t, primary.URL, "/spend")
}

// TestRecoveryActionsFor_PostFormsCarryConfirm — destructive POST
// actions (retry, close) must carry a Confirm string so the
// template fires a window.confirm() before submission. Stops
// operator-mistake clicks on mobile.
func TestRecoveryActionsFor_PostFormsCarryConfirm(t *testing.T) {
	for _, class := range []string{
		persistence.TaskFailureClassLLMError,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassStuckExecution,
	} {
		actions := RecoveryActionsFor(class, "task_x")
		for _, a := range actions {
			if a.Method == "POST" {
				assert.NotEmpty(t, a.Confirm,
					"class %q action %q is a POST without Confirm — mobile mis-tap risk",
					class, a.Label)
			}
		}
	}
}
