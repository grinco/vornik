package workflowhealing

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Operator validation, 2026-09-16. Two candidates sat in trial_passed and were
// about to be promoted. One of them — whc_20260816221154_* against the
// `adaptive` workflow — was generated on 2026-08-16 against a file that has
// been edited TWICE since:
//
//   - its `route` timeout had been raised to 180s; the candidate genome still
//     carried 60s, so promoting it would have CUT the timeout to a third,
//     the exact opposite of the proposal's own stated intent ("increasing to
//     60s", which was true against August's file);
//   - a whole prompt paragraph added 2026-09-10 ("Route on the task prompt and
//     the candidate list alone — do not read files…") was absent from the
//     genome and would have been deleted.
//
// A healing genome is a WHOLE-FILE replacement. Nothing in the system compared
// it against the file it would overwrite: the static trial checks SHAPE, the
// replay trial checks BEHAVIOUR, and neither checks CURRENCY.
// `baseline_genome_hash` — the column that exists precisely for this, and that
// the recipe path already populates — was empty on both candidates, and the
// promoter never read it.
//
// This is tenet 4 in its exact form: a control that cannot distinguish
// "examined and clean" from "never examined" reports the first and means the
// second. The gate said trial_passed, which was true, and said nothing about
// the genome being five workflow-edits stale.
//
// The check is a HASH COMPARISON — deterministic, free, and incapable of the
// judgement error that produced these candidates in the first place. It needs
// no model.

// fakeWorkflowLookup answers "what is live right now" with a parsed workflow.
type fakeWorkflowLookup struct {
	wf      *registry.Workflow
	lookups int
}

func (f *fakeWorkflowLookup) GetWorkflow(string) *registry.Workflow {
	f.lookups++
	return f.wf
}

// liveWorkflow parses a minimal valid WORKFLOW.md so its genome can be hashed
// the same way production hashes it.
func liveWorkflow(t *testing.T, timeout string) *registry.Workflow {
	t.Helper()
	wf, err := parseLiveWorkflow(timeout)
	if err != nil {
		t.Fatalf("parse live workflow: %v", err)
	}
	return wf
}

// mustLiveWorkflow is the same thing where no *testing.T is in scope (shared
// fixtures). A failure here is a broken test fixture, not a test outcome.
func mustLiveWorkflow(timeout string) *registry.Workflow {
	wf, err := parseLiveWorkflow(timeout)
	if err != nil {
		panic("workflowhealing test fixture: " + err.Error())
	}
	return wf
}

func parseLiveWorkflow(timeout string) (*registry.Workflow, error) {
	src := `---
workflowId: "wf_1"
displayName: "Test"
description: "d"
version: "1.0"
entrypoint: "route"
steps:
  route:
    type: "agent"
    role: "lead"
    on_success: "done"
    on_fail: "failed"
    timeout: "` + timeout + `"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
---

# Test

## Prompts

### route

Pick one.
`
	return registry.ParseWorkflowMarkdown([]byte(src), "wf_1.md")
}

func driftPromoter(cr *fakeCandidateRepo, pr *fakeProposalRepo, ap ProposalApplier, look WorkflowLookup) *Promoter {
	return NewPromoter(cr, pr, ap, nil, zerolog.Nop()).WithWorkflowLookup(look)
}

// THE INCIDENT: the live file moved on after the candidate was generated.
func TestPromote_RefusesAGenomeWhoseBaselineNoLongerMatchesProduction(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	// Generated against the 60s file…
	cand.BaselineGenomeHash = GenomeHash(liveWorkflow(t, "60s"))
	cr.put(cand)
	// …but production is now 180s.
	look := &fakeWorkflowLookup{wf: liveWorkflow(t, "180s")}
	ap := &fakeApplier{repo: pr}

	_, err := driftPromoter(cr, pr, ap, look).Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrGenomeDrift) {
		t.Fatalf("err = %v, want ErrGenomeDrift — a whole-file genome generated against a "+
			"superseded file must not silently revert the edits made since", err)
	}
	// The load-bearing assertion: production was NOT touched.
	if ap.calls != 0 {
		t.Errorf("applier ran %d times on a drifted genome; promotion must refuse BEFORE mutating", ap.calls)
	}
	got, _ := cr.Get(context.Background(), "whc_test")
	if got.Status != persistence.HealingCandidateTrialPassed {
		t.Errorf("status = %s; a refused promotion must leave the candidate promotable once "+
			"it is regenerated, not consume it", got.Status)
	}
}

