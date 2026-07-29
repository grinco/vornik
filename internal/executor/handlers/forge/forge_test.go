package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// --- fakes ---

type fakeProvider struct {
	pushErr, openErr, reviewErr, diffErr error
	openURL                              string
	diff                                 []byte

	pushedDir, pushedBranch, pushedSha string
	gotSpec                            forgeapi.ChangeRequestSpec
	gotReview                          forgeapi.ReviewSpec
	gotRepo                            string
	gotNumber                          int
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) ClassifyEvent(http.Header, []byte) (forgeapi.ForgeJob, bool) {
	return forgeapi.ForgeJob{}, false
}
func (f *fakeProvider) FetchDiff(context.Context, string, int) ([]byte, error) {
	return f.diff, f.diffErr
}
func (f *fakeProvider) PushBranch(_ context.Context, dir, _ /*repo*/, branch, sha string) error {
	f.pushedDir, f.pushedBranch, f.pushedSha = dir, branch, sha
	return f.pushErr
}
func (f *fakeProvider) OpenChangeRequest(_ context.Context, s forgeapi.ChangeRequestSpec) (string, error) {
	f.gotSpec = s
	return f.openURL, f.openErr
}
func (f *fakeProvider) PostReview(_ context.Context, repo string, number int, r forgeapi.ReviewSpec) error {
	f.gotRepo, f.gotNumber, f.gotReview = repo, number, r
	return f.reviewErr
}
func (f *fakeProvider) VerifyPushAccess(context.Context) error { return nil }

type fakeResolver struct {
	p   forgeapi.ForgeProvider
	err error
}

func (r fakeResolver) ForgeProvider(context.Context, string) (forgeapi.ForgeProvider, error) {
	return r.p, r.err
}

type fakeSource struct {
	dir, sha string
	err      error
}

func (s fakeSource) PublishSource(context.Context, *persistence.Task) (string, string, error) {
	return s.dir, s.sha, s.err
}

func taskWithJob(j forgeapi.ForgeJob) *persistence.Task {
	pl, _ := json.Marshal(map[string]any{"context": map[string]any{"forge_job": j}})
	return &persistence.Task{ProjectID: "proj-1", Payload: pl}
}

// --- helper tests ---

func TestForgeJobFromTask(t *testing.T) {
	if _, err := forgeJobFromTask(nil, "h"); err == nil {
		t.Error("nil task should error")
	}
	if _, err := forgeJobFromTask(&persistence.Task{}, "h"); err == nil {
		t.Error("no payload should error (no forge_job)")
	}
	if _, err := forgeJobFromTask(taskWithJob(forgeapi.ForgeJob{Repo: "o/r"}), "h"); err == nil {
		t.Error("missing number should error")
	}
	j, err := forgeJobFromTask(taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 5}), "h")
	if err != nil || j.Number != 5 {
		t.Fatalf("valid job: %+v err=%v", j, err)
	}
}

func TestBranchAndTitle(t *testing.T) {
	bug := forgeapi.ForgeJob{Number: 7, Labels: []string{"bug"}}
	feat := forgeapi.ForgeJob{Number: 8, Labels: []string{"enhancement"}}
	if branchForJob(bug) != "fix/issue-7" {
		t.Errorf("bug branch=%s", branchForJob(bug))
	}
	if branchForJob(feat) != "feat/issue-8" {
		t.Errorf("feat branch=%s", branchForJob(feat))
	}
	if isFeature(bug) || !isFeature(feat) {
		t.Error("isFeature classification wrong")
	}
	if titleForJob(bug) != "Fix #7" || titleForJob(feat) != "Implement #8" {
		t.Errorf("titles: %q %q", titleForJob(bug), titleForJob(feat))
	}
	if bodyForJob(bug) == "" {
		t.Error("body should not be empty")
	}
}

// --- open_change_request ---

