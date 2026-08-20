package outputguard

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// syntheticGoogleKey has the SHAPE internal/secrets' google_api_key pattern
// matches (AIza + 35 chars) and is not a real credential. No test in this repo
// may carry a live token; assertions below check markers, never the value.
const syntheticGoogleKey = "AIzaSyDUMMYKEYFORTESTINGONLY0123456789A"

// The gap this closes: outputguard's secret class was exactly two kinds
// (adversarial URL, encoded payload), so a vendor-prefixed API key scraped by
// mcp__scraper__web_fetch crossed the tool-result boundary uncleaned and
// reached the model. internal/secrets already knew the shape; the two packages
// held one vocabulary and only one of them had it.
func TestCredentialRulesRedactVendorKeys(t *testing.T) {
	body := "the page footer contained key=" + syntheticGoogleKey + " and nothing else"

	rep := ScanWithProvenance(body, ProvenanceThirdParty)
	if !rep.HasFinding() {
		t.Fatal("a vendor-prefixed API key in third-party content must be found")
	}
	out := Redact(body, rep)
	if strings.Contains(out, syntheticGoogleKey) {
		t.Error("the key survived redaction")
	}
	if !strings.Contains(out, "[REDACTED:google_api_key]") {
		t.Errorf("redaction must carry the typed marker for forensics; got %q", out)
	}
}

// The provenance design's safety invariant: secret-class rules are NEVER
// skipped. A model can echo a credential it saw upstream, so first-party
// content is not exempt.
func TestCredentialRulesRunOnFirstPartyContent(t *testing.T) {
	body := "I found this key: " + syntheticGoogleKey
	rep := ScanWithProvenance(body, ProvenanceFirstParty)
	if !rep.HasFinding() {
		t.Fatal("secret-class rules must run regardless of provenance — a first-party " +
			"document can echo a real credential")
	}
	if strings.Contains(Redact(body, rep), syntheticGoogleKey) {
		t.Error("the key survived redaction on the first-party path")
	}
}

// The counterweight, and the reason only STRONG patterns are wired here.
// outputguard redacts what the MODEL sees, so a heuristic false positive
// corrupts the agent's working data rather than merely dirtying an audit row.
// On 2026-08-20 entropy-shaped case ids did exactly that to result.json.
func TestCredentialRulesDoNotEnforceHeuristics(t *testing.T) {
	for _, benign := range []string{
		"case s1_case_1 passed and a8Kd93jXqLm2 was the run id",
		"commit 4f2a9c1e8b3d5a7f0c2e4b6d8a0f2c4e6b8d0a2f landed",
		"status=ok retries=3 duration_ms=1204",
	} {
		rep := ScanWithProvenance(benign, ProvenanceThirdParty)
		for _, f := range rep.Findings {
			if secrets.IsHeuristicType(string(f.Kind)) {
				t.Errorf("heuristic rule %q fired on benign content %q — outputguard must "+
					"enforce only strong prefix-anchored patterns", f.Kind, benign)
			}
		}
	}
}

// Operator configuration is honoured through the same corpus the detector
// compiles, so a disabled pattern is disabled in both places.
func TestSetCredentialPatternsHonoursOperatorCorpus(t *testing.T) {
	t.Cleanup(func() {
		if err := SetCredentialPatterns(secrets.StrongPatterns(secrets.DefaultPatterns())); err != nil {
			t.Fatalf("restore default credential patterns: %v", err)
		}
	})

	if err := SetCredentialPatterns(secrets.StrongPatterns(
		secrets.EffectivePatterns([]string{"google_api_key"}, nil))); err != nil {
		t.Fatalf("SetCredentialPatterns: %v", err)
	}
	body := "key=" + syntheticGoogleKey
	for _, f := range ScanWithProvenance(body, ProvenanceThirdParty).Findings {
		if string(f.Kind) == "google_api_key" {
			t.Error("a pattern the operator disabled must not be enforced by outputguard")
		}
	}

	// Restoring the full corpus brings it back — proving the first assertion
	// measured configuration, not a broken rule.
	if err := SetCredentialPatterns(secrets.StrongPatterns(secrets.DefaultPatterns())); err != nil {
		t.Fatalf("SetCredentialPatterns: %v", err)
	}
	var found bool
	for _, f := range ScanWithProvenance(body, ProvenanceThirdParty).Findings {
		if string(f.Kind) == "google_api_key" {
			found = true
		}
	}
	if !found {
		t.Error("the default corpus must enforce google_api_key")
	}
}

// A corpus that fails to compile must not silently leave the previous rules in
// place under a caller that believes it swapped them, nor wipe them.
func TestSetCredentialPatternsRejectsBadRegex(t *testing.T) {
	before := ScanWithProvenance("key="+syntheticGoogleKey, ProvenanceThirdParty).HasFinding()
	err := SetCredentialPatterns([]secrets.Pattern{{Name: "broken", Regex: "([unclosed"}})
	if err == nil {
		t.Fatal("an uncompilable pattern must be an error")
	}
	after := ScanWithProvenance("key="+syntheticGoogleKey, ProvenanceThirdParty).HasFinding()
	if before != after {
		t.Error("a failed swap must leave the existing rules intact")
	}
}
