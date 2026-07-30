package forge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/registry"
)

// Regression tests for G6 finding C (six-surface Art 50 trace, re-walked
// 2026-07-30).
//
// Finding A gave forge.post_review the Art 50(1) notice. The OTHER publication
// sink in the same provider interface — forge.open_change_request — was left
// out: it passed bodyForJob straight through, so a pull request opened
// autonomously carried "_Opened automatically by vornik…_" and nothing else.
// That is an automation trailer, not a disclosure: it does not say the text was
// written by an AI system and it carries no transparency URL.
//
// The surface reaches a natural person on the same terms that closed finding A —
// the developer who reviews and un-drafts the PR, plus anyone reading the PR
// list where the repository is public (grinco/vornik) — and it reaches them
// through the forge abstraction, outside the dispatcher's channel chokepoint.
//
// What made it a finding rather than a judgement call: two sinks in ONE
// abstraction disclosing differently. Art 50(1)'s "obvious to a reasonably
// well-informed person" carve-out was arguably available here, but it was
// equally available for finding A and was not relied on there, so the asymmetry
// rested on nothing an auditor could be shown.
//
// Trace row: docs/legal/editorial-responsibility.md §Part 2 rows 2, 3 and 6.

// jobForCR is a minimal issue-origin forge job for the disclosure tests.
func jobForCR() forgeapi.ForgeJob {
	return forgeapi.ForgeJob{Repo: "o/r", Number: 7, Labels: []string{"bug"}, Body: "the thing is broken"}
}

func TestOpenChangeRequest_G6C_BodyCarriesTheAIDisclosure(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/15"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil, realDiscloser())

	if _, err := h.Execute(context.Background(), executor.SystemStepInput{Task: taskWithJob(jobForCR())}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := prov.gotSpec.Body
	if !strings.Contains(got, noticeText()) {
		t.Fatalf("PR body carries no AI disclosure:\n%s", got)
	}
	// The notice must be the LAST thing in the body, behind the same rule
	// post_review uses. Art 50(5) wants it clear and distinguishable; matching
	// the sibling sink means the perimeter is one rule, not two conventions.
	if want := wantDisclosedBody(bodyForJob(jobForCR())); got != want {
		t.Errorf("body =\n%q\nwant\n%q", got, want)
	}
	// The disclosure must not have displaced what the PR already said.
	if !strings.Contains(got, "Closes #7.") || !strings.Contains(got, "the thing is broken") {
		t.Errorf("disclosure displaced the PR body:\n%s", got)
	}
}

func TestOpenChangeRequest_G6C_BacklogOriginAlsoDiscloses(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/16"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil, realDiscloser())

	// The backlog path is the AUTONOMOUS one — no human filed an issue, so it is
	// the path where a reader is least likely to know what authored the change.
	job := forgeapi.ForgeJob{Repo: "o/r", Number: 3, Kind: "backlog", Body: "tighten the retention sweeper"}
	if _, err := h.Execute(context.Background(), executor.SystemStepInput{Task: taskWithJob(job)}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(prov.gotSpec.Body, noticeText()) {
		t.Fatalf("backlog-origin PR body carries no AI disclosure:\n%s", prov.gotSpec.Body)
	}
}

// A nil discloser must refuse BEFORE any remote side effect. Failing after the
// push would leave a branch on a public forge that no PR explains, so the check
// belongs with the other dependency checks at the top of Execute — not next to
// the OpenChangeRequest call that consumes the notice.
func TestOpenChangeRequest_G6C_NilDiscloserRefusesAndPublishesNothing(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/15"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil, nil)

	_, err := h.Execute(context.Background(), executor.SystemStepInput{Task: taskWithJob(jobForCR())})
	if err == nil {
		t.Fatal("nil discloser must refuse, not publish an undisclosed pull request")
	}
	if !strings.Contains(err.Error(), "disclosure") {
		t.Errorf("refusal should name the missing dependency: %v", err)
	}
	if prov.pushedBranch != "" {
		t.Errorf("refused step pushed branch %q — the refusal must precede every remote side effect", prov.pushedBranch)
	}
	if prov.gotSpec.Head != "" {
		t.Errorf("refused step opened a change request: %+v", prov.gotSpec)
	}
}

// The no_change short-circuit and the taint park are both non-publishing exits.
// Neither may be turned into a failure by the disclosure dependency — a wired
// deployment must still be able to skip and park.
func TestOpenChangeRequest_G6C_DisclosureDoesNotDisturbNonPublishingExits(t *testing.T) {
	if _, err := execLookGit(); err != nil {
		t.Skip("git not installed")
	}
	// A repo whose HEAD is exactly its base — the no_change short-circuit.
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
	gitRun(t, dir, "branch", "-M", "main")
	sha := gitOut(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", sha)

	prov := &fakeProvider{}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: dir, sha: sha}, nil, nil, realDiscloser())

	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7, DefaultBranch: "main"}),
	})
	if err != nil {
		t.Fatalf("no_change exit should not error: %v", err)
	}
	var out openResult
	if uerr := json.Unmarshal(res.Result, &out); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if out.State != "no_change" {
		t.Fatalf("state=%q, want no_change", out.State)
	}
	if prov.pushedBranch != "" {
		t.Errorf("no_change exit pushed %q", prov.pushedBranch)
	}

	// Taint park: reached only when a discloser IS wired, so the park signal must
	// survive the new dependency unchanged.
	gate := &fakeTaintGate{sig: &executor.TaintReviewSignal{State: executor.TaintReviewState, SourceCount: 1, ShownCount: 1}}
	hp := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, gate, realDiscloser())
	pres, perr := hp.Execute(context.Background(), executor.SystemStepInput{
		Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7}),
		Step: &registry.WorkflowStep{Handler: "forge.open_change_request"},
	})
	if perr != nil {
		t.Fatalf("taint park should not error: %v", perr)
	}
	var sig executor.TaintReviewSignal
	if uerr := json.Unmarshal(pres.Result, &sig); uerr != nil {
		t.Fatalf("unmarshal park: %v", uerr)
	}
	if sig.State != executor.TaintReviewState {
		t.Errorf("park state=%q", sig.State)
	}
	if prov.gotSpec.Head != "" {
		t.Errorf("parked step opened a change request: %+v", prov.gotSpec)
	}
}

// Both forge publication sinks must reach GitHub with the SAME notice. This is
// the assertion finding C existed for: it fails the moment one sink is given a
// bespoke disclosure or has its own dropped.
func TestOpenChangeRequest_G6C_BothForgeSinksDiscloseIdentically(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/15"}
	cr := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil, realDiscloser())
	if _, err := cr.Execute(context.Background(), executor.SystemStepInput{Task: taskWithJob(jobForCR())}); err != nil {
		t.Fatalf("open: %v", err)
	}

	rv := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
	if _, err := rv.Execute(context.Background(), executor.SystemStepInput{
		Task:       taskWithJob(jobForCR()),
		Step:       &registry.WorkflowStep{Handler: "forge.post_review"},
		PrevResult: json.RawMessage(`{"body":"looks good"}`),
	}); err != nil {
		t.Fatalf("review: %v", err)
	}

	if !strings.HasSuffix(prov.gotSpec.Body, discloseSuffix+noticeText()) {
		t.Errorf("PR body does not end with the shared notice:\n%s", prov.gotSpec.Body)
	}
	if !strings.HasSuffix(prov.gotReview.Body, discloseSuffix+noticeText()) {
		t.Errorf("review body does not end with the shared notice:\n%s", prov.gotReview.Body)
	}
}
