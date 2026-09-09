package github

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/forge"
)

// classifyWorkflowRun sets HeadRef so the review it triggers gets a working
// tree AT the commit under review (design §17).

func wrPayload(prNumbers ...int) ghEventPayload {
	var pl ghEventPayload
	pl.Action = "completed"
	pl.Repository.FullName = "acme/infra"
	pl.WorkflowRun = &struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		HeadSHA      string `json:"head_sha"`
		Conclusion   string `json:"conclusion"`
		Status       string `json:"status"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}{ID: 9, Name: "CI", Path: ".github/workflows/ci.yml", HeadSHA: "deadbeef", Conclusion: "failure"}
	for _, n := range prNumbers {
		pl.WorkflowRun.PullRequests = append(pl.WorkflowRun.PullRequests,
			struct {
				Number int `json:"number"`
			}{Number: n})
	}
	return pl
}

// THE REGRESSION: without HeadRef the reviewer works against the base branch,
// and reviewers reported main's last change instead of the pull request's.
func TestClassifyWorkflowRun_SetsHeadRefForAPullRequest(t *testing.T) {
	job, ok := classifyWorkflowRun(wrPayload(54), forge.ForgeJob{})
	if !ok {
		t.Fatal("a completed run must classify")
	}
	if job.HeadRef != "refs/pull/54/head" {
		t.Errorf("HeadRef = %q, want refs/pull/54/head — the reviewer's working "+
			"tree must be AT the commit it reviews", job.HeadRef)
	}
	// IsChangeRequest is a ROUTING flag and stays false: flipping it would send
	// CI deliveries to change_request_workflow_id.
	if job.IsChangeRequest {
		t.Error("IsChangeRequest must stay false for a CI run")
	}
	if job.Number != 54 {
		t.Errorf("Number = %d, want 54", job.Number)
	}
}

// No pull request (default-branch build, or a fork PR whose event carries an
// empty pull_requests[] — §3.3): nothing to materialize, so no HeadRef.
func TestClassifyWorkflowRun_NoPullRequestSetsNoHeadRef(t *testing.T) {
	job, ok := classifyWorkflowRun(wrPayload(), forge.ForgeJob{})
	if !ok {
		t.Fatal("a run with no pull request is still classified")
	}
	if job.HeadRef != "" {
		t.Errorf("HeadRef = %q, want empty — there is no pull request head to check out", job.HeadRef)
	}
	if job.Number != 0 {
		t.Errorf("Number = %d, want 0", job.Number)
	}
}

// The ref is CONSTRUCTED from the numeric PR id, never from a payload string.
func TestClassifyWorkflowRun_HeadRefIsConstructedFromTheNumber(t *testing.T) {
	job, _ := classifyWorkflowRun(wrPayload(7), forge.ForgeJob{})
	if !strings.HasPrefix(job.HeadRef, "refs/pull/") || !strings.HasSuffix(job.HeadRef, "/head") {
		t.Errorf("HeadRef = %q is not the canonical pull ref shape", job.HeadRef)
	}
}
