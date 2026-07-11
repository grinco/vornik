package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// fakeNarrationRepo is a minimal in-memory persistence.
// ExecutionNarrationRepository double for the story-panel tests
// (task 2.2). Mirrors the fakeOutcomeRepo convention used elsewhere
// in this package (hallucination_summary_test.go).
type fakeNarrationRepo struct {
	rows []*persistence.ExecutionNarration
	err  error
}

func (f *fakeNarrationRepo) Insert(_ context.Context, row *persistence.ExecutionNarration) (int64, error) {
	return row.Seq, nil
}

func (f *fakeNarrationRepo) ListByExecution(_ context.Context, _ string) ([]*persistence.ExecutionNarration, error) {
	return f.rows, f.err
}

// --- storyLines (server-side loader) ---

func TestStoryLines_SeedsInSeqOrder(t *testing.T) {
	srv := NewServer(WithExecutionNarrationRepository(&fakeNarrationRepo{rows: []*persistence.ExecutionNarration{
		// Deliberately out of order — the repo contract promises
		// seq-ascending, but the loader sorts defensively (see the
		// comment on storyLines).
		{Seq: 3, Text: "Writing your one-page summary (step 3 of 3).", Kind: persistence.ExecutionNarrationKindStep},
		{Seq: 1, Text: "Reading the pricing pages you gave me.", Kind: persistence.ExecutionNarrationKindStep},
		{Seq: 2, Text: "Working on step 2 of 3.", Kind: persistence.ExecutionNarrationKindStep, Degraded: true},
	}}))
	lines := srv.storyLines(context.Background(), "exec_1")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d (%+v)", len(lines), lines)
	}
	if lines[0].Seq != 1 || lines[1].Seq != 2 || lines[2].Seq != 3 {
		t.Fatalf("expected seq-ascending order 1,2,3; got %d,%d,%d", lines[0].Seq, lines[1].Seq, lines[2].Seq)
	}
	if !lines[1].Degraded {
		t.Errorf("expected the seq=2 line to carry Degraded=true")
	}
	if lines[0].Degraded || lines[2].Degraded {
		t.Errorf("only the seq=2 line should be Degraded")
	}
}

func TestStoryLines_NilRepoOrEmptyExecutionID(t *testing.T) {
	srv := NewServer()
	if got := srv.storyLines(context.Background(), "exec_1"); got != nil {
		t.Errorf("expected nil with no narration repo wired, got %+v", got)
	}

	srv2 := NewServer(WithExecutionNarrationRepository(&fakeNarrationRepo{rows: []*persistence.ExecutionNarration{{Seq: 1, Text: "x"}}}))
	if got := srv2.storyLines(context.Background(), ""); got != nil {
		t.Errorf("expected nil with empty executionID, got %+v", got)
	}
}

func TestStoryLines_RepoErrorIsBestEffort(t *testing.T) {
	srv := NewServer(WithExecutionNarrationRepository(&fakeNarrationRepo{err: errors.New("boom")}))
	if got := srv.storyLines(context.Background(), "exec_1"); got != nil {
		t.Errorf("expected nil on repo error (best-effort, page still renders), got %+v", got)
	}
}

// --- TaskLive / ExecutionLive story panel + role gating ---