func TestOpenChangeRequest_HappyPath(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/15"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/work/proj", sha: "deadbeef"}, nil, nil)
	if h.Name() != "forge.open_change_request" {
		t.Fatalf("name=%s", h.Name())
	}
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7, Labels: []string{"enhancement"}, DefaultBranch: "trunk"})}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if prov.pushedDir != "/work/proj" || prov.pushedSha != "deadbeef" || prov.pushedBranch != "feat/issue-7" {
		t.Errorf("push args dir=%s sha=%s branch=%s", prov.pushedDir, prov.pushedSha, prov.pushedBranch)
	}
	if prov.gotSpec.Base != "trunk" || prov.gotSpec.Head != "feat/issue-7" || !prov.gotSpec.Draft {
		t.Errorf("spec=%+v (feature should be draft, base from job)", prov.gotSpec)
	}
	var out openResult
	if err := json.Unmarshal(res.Result, &out); err != nil || out.CRURL != "https://forge/o/r/pull/15" || out.Branch != "feat/issue-7" {
		t.Errorf("result=%s err=%v", res.Result, err)
	}
}

func TestOpenChangeRequest_DefaultBaseFallback(t *testing.T) {
	prov := &fakeProvider{openURL: "u"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, nil)
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 1, Labels: []string{"bug"}})}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if prov.gotSpec.Base != "main" {
		t.Errorf("empty default branch should fall back to main, got %q", prov.gotSpec.Base)
	}
	if !prov.gotSpec.Draft {
		t.Error("automated PRs must always open as draft (incl. bugs)")
	}
}

func TestOpenChangeRequest_Errors(t *testing.T) {
	good := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 1})
	ctx := context.Background()

	// missing deps
	if _, err := (&OpenChangeRequestHandler{}).Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("missing deps should error")
	}
	// missing job
	h := NewOpenChangeRequestHandler(fakeResolver{p: &fakeProvider{}}, fakeSource{}, nil, nil)
	if _, err := h.Execute(ctx, executor.SystemStepInput{Task: &persistence.Task{}}); err == nil {
		t.Error("missing forge job should error")
	}
	// resolver error
	hr := NewOpenChangeRequestHandler(fakeResolver{err: errors.New("no provider")}, fakeSource{}, nil, nil)
	if _, err := hr.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("resolver error should propagate")
	}
	// source error
	hs := NewOpenChangeRequestHandler(fakeResolver{p: &fakeProvider{}}, fakeSource{err: errors.New("no worktree")}, nil, nil)
	if _, err := hs.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("source error should propagate")
	}
	// push error
	hp := NewOpenChangeRequestHandler(fakeResolver{p: &fakeProvider{pushErr: errors.New("push boom")}}, fakeSource{dir: "/d", sha: "s"}, nil, nil)
	if _, err := hp.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("push error should propagate")
	}
	// open error
	ho := NewOpenChangeRequestHandler(fakeResolver{p: &fakeProvider{openErr: errors.New("open boom")}}, fakeSource{dir: "/d", sha: "s"}, nil, nil)
	if _, err := ho.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("open error should propagate")
	}
}

// fakeTaintGate returns a preset review signal (or nil for "proceed") and an
// optional error (to exercise the M5 fail-closed path).
type fakeTaintGate struct {
	sig    *executor.TaintReviewSignal
	err    error
	called bool
}

func (g *fakeTaintGate) ReviewForgeWrite(_ context.Context, _, _ string) (*executor.TaintReviewSignal, error) {
	g.called = true
	return g.sig, g.err
}

