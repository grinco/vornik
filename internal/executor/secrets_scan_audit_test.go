package executor

import (
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// TestFilterFindingsOutsidePathSpans_OnlyEntropyExempt guards the fix that
// the path-span exemption applies ONLY to entropy findings. A deterministic,
// prefix-anchored credential pattern (anthropic_key here) must STILL redact
// even when it overlaps a path-shaped JSON value — otherwise an agent has a
// labeled channel to smuggle a secret past redaction by parking it in a
// `storagePath`-style field. Entropy findings stay path-exempt (the
// 2026-05-18 janka verifyClaimedFiles regression).
func TestFilterFindingsOutsidePathSpans_OnlyEntropyExempt(t *testing.T) {
	span := secrets.Span{Start: 10, End: 50} // a path-shaped JSON value
	findings := []secrets.Finding{
		{Type: secrets.FindingTypeEntropy, Start: 20, End: 40, Match: "highentropyblob"}, // inside → dropped
		{Type: "anthropic_key", Start: 22, End: 42, Match: "sk-ant-secret"},              // inside → MUST be kept
		{Type: secrets.FindingTypeEntropy, Start: 60, End: 70, Match: "otherblob"},       // outside → kept
	}

	out := filterFindingsOutsidePathSpans(findings, []secrets.Span{span})

	var entropyInside, anthropic, entropyOutside bool
	for _, f := range out {
		switch {
		case f.Type == secrets.FindingTypeEntropy && f.Start == 20:
			entropyInside = true
		case f.Type == "anthropic_key":
			anthropic = true
		case f.Type == secrets.FindingTypeEntropy && f.Start == 60:
			entropyOutside = true
		}
	}
	if entropyInside {
		t.Error("entropy finding inside a path span should be dropped (path-exempt)")
	}
	if !anthropic {
		t.Error("deterministic anthropic_key inside a path span MUST still redact — this was the labeled-exfil channel")
	}
	if !entropyOutside {
		t.Error("entropy finding outside any path span should be kept")
	}
}
