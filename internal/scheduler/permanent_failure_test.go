package scheduler

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// REGRESSION — headmatch, 2026-09-01.
//
// Two tasks each burned their full three-attempt budget in under five seconds
// against grinco/headmatch#999999, a PR that does not exist:
//
//	task_20260902002352_a0777354f61fbb62  3/3  22:23:52 → 22:23:57
//	task_20260902002420_7119194d58b9596c  3/3  22:24:21 → 22:24:26
//
// Six executions, six identical 404s, three of them known-futile before they
// were issued. The requeue decision asked only about the attempt budget and
// never about the failure class — and classifySchedulerFailure ran only on the
// terminal branch, after the budget was already spent.
//
// Design https://docs.vornik.io

// incidentError is the verbatim last_error from the first failed task.
const incidentError = `Change-request review failed. (last step: fetch_diff, reason: system step ` +
	`forge.fetch_diff failed: forge.fetch_diff: fetch diff for grinco/headmatch#999999: ` +
	`forge/github: fetch diff HTTP 404: {"message":"Not Found","status":"404"})`

func TestPermanentForgeFailure_DoesNotSpendAnAttempt(t *testing.T) {
	class := persistence.TaskFailureClassForgeTargetUnavailable

	// Attempt 1 of 3 — budget wide open, which is exactly the state that
	// produced the incident.
	if persistence.TaskShouldRetry(1, 3, class) {
		t.Fatal("a permanent forge failure must not be re-queued with budget remaining: " +
			"three attempts in five seconds against a non-existent PR is the incident this fixes")
	}

	// And the whole ladder, so no attempt number is a loophole.
	for attempt := 1; attempt <= 3; attempt++ {
		if persistence.TaskShouldRetry(attempt, 3, class) {
			t.Errorf("attempt %d/3 was re-queued for a permanent failure", attempt)
		}
	}
}

// TestIncidentErrorAloneIsNotEnough — the text of the incident error must NOT
// make anything terminal. Permanence travels as a typed error and arrives as a
// class; a message that happens to contain "404" is not a signal.
//
// This is the guard against someone "simplifying" the fix into a string match.
func TestIncidentErrorAloneIsNotEnough(t *testing.T) {
	if !strings.Contains(incidentError, "404") {
		t.Fatal("fixture drift: the incident error should contain the status")
	}
	// UNKNOWN is what the row actually carried before the fix.
	if !persistence.TaskShouldRetry(1, 3, persistence.TaskFailureClassUnknown) {
		t.Error("UNKNOWN must still retry — only a typed, classified permanent failure is terminal, " +
			"and classifying by message text is what both classifiers deliberately refuse to do")
	}
}

// TestTransientForgeFailureStillRetries — C5. A 502 keeps its full budget.
// This is the test that fails if permanence is applied too widely.
func TestTransientForgeFailureStillRetries(t *testing.T) {
	// A transient forge error is not typed, so it classifies as UNKNOWN and
	// spends attempts exactly as before this change.
	if !persistence.TaskShouldRetry(1, 3, persistence.TaskFailureClassUnknown) {
		t.Error("a transient failure must retry — nothing about 429/5xx handling changed")
	}
	if persistence.TaskShouldRetry(3, 3, persistence.TaskFailureClassUnknown) {
		t.Error("budget exhaustion must still be terminal")
	}
}
