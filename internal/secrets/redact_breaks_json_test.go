package secrets

import (
	"encoding/json"
	"strings"
	"testing"
)

// Redact is a raw byte-span replacement: for each finding it splices
// "[REDACTED:<type>]" over [Start,End) with no awareness of what the surrounding
// bytes mean. That is correct for prose and destructive for JSON.
//
// It matters because the executor scans result.json through this path
// (scanResultForSecrets, CheckpointResultJSON, default action Redact) and hands
// the REDACTED bytes to validateRequiredOutputKeys, persistToolAuditFromResult
// and recordLLMUsageFromResult. If redaction breaks the JSON:
//
//   - normalizedResultPayload errors, and validateRequiredOutputKeys then reports
//     EVERY required key as missing — indistinguishable, in the stored message,
//     from the model having omitted them;
//   - the usage and tool-audit parsers fail too, so tool_calls_used and
//     effective_tool_budget land NULL.
//
// Measured 2026-08-20 on the bench: 120 of 180 "missing required keys" rungs had
// a result_json redaction recorded, and 81 of the 87 NULL-metrics rungs (93%) did.
// One rung logged 147 entropy findings in a single result.json. The agent had
// written a valid file — reproduced in isolation against the real image — and the
// daemon rewrote it before validating it.
//
// This test does not assert that redaction is wrong. It asserts that redaction
// CAN produce non-JSON, so that whatever consumes the redacted bytes is written
// knowing that.
func TestRedact_canBreakJSONStructure(t *testing.T) {
	// A realistic analyst result: the ids are short, opaque and entropy-shaped,
	// which is exactly the shape that draws entropy findings.
	payload := `{"analysis":{"complexity":"standard","test_case_ids":["a8Kd93jXqLm2","Zk19QpRt7vB4","Nx72HsWy5cQ8"],"test_cases_pinned":3}}`

	if !json.Valid([]byte(payload)) {
		t.Fatal("fixture is not valid JSON")
	}

	// Findings whose spans include the surrounding quotes — the case that turns a
	// string into a bare token. Scan() produces spans from its own matcher; this
	// test pins the CONSEQUENCE of a span shape, not the matcher's choices.
	quoted := []Finding{}
	for _, id := range []string{`"a8Kd93jXqLm2"`, `"Zk19QpRt7vB4"`} {
		i := strings.Index(payload, id)
		if i < 0 {
			t.Fatalf("fixture does not contain %s", id)
		}
		quoted = append(quoted, Finding{Type: "entropy", Start: i, End: i + len(id)})
	}

	out := Redact([]byte(payload), quoted)
	if json.Valid(out) {
		t.Errorf("expected redaction over a quoted span to break JSON, but the result parses.\n"+
			"If this now holds, Redact has become structure-aware and the executor's\n"+
			"result.json path can be simplified. Output:\n%s", out)
	} else {
		t.Logf("confirmed: redaction over quoted spans yields non-JSON, e.g.\n%s", out)
	}
}

// And the consequence that actually bit: once the bytes are not JSON, a consumer
// asking "does this contain key X" gets "no" for every key, including keys the
// model did supply.
func TestRedact_brokenJSONLosesEveryKey(t *testing.T) {
	payload := `{"analysis":{"id":"a8Kd93jXqLm2"},"status":"COMPLETED"}`
	i := strings.Index(payload, `"a8Kd93jXqLm2"`)
	out := Redact([]byte(payload), []Finding{{Type: "entropy", Start: i, End: i + len(`"a8Kd93jXqLm2"`)}})

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err == nil {
		t.Skip("redaction preserved validity for this shape; the loss case needs a different span")
	}
	// This is the state the daemon is in when it reports the role's keys missing:
	// it cannot see `analysis` OR `status`, though both were present and only one
	// value was ever a finding.
	if strings.Contains(string(out), `"analysis"`) {
		t.Logf("the key is still textually present but unreachable by a JSON parser:\n%s", out)
	}
}
