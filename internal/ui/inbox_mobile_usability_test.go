package ui

// Tests for the 2026-07-12 inbox mobile-usability pass
// (https://docs.vornik.io):
// attention rows must SAY what is being asked (human headline, the
// checkpoint's question + expectation), request cards must carry a
// state-specific call to action and a done-outcome summary, and
// deliverable chips collapse into a <details> element.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// --- attention rows: human headline -----------------------------------

func TestInbox_AttentionRow_HumanTitleFromPayload(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{{
		ID: "t-approve-1", ProjectID: "p1",
		Status:    persistence.TaskStatusAwaitingApproval,
		Payload:   []byte(`{"context":{"prompt":"Summarise the weekly sales report and email it to the team"}}`),
		CreatedAt: now, UpdatedAt: now,
	}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Summarise the weekly sales report") {
		t.Error("attention row must headline the task's prompt excerpt, not just the id")
	}
	// The id stays available (demoted), so support flows can still copy it.
	if !strings.Contains(body, "t-approve-1") {
		t.Error("task id must still render (demoted) on the row")
	}
}

func TestInbox_AttentionRow_FallsBackToIDWithoutPayload(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{{
		ID: "t-bare", ProjectID: "p1",
		Status:    persistence.TaskStatusAwaitingApproval,
		CreatedAt: now, UpdatedAt: now,
	}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))

	if !strings.Contains(rec.Body.String(), "t-bare") {
		t.Error("a payload-less task must still render its id as the headline")
	}
}

// --- attention rows: the ask is visible --------------------------------

func TestInbox_NeedsInputRow_ShowsQuestionAndExpectation(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{{
		ID: "t-input-q", ProjectID: "p1",
		Status:    persistence.TaskStatusAwaitingInput,
		CreatedAt: now, UpdatedAt: now,
	}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	msgRepo := &uiTcStubMsgRepo{checkpoint: &persistence.TaskMessage{
		ID: "cp-q",
		Metadata: []byte(`{
			"kind":"decision",
			"question":"Which environment should I deploy to?",
			"task_for_human":"Deploying the reporting stack",
			"expected_by":"today 18:00",
			"default_if_no_response":"deploy to staging only",
			"options":[{"id":"prod","label":"Production"},{"id":"stage","label":"Staging"}]
		}`),
	}}
	srv := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(msgRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	for _, want := range []string{
		"Which environment should I deploy to?", // the ask itself
		"Deploying the reporting stack",         // task-for-human context
		"today 18:00",                           // when the answer is expected
		"deploy to staging only",                // what happens without one
		"Production", "Staging",                 // the decision buttons still render
	} {
		if !strings.Contains(body, want) {
			t.Errorf("needs-input row must render %q", want)
		}
	}
}

// --- request cards: state-specific CTA ---------------------------------

func TestInbox_RequestCard_CTALabelPerWinnerStatus(t *testing.T) {
	cases := []struct {
		status persistence.TaskStatus
		want   string
	}{
		{persistence.TaskStatusAwaitingApproval, "Approve or reject"},
		{persistence.TaskStatusAwaitingInput, "Answer the question"},
		{persistence.TaskStatusFailed, "Fix or retry"},
		{persistence.TaskStatusRunning, "Watch progress"},
		{persistence.TaskStatusQueued, "Watch progress"},
		{persistence.TaskStatusWaitingForChildren, "Watch progress"},
		{persistence.TaskStatusCompleted, "See what you got"},
		{persistence.TaskStatusCancelled, "See details"},
	}
	now := time.Now()
	for _, tc := range cases {
		task := &persistence.Task{ID: "req-cta", ProjectID: "p1", Status: tc.status, CreatedAt: now, UpdatedAt: now}
		taskRepo := &mocks.MockTaskRepository{
			ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
				return mocks.FilterTasks([]*persistence.Task{task}, f), nil
			},
		}
		srv := NewServer(WithTaskRepository(taskRepo))
		cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
		if len(cards) != 1 {
			t.Fatalf("%s: expected 1 card, got %d", tc.status, len(cards))
		}
		if cards[0].CTALabel != tc.want {
			t.Errorf("%s: CTALabel = %q, want %q", tc.status, cards[0].CTALabel, tc.want)
		}
	}
}

func TestInbox_RequestCard_CTARenderedInsteadOfView(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{{
		ID: "req-render", ProjectID: "p1",
		Status:    persistence.TaskStatusAwaitingApproval,
		CreatedAt: now, UpdatedAt: now,
	}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Approve or reject") {
		t.Error("request card must render the state-specific CTA label")
	}
	if strings.Contains(body, ">View →<") {
		t.Error("the uniform 'View →' CTA must be gone from request cards")
	}
}

// --- request cards: done-outcome summary -------------------------------

func TestInbox_DoneCard_OutcomeSummaryNamesDeliverables(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-done", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-done": {ID: "exec-d", TaskID: "req-done"}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
				{ID: "a2", Name: "summary.md", ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].StatusLine != "Done — 2 files ready below" {
		t.Errorf("StatusLine = %q, want the deliverable-counting outcome summary", cards[0].StatusLine)
	}
}

func TestInbox_DoneCard_NoDeliverablesKeepsPlainLabel(t *testing.T) {
	now := time.Now()
	task := &persistence.Task{ID: "req-done-bare", ProjectID: "p1", Status: persistence.TaskStatusCompleted, CreatedAt: now, UpdatedAt: now}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks([]*persistence.Task{task}, f), nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))

	cards := srv.buildRequestCards(context.Background(), []*persistence.Task{task})
	if len(cards) != 1 {
		t.Fatalf("expected 1 card, got %d", len(cards))
	}
	if cards[0].StatusLine != "Completed" {
		t.Errorf("StatusLine = %q, want the plain rollup label when nothing was delivered", cards[0].StatusLine)
	}
}

// --- request cards: collapsible deliverables ---------------------------

func TestInbox_DeliverablesCollapseIntoDetails(t *testing.T) {
	now := time.Now()
	seed := []*persistence.Task{{
		ID: "req-files", ProjectID: "p1",
		Status:    persistence.TaskStatusAwaitingApproval,
		CreatedAt: now, UpdatedAt: now,
	}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			return mocks.FilterTasks(seed, f), nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDsFunc: func(_ context.Context, _ []string) (map[string]*persistence.Execution, error) {
			return map[string]*persistence.Execution{"req-files": {ID: "exec-f", TaskID: "req-files"}}, nil
		},
	}
	artifactRepo := &mocks.MockArtifactRepository{
		ListFunc: func(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{
				{ID: "a1", Name: "report.pdf", ArtifactClass: persistence.ArtifactClassOutput},
			}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo), WithExecutionRepository(execRepo), WithArtifactRepository(artifactRepo))

	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/ui/inbox", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "<details") || !strings.Contains(body, "1 file") {
		t.Error("deliverable chips must sit inside a collapsed <details> with a file count summary")
	}
	if !strings.Contains(body, "report.pdf") {
		t.Error("the chip itself must still render inside the details body")
	}
}
