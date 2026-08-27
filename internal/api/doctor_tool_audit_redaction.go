package api

import (
	"fmt"

	"vornik.io/vornik/internal/auditredact"
)

// checkToolAuditRedaction reports whether tool-audit rows are scanned for
// secrets before they are persisted, and when they are not, WHY.
//
// Three states let a row reach tool_audit_log unscanned: secrets.enabled=false,
// a detector that fails to construct, and a decorator built with no detector.
// All three are deliberate; until 2026-08-27 all three were legible only in a
// boot log, so a deployment that had been writing raw rows for a month looked
// identical to one that had never had a secret to redact (Finding C,
// docs/audits/2026-08-26-silent-controls-audit.md).
//
// The counter (vornik_tool_audit_rows_total) answers "how many rows since
// boot" for whoever scrapes. This answers "is this deployment writing plaintext
// right now" for an operator who has no Prometheus — which is the question the
// finding is actually about. Replacing one unread surface with one unscraped
// surface would not close it.
//
// SEVERITY GRADATION, stated once so it does not erode. A control the operator
// ASKED FOR that is not running (detector_unavailable) is ERROR: the daemon is
// not honouring the config it was given. A control the operator did not ask for
// (secrets_disabled, detector_nil) is WARNING: a deployment choice, reported so
// it is visible, not so it fails a gate. And a check that could not evaluate
// anything is SKIPPED, never OK — that is Finding A, and this check must not
// reintroduce it. See https://docs.vornik.io
// and 2026-08-27-fail-open-census-and-coverage-denominators-design.md D4.
func (h *DoctorHandlers) checkToolAuditRedaction() DoctorCheck {
	const name = "tool_audit_redaction"

	if !h.toolAuditRedactionKnown {
		// The container never wired the snapshot — direct handler tests. Not
		// evaluated, so not OK.
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no redaction snapshot captured, skipping"}
	}

	switch h.toolAuditRedactionReason {
	case auditredact.ReasonNone:
		action := h.toolAuditRedactionAction
		if action == "" {
			action = "redact"
		}
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("tool-audit rows are scanned before persist (action=%s)", action),
		}
	case auditredact.ReasonSecretsDisabled:
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: "secret scanning is off (secrets.enabled=false), so tool_audit_log rows are " +
				"persisted unscanned and may hold plaintext credentials at rest; the rows this " +
				"lets through are counted on vornik_tool_audit_rows_total{status=\"skipped\"}",
		}
	case auditredact.ReasonDetectorUnavailable:
		return DoctorCheck{
			Name:   name,
			Status: "ERROR",
			Message: "secrets.enabled=true but the secret detector failed to construct, so tool-audit " +
				"rows are persisted unscanned — the daemon is not running a control this deployment " +
				"asked for; check secrets.patterns.custom for an invalid regex in the daemon log",
		}
	default:
		return DoctorCheck{
			Name:   name,
			Status: "WARNING",
			Message: fmt.Sprintf("tool-audit rows are persisted with no detector (reason=%s); expected on "+
				"CE and dev deployments, and every such row is counted on "+
				"vornik_tool_audit_rows_total{status=\"skipped\"}", h.toolAuditRedactionReason),
		}
	}
}

// SetToolAuditRedaction records how the container wired the tool-audit
// redaction seam. Called at boot beside the other doctor snapshots; without it
// the check reports SKIPPED rather than guessing.
func (h *DoctorHandlers) SetToolAuditRedaction(reason auditredact.Reason, action string) {
	h.toolAuditRedactionKnown = true
	h.toolAuditRedactionReason = reason
	h.toolAuditRedactionAction = action
}