// TestOpenChangeRequest_TaintGate_ErrorFailsClosed verifies M5: a reviewer error
// parks (fail-closed) and NEVER pushes the branch, even with a nil signal.
func TestOpenChangeRequest_TaintGate_ErrorFailsClosed(t *testing.T) {
	prov := &fakeProvider{openURL: "u"}
	gate := &fakeTaintGate{sig: nil, err: errors.New("reviewer boom")}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, gate)
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7})}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute err=%v", err)
	}
	if _, ok := executor.AsTaintReview(res.Result); !ok {
		t.Fatalf("a reviewer error must fail closed to a taint-review park, got %s", res.Result)
	}
	if prov.pushedBranch != "" {
		t.Fatalf("branch must NOT be pushed on a reviewer error (fail-closed, M5)")
	}
}

// TestOpenChangeRequest_TaintGate_Parks verifies that when the taint gate
// returns a review signal, Execute returns it as the step result (a taint-review
// park) and NEVER pushes the branch (taint-lineage-tracking §4.5).
func TestOpenChangeRequest_TaintGate_Parks(t *testing.T) {
	prov := &fakeProvider{openURL: "u"}
	gate := &fakeTaintGate{sig: &executor.TaintReviewSignal{
		State: executor.TaintReviewState, SourceSetHash: "abc", SourceCount: 2, ShownCount: 2,
	}}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, gate)
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3})}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute err=%v", err)
	}
	if !gate.called {
		t.Fatalf("taint gate was not consulted")
	}
	sig, ok := executor.AsTaintReview(res.Result)
	if !ok || sig.SourceSetHash != "abc" {
		t.Fatalf("result is not a taint-review signal: %s", res.Result)
	}
	if prov.pushedBranch != "" {
		t.Fatalf("branch must NOT be pushed when parking for taint review (pushed %q)", prov.pushedBranch)
	}
}

// TestOpenChangeRequest_TaintGate_ProceedsWhenNil verifies a nil signal (off/
// advisory / no taint) lets the write proceed to push+open normally.
func TestOpenChangeRequest_TaintGate_ProceedsWhenNil(t *testing.T) {
	prov := &fakeProvider{openURL: "https://forge/o/r/pull/9"}
	gate := &fakeTaintGate{sig: nil}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: "/d", sha: "s"}, nil, gate)
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})}
	if _, err := h.Execute(context.Background(), in); err != nil {
		t.Fatalf("Execute err=%v", err)
	}
	if !gate.called {
		t.Fatalf("taint gate was not consulted")
	}
	if prov.pushedBranch == "" {
		t.Fatalf("branch should be pushed when the gate returns nil")
	}
}

// --- post_review ---

func TestPostReview_HappyPathAndEventMapping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prev      string
		gating    bool
		wantBody  string
		wantEvent forgeapi.ReviewEvent
	}{
		// Without gating every review is a non-gating COMMENT, even an explicit
		// approve/request_changes — the PR is never gated unless opted in.
		{"explicit approve, no gating", `{"body":"looks good","event":"approve"}`, false, "looks good", forgeapi.ReviewComment},
		{"explicit request_changes, no gating", `{"result":"needs work","event":"request_changes"}`, false, "needs work", forgeapi.ReviewComment},
		{"plain note, no gating", `{"output":"a note"}`, false, "a note", forgeapi.ReviewComment},
		{"prose, no gating", `{"message":"agent review prose"}`, false, "agent review prose", forgeapi.ReviewComment},
		// With gating the explicit event drives a real forge review state.
		{"explicit approve, gating", `{"body":"looks good","event":"approve"}`, true, "looks good", forgeapi.ReviewApprove},
		{"explicit request_changes, gating", `{"result":"needs work","event":"request_changes"}`, true, "needs work", forgeapi.ReviewRequestChanges},
		// With gating but no verdict at all it stays a COMMENT — never a silent approve.
		{"prose, gating, no verdict", `{"message":"agent review prose"}`, true, "agent review prose", forgeapi.ReviewComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
			in := executor.SystemStepInput{
				Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3}),
				Step:       &registry.WorkflowStep{Handler: "forge.post_review", GatingReviews: tc.gating},
				PrevResult: json.RawMessage(tc.prev),
			}
			res, err := h.Execute(context.Background(), in)
			if err != nil {
				t.Fatalf("prev=%s: %v", tc.prev, err)
			}
			if prov.gotRepo != "o/r" || prov.gotNumber != 3 {
				t.Errorf("target repo=%s number=%d", prov.gotRepo, prov.gotNumber)
			}
			// wantBody is the review text; every posted body also carries the
			// Art 50(1) disclosure trailer (G6 finding A).
			if prov.gotReview.Body != wantDisclosedBody(tc.wantBody) || prov.gotReview.Event != tc.wantEvent {
				t.Errorf("prev=%s: review=%+v want body=%q event=%q", tc.prev, prov.gotReview, wantDisclosedBody(tc.wantBody), tc.wantEvent)
			}
			if len(res.Result) == 0 {
				t.Error("expected a result envelope")
			}
		})
	}
}

