package forge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// The CI approval gate (LLD 2026-09-08-forge-ci-outcomes-design.md §16).
//
// THE INCIDENT: three APPROVALs were posted on headmatch PR #53 in one
// afternoon, each while a recorded run for that head had FAILED, each dismissing
// the failure differently — "a pre-existing environment issue", a fabricated
// passing mypy output, and finally "in a file this diff does not touch" about
// the only file the diff touched. The third came AFTER the reviewer's prompt was
// given an explicit blocker rule, and took its excuse from the list of
// legitimate overrides that rule supplied.
//
// So every assertion here is on whether PostReview was CALLED. The defect is a
// submission that happened, not a value that was wrong.

// gateOutcomes returns canned CI outcomes for a head.
type gateOutcomes struct {
	outs []*persistence.ForgeCIOutcome
	err  error
}

func (g *gateOutcomes) Upsert(context.Context, *persistence.ForgeCIOutcome) error { return nil }
func (g *gateOutcomes) ListByHeadSHA(context.Context, string, string, string) ([]*persistence.ForgeCIOutcome, error) {
	return g.outs, g.err
}
func (g *gateOutcomes) ClaimComment(context.Context, string, string, int64, time.Time) (bool, error) {
	return true, nil
}
func (g *gateOutcomes) ReleaseComment(context.Context, string, string, int64) error { return nil }
func (g *gateOutcomes) PruneBefore(context.Context, time.Time) (int64, error)       { return 0, nil }

func gateInput(verdict string) executor.SystemStepInput {
	task := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7, HeadSHA: "sha-head"})
	task.ID = "task-cigate"
	return executor.SystemStepInput{
		Task: task,
		Step: &registry.WorkflowStep{Handler: "forge.post_review", GatingReviews: true},
		PrevResult: json.RawMessage(
			`{"message":"the change is fine","review":{"approved":` + verdict + `,"summary":"s"}}`),
	}
}

func redRun() []*persistence.ForgeCIOutcome {
	return []*persistence.ForgeCIOutcome{
		{WorkflowPath: ".github/workflows/pytest.yml", Conclusion: "success"},
		{WorkflowPath: ".github/workflows/typecheck.yml", Conclusion: "failure"},
	}
}

func gateHandler(prov *fakeProvider, outs *gateOutcomes, blocks bool) *PostReviewHandler {
	return NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser()).
		WithCIGate(outs, func(string) bool { return blocks })
}

// THE REGRESSION: an APPROVE over a red pipeline is not submitted.
func TestPostReview_ApprovalOverRedCIIsNotSubmitted(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: redRun()}, true)

	res, err := h.Execute(context.Background(), gateInput("true"))
	if err != nil {
		t.Fatalf("withholding is a success, not an error: %v", err)
	}
	if prov.gotReview.Body != "" || prov.gotNumber != 0 {
		t.Fatal("PostReview was CALLED with CI red — this is the bot approving a change " +
			"its own pipeline rejected")
	}
	// It becomes a COMMENT, not silence: the pull request must say why.
	if prov.commentBody == "" {
		t.Fatal("nothing was posted at all; the withheld approval must still carry the review")
	}
	if !strings.Contains(prov.commentBody, "typecheck.yml") {
		t.Errorf("the comment must NAME the failing run:\n%s", prov.commentBody)
	}
	if !strings.Contains(prov.commentBody, "the change is fine") {
		t.Errorf("the reviewer's prose must survive:\n%s", prov.commentBody)
	}
	var payload map[string]any
	_ = json.Unmarshal(res.Result, &payload)
	if withheld, _ := payload["approval_withheld"].(bool); !withheld {
		t.Errorf("the step result must record the withholding: %v", payload)
	}
}

// Green CI approves for real — the case that must not regress, or the gate has
// simply broken approvals.
func TestPostReview_ApprovalWithGreenCIIsSubmitted(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: []*persistence.ForgeCIOutcome{
		{WorkflowPath: ".github/workflows/pytest.yml", Conclusion: "success"},
	}}, true)

	if _, err := h.Execute(context.Background(), gateInput("true")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotReview.Event != forgeapi.ReviewApprove {
		t.Errorf("event = %q, want approve — green CI must still approve", prov.gotReview.Event)
	}
	if prov.commentBody != "" {
		t.Error("a green approval must be a REVIEW, not downgraded to a comment")
	}
}

// NO RECORDED RUNS IS NOT A FAILURE. "Nothing ingested" and "CI failed" are
// different facts — the same distinction fetch_ci draws between a blank and a
// pass — and a project recording nothing must behave as it did before the gate.
func TestPostReview_NoRecordedRunsDoesNotBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		outs *gateOutcomes
	}{
		{"no rows", &gateOutcomes{}},
		{"store errors", &gateOutcomes{err: errors.New("db down")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			if _, err := gateHandler(prov, tc.outs, true).Execute(context.Background(), gateInput("true")); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if prov.gotReview.Event != forgeapi.ReviewApprove {
				t.Errorf("event = %q, want approve — an unreadable or empty CI record "+
					"must not become a reviewing outage", prov.gotReview.Event)
			}
		})
	}
}

// A REQUEST_CHANGES over red CI stays a REVIEW. Turning a blocking verdict into
// a non-blocking comment is the one way this change could make things worse.
func TestPostReview_RequestChangesOverRedCIStaysAReview(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: redRun()}, true)

	if _, err := h.Execute(context.Background(), gateInput("false")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotReview.Event != forgeapi.ReviewRequestChanges {
		t.Errorf("event = %q, want request_changes — the gate must not weaken a "+
			"blocking verdict", prov.gotReview.Event)
	}
	if prov.commentBody != "" {
		t.Error("a REQUEST_CHANGES must not be downgraded to a comment")
	}
}

// The switch off is today's behaviour exactly.
func TestPostReview_GateDisabledApprovesOverRedCI(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: redRun()}, false)

	if _, err := h.Execute(context.Background(), gateInput("true")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotReview.Event != forgeapi.ReviewApprove {
		t.Errorf("event = %q — block_approval_on_failure: false must behave as before", prov.gotReview.Event)
	}
}

// A handler with no CI store wired — every project without CI ingestion — is
// untouched.
func TestPostReview_NoCIStoreIsUnchanged(t *testing.T) {
	prov := &fakeProvider{}
	h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	if _, err := h.Execute(context.Background(), gateInput("true")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.gotReview.Event != forgeapi.ReviewApprove {
		t.Errorf("event = %q, want approve", prov.gotReview.Event)
	}
}

// A workflow name is repository CI config, so whoever can push a branch chooses
// it. It must not escape the code span it renders in.
func TestPostReview_WithheldNoticeSanitisesTheWorkflowName(t *testing.T) {
	prov := &fakeProvider{}
	h := gateHandler(prov, &gateOutcomes{outs: []*persistence.ForgeCIOutcome{
		{WorkflowPath: "` [click](https://evil.example) `", Conclusion: "failure"},
	}}, true)

	if _, err := h.Execute(context.Background(), gateInput("true")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Count(prov.commentBody, "`")%2 != 0 {
		t.Errorf("unbalanced code spans — a payload backtick escaped:\n%s", prov.commentBody)
	}
	if strings.Contains(sanitiseWorkflowName("a`b"), "`") {
		t.Error("sanitiseWorkflowName left a backtick")
	}
}
