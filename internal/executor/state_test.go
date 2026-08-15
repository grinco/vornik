package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// Regression: 3 of 60 benchmark runs died on 2026-08-14 with
//
//	failed to marshal execution checkpoint: json: error calling MarshalJSON for
//	type json.RawMessage: invalid character 'x' in string escape code
//
// The step result was built with fmt.Sprintf("%q") — GO quoting, not JSON
// quoting. %q escapes a non-printable byte as \xNN, and JSON has no \x escape.
// The bad bytes sat in LastResult (a json.RawMessage) until the NEXT checkpoint
// tried to marshal them, so a recoverable step error killed the whole execution
// several steps from where it went wrong.
func TestErrorResultJSON_SurvivesAByteThatGoQuotesAsHexEscape(t *testing.T) {
	// \x1b is what %q renders as `\x1b` — invalid JSON, and exactly the shape
	// an error quoting agent terminal output carries.
	err := errors.New("agent output: \x1b[31mFAILED\x1b[0m")

	raw := errorResultJSON(err)

	// It must be valid JSON on its own...
	var decoded map[string]string
	if e := json.Unmarshal(raw, &decoded); e != nil {
		t.Fatalf("errorResultJSON produced invalid JSON: %v (%s)", e, raw)
	}
	if decoded["error"] != err.Error() {
		t.Errorf("error text mangled: %q", decoded["error"])
	}
	// ...AND survive the checkpoint marshal that actually failed in production.
	if _, e := json.Marshal(executionState{LastResult: raw}); e != nil {
		t.Fatalf("checkpoint marshal failed — the original bug: %v", e)
	}
}

// The old code path, kept as a demonstration that the test above would have
// caught the bug rather than passing vacuously.
func TestGoQuotingWouldStillBreakTheCheckpoint(t *testing.T) {
	bad := json.RawMessage(fmt.Sprintf(`{"error":%q}`, "\x1b[31m"))

	if _, err := json.Marshal(executionState{LastResult: bad}); err == nil {
		t.Skip("this Go version's percent-q no longer emits hex escapes; the guard is now belt-and-braces")
	}
}

func TestErrorResultJSON_HandlesANilError(t *testing.T) {
	var decoded map[string]string
	if err := json.Unmarshal(errorResultJSON(nil), &decoded); err != nil {
		t.Fatalf("nil error produced invalid JSON: %v", err)
	}
	if decoded["error"] != "" {
		t.Errorf(`want an empty error string, got %q`, decoded["error"])
	}
}

// A result that cannot be encoded must still yield valid JSON: the caller
// stores it into a RawMessage and walks away, so an invalid value here fails at
// the next checkpoint instead — which is the failure mode being fixed.
func TestResultJSON_FallsBackToValidJSON(t *testing.T) {
	raw := resultJSON(map[string]any{"ch": make(chan int)})

	if !json.Valid(raw) {
		t.Fatalf("unencodable result produced invalid JSON: %s", raw)
	}
	if _, err := json.Marshal(executionState{LastResult: raw}); err != nil {
		t.Fatalf("checkpoint marshal failed on the fallback: %v", err)
	}
}