// TestPostReview_GatingDerivesEventFromVerdict pins the fix for the user report
// (2026-06-14): the reviewer role emits a structured {"review":{"approved":...}}
// verdict that post_review rendered into a "✅ Approved" tickbox body, but the
// forge review was always posted as a non-gating COMMENT — it never actually
// approved the PR. With gating_reviews enabled, approved=true must post a real
// APPROVE and approved=false a REQUEST_CHANGES; with gating off (default) it
// stays a COMMENT regardless of the verdict.
func TestPostReview_GatingDerivesEventFromVerdict(t *testing.T) {
	approved := `{"message":"{\"review\":{\"approved\":true,\"feedback\":\"Looks solid.\",\"summary\":\"Approved.\"}}"}`
	rejected := `{"message":"{\"review\":{\"approved\":false,\"feedback\":\"Needs a regression test.\",\"summary\":\"Changes requested.\"}}"}`
	for _, tc := range []struct {
		name      string
		prev      string
		gating    bool
		wantEvent forgeapi.ReviewEvent
	}{
		{"approved + gating → APPROVE", approved, true, forgeapi.ReviewApprove},
		{"rejected + gating → REQUEST_CHANGES", rejected, true, forgeapi.ReviewRequestChanges},
		{"approved + no gating → COMMENT", approved, false, forgeapi.ReviewComment},
		{"rejected + no gating → COMMENT", rejected, false, forgeapi.ReviewComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeProvider{}
			h := NewPostReviewHandler(fakeResolver{p: prov}, realDiscloser())
			in := executor.SystemStepInput{
				Task:       taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 7}),
				Step:       &registry.WorkflowStep{Handler: "forge.post_review", GatingReviews: tc.gating},
				PrevResult: json.RawMessage(tc.prev),
			}
			if _, err := h.Execute(context.Background(), in); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if prov.gotReview.Event != tc.wantEvent {
				t.Errorf("event=%q want %q", prov.gotReview.Event, tc.wantEvent)
			}
		})
	}
}

func TestPostReview_Errors(t *testing.T) {
	good := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 3})
	ctx := context.Background()

	if h := (&PostReviewHandler{}); func() bool { _, err := h.Execute(ctx, executor.SystemStepInput{Task: good}); return err == nil }() {
		t.Error("missing deps should error")
	}
	// empty body
	h := NewPostReviewHandler(fakeResolver{p: &fakeProvider{}}, realDiscloser())
	if _, err := h.Execute(ctx, executor.SystemStepInput{Task: good, PrevResult: json.RawMessage(`{}`)}); err == nil {
		t.Error("empty review body should error")
	}
	// missing job
	if _, err := h.Execute(ctx, executor.SystemStepInput{Task: &persistence.Task{}, PrevResult: json.RawMessage(`{"body":"x"}`)}); err == nil {
		t.Error("missing job should error")
	}
	// resolver error
	hr := NewPostReviewHandler(fakeResolver{err: errors.New("no provider")}, realDiscloser())
	if _, err := hr.Execute(ctx, executor.SystemStepInput{Task: good, PrevResult: json.RawMessage(`{"body":"x"}`)}); err == nil {
		t.Error("resolver error should propagate")
	}
	// post error
	hp := NewPostReviewHandler(fakeResolver{p: &fakeProvider{reviewErr: errors.New("post boom")}}, realDiscloser())
	if _, err := hp.Execute(ctx, executor.SystemStepInput{Task: good, PrevResult: json.RawMessage(`{"body":"x"}`)}); err == nil {
		t.Error("post error should propagate")
	}
	if h.Name() != "forge.post_review" {
		t.Errorf("name=%s", h.Name())
	}
}

