package executor

import (
	"strings"
	"testing"
)

// TestRecoveryOutcomeLabel is the outcome of four design rounds that ended in
// withdrawing a much larger change (a receipt-time enum check on the recovery
// `outcome`). The rounds established that the enum needs no receipt check —
// ParseLeadOutcome is STRICT and refuses an unknown outcome by name, and an
// exact-match enum check would have raised a false alarm on case variants the
// parser deliberately normalises.
//
// What they did find is this: the caller DISCARDS the parser's error
//
//	parsedOutcome, parsedOK, _ := ParseLeadOutcome(resultBytes)
//
// and then labels the violation "missing". For an unrecognised outcome that is
// false — the value was present, the parser knew exactly what it was and said
// so, and the label throws the diagnosis away and replaces it with a different
// claim. Same defect class as the rescore refusal fixed earlier today: a
// message that says something untrue about why it refused.
func TestRecoveryOutcomeLabel(t *testing.T) {
	for _, tt := range []struct {
		name      string
		result    string
		wantLabel string
		wantNot   string
	}{
		{
			name:      "an unrecognised outcome is named, not called missing",
			result:    `{"outcome":"frobnicate"}`,
			wantLabel: "frobnicate",
			wantNot:   "missing",
		},
		{
			// The forbidden-but-known case: continue parses, and
			// recoveryContractViolated's default branch rejects it.
			name:      "a forbidden known outcome keeps its name",
			result:    `{"outcome":"continue","plan":{"steps":["s1"]}}`,
			wantLabel: "continue",
		},
		{
			// A wrong-kind checkpoint already refined to "checkpoint:<kind>"
			// before this change; it must keep doing so.
			name:      "a wrong-kind checkpoint keeps its refined label",
			result:    `{"outcome":"checkpoint","checkpoint":{"kind":"review","draft":"d"}}`,
			wantLabel: "checkpoint:review",
		},
		{
			// Genuinely absent is the one case "missing" is true for.
			name:      "an absent outcome is still missing",
			result:    `{"nothing":"here"}`,
			wantLabel: "missing",
		},
		{
			// Case variants are normalised by the parser and must NOT be
			// reported as anything unusual — the false-alarm mode that
			// disqualified the withdrawn enum check. "checkpoint:decision"
			// contains "checkpoint", which is what the assertion checks.
			name:      "a case variant is normalised, not flagged",
			result:    `{"outcome":"CHECKPOINT","checkpoint":{"kind":"decision","question":"q?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}}`,
			wantLabel: "checkpoint",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := recoveryOutcomeLabel([]byte(tt.result))
			if !strings.Contains(got, tt.wantLabel) {
				t.Fatalf("recoveryOutcomeLabel(%s) = %q, want it to contain %q",
					tt.result, got, tt.wantLabel)
			}
			if tt.wantNot != "" && strings.Contains(got, tt.wantNot) {
				t.Fatalf("recoveryOutcomeLabel(%s) = %q, must not say %q",
					tt.result, got, tt.wantNot)
			}
		})
	}
}
