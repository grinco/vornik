package email

import (
	"context"
	"testing"
)

// TestHeaderAuth_StrictRejectsForgedAuthResults is the regression for
// the 2026-06-04 bug sweep: HeaderAuthVerifier consulted every
// Authentication-Results / Received-SPF header on a message regardless
// of which server stamped it. Under strict policy an attacker could
// embed a forged `Authentication-Results: <anything>; dkim=pass` (or
// `Received-SPF: pass`) inside the message they send and satisfy the
// pass requirement. With TrustedServerIDs configured (RFC 8601 §5),
// only the trusted relay's A-R header counts.
func TestHeaderAuth_StrictRejectsForgedAuthResults(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"mx.example.com"},
	}

	// Attacker-injected A-R header from an untrusted authserv-id
	// claiming dkim=pass, plus a forged Received-SPF: pass. No header
	// from the trusted relay.
	err := v.Verify(context.Background(), ParsedMessage{
		From: "spoofer@evil.test",
		AuthResults: []string{
			"attacker.evil.test; dkim=pass header.d=evil.test; spf=pass",
		},
		ReceivedSPF: []string{"pass (mailfrom: evil.test) client-ip=1.2.3.4"},
	})
	if err == nil {
		t.Fatal("strict policy must reject a forged pass from an untrusted authserv-id")
	}
}

// TestHeaderAuth_StrictAdmitsTrustedAuthResults — the trusted relay's
// stamp is honoured, so legitimate mail still passes.
func TestHeaderAuth_StrictAdmitsTrustedAuthResults(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"mx.example.com"},
	}
	err := v.Verify(context.Background(), ParsedMessage{
		From: "alice@example.com",
		AuthResults: []string{
			"attacker.evil.test; dkim=pass header.d=evil.test", // ignored
			"mx.example.com; spf=pass smtp.mailfrom=alice@example.com",
		},
	})
	if err != nil {
		t.Fatalf("trusted relay's spf=pass must admit, got %v", err)
	}
}

// TestHeaderAuth_TrustedFilterStillHonoursForgedFail — defense stays
// fail-closed: even an untrusted/forged explicit fail still rejects
// (an attacker gains nothing by injecting a fail, and a real relay
// fail must never be masked).
func TestHeaderAuth_TrustedFilterStillHonoursForgedFail(t *testing.T) {
	v := HeaderAuthVerifier{
		Policy:           AuthPolicyStrict,
		TrustedServerIDs: []string{"mx.example.com"},
	}
	// Trusted relay says pass, but a Received-SPF fail is present.
	err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"mx.example.com; dkim=pass header.d=example.com"},
		ReceivedSPF: []string{"fail (mailfrom: example.com)"},
	})
	if err == nil {
		t.Fatal("an explicit SPF fail must reject even with a trusted dkim=pass")
	}
}

// TestHeaderAuth_NoTrustedSetKeepsLegacyBehaviour — when no trusted
// servers are configured, every A-R header is consulted (back-compat).
func TestHeaderAuth_NoTrustedSetKeepsLegacyBehaviour(t *testing.T) {
	v := HeaderAuthVerifier{Policy: AuthPolicyStrict} // no TrustedServerIDs
	err := v.Verify(context.Background(), ParsedMessage{
		From:        "alice@example.com",
		AuthResults: []string{"any.relay.test; spf=pass smtp.mailfrom=alice@example.com"},
	})
	if err != nil {
		t.Fatalf("legacy behaviour: any relay's pass admits when no trust set is configured, got %v", err)
	}
}
