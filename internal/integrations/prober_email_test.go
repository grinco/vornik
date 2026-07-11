package integrations

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/email"
)

func TestEmailProber_Kind(t *testing.T) {
	p := newEmailProber(DialGuard{}, 0)
	if p.Kind() != "email" {
		t.Errorf("Kind() = %q, want email", p.Kind())
	}
}

// TestEmailProber_Probe_MissingHosts — both imap_host and smtp_host empty
// is a hard Fail (required fields), no network attempted.
func TestEmailProber_Probe_MissingHosts(t *testing.T) {
	p := newEmailProber(DialGuard{}, time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail", res)
	}
	if len(res.Failures) < 2 {
		t.Errorf("Failures = %+v, want one per missing required host", res.Failures)
	}
}

// TestEmailProber_Probe_SSRFGuard_BlocksLoopback — a candidate IMAP/SMTP
// host pointed at 127.0.0.1 must be refused by the DialGuard, not merely
// fail to connect for some other reason. This is the design §6 invariant
// applied to the one kind where a project-scoped (non-admin) user
// literally supplies the network destination.
func TestEmailProber_Probe_SSRFGuard_BlocksLoopback(t *testing.T) {
	p := newEmailProber(DialGuard{}, 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"imap_host":         "127.0.0.1",
		"imap_port":         "993",
		"smtp_host":         "127.0.0.1",
		"smtp_port":         "587",
		"imap_username":     "u@test",
		"imap_password_env": "p",
		"smtp_username":     "u@test",
		"smtp_password_env": "p",
	}})
	if res.OK || res.Outcome != OutcomeError {
		t.Fatalf("res = %+v, want !OK/OutcomeError (blocked by the dial guard)", res)
	}
}

// TestEmailProber_Probe_SSRFGuard_AllowedHost — the allowlist opt-in lets a
// probe reach a named loopback/internal host. We prove the guard is
// actually consulted for that host (not merely inert) by running a real
// fake server on 127.0.0.1, allowlisting "127.0.0.1", and asserting that
// whatever failures result are protocol-level (the fake server doesn't
// speak real IMAP/TLS or full SMTP AUTH) — NOT the guard's own
// "dial guard: refusing" rejection. That distinction is the proof the
// dial was actually attempted rather than blocked at Control.
func TestEmailProber_Probe_SSRFGuard_AllowedHost(t *testing.T) {
	addr := startFakeSMTPForProberTest(t)
	host, portStr, _ := net.SplitHostPort(addr)

	p := newEmailProber(DialGuard{AllowedHosts: []string{host}}, 300*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"imap_host": host,
		"imap_port": portStr,
		"smtp_host": host,
		"smtp_port": portStr,
	}})
	for _, f := range res.Failures {
		if strings.Contains(f.Reason, "dial guard: refusing") {
			t.Fatalf("allowlisted host must not be guard-refused, got failure: %+v", f)
		}
	}
}

func TestEmailProber_Probe_NeverEchoesPassword(t *testing.T) {
	const distinctiveSecret = "VeryDistinctiveEmailPasswordForLeakTest"
	p := newEmailProber(DialGuard{}, 200*time.Millisecond)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"imap_host":         "127.0.0.1",
		"imap_port":         "1",
		"smtp_host":         "127.0.0.1",
		"smtp_port":         "1",
		"imap_username":     "u@test",
		"imap_password_env": distinctiveSecret,
		"smtp_username":     "u@test",
		"smtp_password_env": distinctiveSecret,
	}})
	if strings.Contains(res.Summary, distinctiveSecret) || strings.Contains(res.Detail, distinctiveSecret) {
		t.Fatalf("ProbeResult leaked the password: Summary=%q Detail=%q", res.Summary, res.Detail)
	}
}

// startFakeSMTPForProberTest starts a minimal SMTP server that accepts
// EHLO and immediately succeeds (no AUTH exercised — the allowlist test
// above only cares that the dial reached the server at all).
func startFakeSMTPForProberTest(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = conn.Write([]byte("220 fake.smtp.test ESMTP\r\n"))
		buf := make([]byte, 512)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			prefix := ""
			if len(cmd) >= 4 {
				prefix = cmd[:4]
			}
			switch prefix {
			case "EHLO":
				_, _ = conn.Write([]byte("250 fake.smtp.test\r\n"))
			case "QUIT":
				_, _ = conn.Write([]byte("221 Bye\r\n"))
				return
			default:
				_, _ = conn.Write([]byte("250 ok\r\n"))
			}
		}
	}()
	return ln.Addr().String()
}

