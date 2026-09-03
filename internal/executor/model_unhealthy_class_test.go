package executor

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// MODEL_UNHEALTHY — a circuit-open fast-reject — landed in the catch-all class.
// Measured on production 2026-09-02: 387 of the 452 unclassified step failures
// on the fleet, i.e. 85.6%. A typed, daemon-generated condition with a stable
// prefix was five-sixths of "we don't know".
//
// Design https://docs.vornik.io

// incidentDetail is the verbatim error_detail from the 2026-08-28 episode.
const incidentDetail = `MODEL_UNHEALTHY: model "glm-5.2" on route "agent" circuit open ` +
	`(open since 2026-08-28T23:32:43+02:00)`

// TestModelUnhealthy_ClassifiesFromBothRoutes — C1 and design test 1.
//
// chat.IsModelUnhealthyFailure accepts the TYPED chat.ModelUnhealthyError (from
// in-daemon callers) and the agent-emitted "MODEL_UNHEALTHY" string (the chat
// proxy returns 503 and the agent surfaces it in result.json). Both reach the
// classifier by different routes, so both are driven here — testing one would
// leave the other silently unclassified, which is the bug.
func TestModelUnhealthy_ClassifiesFromBothRoutes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"agent-emitted string form", errors.New(incidentDetail)},
		{"wrapped string form", fmt.Errorf("step failed: %w", errors.New(incidentDetail))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !chat.IsModelUnhealthyFailure(tc.err) {
				t.Fatalf("fixture drift: chat.IsModelUnhealthyFailure must recognise %v", tc.err)
			}
			outcome, class := refineAgentFailureOutcomeErr(tc.err)
			if class != stepoutcome.ClassModelUnhealthy {
				t.Errorf("class = %q, want %q — 85.6%% of the unclassified population is this "+
					"one condition, and it has a name", class, stepoutcome.ClassModelUnhealthy)
			}
			if outcome != stepoutcome.Failed {
				t.Errorf("outcome = %q, want %q — only the CLASS changes; recording a different "+
					"outcome would move these rows between metrics that already count them",
					outcome, stepoutcome.Failed)
			}
		})
	}
}

// TestModelUnhealthy_IsNotTerminal — C4, and the constraint a later change is
// most likely to get wrong.
//
// An open circuit is permanent only WHILE open — minutes to days. The shipped
// circuit-breaker design makes MODEL_UNHEALTHY a model-fallback trigger, and
// parks to AWAITING_INPUT when both primary and fallback are down. Making it
// terminal would bypass that and discard work the fallback would have finished.
func TestModelUnhealthy_IsNotTerminal(t *testing.T) {
	if persistence.IsTerminalFailureClass(stepoutcome.ClassModelUnhealthy) {
		t.Error("MODEL_UNHEALTHY must never be a terminal failure class: an open circuit is " +
			"permanent only while open, and the model-fallback hop is the designed answer")
	}
	// And the task ladder must still spend attempts on it.
	if !persistence.TaskShouldRetry(1, 3, stepoutcome.ClassModelUnhealthy) {
		t.Error("a task must keep its retry budget when a model circuit is open")
	}
}

// TestModelUnhealthy_IsInTheStepVocabulary — D2's other half.
//
// The two vocabularies are disjoint and vocabulary_test.go enforces it via
// go/ast. Lowercase is the step convention; a SCREAMING_SNAKE value here would
// mean the constant landed in the wrong vocabulary.
func TestModelUnhealthy_IsInTheStepVocabulary(t *testing.T) {
	got := stepoutcome.ClassModelUnhealthy
	if got == "" {
		t.Fatal("the class constant must exist")
	}
	for _, r := range got {
		if r >= 'A' && r <= 'Z' {
			t.Errorf("step class %q contains uppercase — the step vocabulary is lowercase and "+
				"disjoint from the task vocabulary", got)
			break
		}
	}
}

// TestModelUnhealthy_NothingElseMovesIntoTheClass — design test 7.
//
// The refiner's arms are ordered against two documented misreadings, and that
// ordering is pinned by an existing test. This asserts the new arm did not
// capture anything the old arms owned.
func TestModelUnhealthy_NothingElseMovesIntoTheClass(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   string
	}{
		{"plausibility violation in output", stepoutcome.ClassPlausibilityViolation},
		{"degenerate loop detected", stepoutcome.ClassDegenerateLoop},
		{"tool iteration limit reached", stepoutcome.ClassIterationCap},
		{"context window exceeded", stepoutcome.ClassContextOverflow},
		{"llm call failed: upstream 500", stepoutcome.ClassLLMCallFailed},
		{"something nobody has seen before", stepoutcome.ClassUnclassified},
	} {
		t.Run(tc.want, func(t *testing.T) {
			_, class := refineAgentFailureOutcomeErr(errors.New(tc.detail))
			if class != tc.want {
				t.Errorf("detail %q classified as %q, want %q — the new arm must not steal "+
					"a message an existing arm owned", tc.detail, class, tc.want)
			}
		})
	}
}

// TestModelUnhealthy_RetryLadderUntouched — C3 and design test 4.
//
// executeAgentStepWithInfraRetry fast-rejects on MODEL_UNHEALTHY before any
// retry (retry.go). That behaviour is CORRECT and this change must not disturb
// it — the 387 hops cost 1.58s in total precisely because the guard works.
func TestModelUnhealthy_RetryLadderUntouched(t *testing.T) {
	err := errors.New(incidentDetail)
	if !isModelUnhealthyFailure(err) {
		t.Fatal("the ladder's fast-reject predicate must still recognise the condition")
	}
	// It is also a model-fallback trigger, which is what routes the work to a
	// healthy model instead of failing it (C4's mechanism).
	if !isModelShapedFailure(err) {
		t.Error("MODEL_UNHEALTHY must remain a model-fallback trigger (isModelShapedFailure): the fallback hop is " +
			"what actually carries the traffic while a circuit is open")
	}
}
