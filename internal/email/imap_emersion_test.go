package email

import (
	"context"
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

// TestNewIMAPClient_ZeroValueSafe — the production adapter is
// constructed without dialling; Close before Connect is a no-op so
// the channel's defer-Close path doesn't blow up if Connect ever
// fails before binding the client.
func TestNewIMAPClient_ZeroValueSafe(t *testing.T) {
	c := NewIMAPClient()
	if c == nil {
		t.Fatal("NewIMAPClient returned nil")
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close before Connect = %v, want nil", err)
	}
}

// TestEmersionIMAPClient_FetchAndMarkBeforeConnect — calling
// FetchUnseen or MarkSeen before Connect must return a sentinel
// error rather than panicking on a nil client pointer.
func TestEmersionIMAPClient_FetchAndMarkBeforeConnect(t *testing.T) {
	c := NewIMAPClient()
	if _, err := c.FetchUnseen(context.Background()); err == nil {
		t.Error("FetchUnseen before Connect must error")
	}
	if err := c.MarkSeen(context.Background(), "1"); err == nil {
		t.Error("MarkSeen before Connect must error")
	}
}

// TestEmersionIMAPClient_Connect_UsesInjectedDialer — cfg.Dialer, when set,
// must actually be threaded into imapclient.DialTLS's Options rather than
// Connect silently building its own default dialer. Proven by a Dialer whose
// Control callback unconditionally rejects: Connect must surface that exact
// rejection, which is only possible if our Dialer (not go-imap's internal
// default) ran. This is the seam internal/integrations relies on to route
// candidate IMAP hosts through the SSRF-guarded DialGuard (regression guard
// for that wiring — see internal/integrations/dialer.go).
func TestEmersionIMAPClient_Connect_UsesInjectedDialer(t *testing.T) {
	sentinel := errors.New("dial guard: sentinel test rejection")
	dialer := &net.Dialer{
		Control: func(_, _ string, _ syscall.RawConn) error {
			return sentinel
		},
	}
	c := NewIMAPClient()
	err := c.Connect(context.Background(), IMAPDialConfig{
		Host:   "127.0.0.1",
		Port:   1,
		Dialer: dialer,
	})
	if err == nil {
		t.Fatal("Connect with a rejecting Dialer must fail")
	}
	if !strings.Contains(err.Error(), sentinel.Error()) {
		t.Errorf("Connect error = %q, want it to wrap the injected Dialer's rejection %q", err.Error(), sentinel.Error())
	}
}

// TestEmersionIMAPClient_DoubleCloseSafe — Close is idempotent.
func TestEmersionIMAPClient_DoubleCloseSafe(t *testing.T) {
	c := NewIMAPClient()
	_ = c.Close()
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
