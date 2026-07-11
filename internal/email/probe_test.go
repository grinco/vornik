package email

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- ProbeIMAP ---

// TestProbeIMAP_Success — Connect succeeds, ProbeIMAP returns nil and
// closes the client.
func TestProbeIMAP_Success(t *testing.T) {
	fake := newFakeIMAP()
	err := ProbeIMAP(context.Background(), fake, IMAPDialConfig{Host: "imap.test", Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("ProbeIMAP() error = %v", err)
	}
	if fake.connectCalls != 1 {
		t.Errorf("connectCalls = %d, want 1", fake.connectCalls)
	}
	if fake.closeCalls != 1 {
		t.Errorf("closeCalls = %d, want 1 (ProbeIMAP must always close)", fake.closeCalls)
	}
}

// TestProbeIMAP_ConnectFailure — Connect's error propagates verbatim and
// the client is still closed.
func TestProbeIMAP_ConnectFailure(t *testing.T) {
	fake := newFakeIMAP()
	fake.connectErr = errors.New("dial TLS imap.test:993: connection refused")
	err := ProbeIMAP(context.Background(), fake, IMAPDialConfig{Host: "imap.test"})
	if err == nil {
		t.Fatal("expected the Connect error to propagate")
	}
	if fake.closeCalls != 1 {
		t.Errorf("closeCalls = %d, want 1 even on Connect failure", fake.closeCalls)
	}
}

// TestIsIMAPAuthFailure_ClassifiesLoginWrap — emersionIMAPClient.Connect
// wraps a login rejection as "IMAP login: %w" (imap_emersion.go) and a dial
// failure as "dial TLS %s: %w". IsIMAPAuthFailure must tell them apart so
// the integrations emailProber can classify OutcomeFail vs OutcomeError.
func TestIsIMAPAuthFailure_ClassifiesLoginWrap(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantAuth bool
	}{
		{"nil", nil, false},
		{"login rejected", errors.New("IMAP login: imap: NO [AUTHENTICATIONFAILED] Invalid credentials"), true},
		{"dial failure", errors.New("dial TLS imap.test:993: connection refused"), false},
		{"select failure", errors.New(`IMAP select "INBOX": imap: NO Mailbox does not exist`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIMAPAuthFailure(tc.err); got != tc.wantAuth {
				t.Errorf("IsIMAPAuthFailure(%v) = %v, want %v", tc.err, got, tc.wantAuth)
			}
		})
	}
}

// --- ProbeSMTP ---

// fakeSMTPServer starts a minimal single-connection SMTP server on
// 127.0.0.1 that advertises AUTH PLAIN and responds to AUTH with the given
// code/message. It does not support STARTTLS (not advertised), which is
// deliberate: net/smtp's PlainAuth only sends credentials over TLS or to
// "127.0.0.1"/"localhost" (auth.go isLocalhost) — using 127.0.0.1 lets the
// test exercise real AUTH PLAIN wire traffic without standing up TLS certs.
func fakeSMTPServer(t *testing.T, authCode int, authMsg string) string {
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
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		write("220 fake.smtp.test ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"):
				write("250-fake.smtp.test greets you")
				write("250 AUTH PLAIN LOGIN")
			case strings.HasPrefix(upper, "AUTH"):
				write(strconv.Itoa(authCode) + " " + authMsg)
			case strings.HasPrefix(upper, "QUIT"):
				write("221 Bye")
				return
			default:
				write("500 unrecognized command")
			}
		}
	}()
	return ln.Addr().String()
}

func dialFuncFor(addr string) SMTPDialFunc {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{}
		return d.DialContext(ctx, network, addr)
	}
}

// TestProbeSMTP_Success — EHLO + AUTH PLAIN with a server that accepts.
func TestProbeSMTP_Success(t *testing.T) {
	addr := fakeSMTPServer(t, 235, "Authentication successful")
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	err := ProbeSMTP(context.Background(), dialFuncFor(addr), SMTPDialConfig{
		Host: host, Port: port, Username: "u@test", Password: "p",
	})
	if err != nil {
		t.Fatalf("ProbeSMTP() error = %v", err)
	}
}

// TestProbeSMTP_AuthFailure — the server rejects AUTH with 535; must
// classify as ErrSMTPAuthFailed.
func TestProbeSMTP_AuthFailure(t *testing.T) {
	addr := fakeSMTPServer(t, 535, "Authentication credentials invalid")
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	err := ProbeSMTP(context.Background(), dialFuncFor(addr), SMTPDialConfig{
		Host: host, Port: port, Username: "u@test", Password: "wrongpass",
	})
	if err == nil {
		t.Fatal("expected an auth failure error")
	}
	if !errors.Is(err, ErrSMTPAuthFailed) {
		t.Errorf("err = %v, want it to wrap ErrSMTPAuthFailed", err)
	}
}

// TestProbeSMTP_ConnectFailure — dial itself fails; must NOT be classified
// as ErrSMTPAuthFailed.
func TestProbeSMTP_ConnectFailure(t *testing.T) {
	dial := func(_ context.Context, _, _ string) (net.Conn, error) {
		return nil, errors.New("dial guard: refused")
	}
	err := ProbeSMTP(context.Background(), dial, SMTPDialConfig{Host: "smtp.test", Port: 587})
	if err == nil {
		t.Fatal("expected the dial error to propagate")
	}
	if errors.Is(err, ErrSMTPAuthFailed) {
		t.Error("a dial failure must not be classified as ErrSMTPAuthFailed")
	}
}

// TestProbeSMTP_Timeout — a server that accepts but never speaks SMTP; the
// context deadline must bound the whole exchange rather than hang forever.
func TestProbeSMTP_Timeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(200 * time.Millisecond) // never greets
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = ProbeSMTP(ctx, dialFuncFor(ln.Addr().String()), SMTPDialConfig{Host: host, Port: port})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestProbeSMTP_NoAuthWhenNoCredentials — a candidate with no username
// skips AUTH entirely (some relays are open/unauthenticated) rather than
// erroring.
func TestProbeSMTP_NoAuthWhenNoCredentials(t *testing.T) {
	addr := fakeSMTPServer(t, 235, "unused")
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	err := ProbeSMTP(context.Background(), dialFuncFor(addr), SMTPDialConfig{Host: host, Port: port})
	if err != nil {
		t.Fatalf("ProbeSMTP() with no credentials should just EHLO, error = %v", err)
	}
}
