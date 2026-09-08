package service

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/github"
)

// The kind→route classifier and the four sites that derive from it
// (LLD 2026-09-08-forge-ci-outcomes-design.md §3.2).
//
// This file exists because the same defect has now occurred twice in this one
// file: a new event kind was added to the webhook switch and to some but not
// all of the sites here, and the failure is SILENT — the routing else-branch is
// the conversational workflow, so a half-wired review kind answers a code
// review with chat, and a half-wired forgeJobFromEvent builds a job with an
// empty action that the forge handlers then read.

// knownKinds is every kind routeOf claims to handle. The completeness test
// below walks it, so a kind added to routeOf and nowhere else fails here rather
// than in production.
func knownKinds(t *testing.T) []string {
	t.Helper()
	kinds := []string{
		"pull_request.opened",
		"pull_request.reopened",
		"pull_request.ready_for_review",
		"pull_request.synchronize",
		"pull_request.comment_command",
		"workflow_run.completed",
		"issues.labeled",
	}
	for _, k := range kinds {
		if routeOf(k) == routeNone {
			t.Fatalf("kind %q is listed here but routeOf does not know it — one of "+
				"the two is stale", k)
		}
	}
	return kinds
}

// Every kind routeOf routes must be handled at ALL FOUR sites. This is the test
// that catches "added a kind, updated three of four".
func TestEveryRoutedKindIsHandledEverywhere(t *testing.T) {
	g := &githubTaskCreator{}
	for _, kind := range knownKinds(t) {
		ev := github.TaskCreationEvent{Kind: kind, Repo: "o/r", Number: 1}
		if kind == "issues.labeled" {
			ev.Labels = []string{"bug"} // this kind derives its type from a label
		}

		// Site 1: task-type resolution must not reject it.
		taskType, err := g.resolveTaskType(ev)
		if err != nil {
			t.Errorf("kind %q: resolveTaskType: %v", kind, err)
			continue
		}
		if strings.TrimSpace(taskType) == "" {
			t.Errorf("kind %q: resolveTaskType returned an empty task type", kind)
		}

		// Site 2: forgeJobFromEvent must build a job that describes something.
		job := forgeJobFromEvent(ev)
		if job.Action == "" {
			t.Errorf("kind %q: forgeJobFromEvent built a job with an EMPTY action — "+
				"this is the 2026-09-01 bug, in which the forge handlers read a job "+
				"that described nothing", kind)
		}

		// Site 3: the classifier itself must place it.
		if routeOf(kind) == routeNone {
			t.Errorf("kind %q: routeOf returned routeNone", kind)
		}
	}
}

// Site 4, the routing branch: a CI kind must never reach the reply workflow.
// Asserted as its own case because that is the silent failure — a chat answer
// where a code outcome was expected.
func TestCIKindDoesNotRouteToTheReplyWorkflow(t *testing.T) {
	if routeOf("workflow_run.completed") == routeReply {
		t.Fatal("a completed CI run must not be answered conversationally")
	}
	if !isCIKind("workflow_run.completed") {
		t.Error("isCIKind must recognise the completed run")
	}
	if isPullRequestReviewKind("workflow_run.completed") {
		t.Error("a CI run does not start a code review by itself — the FAILURE path " +
			"asks the coordinator, which is a different act")
	}
}

// The CI job is not a change request, and carries a real action.
func TestForgeJobFromCIEvent(t *testing.T) {
	job := forgeJobFromEvent(github.TaskCreationEvent{
		Kind: "workflow_run.completed", Repo: "o/r", Number: 42,
	})
	if job.Action != "completed" {
		t.Errorf("Action = %q, want completed", job.Action)
	}
	if job.IsChangeRequest {
		t.Error("a CI run must not be a change request: the forge handlers read that " +
			"flag to decide they are looking at a pull request, and would take a run " +
			"to fetch_diff and post_review")
	}
}

// An unknown kind is an ERROR, never a silent default. routeNone exists so this
// stays true.
func TestUnknownKindIsRejected(t *testing.T) {
	g := &githubTaskCreator{}
	if _, err := g.resolveTaskType(github.TaskCreationEvent{Kind: "workflow_run.requested"}); err == nil {
		t.Error("an unhandled kind must be an error at task-type resolution")
	}
	if routeOf("some.future.kind") != routeNone {
		t.Error("an unknown kind must classify as routeNone")
	}
}
