package workflowhealing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func opsJSON(t *testing.T, ops ...assistantFileOp) string {
	t.Helper()
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// sampleGenome is a REAL parseable genome, not a plausible-looking string.
// The first version of this file used a few lines of markdown; the architect
// path returned no hash for it (ParseWorkflowMarkdown rejected it), and the
// parity assertion below could not run at all. A fixture the production code
// refuses proves nothing about the production code.
const sampleGenome = `---
workflowId: "nightly-digest"
displayName: "Nightly Digest"
version: "1.0"
entrypoint: "plan"
maxStepVisits: 3
maxWallClock: "1h"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "complete"
    on_fail: "failed"
terminals:
  complete:
    status: "success"
  failed:
    status: "failed"
---

# Nightly Digest

## Prompts

### plan

Plan the work and hand off.
`

// TestGenomeFromAssistantProposal_AcceptsExactlyOneWorkflowEdit is the positive
// case, and it asserts the property the whole bridge rests on: the genome comes
// back BYTE-IDENTICAL to the op's content.
//
// Not "equivalent", not "trimmed" — identical. The trial runner hashes these
// bytes and the comparison window compares that hash against the architect
// path's for the same trigger; a normalising bridge would produce a different
// hash for the same logical repair and quietly break both.
func TestGenomeFromAssistantProposal_AcceptsExactlyOneWorkflowEdit(t *testing.T) {
	p := &persistence.ControlPlaneProposal{
		ID:       "cap_1",
		ApplyOps: opsJSON(t, assistantFileOp{Op: "replace", Path: "workflows/nightly-digest.md", Content: sampleGenome}),
	}
	got, err := GenomeFromAssistantProposal(p, "nightly-digest")
	if err != nil {
		t.Fatalf("GenomeFromAssistantProposal: %v", err)
	}
	if got != sampleGenome {
		t.Errorf("the genome was altered in transit.\n got: %q\nwant: %q", got, sampleGenome)
	}
}

// TestGenomeFromAssistantProposal_RefusesRatherThanTrims is the rule §6.3.4
// calls out for mutation. Each of these could be "made to work" by picking an
// op and ignoring the rest, and each would then trial something nobody
// proposed.
func TestGenomeFromAssistantProposal_RefusesRatherThanTrims(t *testing.T) {
	cases := map[string]*persistence.ControlPlaneProposal{
		"two files — the trimming temptation": {ApplyOps: opsJSON(t,
			assistantFileOp{Op: "replace", Path: "workflows/nightly-digest.md", Content: sampleGenome},
			assistantFileOp{Op: "replace", Path: "projects/assistant.yaml", Content: "x: 1"},
		)},
		"the workflow plus a trivial second op": {ApplyOps: opsJSON(t,
			assistantFileOp{Op: "replace", Path: "workflows/nightly-digest.md", Content: sampleGenome},
			assistantFileOp{Op: "create", Path: "workflows/notes.md", Content: "# notes"},
		)},
		"a different workflow": {ApplyOps: opsJSON(t,
			assistantFileOp{Op: "replace", Path: "workflows/other.md", Content: sampleGenome},
		)},
		"a file that merely ends in the right name": {ApplyOps: opsJSON(t,
			assistantFileOp{Op: "replace", Path: "role-library/nightly-digest.md", Content: sampleGenome},
		)},
		"zero ops": {ApplyOps: "[]"},
		"empty content": {ApplyOps: opsJSON(t,
			assistantFileOp{Op: "replace", Path: "workflows/nightly-digest.md", Content: "   "},
		)},
		"unreadable ops":                {ApplyOps: "{not json"},
		"review-only, nothing to apply": {},
	}
	for name, p := range cases {
		got, err := GenomeFromAssistantProposal(p, "nightly-digest")
		if !errors.Is(err, ErrNotASingleWorkflowEdit) {
			t.Errorf("%s: err = %v, want ErrNotASingleWorkflowEdit", name, err)
		}
		if got != "" {
			t.Errorf("%s: returned a genome (%q) alongside a refusal; a caller checking only the "+
				"value would trial it", name, got)
		}
	}
}

// TestGenomeFromAssistantProposal_ReadsTheBackCompatShape — the apply engine
// accepts a single (ApplyTarget, ApplyContent) as one implicit replace, so the
// bridge must read a proposal the same way the applier does. Two readers
// disagreeing about what a proposal contains is the divergence this codebase
// keeps paying for.
func TestGenomeFromAssistantProposal_ReadsTheBackCompatShape(t *testing.T) {
	p := &persistence.ControlPlaneProposal{
		ID: "cap_2", ApplyTarget: "workflows/nightly-digest.md", ApplyContent: sampleGenome,
	}
	got, err := GenomeFromAssistantProposal(p, "nightly-digest")
	if err != nil {
		t.Fatalf("the single-target shape was refused: %v", err)
	}
	if got != sampleGenome {
		t.Errorf("genome = %q, want the content verbatim", got)
	}
}

// TestCandidateFromAssistantProposal_HashesTheSameGenomeTheArchitectWould is
// the comparison window's load-bearing property, asserted directly rather than
// waited for in production: the same genome bytes must produce the same
// CandidateGenomeHash whichever path built the candidate.
//
// If this ever fails, the comparison window would report a disagreement on
// every trigger and the switch could never be justified — and the cause would
// be the bridge, not the models.
func TestCandidateFromAssistantProposal_HashesTheSameGenomeTheArchitectWould(t *testing.T) {
	trigger := &persistence.HealingTrigger{ID: "trg_1", ProjectID: "p", WorkflowID: "nightly-digest"}

	fromArchitect := CandidateFromArchitectProposal(trigger, &persistence.WorkflowProposal{
		ID: "wp_1", WorkflowID: "nightly-digest", ProposalYAML: sampleGenome, Motivation: "latency regression",
	})
	fromAssistant := CandidateFromAssistantProposal(trigger, &persistence.ControlPlaneProposal{
		ID: "cap_1", Rationale: "latency regression",
	}, sampleGenome)

	if fromArchitect == nil || fromAssistant == nil {
		t.Fatal("a builder returned nil for valid input")
	}
	if fromArchitect.CandidateGenomeHash == "" {
		t.Fatal("the architect path produced no hash; this test cannot compare")
	}
	if fromAssistant.CandidateGenomeHash != fromArchitect.CandidateGenomeHash {
		t.Errorf("the two paths hash the same genome differently:\n assistant: %s\n architect: %s\n"+
			"the comparison window would disagree on every trigger, and the bridge would be the cause",
			fromAssistant.CandidateGenomeHash, fromArchitect.CandidateGenomeHash)
	}
	// The candidate must carry the proposal it came from, or an operator
	// reviewing it cannot reach the proposal.
	if fromAssistant.ProposalID != "cap_1" {
		t.Errorf("ProposalID = %q, want the control-plane proposal's id", fromAssistant.ProposalID)
	}
	if fromAssistant.ProposalDiff != sampleGenome {
		t.Error("the candidate's diff is not the genome it was built from")
	}
	if !strings.Contains(fromAssistant.ExpectedEffect, "Assistant") {
		t.Errorf("the candidate does not say which surface proposed it: %q", fromAssistant.ExpectedEffect)
	}
}

// TestCandidateFromAssistantProposal_NilInputs matches the sibling's contract.
func TestCandidateFromAssistantProposal_NilInputs(t *testing.T) {
	trigger := &persistence.HealingTrigger{ID: "trg_1", WorkflowID: "w"}
	if CandidateFromAssistantProposal(nil, &persistence.ControlPlaneProposal{}, sampleGenome) != nil {
		t.Error("a nil trigger produced a candidate")
	}
	if CandidateFromAssistantProposal(trigger, nil, sampleGenome) != nil {
		t.Error("a nil proposal produced a candidate")
	}
	if CandidateFromAssistantProposal(trigger, &persistence.ControlPlaneProposal{}, "") != nil {
		t.Error("an empty genome produced a candidate; it would hash to something the trial " +
			"runner would apply")
	}
}
