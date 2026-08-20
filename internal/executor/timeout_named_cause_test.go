package executor

import (
	"errors"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// A step can hit its wall-clock deadline WHILE failing for a cause the agent
// named in result.json. A degenerate loop that spins until the step timeout is
// the common shape: the loop is why it failed, the deadline is merely when.
//
// Before this, classifyStepOutcome's "timeout" verdict short-circuited the
// switch and refineAgentFailureOutcome — the only place "degenerate loop" is
// recognised — sat in the default arm and never ran. Measured 2026-08-20:
// 2 of 248 degenerate loops in the bench DB were recorded as context_timeout
// this way, one of them exec_20260820121252's report_shape_retry.
//
// This is not cosmetic. persistence.TimeoutStepOutcomes is documented as "the
// degraded outcomes that mean TRUNCATION — the only ones a timeout raise can
// address". Filing a degenerate loop there tells that machinery the remedy is
// MORE WALL CLOCK, which buys the loop more time to spin. The label picks a
// remediation, so a wrong label picks a wrong one.
func TestTimeoutOutcome_namedCauseWinsOverTheWallClock(t *testing.T) {
	cases := map[string]struct {
		msg         string
		wantOutcome string
		wantClass   string
	}{
		"degenerate loop that ran until the deadline": {
			msg: `agent reported FAILED status: Agent entered a degenerate loop (repeated ` +
				`run_shell 4 times with the same arguments). Context was only 5% full, so ` +
				`this is NOT context exhaustion`,
			wantOutcome: string(stepoutcome.DegenerateLoop),
			wantClass:   stepoutcome.ClassDegenerateLoop,
		},
		"plausibility violation that ran until the deadline": {
			msg:         `plausibility violation: role "reviewer" failed 1 rule(s): x`,
			wantOutcome: string(stepoutcome.SchemaViolation),
			wantClass:   stepoutcome.ClassPlausibilityViolation,
		},
		"iteration cap that ran until the deadline": {
			msg:         `agent hit the tool iteration limit`,
			wantOutcome: string(stepoutcome.IterationExhausted),
			wantClass:   stepoutcome.ClassIterationCap,
		},
	}
	for name, c := range cases {
		gotOutcome, gotClass := timeoutOutcomeAndClass(errors.New(c.msg))
		if gotOutcome != c.wantOutcome || gotClass != c.wantClass {
			t.Errorf("%s:\n  got  outcome=%q class=%q\n  want outcome=%q class=%q",
				name, gotOutcome, gotClass, c.wantOutcome, c.wantClass)
		}
	}
}

// A genuine timeout — nothing named, the step simply ran out of clock — must
// STAY a timeout, or the fix would delete the one signal a timeout raise can
// legitimately act on.
func TestTimeoutOutcome_keepsTimeoutWhenNothingIsNamed(t *testing.T) {
	for _, msg := range []string{
		"container exited with code 137",
		"context deadline exceeded",
		"", // no message at all
	} {
		gotOutcome, gotClass := timeoutOutcomeAndClass(errors.New(msg))
		if gotOutcome != string(stepoutcome.Timeout) || gotClass != stepoutcome.ClassContextTimeout {
			t.Errorf("%q: got outcome=%q class=%q, want the timeout pair — a real timeout "+
				"with no named cause must remain addressable by a timeout raise",
				msg, gotOutcome, gotClass)
		}
	}
}

// The named cause is read from the daemon's message, never from the container
// log appended after it — same hazard as the other classifiers.
func TestTimeoutOutcome_ignoresTheContainerLogTail(t *testing.T) {
	poisoned := "container exited with code 137" +
		containerLogSection("[vornik-agent] echoing prior hint: Agent entered a degenerate loop\n")
	gotOutcome, gotClass := timeoutOutcomeAndClass(errors.New(poisoned))
	if gotOutcome != string(stepoutcome.Timeout) || gotClass != stepoutcome.ClassContextTimeout {
		t.Errorf("a phrase in the LOG reclassified a real timeout: outcome=%q class=%q",
			gotOutcome, gotClass)
	}
}
