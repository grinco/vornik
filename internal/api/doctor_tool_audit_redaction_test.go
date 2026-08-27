package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/auditredact"
)

// Finding C of the 2026-08-26 silent-controls audit: the three unredacted-write
// bypasses were legible only in a boot log. A counter answers "how many rows
// since boot" for whoever scrapes; this check answers "is this deployment
// writing plaintext right now" for an operator who has no Prometheus.
func TestDoctorToolAuditRedaction(t *testing.T) {
	cases := []struct {
		name       string
		set        bool
		reason     auditredact.Reason
		action     string
		wantStatus string
		wantIn     string
	}{
		{name: "wired", set: true, reason: auditredact.ReasonNone, action: "redact", wantStatus: "OK", wantIn: "redact"},
		{name: "secrets disabled", set: true, reason: auditredact.ReasonSecretsDisabled, wantStatus: "WARNING", wantIn: "secrets.enabled"},
		{name: "detector failed", set: true, reason: auditredact.ReasonDetectorUnavailable, wantStatus: "ERROR", wantIn: "detector"},
		{name: "no detector", set: true, reason: auditredact.ReasonDetectorNil, wantStatus: "WARNING", wantIn: "no detector"},
		// Never OK: an unevaluated check reporting OK is Finding A, and this
		// check must not reintroduce it on the direct-handler-test path.
		{name: "no snapshot", set: false, wantStatus: "SKIPPED", wantIn: "skipping"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &DoctorHandlers{}
			if tc.set {
				h.SetToolAuditRedaction(tc.reason, tc.action)
			}
			got := h.checkToolAuditRedaction()
			if got.Name != "tool_audit_redaction" {
				t.Errorf("Name = %q", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (message: %s)", got.Status, tc.wantStatus, got.Message)
			}
			if !strings.Contains(got.Message, tc.wantIn) {
				t.Errorf("Message = %q, want it to mention %q", got.Message, tc.wantIn)
			}
		})
	}
}

// The severity gradation this check introduces, asserted so it cannot erode:
// a control the operator ASKED FOR that is not running outranks one they did
// not ask for. ERROR means "this deployment is broken", not "this deployment
// is unusual".
func TestDoctorToolAuditRedactionGradation(t *testing.T) {
	askedForButFailed := &DoctorHandlers{}
	askedForButFailed.SetToolAuditRedaction(auditredact.ReasonDetectorUnavailable, "")
	notAskedFor := &DoctorHandlers{}
	notAskedFor.SetToolAuditRedaction(auditredact.ReasonSecretsDisabled, "")

	if askedForButFailed.checkToolAuditRedaction().Status != "ERROR" {
		t.Error("a control the operator configured but the daemon failed to run must be ERROR")
	}
	if notAskedFor.checkToolAuditRedaction().Status != "WARNING" {
		t.Error("a control the operator turned off must be WARNING, not ERROR")
	}
}