// TestReviewBodyEvent_RendersStructuredOutput: the reviewer role emits a
// structured envelope ({"review":{approved,feedback,summary,remaining,...}}),
// often preceded by reasoning prose. post_review must post the rendered
// feedback/summary — NEVER the raw JSON envelope or the agent's reasoning
// (user report 2026-06-13: GitHub reviews showed the raw {"review":...} blob
// and lines like "I now have a complete understanding...").
func TestReviewBodyEvent_RendersStructuredOutput(t *testing.T) {
	const rawJSONMarker = `"review"`

	t.Run("pure structured output in message envelope", func(t *testing.T) {
		prev := json.RawMessage(`{"message":"{\"review\":{\"approved\":true,\"feedback\":\"Looks solid; tests cover the new paths.\",\"summary\":\"Approved.\",\"remaining\":[]}}"}`)
		body, _ := reviewBodyEvent(prev, false)
		if !strings.Contains(body, "Looks solid; tests cover the new paths.") || !strings.Contains(body, "Approved.") {
			t.Errorf("body missing rendered feedback/summary: %q", body)
		}
		if strings.Contains(body, rawJSONMarker) || strings.Contains(body, `\"feedback\"`) {
			t.Errorf("body still contains raw JSON: %q", body)
		}
	})

	t.Run("reasoning prose then trailing JSON", func(t *testing.T) {
		prev := json.RawMessage(`{"message":"I now have a complete understanding of the changes. Let me summarize my review findings:\n{\"review\":{\"approved\":false,\"feedback\":\"Missing a regression test for the SSRF guard.\",\"summary\":\"Changes requested.\",\"remaining\":[\"add SSRF regression test\"]}}"}`)
		body, _ := reviewBodyEvent(prev, false)
		if !strings.Contains(body, "Missing a regression test for the SSRF guard.") {
			t.Errorf("body missing feedback: %q", body)
		}
		if !strings.Contains(body, "add SSRF regression test") {
			t.Errorf("body missing remaining item: %q", body)
		}
		if strings.Contains(body, "I now have a complete understanding") {
			t.Errorf("body leaked reasoning prose: %q", body)
		}
		if strings.Contains(body, rawJSONMarker) {
			t.Errorf("body leaked raw JSON: %q", body)
		}
	})

	t.Run("review object at top level of prev", func(t *testing.T) {
		prev := json.RawMessage(`{"review":{"approved":true,"feedback":"Top-level review prose.","summary":"OK."}}`)
		body, _ := reviewBodyEvent(prev, false)
		if !strings.Contains(body, "Top-level review prose.") || strings.Contains(body, rawJSONMarker) {
			t.Errorf("top-level review not rendered cleanly: %q", body)
		}
	})

	t.Run("plain prose passes through unchanged (back-compat)", func(t *testing.T) {
		prev := json.RawMessage(`{"message":"Just plain review prose, no JSON here."}`)
		body, _ := reviewBodyEvent(prev, false)
		if body != "Just plain review prose, no JSON here." {
			t.Errorf("plain prose should pass through verbatim, got %q", body)
		}
	})
}

func taskWithTopLevelJob(j forgeapi.ForgeJob) *persistence.Task {
	pl, _ := json.Marshal(map[string]any{"forge_job": j, "context": map[string]string{"prompt": "p"}})
	return &persistence.Task{ProjectID: "proj-1", Payload: pl}
}

