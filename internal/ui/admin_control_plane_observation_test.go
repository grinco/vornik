package ui

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// buildCPRowForTest renders one ledger row through the REAL builder, so the test
// pins what the page actually shows rather than a restatement of the rule.
func buildCPRowForTest(t *testing.T, kind, status string) AdminCPRow {
	t.Helper()
	s := &Server{}
	return s.cpLedgerRow(&persistence.ControlPlaneProposal{
		ID: "cpp_test", Kind: kind, Status: status,
		Title: "Tune: high p95 latency", ProposedBy: "tune-detector",
	}, map[string]bool{})
}

// Observations cannot be approved, so the page must not offer Approve.
//
// Reported from the Control Center on 2026-08-12: two tune events, "click
// approve — nothing happens". Reject worked and removed one; the other stayed.
//
// Both were `observation` proposals. ProposalRepository.SetStatus refuses to
// approve those (ErrProposalNotDecidable) because an observation carries no
// change to accept — but the row still rendered an Approve button, because the
// gate was `Status == DRAFT` and nothing else. Clicking it produced
// `?done=error` and the generic "That action could not be completed", which from
// the operator's side is indistinguishable from a dead button.
//
// The domain rule was right. The UI offered an action the domain forbids, and
// then failed to say why.

func TestAdminCPRow_ObservationOffersNoApprove(t *testing.T) {
	row := buildCPRowForTest(t, persistence.ProposalKindObservation, persistence.ProposalStatusDraft)

	if row.CanApprove {
		t.Error("Approve offered on an observation. SetStatus always refuses it " +
			"(ErrProposalNotDecidable), so the button can only ever produce an error — " +
			"which is exactly what was reported as 'nothing happens'.")
	}
	if !row.CanReject {
		t.Error("Reject withdrawn from an observation; dismissing one IS allowed and is " +
			"the only way an operator can clear it from the list")
	}
	if !row.IsObservation {
		t.Error("row not marked as an observation, so the page cannot explain why the " +
			"Approve button is absent")
	}
}

// A real change proposal must keep its Approve button — the fix must not turn a
// too-permissive gate into a too-strict one.
func TestAdminCPRow_ConfigDraftStillApprovable(t *testing.T) {
	row := buildCPRowForTest(t, persistence.ProposalKindConfig, persistence.ProposalStatusDraft)

	if !row.CanApprove {
		t.Error("a DRAFT config proposal lost its Approve button")
	}
	if row.IsObservation {
		t.Error("a config proposal was marked as an observation")
	}
}

// TestCPFlash_NotDecidableExplainsItself: when the refusal does reach an
// operator, it has to say what happened. "That action could not be completed"
// sent the reporter looking for a broken button instead of reading the rule.
func TestCPFlash_NotDecidableExplainsItself(t *testing.T) {
	msg, ok := cpFlashMessages["not-decidable"]
	if !ok {
		t.Fatal("no flash for not-decidable; the refusal falls back to the generic " +
			"'could not be completed', which is what made this look like a UI bug")
	}
	if !strings.Contains(strings.ToLower(msg), "observation") {
		t.Errorf("flash %q should name the reason — that this is an observation", msg)
	}
}
