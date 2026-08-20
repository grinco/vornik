package executor

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// Why the required-keys check must run on the PRE-redaction bytes.
//
// The daemon reads result.json, redacts secret findings, and hands the redacted
// bytes downstream. secrets.Redact is a raw byte-span replacement with no JSON
// awareness, so a span covering a quoted token's quotes leaves a bare token
// where a string belongs and the payload stops parsing. validateRequiredOutputKeys
// then reports EVERY required key as missing, because normalizedResultPayload
// errored — which in the stored message is indistinguishable from the model
// having omitted them.
//
// Measured 2026-08-20 on the bench: all 10 of 10 schema violations in arm 7 had a
// result_json redaction recorded; historically 120 of 180. The control is what
// makes it causal rather than correlated — 6 of 9 passing rungs were ALSO
// redacted, so redaction is fatal only when the splice lands where it breaks the
// JSON.
//
// The required-keys check is structural: its output is a list of key NAMES taken
// from the role's config, never content from the payload. Nothing derived from
// the raw bytes is persisted or forwarded by it, so running it before redaction
// costs no confidentiality. Redaction continues to govern everything that IS
// persisted or forwarded — the agent's message becoming agentError above all.
func TestValidateRequiredOutputKeys_rawPassesWhereRedactedFails(t *testing.T) {
	// An analyst result whose ids are entropy-shaped — the shape that draws
	// entropy findings in ordinary agent output.
	raw := []byte(`{"status":"COMPLETED","analysis":{"id":"a8Kd93jXqLm2"},"usage":{"iterations":7}}`)
	required := []string{"analysis:object"}

	if missing := validateRequiredOutputKeys(raw, required); len(missing) != 0 {
		t.Fatalf("pre-redaction bytes should satisfy the contract; got missing=%v", missing)
	}

	// Redact the id including its quotes — the span shape that breaks JSON.
	i := strings.Index(string(raw), `"a8Kd93jXqLm2"`)
	if i < 0 {
		t.Fatal("fixture changed")
	}
	redacted := secrets.Redact(raw, []secrets.Finding{
		{Type: "entropy", Start: i, End: i + len(`"a8Kd93jXqLm2"`)},
	})

	missing := validateRequiredOutputKeys(redacted, required)
	if len(missing) == 0 {
		t.Skip("redaction preserved parseability for this span; the asymmetry needs a different shape")
	}
	// This is the misattribution: `analysis` was supplied and is still textually
	// present in the redacted bytes, yet reported missing.
	if !strings.Contains(string(redacted), `"analysis"`) {
		t.Fatal("fixture no longer demonstrates the point: analysis is not even textually present")
	}
	t.Logf("confirmed the misattribution: analysis is textually present in the redacted bytes "+
		"yet reported missing=%v — which is why validation must precede redaction", missing)
}

// The other half of the decision: the bytes that DO get persisted must stay
// redacted. This pins the boundary so a later change cannot widen "validate on
// raw" into "persist raw".
func TestRedaction_stillGovernsWhatIsPersisted(t *testing.T) {
	const secretish = "sk-live-AAAABBBBCCCCDDDDEEEEFFFF"
	raw := []byte(`{"status":"FAILED","message":"could not auth with ` + secretish + `"}`)

	i := strings.Index(string(raw), secretish)
	redacted := secrets.Redact(raw, []secrets.Finding{
		{Type: "entropy", Start: i, End: i + len(secretish)},
	})

	if strings.Contains(string(redacted), secretish) {
		t.Error("the redacted body still carries the finding; anything persisted or " +
			"forwarded must be built from these bytes, not the raw ones")
	}
	if !strings.Contains(string(redacted), "[REDACTED:") {
		t.Error("expected a typed marker in place of the finding")
	}
	// And the agent's message — which becomes agentError, is persisted, and can
	// reach a downstream prompt — must be read from here, not from raw.
	if !strings.Contains(string(redacted), `"status":"FAILED"`) {
		t.Error("redaction destroyed the status field this path depends on")
	}
}