// The matching case: nothing moved, so promotion proceeds exactly as before.
func TestPromote_ProceedsWhenTheBaselineStillMatchesProduction(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	live := liveWorkflow(t, "180s")
	cand.BaselineGenomeHash = GenomeHash(live)
	cr.put(cand)
	ap := &fakeApplier{repo: pr}

	got, err := driftPromoter(cr, pr, ap, &fakeWorkflowLookup{wf: live}).Promote(context.Background(), "whc_test", "op@x")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got.Status != persistence.HealingCandidatePromoted {
		t.Errorf("status = %s, want promoted", got.Status)
	}
	if ap.calls != 1 {
		t.Errorf("applier calls = %d, want 1", ap.calls)
	}
}

// An EMPTY baseline is "never examined", not "examined and clean". Both live
// candidates were in this state, which is why nothing objected.
func TestPromote_RefusesACandidateWithNoRecordedBaseline(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	// The shape both live candidates were in: no baseline ever recorded.
	cand.BaselineGenomeHash = ""
	cr.put(cand)
	ap := &fakeApplier{repo: pr}

	_, err := driftPromoter(cr, pr, ap, &fakeWorkflowLookup{wf: liveWorkflow(t, "180s")}).
		Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrBaselineUnknown) {
		t.Fatalf("err = %v, want ErrBaselineUnknown — a candidate with no recorded baseline "+
			"cannot be shown to be current, and unverifiable must not read as safe", err)
	}
	if ap.calls != 0 {
		t.Errorf("applier ran %d times for an unverifiable candidate", ap.calls)
	}
}

// Fail closed when the check itself is unavailable. An unwired lookup is the
// same epistemic position as an empty baseline: we cannot tell.
func TestPromote_RefusesWhenTheDriftCheckIsNotWired(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	cand.BaselineGenomeHash = "abc123"
	cr.put(cand)
	ap := &fakeApplier{repo: pr}

	_, err := NewPromoter(cr, pr, ap, nil, zerolog.Nop()).Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrDriftCheckUnavailable) {
		t.Fatalf("err = %v, want ErrDriftCheckUnavailable", err)
	}
	if ap.calls != 0 {
		t.Errorf("applier ran %d times with no drift check wired", ap.calls)
	}
}

// A workflow that has been DELETED since the candidate was generated is drift
// of the most complete kind — there is no file left to patch.
func TestPromote_RefusesWhenTheWorkflowNoLongerExists(t *testing.T) {
	cr := newFakeCandidateRepo()
	pr := newFakeProposalRepo()
	pr.put(&persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "wf_1", Status: persistence.WorkflowProposalStatusPending})
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	cand.BaselineGenomeHash = "abc123"
	cr.put(cand)
	ap := &fakeApplier{repo: pr}

	_, err := driftPromoter(cr, pr, ap, &fakeWorkflowLookup{wf: nil}).Promote(context.Background(), "whc_test", "op@x")
	if !errors.Is(err, ErrGenomeDrift) {
		t.Fatalf("err = %v, want ErrGenomeDrift for a vanished workflow", err)
	}
	if ap.calls != 0 {
		t.Errorf("applier ran %d times against a workflow that no longer exists", ap.calls)
	}
}

// Reject must stay usable on a drifted candidate — it is the ONLY disposition
// left for one, so a drift check that also blocked rejection would strand it.
func TestReject_StillWorksOnADriftedCandidate(t *testing.T) {
	cr := newFakeCandidateRepo()
	cand := seedPromoCandidate(t, cr, persistence.HealingCandidateTrialPassed, "wpr_1")
	cand.BaselineGenomeHash = "stale"
	cr.put(cand)

	got, err := NewPromoter(cr, nil, nil, nil, zerolog.Nop()).Reject(context.Background(), "whc_test")
	if err != nil {
		t.Fatalf("Reject on a drifted candidate: %v", err)
	}
	if got.Status != persistence.HealingCandidateRejected {
		t.Errorf("status = %s, want rejected", got.Status)
	}
}