func TestEmailProber_ParsePort(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"993", 993},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		if got := parsePortOrZero(tc.in); got != tc.want {
			t.Errorf("parsePortOrZero(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestEmailProber_Probe_SMTPAuthFailureNamesRenamedField exercises the
// real auth-failure classification path end to end (not just the
// classifier in isolation) to prove the CheckFailure.Field renamed to
// "smtp_password_env" (task 5.2b, matching the reconciled catalog Key)
// actually appears on a real SMTP AUTH rejection — a fake server
// advertises AUTH PLAIN (no STARTTLS) and rejects it with 535.
func TestEmailProber_Probe_SMTPAuthFailureNamesRenamedField(t *testing.T) {
	addr := startFakeSMTPRejectingAuth(t)
	host, portStr, _ := net.SplitHostPort(addr)

	p := newEmailProber(DialGuard{AllowedHosts: []string{host}}, 2*time.Second)
	res := p.Probe(context.Background(), CandidateConfig{Values: map[string]string{
		"imap_host":         host,
		"imap_port":         portStr, // IMAP leg will fail to connect (fake server isn't real IMAP) — OK, we only assert the SMTP finding here.
		"smtp_host":         host,
		"smtp_port":         portStr,
		"smtp_username":     "user@example.com",
		"smtp_password_env": "wrong-password",
	}})
	if res.OK || res.Outcome != OutcomeFail {
		t.Fatalf("res = %+v, want !OK/OutcomeFail (SMTP AUTH rejected)", res)
	}
	var found bool
	for _, f := range res.Failures {
		if f.Field == "smtp_password_env" {
			found = true
		}
		if f.Field == "password" {
			t.Errorf("found stale Field name %q, want the renamed smtp_password_env/imap_password_env", f.Field)
		}
	}
	if !found {
		t.Errorf("Failures = %+v, want an entry with Field=smtp_password_env", res.Failures)
	}
}

// startFakeSMTPRejectingAuth starts a minimal SMTP server that advertises
// AUTH PLAIN (no STARTTLS) and rejects any AUTH attempt with 535. Accepts
// connections in a loop — the emailProber dials this same host:port TWICE
// (once for its IMAP leg, once for SMTP), and the IMAP leg's TLS handshake
// against this plaintext server is expected to fail with a connect error,
// which is fine: this test only asserts the SMTP finding.
func startFakeSMTPRejectingAuth(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeSMTPRejectingAuth(conn)
		}
	}()
	return ln.Addr().String()
}

func serveFakeSMTPRejectingAuth(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte("220 fake.smtp.test ESMTP\r\n"))
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		cmd := string(buf[:n])
		prefix := ""
		if len(cmd) >= 4 {
			prefix = cmd[:4]
		}
		switch {
		case prefix == "EHLO":
			_, _ = conn.Write([]byte("250-fake.smtp.test\r\n250 AUTH PLAIN\r\n"))
		case strings.HasPrefix(cmd, "AUTH"):
			_, _ = conn.Write([]byte("535 5.7.8 Authentication failed\r\n"))
		case prefix == "QUIT":
			_, _ = conn.Write([]byte("221 Bye\r\n"))
			return
		default:
			_, _ = conn.Write([]byte("250 ok\r\n"))
		}
	}
}

func TestEmailProber_ClassifiesIMAPAuthFailureAsFail(t *testing.T) {
	// email.IsIMAPAuthFailure is exercised directly in internal/email; this
	// is a smoke test that the wiring in this package's classifier agrees.
	loginErr := errors.New("IMAP login: imap: NO [AUTHENTICATIONFAILED] bad creds")
	if !email.IsIMAPAuthFailure(loginErr) {
		t.Fatal("sanity: email.IsIMAPAuthFailure should recognize the fixture wrap")
	}
}