// TestForgeJobFromTask_TopLevel: the github channel stamps forge_job at the top
// level (context stays map[string]string) — the handler must read it there.
func TestForgeJobFromTask_TopLevel(t *testing.T) {
	j, err := forgeJobFromTask(taskWithTopLevelJob(forgeapi.ForgeJob{Repo: "o/r", Number: 9}), "h")
	if err != nil || j.Number != 9 {
		t.Fatalf("top-level forge_job not read: %+v err=%v", j, err)
	}
}

func TestFetchDiff_HappyAndErrors(t *testing.T) {
	ctx := context.Background()
	good := taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 4})

	// missing deps
	if _, err := (&FetchDiffHandler{}).Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("missing deps should error")
	}
	// happy path: diff flows into result.message for the next agent step
	prov := &fakeProvider{}
	prov.diff = []byte("diff --git a b\n+line\n")
	h := NewFetchDiffHandler(fakeResolver{p: prov})
	if h.Name() != "forge.fetch_diff" {
		t.Fatalf("name=%s", h.Name())
	}
	res, err := h.Execute(ctx, executor.SystemStepInput{Task: good})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out["message"] != "diff --git a b\n+line\n" {
		t.Errorf("diff not in message: %v", out["message"])
	}
	// missing job
	if _, err := h.Execute(ctx, executor.SystemStepInput{Task: &persistence.Task{}}); err == nil {
		t.Error("missing job should error")
	}
	// resolver error
	hr := NewFetchDiffHandler(fakeResolver{err: errors.New("no provider")})
	if _, err := hr.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("resolver error should propagate")
	}
	// fetch error
	prov2 := &fakeProvider{diffErr: errors.New("404")}
	hf := NewFetchDiffHandler(fakeResolver{p: prov2})
	if _, err := hf.Execute(ctx, executor.SystemStepInput{Task: good}); err == nil {
		t.Error("fetch error should propagate")
	}
}

// TestOpenChangeRequest_NoChangeSkips: when HEAD has no commits beyond base (the
// child merged nothing), publish is a clean no-op (state=no_change), not a push
// or a failure. Uses a real temp repo where the worktree branch == base.
func TestOpenChangeRequest_NoChangeSkips(t *testing.T) {
	if _, err := execLookGit(); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
	gitRun(t, dir, "branch", "-M", "main")
	sha := gitOut(t, dir, "rev-parse", "HEAD")
	// origin/main == HEAD (simulate the fetched base at the same commit).
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", sha)

	prov := &fakeProvider{openURL: "should-not-be-used"}
	h := NewOpenChangeRequestHandler(fakeResolver{p: prov}, fakeSource{dir: dir, sha: sha}, nil, nil)
	in := executor.SystemStepInput{Task: taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Number: 1, DefaultBranch: "main", Labels: []string{"bug"}})}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var out openResult
	_ = json.Unmarshal(res.Result, &out)
	if out.State != "no_change" {
		t.Errorf("state=%q want no_change", out.State)
	}
	if prov.pushedBranch != "" {
		t.Error("must not push when there is no change")
	}
}

