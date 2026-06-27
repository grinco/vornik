package email

import (
	"context"
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

// TestEmersionIMAPClient_DoubleCloseSafe — Close is idempotent.
func TestEmersionIMAPClient_DoubleCloseSafe(t *testing.T) {
	c := NewIMAPClient()
	_ = c.Close()
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