// TestTaskLive_StoryPanelSeedsAndRoleGatesTechnicalDetails covers the
// handler/render contract from the task-2.2 brief: seeded narration
// lines render in seq order (including the degraded "(simplified)"
// marker), the story panel is unconditionally present, and the
// technical "Step timeline" section is collapsed for a RoleUser
// session but open for everyone else (admin, or auth disabled — the
// pre-2.2 default).
func TestTaskLive_StoryPanelSeedsAndRoleGatesTechnicalDetails(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{
				ID: "exec_story_1", TaskID: "task_story", ProjectID: "",
				Status: persistence.ExecutionStatusRunning,
			}}, nil
		},
	}
	narrationRepo := &fakeNarrationRepo{rows: []*persistence.ExecutionNarration{
		{Seq: 2, Text: "Now writing your one-page summary — step 2 of 2.", Kind: persistence.ExecutionNarrationKindStep},
		{Seq: 1, Text: "Reading the three pricing pages you gave me.", Kind: persistence.ExecutionNarrationKindStep, Degraded: true},
	}}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithExecutionNarrationRepository(narrationRepo),
	)

	t.Run("RoleUser: story seeded, technical details collapsed", func(t *testing.T) {
		req := sessionUserUIRequest(http.MethodGet, "/ui/tasks/task_story/live", nil)
		rr := httptest.NewRecorder()
		srv.TaskLive(rr, req, "task_story")
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()

		if !strings.Contains(body, "Reading the three pricing pages you gave me.") {
			t.Errorf("expected seq=1 story line in body")
		}
		if !strings.Contains(body, "Now writing your one-page summary") {
			t.Errorf("expected seq=2 story line in body")
		}
		// Seq order: the seq=1 line's text must appear before seq=2's.
		i1 := strings.Index(body, "Reading the three pricing pages")
		i2 := strings.Index(body, "Now writing your one-page summary")
		if i1 == -1 || i2 == -1 || i1 > i2 {
			t.Errorf("expected seq=1 line to render before seq=2 line (i1=%d, i2=%d)", i1, i2)
		}
		if !strings.Contains(body, "(simplified)") {
			t.Errorf("expected the degraded line to carry the (simplified) marker")
		}
		if !strings.Contains(body, "Show technical details") {
			t.Errorf("expected the technical section behind a 'Show technical details' disclosure")
		}
		// Collapsed: the <details ...> wrapper must NOT carry the open
		// attribute for a RoleUser session.
		if strings.Contains(body, `overflow-hidden" open>`) {
			t.Errorf("technical details must be collapsed (no `open` attribute) for a RoleUser session")
		}
	})

	t.Run("admin/no-session: technical details open", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_story/live", nil)
		rr := httptest.NewRecorder()
		srv.TaskLive(rr, req, "task_story")
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, `overflow-hidden" open>`) {
			t.Errorf("technical details must be open by default for a non-RoleUser (admin/no-session) viewer")
		}
	})
}

// TestTaskLive_StoryPanelEmptyState — with no narration repo wired
// (or no rows recorded yet), the story panel must still render its
// empty state rather than erroring or blanking the page.
func TestTaskLive_StoryPanelEmptyState(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	execRepo.ListFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: "exec_empty", TaskID: "task_empty", Status: persistence.ExecutionStatusRunning}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_empty/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_empty")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Waiting for the first update") {
		t.Errorf("expected the story panel's empty state to render")
	}
}

// --- TaskDetail story panel + role gating ---

// TestTaskDetail_StoryPanelSeedsAndRoleGatesCostTable mirrors the
// live-page test for the completed/task-detail surface: the story
// panel seeds from execution_narration in seq order, and the LLM
// Cost technical table collapses for RoleUser but stays open for
// admin/no-session, matching pre-2.2 behaviour.
func TestTaskDetail_StoryPanelSeedsAndRoleGatesCostTable(t *testing.T) {
	leaseID := "lease_story_1"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted, LeaseID: &leaseID}, nil
		},
	}
	execRepo := &mocks.MockExecutionRepository{
		ListFunc: func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{
				ID: "exec_detail_1", TaskID: "task_detail_story", ProjectID: "",
				Status: persistence.ExecutionStatusCompleted,
			}}, nil
		},
	}
	narrationRepo := &fakeNarrationRepo{rows: []*persistence.ExecutionNarration{
		{Seq: 1, Text: "Read three pricing pages.", Kind: persistence.ExecutionNarrationKindStep},
		{Seq: 2, Text: "Wrote your one-page summary. All done!", Kind: persistence.ExecutionNarrationKindCompletion},
	}}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
		WithExecutionNarrationRepository(narrationRepo),
	)

	t.Run("RoleUser: story seeded, cost table collapsed", func(t *testing.T) {
		req := sessionUserUIRequest(http.MethodGet, "/ui/tasks/task_detail_story", nil)
		req.URL.Path = "/tasks/task_detail_story" // TaskDetail parses r.URL.Path[len("/tasks/"):]
		rr := httptest.NewRecorder()
		srv.TaskDetail(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Read three pricing pages.") || !strings.Contains(body, "Wrote your one-page summary") {
			t.Errorf("expected both seeded story lines in body")
		}
		i1 := strings.Index(body, "Read three pricing pages.")
		i2 := strings.Index(body, "Wrote your one-page summary")
		if i1 == -1 || i2 == -1 || i1 > i2 {
			t.Errorf("expected seq=1 line before seq=2 line (i1=%d, i2=%d)", i1, i2)
		}
		if !strings.Contains(body, "Show technical details") {
			t.Errorf("expected the LLM Cost table gated behind 'Show technical details'")
		}
		if strings.Contains(body, `panel-ref" open>`) {
			t.Errorf("LLM Cost details must be collapsed for a RoleUser session")
		}

		// companion-review review-20260710-d9af.md finding #2: the
		// Lease and Execution-Attempts panels must ALSO gate behind
		// the same role-gated <details> pattern as LLM Cost — for
		// story-first consistency, both collapse for a RoleUser
		// session.
		leaseTag := detailsTagBefore(t, body, ">Lease</h2>")
		if strings.Contains(leaseTag, "open") {
			t.Errorf("Lease details must be collapsed for a RoleUser session; tag=%q", leaseTag)
		}
		execAttemptsTag := detailsTagBefore(t, body, ">Execution Attempts</h2>")
		if strings.Contains(execAttemptsTag, "open") {
			t.Errorf("Execution Attempts details must be collapsed for a RoleUser session; tag=%q", execAttemptsTag)
		}
	})

	t.Run("admin/no-session: cost table open", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/tasks/task_detail_story", nil)
		rr := httptest.NewRecorder()
		srv.TaskDetail(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, `panel-ref" open>`) {
			t.Errorf("LLM Cost details must be open by default for a non-RoleUser (admin/no-session) viewer")
		}

		leaseTag := detailsTagBefore(t, body, ">Lease</h2>")
		if !strings.Contains(leaseTag, "open") {
			t.Errorf("Lease details must be open by default for a non-RoleUser (admin/no-session) viewer; tag=%q", leaseTag)
		}
		execAttemptsTag := detailsTagBefore(t, body, ">Execution Attempts</h2>")
		if !strings.Contains(execAttemptsTag, "open") {
			t.Errorf("Execution Attempts details must be open by default for a non-RoleUser (admin/no-session) viewer; tag=%q", execAttemptsTag)
		}
	})
}