func TestCommitsBeyondBase(t *testing.T) {
	if _, err := execLookGit(); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
	gitRun(t, dir, "branch", "-M", "main")
	baseSha := gitOut(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", baseSha)

	// No commits beyond base.
	if n, ok := commitsBeyondBase(context.Background(), dir, "main", baseSha); !ok || n != 0 {
		t.Errorf("no-change: n=%d ok=%v want 0,true", n, ok)
	}
	// One commit beyond base.
	gitRun(t, dir, "commit", "--allow-empty", "-m", "fix")
	head := gitOut(t, dir, "rev-parse", "HEAD")
	if n, ok := commitsBeyondBase(context.Background(), dir, "main", head); !ok || n != 1 {
		t.Errorf("one-ahead: n=%d ok=%v want 1,true", n, ok)
	}
	// Unknown dir → ok=false (publish proceeds).
	if _, ok := commitsBeyondBase(context.Background(), "/no/such/dir", "main", head); ok {
		t.Error("nonexistent gitDir should be ok=false")
	}
}

// fakeArtifactStore records the Store call and returns a canned artifact,
// reading back the patch bytes it was asked to persist.
type fakeArtifactStore struct {
	project, exec, task, name, path string
	body                            []byte
	err                             error
}

func (f *fakeArtifactStore) Store(_ context.Context, project, execID, taskID, name, sourcePath string) (*persistence.Artifact, error) {
	f.project, f.exec, f.task, f.name, f.path = project, execID, taskID, name, sourcePath
	if b, e := os.ReadFile(sourcePath); e == nil {
		f.body = b
	}
	if f.err != nil {
		return nil, f.err
	}
	return &persistence.Artifact{ID: "art-1", Name: name}, nil
}

// TestOpenChangeRequest_PushRejected_ParksWithPatch: when the forge REJECTS the
// push (typed *forge.PushRejectedError), the handler must NOT error — it returns
// a PublishBlockedSignal (state=blocked_awaiting_operator) carrying the reason +
// remediation, and captures the committed change as a mail-in patch artifact so
// an operator can submit it by hand. Regression for the 2026-07-11 headmatch
// incident (TASK-112: push rejected because the GitHub App lacked `workflows`).
func TestOpenChangeRequest_PushRejected_ParksWithPatch(t *testing.T) {
	if _, err := execLookGit(); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "commit", "--allow-empty", "-m", "base")
	gitRun(t, dir, "branch", "-M", "main")
	baseSha := gitOut(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", baseSha)
	// A real file change beyond base → a non-empty patch to capture.
	if err := os.WriteFile(filepath.Join(dir, "coverage.yml"), []byte("MIN_COVERAGE: 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "raise coverage gate")
	head := gitOut(t, dir, "rev-parse", "HEAD")

	rej := &forgeapi.PushRejectedError{
		Branch: "backlog/task-112",
		Kind:   forgeapi.PushRejectionPermission,
		Output: "refusing to allow a GitHub App to create or update workflow without `workflows` permission",
	}
	store := &fakeArtifactStore{}
	h := NewOpenChangeRequestHandler(
		fakeResolver{p: &fakeProvider{pushErr: rej}},
		fakeSource{dir: dir, sha: head},
		store,
		nil,
	)
	in := executor.SystemStepInput{
		Task:      taskWithJob(forgeapi.ForgeJob{Repo: "o/r", Kind: "backlog", Slug: "task-112", DefaultBranch: "main"}),
		Execution: &persistence.Execution{ID: "exec-1"},
	}
	res, err := h.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("a rejected push must PARK, not error: %v", err)
	}
	sig, ok := executor.AsPublishBlocked(res.Result)
	if !ok {
		t.Fatalf("result is not a blocked signal: %s", res.Result)
	}
	if !strings.Contains(sig.Reason, "workflows") {
		t.Errorf("reason should carry the rejection detail: %q", sig.Reason)
	}
	if sig.Remediation == "" {
		t.Error("remediation should be set for a permission rejection")
	}
	if sig.ArtifactID != "art-1" || sig.ArtifactName == "" {
		t.Errorf("expected a captured patch artifact, got id=%q name=%q", sig.ArtifactID, sig.ArtifactName)
	}
	if len(store.body) == 0 || !strings.Contains(string(store.body), "MIN_COVERAGE") {
		t.Errorf("captured patch should contain the change; got %d bytes", len(store.body))
	}
}

func execLookGit() (string, error)     { return exec.LookPath("git") }
func gitInit(t *testing.T, dir string) { gitRun(t, dir, "init", "-q") }
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
