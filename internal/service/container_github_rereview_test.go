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

// The knobs must survive the whole path from project YAML to the gate that
// reads them. A config key an operator can set that never reaches the gate is
// the "parsed and did nothing" class this release spent its time removing.
//
// CHANGED 2026-09-13: the path used to end at the GitHub App channel's
// InstallationConfig, which is precisely why the generic `webhooks.sources`
// relay had neither knob (BACKLOG 2026-09-03). It now ends at the shared
// forgereview policy both ingresses consult, so this asserts the resolution
// rather than the plumbing into one channel.
func TestForgeReviewPolicy_ResolvesFromEitherSpelling(t *testing.T) {
	off, on := false, true

	// 1. The github_app: spelling that shipped in 2026.9.1 — the one deployed
	//    configs carry — still resolves, and now reaches BOTH ingresses.
	legacy := &registry.Project{GitHubApp: registry.ProjectGitHubApp{
		AutoReviewOnPush: &off,
		ReviewDraftPRs:   true,
	}}
	if got := legacy.ForgeReview(); got.AutoReviewOnPush || !got.ReviewDraftPRs {
		t.Errorf("github_app: spelling resolved to %+v, want {false true}", got)
	}

	// 2. The ingress-neutral forge: spelling wins over it, in both directions,
	//    so an operator can override a legacy value without deleting it.
	both := &registry.Project{
		Forge:     registry.ProjectForge{AutoReviewOnPush: &on, ReviewDraftPRs: &off},
		GitHubApp: registry.ProjectGitHubApp{AutoReviewOnPush: &off, ReviewDraftPRs: true},
	}
	if got := both.ForgeReview(); !got.AutoReviewOnPush || got.ReviewDraftPRs {
		t.Errorf("forge: spelling did not win: %+v, want {true false}", got)
	}

	// 3. Neither set resolves to the documented defaults: review pushes
	//    (forge.md's promise), skip drafts (work nobody has said is ready).
	if got := (&registry.Project{}).ForgeReview(); !got.AutoReviewOnPush || got.ReviewDraftPRs {
		t.Errorf("defaults = %+v, want {true false}", got)
	}
}