// detailsTagBefore returns the opening `<details ...>` tag that most
// closely precedes the given marker substring in body — used to
// assert a section's open/collapsed state by anchoring on its
// sectionHeader title text. Mirrors the back-scan pattern in
// execution_detail_branches_test.go (`<details class="panel-ref` before
// `>Result<`), generalised to any marker/details prefix.
func detailsTagBefore(t *testing.T, body, marker string) string {
	t.Helper()
	idx := strings.Index(body, marker)
	if idx == -1 {
		t.Fatalf("marker %q not found in body", marker)
	}
	dStart := strings.LastIndex(body[:idx], "<details")
	if dStart == -1 {
		t.Fatalf("no <details> tag found before marker %q", marker)
	}
	end := strings.Index(body[dStart:], ">")
	if end == -1 {
		t.Fatalf("unterminated <details> tag before marker %q", marker)
	}
	return body[dStart : dStart+end+1]
}

// TestTaskDetail_StoryPanelEmptyState — a task with no recorded
// narration still renders the story panel's "no story" note rather
// than an empty gap or an error.
func TestTaskDetail_StoryPanelEmptyState(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusCompleted}, nil
		},
	}
	srv := NewServer(WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodGet, "/tasks/task_no_story", nil)
	rr := httptest.NewRecorder()
	srv.TaskDetail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No story recorded for this task yet.") {
		t.Errorf("expected the story panel's empty state to render")
	}
}

// TestTaskLive_NarrationLineJSWireup pins the client-side dual-path
// merge wiring described in narrated-execution-design.md §5.6: the
// live WebSocket client must recognise the narration_line frame kind
// and route it through the buffer-then-merge path (rather than, say,
// silently dropping an unrecognised kind the way the switch's default
// case does for anything else). There is no JS test runner in this
// repo (server-rendered Go + HTMX, no package.json/node harness under
// internal/ui) — see the code comments in task_live.html's
// mergeNarrationFrame/onNarrationLine/seedStory for the documented
// merge invariant this test can't execute directly.
//
// It also pins the companion-review review-20260710-d9af.md finding
// #1 fix: mergeNarrationFrame must drop (not append) a live frame
// whose seq is not a number, since NarrationLinePayload.Seq is always
// set server-side and a missing seq means a malformed frame.
func TestTaskLive_NarrationLineJSWireup(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	execRepo.ListFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: "exec_js", TaskID: "task_js", Status: persistence.ExecutionStatusRunning}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_js/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_js")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"case 'narration_line':",
		"function onNarrationLine(",
		"function mergeNarrationFrame(",
		"function seedStory(",
		"narrationSeedDone",
		"highestSeededNarrationSeq",
		"if (seq === null) {",
		"dropping as malformed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the dual-path merge wiring %q in the rendered JS", want)
		}
	}
}
