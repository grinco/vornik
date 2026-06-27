package email

import (
	"strings"
	"testing"
	"time"
)

func TestParseRFC5322_BasicMessage(t *testing.T) {
	raw := []byte(
		"From: \"Alice\" <alice@ext.test>\r\n" +
			"To: vornik@test\r\n" +
			"Subject: Hello\r\n" +
			"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
			"Message-ID: <abc-123>\r\n" +
			"\r\n" +
			"hello body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.MessageID != "abc-123" {
		t.Errorf("MessageID = %q", parsed.MessageID)
	}
	if parsed.From != "alice@ext.test" {
		t.Errorf("From = %q (want lowercased bare address)", parsed.From)
	}
	if parsed.Subject != "Hello" {
		t.Errorf("Subject = %q", parsed.Subject)
	}
	if !strings.Contains(parsed.Body, "hello body") {
		t.Errorf("Body = %q", parsed.Body)
	}
	if parsed.Date.IsZero() {
		t.Errorf("Date is zero")
	}
}

func TestParseRFC5322_StripsAnglesOnMessageID(t *testing.T) {
	raw := []byte("Message-ID: <id-with-angles>\r\nFrom: a@b\r\n\r\nbody\r\n")
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.MessageID != "id-with-angles" {
		t.Errorf("MessageID = %q", parsed.MessageID)
	}
}

func TestParseRFC5322_NoHeadersError(t *testing.T) {
	if _, err := ParseRFC5322([]byte("garbage")); err == nil {
		t.Error("garbage input must error")
	}
}

func TestParseRFC5322_NoFrom(t *testing.T) {
	raw := []byte("Subject: X\r\nMessage-ID: <a>\r\n\r\nbody\r\n")
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.From != "" {
		t.Errorf("From = %q, want empty", parsed.From)
	}
}

func TestParseRFC5322_ReferencesHeader(t *testing.T) {
	raw := []byte(
		"From: a@b\r\n" +
			"Message-ID: <leaf>\r\n" +
			"References: <root> <middle> <parent>\r\n" +
			"In-Reply-To: <parent>\r\n" +
			"\r\n" +
			"body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if got := parsed.References; len(got) != 3 || got[0] != "root" || got[2] != "parent" {
		t.Errorf("References = %v", got)
	}
	if parsed.InReplyTo != "parent" {
		t.Errorf("InReplyTo = %q", parsed.InReplyTo)
	}
}

func TestParseRFC5322_EncodedSubject(t *testing.T) {
	raw := []byte(
		"From: a@b\r\n" +
			"Message-ID: <x>\r\n" +
			"Subject: =?UTF-8?B?SGVsbG8=?=\r\n" +
			"\r\n" +
			"body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.Subject != "Hello" {
		t.Errorf("Subject = %q, want Hello", parsed.Subject)
	}
}

func TestParseRFC5322_DateUnparseableLeavesZero(t *testing.T) {
	raw := []byte(
		"From: a@b\r\n" +
			"Message-ID: <x>\r\n" +
			"Date: not-a-date\r\n" +
			"\r\n" +
			"body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if !parsed.Date.IsZero() {
		t.Errorf("Date should be zero on unparseable input, got %v", parsed.Date)
	}
}

func TestParseRFC5322_TolerantOfMalformedFrom(t *testing.T) {
	// "From: junk" is non-parseable per RFC 5322 but we tolerate
	// it (lowercased raw) so the allowlist gate handles it.
	raw := []byte(
		"From: junk-no-at\r\n" +
			"Message-ID: <x>\r\n" +
			"\r\n" +
			"body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if parsed.From != "junk-no-at" {
		t.Errorf("From = %q", parsed.From)
	}
}

func TestStripAngles(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<x>", "x"},
		{"  <x>  ", "x"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripAngles(c.in); got != c.want {
			t.Errorf("stripAngles(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseReferences_SpaceSeparated(t *testing.T) {
	got := parseReferences("<a> <b>")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v", got)
	}
}

func TestParseReferences_Empty(t *testing.T) {
	if got := parseReferences("   "); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParseReferences_TolerantOfCommas(t *testing.T) {
	got := parseReferences("<a>,<b>")
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestThreadSessionID_PrefersReferences(t *testing.T) {
	p := ParsedMessage{
		MessageID:  "leaf",
		InReplyTo:  "parent",
		References: []string{"root", "middle", "parent"},
	}
	if got := threadSessionID(p); got != "root" {
		t.Errorf("got %q, want root", got)
	}
}

func TestThreadSessionID_FallsBackToInReplyTo(t *testing.T) {
	p := ParsedMessage{MessageID: "leaf", InReplyTo: "parent"}
	if got := threadSessionID(p); got != "parent" {
		t.Errorf("got %q", got)
	}
}

func TestThreadSessionID_FallsBackToMessageID(t *testing.T) {
	p := ParsedMessage{MessageID: "leaf"}
	if got := threadSessionID(p); got != "leaf" {
		t.Errorf("got %q", got)
	}
}

// Smoke test: make sure ParseRFC5322 handles a multipart/alternative
// payload without choking — exercises the body extractor under the
// realistic email shape Gmail/Outlook produce.
func TestParseRFC5322_MultipartAlternative(t *testing.T) {
	raw := []byte(
		"From: alice@ext.test\r\n" +
			"Message-ID: <multipart>\r\n" +
			"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
			`Content-Type: multipart/alternative; boundary="X"` + "\r\n" +
			"\r\n" +
			"--X\r\n" +
			"Content-Type: text/html\r\n\r\n" +
			"<p>html</p>\r\n" +
			"--X\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"plaintext\r\n" +
			"--X--\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	if !strings.Contains(parsed.Body, "plaintext") {
		t.Errorf("Body = %q, want plaintext", parsed.Body)
	}
}

// Sanity check on Date parsing — RFC 1123Z is the canonical form.
func TestParseRFC5322_DateRFC1123Z(t *testing.T) {
	raw := []byte(
		"From: a@b\r\n" +
			"Message-ID: <x>\r\n" +
			"Date: Sat, 17 May 2026 12:34:56 +0000\r\n" +
			"\r\n" +
			"body\r\n",
	)
	parsed, err := ParseRFC5322(raw)
	if err != nil {
		t.Fatalf("ParseRFC5322: %v", err)
	}
	want := time.Date(2026, 5, 17, 12, 34, 56, 0, time.UTC)
	if !parsed.Date.Equal(want) {
		t.Errorf("Date = %v, want %v", parsed.Date, want)
	}
}
