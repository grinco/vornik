package secrets

import (
	"strings"
	"testing"
)

// P1, 2026-08-27: internal/secrets has two allowlists and only one enforced the
// heuristic/strong boundary. FilterAllowlisted gates on allowlistEligible;
// MultiDetector.Scan did not, so a shipped DefaultAllowlist() entry aimed at
// documentation placeholders could delete a real credential finding outright —
// at rest AND on the Telegram egress path, with no marker and no audit row.
//
// Design: https://docs.vornik.io

func realDetector(t *testing.T) Detector {
	t.Helper()
	d, err := NewMultiDetector(Config{Patterns: DefaultPatterns(), Allowlist: DefaultAllowlist()})
	if err != nil {
		t.Fatalf("build detector: %v", err)
	}
	return d
}

func typesOf(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Type)
	}
	return out
}

// THE LEAK. A password containing angle brackets matched DefaultAllowlist()'s
// `<[^>]+>` placeholder disjunct, which deleted the whole connection_string
// finding — measured 2026-08-27 as zero findings, stored verbatim.
func TestDetectorAllowlistCannotRescueAStrongFinding(t *testing.T) {
	d := realDetector(t)

	cases := []struct{ name, text string }{
		{"password contains angle brackets", `{"dsn":"postgres://admin:pw<redactme>tail@db.internal:5432/prod"}`},
		{"value contains the word example", `{"dsn":"postgres://admin:pwexamplepw@db.internal:5432/prod"}`},
		{"ordinary password (control)", `{"dsn":"postgres://admin:pwZZZQQQ111@db.internal:5432/prod"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := d.Scan([]byte(tc.text))
			if len(got) == 0 {
				t.Fatalf("no findings — the allowlist deleted a strong credential match (types=%v)", typesOf(got))
			}
			out := string(Redact([]byte(tc.text), got))
			if strings.Contains(out, "admin:pw") {
				t.Errorf("credential survived redaction: %s", out)
			}
		})
	}
}

// G2: the allowlist must keep doing its actual job. A fix that simply deleted
// the suppression would pass the test above and fail this one.
func TestDetectorAllowlistStillSuppressesHeuristics(t *testing.T) {
	d := realDetector(t)
	// A UUID is exactly what DefaultAllowlist()'s second entry exists to quiet:
	// high-entropy shaped, not a credential.
	text := `{"request_id":"f47ac10b-58cc-4372-a567-0e02b2c3d479","note":"padding to clear the 16-byte floor"}`

	for _, f := range d.Scan([]byte(text)) {
		if f.Match == "f47ac10b-58cc-4372-a567-0e02b2c3d479" {
			t.Errorf("the UUID was reported as %s — heuristic suppression regressed", f.Type)
		}
	}
}

// The corpus-wide property, asserted over every shipped rule rather than a
// sample, so a pattern added later is covered without anyone extending a list.
// This is what buys the regression signal in place of a suppression counter.
func TestNoDefaultAllowlistEntryCanSuppressAnyStrongPattern(t *testing.T) {
	d := realDetector(t)

	// Values that match a strong pattern AND carry an allowlist trigger.
	cases := []struct{ pattern, value string }{
		{"aws_access_key", "AKIAIOSFODNN7EXAMPLE"},
		{"connection_string", "postgres://admin:pw<x>y@db.internal:5432/prod"},
		{"openai_key", "sk-exampleABCDEFGHIJKLMNOPQRSTUVWXYZ012345"},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			found := false
			for _, f := range d.Scan([]byte("value: " + tc.value + " trailing padding")) {
				if f.Type == tc.pattern {
					found = true
				}
			}
			if !found {
				t.Errorf("%s was suppressed by the default allowlist; a strong pattern must never be", tc.pattern)
			}
		})
	}
}

// The gate is a property of the PATTERN, not of one input. A whole-match test
// (does the regex span this value?) looked equivalent and was the first
// implementation; it let an operator restore the leak by wrapping a substring
// rule in .* — measured 2026-08-27. LiteralPrefix(complete) cannot be widened
// that way, and is immune to RE2 leftmost branch-order effects.
func TestOnlyAnExactLiteralEntryCanExcuseAStrongFinding(t *testing.T) {
	const key = "AKIAQWERTYUIOPASDFGH"
	cases := []struct {
		name, entry string
		suppress    bool
	}{
		{"bare literal names the value", key, true},
		{"anchored literal names the value", "^" + key + "$", true},
		{"substring of the value", "QWERTY", false},
		{"dot-star wrapped substring", ".*QWERTY.*", false},
		{"match anything", ".*", false},
		{"character-class wildcard", "AKIA[0-9A-Z]*", false},
		{"alternation containing the value", key + "|AKIA", false},
		{"literal naming a DIFFERENT value", "AKIAZZZZZZZZZZZZZZZZ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewMultiDetector(Config{Patterns: DefaultPatterns(), Allowlist: []string{tc.entry}})
			if err != nil {
				t.Fatalf("build detector: %v", err)
			}
			found := false
			for _, f := range d.Scan([]byte("creds " + key + " here in env config")) {
				if f.Type == "aws_access_key" {
					found = true
				}
			}
			if suppressed := !found; suppressed != tc.suppress {
				t.Errorf("entry %q: strong finding suppressed = %v, want %v", tc.entry, suppressed, tc.suppress)
			}
		})
	}
}

// G4: the invariant now has two enforcement sites. Naming both here gives a
// third one somewhere to fail.
func TestAllowlistEligibleIsEnforcedAtBothSites(t *testing.T) {
	strong := Finding{Type: "aws_access_key", Match: "AKIAIOSFODNN7EXAMPLE"}

	// Site 1: the per-call filter.
	if got := FilterAllowlisted([]Finding{strong}, [][]byte{[]byte("AKIAIOSFODNN7EXAMPLE")}); len(got) != 1 {
		t.Error("FilterAllowlisted suppressed a strong finding")
	}

	// Site 2: the detector's own allowlist.
	d := realDetector(t)
	found := false
	for _, f := range d.Scan([]byte("key AKIAIOSFODNN7EXAMPLE trailing padding here")) {
		if f.Type == "aws_access_key" {
			found = true
		}
	}
	if !found {
		t.Error("MultiDetector.Scan suppressed a strong finding")
	}
}
