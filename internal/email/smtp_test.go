package email

import (
	"strings"
	"testing"
	"time"
)

func TestRenderRFC5322_Basic(t *testing.T) {
	out := OutboundMessage{
		From:    "vornik@test",
		To:      "alice@ext.test",
		Subject: "Re: Hello",
		Body:    "Reply body\n",
		Date:    time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
	}
	rendered, msgID, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	if msgID == "" {
		t.Fatal("msgID empty")
	}
	s := string(rendered)
	for _, h := range []string{
		"From: vornik@test",
		"To: alice@ext.test",
		"Subject: Re: Hello",
		"Date: ",
		"Message-ID: <" + msgID + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
	} {
		if !strings.Contains(s, h) {
			t.Errorf("rendered missing header %q:\n%s", h, s)
		}
	}
	// Body must appear after the blank-line separator.
	idx := strings.Index(s, "\r\n\r\n")
	if idx < 0 {
		t.Fatal("missing header/body separator")
	}
	body := s[idx+4:]
	if !strings.Contains(body, "Reply body") {
		t.Errorf("body = %q", body)
	}
}

func TestRenderRFC5322_ThreadingHeaders(t *testing.T) {
	out := OutboundMessage{
		From:       "vornik@test",
		To:         "alice@ext.test",
		Subject:    "Re: Hello",
		Body:       "body",
		InReplyTo:  "parent-1",
		References: "root-1 parent-1",
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	s := string(rendered)
	if !strings.Contains(s, "In-Reply-To: <parent-1>") {
		t.Errorf("missing In-Reply-To:\n%s", s)
	}
	if !strings.Contains(s, "References: <root-1> <parent-1>") {
		t.Errorf("References header malformed:\n%s", s)
	}
}

func TestRenderRFC5322_RequiresFromAndTo(t *testing.T) {
	if _, _, err := RenderRFC5322(OutboundMessage{To: "x"}); err == nil {
		t.Error("missing From must error")
	}
	if _, _, err := RenderRFC5322(OutboundMessage{From: "x"}); err == nil {
		t.Error("missing To must error")
	}
}

func TestRenderRFC5322_BodyCRLFNormalised(t *testing.T) {
	out := OutboundMessage{
		From: "a@b",
		To:   "c@d",
		Body: "line1\nline2\nline3\n",
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	if strings.Contains(string(rendered)[strings.Index(string(rendered), "\r\n\r\n")+4:], "\nline2") &&
		!strings.Contains(string(rendered)[strings.Index(string(rendered), "\r\n\r\n")+4:], "\r\nline2") {
		// A bare LF would mean we missed the normalisation pass.
		t.Errorf("body not CRLF-normalised:\n%q", string(rendered))
	}
}

func TestRenderRFC5322_SanitizesHeaderValues(t *testing.T) {
	out := OutboundMessage{
		From:    "vornik@test",
		To:      "alice@ext.test\r\nBcc: attacker@ext.test",
		Subject: "Report\r\nX-Injected: yes",
		Body:    "body",
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	headers := string(rendered[:strings.Index(string(rendered), "\r\n\r\n")])
	if strings.Contains(headers, "\r\nBcc:") || strings.Contains(headers, "\r\nX-Injected:") {
		t.Fatalf("header injection survived:\n%s", headers)
	}
	if !strings.Contains(headers, "Subject: Report X-Injected: yes") {
		t.Fatalf("subject was not flattened:\n%s", headers)
	}
	if !strings.Contains(headers, "To: alice@ext.test Bcc: attacker@ext.test") {
		t.Fatalf("recipient was not flattened:\n%s", headers)
	}
}

func TestRenderRFC5322_BodyAlreadyCRLF(t *testing.T) {
	// Pre-normalised body must round-trip without doubling CRs.
	out := OutboundMessage{
		From: "a@b",
		To:   "c@d",
		Body: "line1\r\nline2\r\n",
	}
	rendered, _, err := RenderRFC5322(out)
	if err != nil {
		t.Fatalf("RenderRFC5322: %v", err)
	}
	s := string(rendered)
	if strings.Contains(s, "\r\r\n") {
		t.Errorf("CRLF normalisation doubled CR:\n%q", s)
	}
}

func TestMessageIDHost_UsesDomainFromAddress(t *testing.T) {
	if got := messageIDHost("alice@example.com"); got != "example.com" {
		t.Errorf("got %q", got)
	}
}

func TestMessageIDHost_FallsBackOnMalformed(t *testing.T) {
	for _, in := range []string{"", "noat", "alice@"} {
		if got := messageIDHost(in); got != "vornik.local" {
			t.Errorf("in=%q got %q, want vornik.local", in, got)
		}
	}
}

func TestRenderReferencesHeader_TrimsAngles(t *testing.T) {
	got := renderReferencesHeader("<a> <b>")
	if got != "<a> <b>" {
		t.Errorf("got %q", got)
	}
}

func TestRenderReferencesHeader_EmptyFieldsDropped(t *testing.T) {
	got := renderReferencesHeader("  a   b  ")
	if got != "<a> <b>" {
		t.Errorf("got %q", got)
	}
}

func TestNormaliseCRLF_NoChangeWhenAlreadyCRLF(t *testing.T) {
	in := "a\r\nb\r\n"
	if got := normaliseCRLF(in); got != in {
		t.Errorf("got %q", got)
	}
}

func TestNormaliseCRLF_RewritesBareLF(t *testing.T) {
	got := normaliseCRLF("a\nb\n")
	if got != "a\r\nb\r\n" {
		t.Errorf("got %q", got)
	}
}

func TestNewNetSMTPSender_DefaultsPort(t *testing.T) {
	s := NewNetSMTPSender("smtp.test", 0, "user", "pass").(*netSMTPSender)
	if s.addr != "smtp.test:587" {
		t.Errorf("addr = %q", s.addr)
	}
	if s.host != "smtp.test" {
		t.Errorf("host = %q", s.host)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
