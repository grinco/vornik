package github

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// The App-channel arm for completed CI runs
// (LLD 2026-09-08-forge-ci-outcomes-design.md §3.2).

type capturingTaskCreator struct{ events []TaskCreationEvent }

func (c *capturingTaskCreator) Create(_ context.Context, ev TaskCreationEvent) error {
	c.events = append(c.events, ev)
	return nil
}

func ciChannel(t *testing.T, enabled bool) (*Channel, *installation, *capturingTaskCreator) {
	t.Helper()
	tc := &capturingTaskCreator{}
	inst := &installation{taskCreator: tc, ciEnabled: enabled}
	return &Channel{logger: zerolog.Nop()}, inst, tc
}

func ciPayload(path string, prNumbers ...int) eventPayload {
	var p eventPayload
	p.Action = "completed"
	p.Repository.FullName = "acme/infra"
	p.Repository.DefaultBranch = "main"
	p.Installation.ID = 99
	p.WorkflowRun = &struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		HeadSHA      string `json:"head_sha"`
		Conclusion   string `json:"conclusion"`
		Status       string `json:"status"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}{ID: 77, Name: "Terraform", Path: path, HeadSHA: "deadbeef", Conclusion: "failure"}
	for _, n := range prNumbers {
		p.WorkflowRun.PullRequests = append(p.WorkflowRun.PullRequests,
			struct {
				Number int `json:"number"`
			}{Number: n})
	}
	return p
}

func TestWorkflowRunCompletedCreatesATask(t *testing.T) {
	c, inst, tc := ciChannel(t, true)
	c.handleWorkflowRunCompleted(context.Background(), "workflow_run", "d1",
		ciPayload(".github/workflows/terraform-plan.yml", 42), inst)

	if len(tc.events) != 1 {
		t.Fatalf("got %d events, want 1", len(tc.events))
	}
	ev := tc.events[0]
	if ev.Kind != "workflow_run.completed" {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.Number != 42 || ev.HeadSHA != "deadbeef" {
		t.Errorf("event did not carry the PR or head: %+v", ev)
	}
	if ev.CI == nil || ev.CI.RunID != 77 || ev.CI.Conclusion != "failure" {
		t.Errorf("CI ref missing or wrong: %+v", ev.CI)
	}
	if ev.CI.WorkflowPath != ".github/workflows/terraform-plan.yml" {
		t.Errorf("WorkflowPath = %q", ev.CI.WorkflowPath)
	}
}

// Off by default: with CI ingestion disabled the delivery is acked and nothing
// happens, which is the pre-feature behaviour exactly.
func TestWorkflowRunIgnoredWhenDisabled(t *testing.T) {
	c, inst, tc := ciChannel(t, false)
	c.handleWorkflowRunCompleted(context.Background(), "workflow_run", "d2",
		ciPayload(".github/workflows/terraform-plan.yml", 42), inst)
	if len(tc.events) != 0 {
		t.Fatalf("got %d events, want none when CI ingestion is off", len(tc.events))
	}
}

// THE CHANNEL DOES NOT FILTER BY WORKFLOW PATH, and that is the fix rather than
// a gap (design §14.4).
//
// It used to. The copy lived here, inside the GitHub App channel, so the
// GENERIC RELAY INGRESS — the door the deployment that found this actually uses
// — had no filter at all and `workflow_paths` did nothing there. The filter now
// lives in forgeci, which both ingresses run through, and is tested there
// against both.
//
// This case pins the division so nobody restores the duplicate: the channel
// forwards every completed run, and forgeci decides what is recorded. Two
// implementations of one safety check means one of them is wrong.
func TestWorkflowRunChannelForwardsEveryPathAndLetsForgeciFilter(t *testing.T) {
	for _, path := range []string{
		".github/workflows/terraform-plan.yml",
		".github/workflows/some-other.yml",
	} {
		c, inst, tc := ciChannel(t, true)
		c.handleWorkflowRunCompleted(context.Background(), "workflow_run", "d3",
			ciPayload(path, 42), inst)
		if len(tc.events) != 1 {
			t.Fatalf("%s: the channel must forward every completed run; got %d events",
				path, len(tc.events))
		}
		if got := tc.events[0].CI.WorkflowPath; got != path {
			t.Errorf("forwarded workflow_path = %q, want %q — forgeci filters on this",
				got, path)
		}
	}
}

// A run with no pull request — a default-branch build, or a fork PR — is still
// recorded, with Number 0 and no PR session (design §3.3).
func TestWorkflowRunWithNoPullRequestStillFires(t *testing.T) {
	c, inst, tc := ciChannel(t, true)
	c.handleWorkflowRunCompleted(context.Background(), "workflow_run", "d5",
		ciPayload(".github/workflows/deploy.yml"), inst)

	if len(tc.events) != 1 {
		t.Fatalf("got %d events, want 1", len(tc.events))
	}
	if tc.events[0].Number != 0 {
		t.Errorf("Number = %d, want 0", tc.events[0].Number)
	}
	if tc.events[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty — there is no PR thread to belong to",
			tc.events[0].SessionID)
	}
}
