package executor

import (
	"bytes"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

// TestURLSpanExemption pins the fix for the 2026-06-20 startupjobs scan
// cascade: a high-entropy URL PATH slug (job-portal offer IDs) must be exempt
// from entropy redaction so the URL isn't mangled into
// "startupjobs.[REDACTED:entropy]" — which the hallucination URL-grounding
// detector then flagged as unsupported, failing the step. The query string is
// NOT exempt: tokens/signatures live there (?sig=, ?token=), so an entropy hit
// in the query still redacts.
func TestURLSpanExemption(t *testing.T) {
	body := []byte(`top match: https://www.startupjobs.cz/nabidka/HIGHENTROPYSLUGzZ9q2Wm7Kx4Tn8Rb/lead-role ` +
		`and a presigned link https://files.example.com/x?sig=QuerySecretZZ9Q2WM7KX4TN8RBSIGTOKEN`)

	slug := []byte("HIGHENTROPYSLUGzZ9q2Wm7Kx4Tn8Rb")
	qsecret := []byte("QuerySecretZZ9Q2WM7KX4TN8RBSIGTOKEN")
	s1 := bytes.Index(body, slug)
	s2 := bytes.Index(body, qsecret)
	if s1 < 0 || s2 < 0 {
		t.Fatal("test fixture offsets not found")
	}

	findings := []secrets.Finding{
		{Type: secrets.FindingTypeEntropy, Match: string(slug), Start: s1, End: s1 + len(slug)},
		{Type: secrets.FindingTypeEntropy, Match: string(qsecret), Start: s2, End: s2 + len(qsecret)},
	}

	spans := extractURLSpans(body)
	kept := filterFindingsOutsidePathSpans(findings, spans)

	if len(kept) != 1 {
		t.Fatalf("expected exactly the query-string finding kept, got %d: %+v", len(kept), kept)
	}
	if kept[0].Match != string(qsecret) {
		t.Fatalf("expected the ?sig= query secret to remain redactable, got %q", kept[0].Match)
	}
}

// TestURLSpanExemption_OnlyEntropy confirms a DETERMINISTIC finding inside a
// URL path is NOT exempted — only entropy findings are (a deterministic
// secret pattern in a path is a smuggling channel and must still redact).
func TestURLSpanExemption_OnlyEntropy(t *testing.T) {
	body := []byte(`see https://h.io/path/eyJhbGciOiJIUzI1NiJ9.abc.def/more`)
	jwt := []byte("eyJhbGciOiJIUzI1NiJ9.abc.def")
	s := bytes.Index(body, jwt)
	findings := []secrets.Finding{
		{Type: "jwt", Match: string(jwt), Start: s, End: s + len(jwt)},
	}
	kept := filterFindingsOutsidePathSpans(findings, extractURLSpans(body))
	if len(kept) != 1 {
		t.Fatalf("deterministic (jwt) finding in a URL path must NOT be exempted; got %d kept", len(kept))
	}
}
