package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

// §1.3 of https://docs.vornik.io
//
// The re-review request looked like a change confined to the webhook switch in
// internal/github/channel.go. It is not. resolveTaskType rejects kinds it does
// not know, and workflow routing tested `Kind == "pull_request.opened"` with the
// REPLY workflow as its else branch — so a new kind wired only into the channel
// would not merely fail, it would route a code review into the conversational
// workflow. That is worse than not firing, and it is silent.

func reviewRoutingProject() *registry.Project {
	p := projectForTaskCreator("p-rr")
	p.GitHubApp.PRReviewWorkflowID = "github-review"
	p.GitHubApp.ReplyWorkflowID = "github-router"
	return p
}

// Every kind that means "review this PR" must resolve to the review task type
// AND the review workflow.
func TestGitHubTaskCreator_AllReviewKinds_RouteToReviewWorkflow(t *testing.T) {
	for _, kind := range []string{
		"pull_request.opened",
		"pull_request.reopened",
		"pull_request.ready_for_review",
		"pull_request.synchronize",
	} {
		t.Run(kind, func(t *testing.T) {
			repo := newRecordingTaskRepo()
			g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
			err := g.Create(context.Background(), github.TaskCreationEvent{
				Kind:           kind,
				SessionID:      "acme/api#pulls/12",
				Title:          "PR title",
				SenderLogin:    "alice",
				Repo:           "acme/api",
				Number:         12,
				InstallationID: 9001,
				IdempotencyKey: "github-app:d-" + kind,
			})
			if err != nil {
				t.Fatalf("Create(%s): %v — an unknown kind is rejected by resolveTaskType", kind, err)
			}
			tasks := repo.snapshotTasks()
			if len(tasks) != 1 {
				t.Fatalf("tasks = %d, want 1", len(tasks))
			}
			var payload map[string]any
			_ = json.Unmarshal(tasks[0].Payload, &payload)

			if got := payload["taskType"]; got != pullRequestReviewTaskType {
				t.Errorf("taskType = %v, want %q", got, pullRequestReviewTaskType)
			}
			// The one that would fail silently: routing to the reply
			// workflow still produces a task, so only asserting on
			// task creation would pass while the review never runs.
			if got := payload["workflowId"]; got != "github-review" {
				t.Errorf("workflowId = %v, want github-review — a review kind routed to the CONVERSATIONAL workflow", got)
			}
		})
	}
}

// The complement: a non-review kind must still reach the reply workflow, so the
// generalisation above did not simply route everything at the reviewer.
func TestGitHubTaskCreator_NonReviewKind_StillRoutesToReplyWorkflow(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "issues.labeled",
		SessionID:      "acme/api#issues/7",
		Title:          "Issue title",
		Labels:         []string{"vornik-task"},
		SenderLogin:    "alice",
		Repo:           "acme/api",
		Number:         7,
		InstallationID: 9001,
		IdempotencyKey: "github-app:d-issue",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks := repo.snapshotTasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	var payload map[string]any
	_ = json.Unmarshal(tasks[0].Payload, &payload)
	if got := payload["workflowId"]; got != "github-router" {
		t.Errorf("workflowId = %v, want github-router", got)
	}
}

// An unknown kind must still be REJECTED rather than silently falling through
// to the reply workflow — the property that made §1.3 dangerous in the first
// place is that "unrecognised" and "conversational" were the same branch.
func TestGitHubTaskCreator_UnknownKind_IsRejected(t *testing.T) {
	repo := newRecordingTaskRepo()
	g := newGitHubTaskCreator(repo, reviewRoutingProject(), nil, zerolog.Nop())
	err := g.Create(context.Background(), github.TaskCreationEvent{
		Kind:           "pull_request.locked",
		SessionID:      "acme/api#pulls/12",
		SenderLogin:    "alice",
		Repo:           "acme/api",
		Number:         12,
		InstallationID: 9001,
		IdempotencyKey: "github-app:d-unknown",
	})
	if err == nil {
		t.Fatal("Create accepted an unknown kind; it must be rejected, not routed")
	}
	if got := len(repo.snapshotTasks()); got != 0 {
		t.Errorf("tasks = %d, want 0 for an unknown kind", got)
	}
}

// The knobs must survive the whole path from project YAML to the resolved
// installation, in BOTH channel modes. A config key an operator can set that
// never reaches the gate is the "parsed and did nothing" class this release
// spent its time removing.
func TestGitHubConfig_ReReviewKnobs_ReachTheInstallation(t *testing.T) {
	off := false
	p := registry.ProjectGitHubApp{
		// Inbound-only: a private key is required as soon as app_id /
		// installation_id are set, and this test is about config
		// plumbing, not outbound auth.
		RepoAllowlist:    []string{"acme/api"},
		WebhookSecretEnv: "GH_SECRET_RR_TEST",
		AutoReviewOnPush: &off,
		ReviewDraftPRs:   true,
	}
	t.Setenv("GH_SECRET_RR_TEST", "shhh")

	cfg, err := resolveGitHubAppConfig(p)
	if err != nil {
		t.Fatalf("resolveGitHubAppConfig: %v", err)
	}
	if cfg.AutoReviewOnPush == nil || *cfg.AutoReviewOnPush {
		t.Errorf("AutoReviewOnPush = %v, want explicit false", cfg.AutoReviewOnPush)
	}
	if !cfg.ReviewDraftPRs {
		t.Error("ReviewDraftPRs did not survive into github.Config")
	}

	// ...and on into the multi-installation form.
	ic := installationConfigFromConfig("p-1", cfg)
	if ic.AutoReviewOnPush == nil || *ic.AutoReviewOnPush {
		t.Errorf("AutoReviewOnPush = %v in InstallationConfig, want explicit false", ic.AutoReviewOnPush)
	}
	if !ic.ReviewDraftPRs {
		t.Error("ReviewDraftPRs did not survive into InstallationConfig")
	}
}
