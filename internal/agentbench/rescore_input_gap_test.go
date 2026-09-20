package agentbench

import (
	"strings"
	"testing"
)

// The F6 defect: rescore refused ONLY a journal already at the current harness
// version and re-stamped everything older. That is correct exactly while a
// bump changes the scoring RULE over inputs every journal carries, and wrong
// the moment a bump changes the INPUTS — which D3' does.
func TestRescoreInputGap(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		wantGap bool
		wantSay string
	}{
		{
			name:    "a journal below the floor cannot be re-scored",
			harness: "6",
			wantGap: true,
			// Naming the missing INPUT, not the version gap: "harness 6
			// journals cannot be re-scored" tells an operator nothing
			// they can act on.
			wantSay: "per-visit result bodies",
		},
		{
			name:    "a journal at the floor can",
			harness: MinRescorableHarness,
			wantGap: false,
		},
		{
			name:    "a journal above the floor can",
			harness: "9",
			wantGap: false,
		},
		{
			// A string compare says "10" < "7", so a lexicographic
			// implementation would start re-scoring exactly the journals
			// this refuses, the moment the counter reaches two digits.
			// The test exists now because it cannot be written later by
			// anyone who does not already suspect the bug.
			name:    "a two-digit harness is compared numerically",
			harness: "10",
			wantGap: false,
		},
		{
			name:    "a journal with no harness version is refused",
			harness: "",
			wantGap: true,
			wantSay: "declares no harness version",
		},
		{
			name:    "a non-numeric harness version is refused rather than guessed",
			harness: "v7",
			wantGap: true,
			wantSay: "not a number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rescoreInputGap(tt.harness, true)
			if tt.wantGap && got == "" {
				t.Fatalf("rescoreInputGap(%q) = \"\", want a refusal", tt.harness)
			}
			if !tt.wantGap && got != "" {
				t.Fatalf("rescoreInputGap(%q) = %q, want no gap", tt.harness, got)
			}
			if tt.wantSay != "" && !strings.Contains(got, tt.wantSay) {
				t.Fatalf("rescoreInputGap(%q) = %q, want it to say %q", tt.harness, got, tt.wantSay)
			}

			// The SAME journal without task scores always crosses: probe
			// verdicts are re-derived from traces, whose shape no harness
			// bump has changed. Refusing those would throw away the one
			// thing re-scoring is genuinely for — applying a corrected
			// probe to a pass that already cost money to run.
			if gap := rescoreInputGap(tt.harness, false); gap != "" {
				t.Fatalf("rescoreInputGap(%q, probe-only) = %q, want no gap", tt.harness, gap)
			}
		})
	}
}

// The floor must never sit above the current harness, which would refuse
// every journal including ones the current harness wrote itself.
func TestMinRescorableHarnessIsNotAheadOfTheCurrentOne(t *testing.T) {
	if gap := rescoreInputGap(HarnessVersion, true); gap != "" {
		t.Fatalf("the CURRENT harness version is below the re-scorable floor: %s", gap)
	}
}
