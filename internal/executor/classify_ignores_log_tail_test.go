package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A failed step's error carries the container-log tail appended after the real
// message (container.go: `agentError += containerLogSection(logs)`), and that
// whole string is what reaches classifyShapeFailure. So every phrase the
// classifier keys on is also matchable in LOG CONTENT the agent happened to
// print — including the corrective hints the retry ladder itself injects into
// the next prompt, which an agent can echo into its own output.
//
// The hazard predates the tail being raised from 50 to 400 lines, but the raise
// multiplies the exposure by eight, so it is fixed here rather than inherited.
//
// The fix is to classify on the message only. The delimiter is the boundary
// between "what the daemon concluded" and "what the container printed", and only
// the former is a machine contract.
func TestClassifyShapeFailure_ignoresTheContainerLogTail(t *testing.T) {
	// A step that failed for an unrelated reason, whose log happens to contain
	// every phrase the classifier routes on.
	poisoned := "container exited with code 1" +
		containerLogSection(strings.Join([]string{
			"[vornik-agent] iteration=3",
			`[vornik-agent] prompt echo: schema violation: output contract for step "report" not met`,
			"[vornik-agent] prompt echo: plausibility violation: role \"analyst\" failed 1 rule(s)",
			"[vornik-agent] done",
		}, "\n"))

	if got := classifyShapeFailure(errors.New(poisoned)); got != shapeFailureNone {
		t.Errorf("classified %v from log CONTENT, not from the error — a phrase the agent "+
			"printed must not steer the retry ladder", got)
	}
}

// The same hazard at the outcome-classification site, which is the more
// dangerous of the two because it matches the bare word "timeout" — something an
// agent log contains routinely, with no adversarial behaviour required.
func TestStepOutcomeClassification_ignoresTheContainerLogTail(t *testing.T) {
	poisoned := "container exited with code 1" +
		containerLogSection("[vornik-agent] run_shell: curl: (28) operation timeout after 2000ms\n"+
			"[vornik-agent] retrying\n")

	if got := classifyStepOutcome(context.Background(), errors.New(poisoned)); got == "timeout" {
		t.Error(`classified "timeout" from a curl message inside the log tail — the step did ` +
			`not time out, it exited non-zero, and the outcome taxonomy now says otherwise`)
	}
}

// And the real messages must still classify, so the strip cannot be implemented
// by simply refusing to match.
func TestClassifyShapeFailure_stillClassifiesRealMessages(t *testing.T) {
	cases := map[string]shapeFailureKind{
		`plausibility violation: role "analyst" failed 1 rule(s): x`:                                     shapeFailurePlausibility,
		`schema violation: output contract for step "report" not met — no file matching "x" was written`: shapeFailureOutputContract,
		`schema violation: role "analyst" result.json is missing required keys: [analysis:object]`:       shapeFailureJSON,
	}
	for msg, want := range cases {
		// Once bare, and once with a log tail appended — the classification must
		// be identical either way.
		if got := classifyShapeFailure(errors.New(msg)); got != want {
			t.Errorf("bare %q: got %v, want %v", msg, got, want)
		}
		withTail := msg + containerLogSection("[vornik-agent] iteration=1\n[vornik-agent] done")
		if got := classifyShapeFailure(errors.New(withTail)); got != want {
			t.Errorf("with log tail %q: got %v, want %v — appending diagnostics must not "+
				"change the classification", msg, got, want)
		}
	}
}
