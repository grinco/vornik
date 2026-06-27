// Tests for the SMTP envelope-address normalisation. Gmail (and
// any strict RFC 5321 relay) rejects MAIL FROM:<"Display Name
// <bare@addr>"> with a 555 5.5.2 syntax error. The envelope must
// be a bare local@domain even though the message's RFC 5322 From:
// header can carry the display-name format. This file pins
// envelopeAddress() — the helper that bridges the two.
//
// Bug origin: 2026-05-20 user had `from_address: "Assistant
// <bot@vornik.io>"` and every outbound reply died with the 555.
// The chat audit log showed replies the recipient never received.
package email

import "testing"

// TestEnvelopeAddress_BareEmailPassThrough — a plain address
// (already in envelope form) returns unchanged.
func TestEnvelopeAddress_BareEmailPassThrough(t *testing.T) {
	got := envelopeAddress("alice@example.com")
	if got != "alice@example.com" {
		t.Errorf("got %q, want alice@example.com", got)
	}
}

// TestEnvelopeAddress_StripsDisplayName — the production fix.
// "Display Name <user@host>" must collapse to "user@host" for
// the SMTP envelope. Gmail's 555 5.5.2 only stops once this is
// in place.
func TestEnvelopeAddress_StripsDisplayName(t *testing.T) {
	cases := map[string]string{
		"Assistant <bot@vornik.io>":          "bot@vornik.io",
		"\"Vadim Grinco\" <vadim@grinco.eu>": "vadim@grinco.eu",
		"Bot <bot@vornik.io>":                "bot@vornik.io",
		// Whitespace tolerance — RFC parsers accept the
		// surrounding spaces, our envelope must still be tight.
		"  Assistant  <bot@vornik.io>  ": "bot@vornik.io",
	}
	for in, want := range cases {
		got := envelopeAddress(in)
		if got != want {
			t.Errorf("envelopeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEnvelopeAddress_AngleOnly — bracketed form without display
// name. Some MUAs emit this; the parser should still extract the
// bare email.
func TestEnvelopeAddress_AngleOnly(t *testing.T) {
	got := envelopeAddress("<user@example.com>")
	if got != "user@example.com" {
		t.Errorf("got %q, want user@example.com", got)
	}
}

// TestEnvelopeAddress_MalformedFallsBackToInput — defensive: a
// completely malformed address (where net/mail can't parse) must
// not crash; return the input verbatim and let SMTP itself reject
// downstream. Better to surface a real SMTP error than to silently
// rewrite the operator's value into something else.
func TestEnvelopeAddress_MalformedFallsBackToInput(t *testing.T) {
	cases := []string{
		"not an address at all",
		"",
		"@nodomain",
	}
	for _, in := range cases {
		got := envelopeAddress(in)
		if got != in {
			t.Errorf("malformed %q: got %q, want %q (fall-through)", in, got, in)
		}
	}
}
