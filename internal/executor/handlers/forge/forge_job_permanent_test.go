package forge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// A forge job that names no pull request cannot become one by retrying.
//
// headmatch, 2026-09-09, task_20260909150605_13cc14a3d079dc09: a green push to
// main (no PR) was routed into the review workflow; forge.fetch_diff refused
// the job correctly — and the task spent all three attempts on the same
// refusal, classified UNKNOWN. The refusal is decided where the payload is in
// hand, which is what forge.PermanentError exists for (2026-09-02 design D1:
// "the payload is malformed"); the classifier then makes it
// FORGE_TARGET_UNAVAILABLE, which TaskShouldRetry refuses to retry.
func TestForgeJobFromTask_NoPullRequestIsPermanent(t *testing.T) {
	_, err := forgeJobFromTask(taskWithJob(forgeapi.ForgeJob{
		Repo: "grinco/headmatch", Action: "completed", HeadSHA: "320683a8",
	}), "forge.fetch_diff")
	if err == nil {
		t.Fatal("a job with no number and no backlog origin must be refused")
	}
	pe, ok := forgeapi.AsPermanent(err)
	if !ok {
		t.Fatalf("the refusal must be permanent, got %T: %v", err, err)
	}
	if pe.Op != "forge.fetch_diff" {
		t.Errorf("Op = %q, want the handler name", pe.Op)
	}
	if !strings.Contains(err.Error(), "issue-driven") {
		t.Errorf("the message must still say what shape a job needs: %q", err.Error())
	}
	if strings.Contains(err.Error(), "HTTP 0") {
		t.Errorf("no request was made; the message must not invent an HTTP status: %q", err.Error())
	}
	// A missing job is a WIRING defect, not a permanent target failure — it
	// keeps its ordinary error so the ladder's own handling of it is unchanged.
	if _, err := forgeJobFromTask(&persistence.Task{}, "h"); err == nil {
		t.Fatal("no job must still be an error")
	} else if _, ok := forgeapi.AsPermanent(err); ok {
		t.Error("a missing forge_job is not a permanent target failure")
	}
}

// forge.fetch_ci joins on head_sha, not the PR number (design §3.3), and a
// deposit workflow on a merged-main build "starts with forge.fetch_ci" (§5.1).
// It therefore must not go through the PR-requiring job check: a job with a
// repo and a head commit is enough.
func TestFetchCI_WorksWithoutAPullRequest(t *testing.T) {
	h := NewFetchCIHandler(stubCIOutcomes{rows: []*persistence.ForgeCIOutcome{{
		RunID: 9, WorkflowPath: ".github/workflows/ci.yml", Conclusion: "success", HeadSHA: "320683a8",
	}}})
	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: taskWithJob(forgeapi.ForgeJob{Repo: "grinco/headmatch", HeadSHA: "320683a8"}),
	})
	if err != nil {
		t.Fatalf("a PR-less job with a head must be readable: %v", err)
	}
	var payload map[string]any
	_ = json.Unmarshal(res.Result, &payload)
	if runs, _ := payload["runs"].(float64); runs != 1 {
		t.Errorf("runs = %v, want 1", payload["runs"])
	}
}
