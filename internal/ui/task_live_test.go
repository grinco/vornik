package ui

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

// liveTaskServer builds a minimal Server with just enough wiring for
// the TaskLive handler. Each call returns the mocks alongside the
// server so tests can adjust per-call behaviour without re-building.
func liveTaskServer(t *testing.T) (*Server, *mocks.MockTaskRepository, *mocks.MockExecutionRepository) {
	t.Helper()
	taskRepo := &mocks.MockTaskRepository{}
	execRepo := &mocks.MockExecutionRepository{}
	srv := NewServer(
		WithTaskRepository(taskRepo),
		WithExecutionRepository(execRepo),
	)
	return srv, taskRepo, execRepo
}

// TestLiveTask_404WhenMissing — TaskLive must 404 when the task
// repository can't find the requested ID. Operators following a
// stale link see a clean error rather than a half-rendered page.
func TestLiveTask_404WhenMissing(t *testing.T) {
	srv, taskRepo, _ := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return nil, persistence.ErrNotFound
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_missing/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_missing")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestLiveTask_RedirectOnTerminalStatus — visits for COMPLETED /
// FAILED / CANCELLED / CLOSED tasks go to the task detail page where
// the replay link lives. The live stream has nothing to show once
// the task is terminal; redirecting keeps operators out of an
// empty/closed-immediately socket.
func TestLiveTask_RedirectOnTerminalStatus(t *testing.T) {
	cases := []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed,
	}
	for _, st := range cases {
		t.Run(string(st), func(t *testing.T) {
			srv, taskRepo, _ := liveTaskServer(t)
			taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
				return &persistence.Task{ID: id, Status: st}, nil
			}
			req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_xyz/live", nil)
			rr := httptest.NewRecorder()
			srv.TaskLive(rr, req, "task_xyz")
			if rr.Code != http.StatusFound {
				t.Fatalf("expected 302, got %d (body: %s)", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Location"); got != "/ui/tasks/task_xyz" {
				t.Fatalf("expected redirect to task detail, got %q", got)
			}
		})
	}
}

// TestLiveTask_RendersForRunningTask — happy path: a RUNNING task
// with a non-terminal execution renders the live page. Asserts the
// task ID, execution ID, and live-stream banner-id land in the HTML
// so the JS layer can wire onto them.
func TestLiveTask_RendersForRunningTask(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	started := time.Now().Add(-2 * time.Minute)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	currentStep := "summarise"
	execRepo.ListFunc = func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{
			ID:             "exec_live_1",
			TaskID:         "task_running",
			ProjectID:      "p1",
			Status:         persistence.ExecutionStatusRunning,
			CurrentStepID:  &currentStep,
			CompletedSteps: []string{"step_a", "step_b"},
			StartedAt:      &started,
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_running/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_running")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"task_running",
		"exec_live_1",
		"reconnect-banner",
		"gap-banner",
		"steer-input",  // inline steering compose (Upgrade #3, replaces hint-modal)
		"fork-section", // inline fork (refactor D, replaces fork-modal)
		"summarise",
		"step_a",
		"step_b",
		"Live observation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected page to contain %q", want)
		}
	}
}

// TestLiveTask_RendersWhenNoExecutionYet — a freshly-queued task may
// not have an execution row yet (the scheduler hasn't leased it).
// The page must still render, with no execution metadata, so the
// operator's URL works during the QUEUED → LEASED transition.
func TestLiveTask_RendersWhenNoExecutionYet(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusQueued}, nil
	}
	execRepo.ListFunc = func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return nil, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_queued/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_queued")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "task_queued") {
		t.Errorf("expected page body to mention the task id")
	}
}

// TestLiveTask_503WhenReposMissing — without taskRepo / execRepo the
// page can't render anything useful; surface a clear 503 instead of
// blank HTML. Matches the contract /ui/executions/<id>/replay uses
// for the same gap.
func TestLiveTask_503WhenReposMissing(t *testing.T) {
	srv := NewServer() // no repos wired
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_x/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_x")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

// TestLiveTask_MethodNotAllowed — POST/PUT/DELETE land on the same
// path but the live page is read-only. Mirrors the replay handler.
func TestLiveTask_MethodNotAllowed(t *testing.T) {
	srv, _, _ := liveTaskServer(t)
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/task_x/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_x")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

// TestLiveTask_EmptyIDIs400 — the route dispatcher slices /live off
// the path; an otherwise-bare request hits the handler with an empty
// taskID. Return 400 so the dispatcher can be made strict later
// without changing the handler contract.
func TestLiveTask_EmptyIDIs400(t *testing.T) {
	srv, _, _ := liveTaskServer(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks//live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

// TestLiveTask_ExecutionRedirectsToReplayOnTerminal — the deep-link
// from the execution side must redirect to the replay page for
// terminal executions. Forks that completed in isolation hit this
// path.
func TestLiveTask_ExecutionRedirectsToReplayOnTerminal(t *testing.T) {
	srv, _, execRepo := liveTaskServer(t)
	execRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: id, Status: persistence.ExecutionStatusCompleted}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/executions/exec_done/live", nil)
	rr := httptest.NewRecorder()
	srv.ExecutionLive(rr, req, "exec_done")
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/ui/executions/exec_done/replay" {
		t.Fatalf("expected redirect to replay, got %q", got)
	}
}

// TestLiveTask_ExecutionRendersForRunningExecution — the
// execution-side deep-link must render the live page with the same
// template shape as the task-side entry point.
func TestLiveTask_ExecutionRendersForRunningExecution(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	currentStep := "research"
	execRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Execution, error) {
		return &persistence.Execution{
			ID:            id,
			TaskID:        "task_t1",
			Status:        persistence.ExecutionStatusRunning,
			CurrentStepID: &currentStep,
		}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/executions/exec_xy/live", nil)
	rr := httptest.NewRecorder()
	srv.ExecutionLive(rr, req, "exec_xy")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "exec_xy") || !strings.Contains(body, "research") {
		t.Errorf("expected execution id + current step in body")
	}
}

// TestLiveTask_IsTerminalTaskStatus — pure helper for the
// terminal-status classification. Cheap to assert exhaustively so
// regressions on the AWAITING_* / PAUSED side (non-terminal) are
// caught in CI.
func TestLiveTask_IsTerminalTaskStatus(t *testing.T) {
	terminal := []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
		persistence.TaskStatusClosed,
	}
	for _, s := range terminal {
		if !isTerminalTaskStatus(s) {
			t.Errorf("expected %q to be terminal", s)
		}
	}
	nonTerminal := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusPaused,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
		persistence.TaskStatusWaitingForChildren,
	}
	for _, s := range nonTerminal {
		if isTerminalTaskStatus(s) {
			t.Errorf("expected %q to be non-terminal", s)
		}
	}
}

// TestLiveTask_NoEmbeddedQuotesInJSStrings — regression sentinel for
// the 2026-05-26 bug where the template used `{{.X | printf "%q"}}`
// inside a <script> block. Go's printf %q wraps a string in literal
// `"` chars; html/template's JS-context autoescape then wraps the
// already-quoted value in another pair, producing JS like
// `executionID = "\"exec_...\""`. encodeURIComponent then percent-
// encodes those embedded quotes into the URL, the daemon's router
// can't match the execution, and every API call returns 400. The
// live page surfaces this as a "Reconnecting" loop with hint POSTs
// erroring out. Any future template edit that re-introduces %q on a
// string value inside <script> reverts this fix; this test catches
// it before it lands.
func TestLiveTask_NoEmbeddedQuotesInJSStrings(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	currentStep := "summarise"
	execRepo.ListFunc = func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{
			ID:             "exec_under_test",
			TaskID:         "task_under_test",
			ProjectID:      "p1",
			Status:         persistence.ExecutionStatusRunning,
			CurrentStepID:  &currentStep,
			CompletedSteps: []string{"step_alpha", "step_beta"},
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_under_test/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_under_test")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	// The fix produces clean JS string literals like:
	//   const executionID = "exec_under_test";
	// The bug produced:
	//   const executionID = "\"exec_under_test\"";
	// Each assignment is asserted explicitly so the failure message
	// names exactly which variable regressed.
	wantClean := map[string]string{
		`const executionID =`:        `const executionID = "exec_under_test"`,
		`const taskID =`:             `const taskID = "task_under_test"`,
		`const initialStatus =`:      `const initialStatus = "RUNNING"`,
		`const initialCurrentStep =`: `const initialCurrentStep = "summarise"`,
	}
	for label, want := range wantClean {
		if !strings.Contains(body, want) {
			t.Errorf("expected clean JS assignment %q in rendered page (%s)", want, label)
		}
		// The bug-shape — an assignment whose value starts with `"\"`
		// — must NOT appear. We grep for the assignment prefix + the
		// double-escape opener.
		bug := label + ` "\"` // e.g. `const executionID = "\"`
		if strings.Contains(body, bug) {
			t.Errorf("regression: %s value is double-quoted (%q present in body) — "+
				"template re-introduced `printf \"%%q\"` inside <script>", label, bug)
		}
	}

	// Seeded-steps array: same hazard. The fix renders
	//   const seededSteps = ["step_alpha","step_beta"];
	// the bug rendered
	//   const seededSteps = ["\"step_alpha\"","\"step_beta\""];
	if !strings.Contains(body, `["step_alpha","step_beta"]`) {
		t.Errorf("seededSteps array not rendered as clean JS strings — check for printf %%q regression")
	}
	if strings.Contains(body, `["\"step_alpha\""`) {
		t.Errorf("regression: seededSteps entries are double-quoted")
	}
}

// TestTaskDetail_RecoveryActionsRenderForFailed — when a task is in
// FAILED status with a recognised LastErrorClass, the recovery
// actions card must render with at least one primary action button.
// Pins the wire-up between the class → action mapping and the
// template render so a future refactor doesn't silently drop the
// card. This is the visible payoff of Upgrade #1.
func TestTaskDetail_RecoveryActionsRenderForFailed(t *testing.T) {
	srv, taskRepo, _ := liveTaskServer(t)
	errClass := persistence.TaskFailureClassRateLimited
	lastErr := "scraper hit captcha on 4/18 fetches"
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{
			ID:             id,
			Status:         persistence.TaskStatusFailed,
			LastError:      &lastErr,
			LastErrorClass: &errClass,
		}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_failed", nil)
	rr := httptest.NewRecorder()
	srv.TaskDetail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Recovery actions") {
		t.Error("Recovery actions card header must render on a FAILED task")
	}
	// The RATE_LIMITED primary mentions requeue (renamed from
	// retry in refactor B, 2026-05-26) — pin the wire-up by
	// asserting the label survives the action map → template path.
	if !strings.Contains(body, "Requeue — backoff layer will absorb transients") {
		t.Error("RATE_LIMITED primary action label not rendered — class→action map disconnected from template")
	}
	// Universal close-action backstop.
	if !strings.Contains(body, "Close — won&#39;t pursue") &&
		!strings.Contains(body, "Close — won't pursue") {
		t.Error("Universal Close action must render on every failure")
	}
}

// TestTaskDetail_FailedSteerSectionWiresHintThenRetry — pins the
// 2026-05-26 follow-up fix: "Steer + retry" must actually retry
// after queuing the hint, not stop at the hint POST. Reversion of
// any of the wiring pieces (Queue & retry button, hidden retry
// form pointing at /ui/tasks/{id}/retry, the alsoRetry branch in
// the JS) = visible test failure.
func TestTaskDetail_FailedSteerSectionWiresHintThenRetry(t *testing.T) {
	srv, taskRepo, _ := liveTaskServer(t)
	errClass := persistence.TaskFailureClassRateLimited
	lastErr := "scraper hit captcha"
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{
			ID:             id,
			ProjectID:      "p1",
			Status:         persistence.TaskStatusFailed,
			LastError:      &lastErr,
			LastErrorClass: &errClass,
		}, nil
	}
	// Path is /tasks/{id} (no /ui prefix) because TaskDetail parses
	// taskID from r.URL.Path[len("/tasks/"):] — the /ui prefix is
	// stripped one layer up by the router. Calling the handler
	// directly with a /tasks/ URL gives the right path slicing.
	req := httptest.NewRequest(http.MethodGet, "/tasks/task_failed", nil)
	rr := httptest.NewRecorder()
	srv.TaskDetail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	// Primary button label changed from "Queue hint" to "Queue &
	// retry" — pins the operator-visible promise.
	if !strings.Contains(body, "Queue &amp; retry") {
		t.Error(`primary button must say "Queue & retry" so the label matches the action`)
	}
	// Hidden retry form is the mechanism that actually fires the
	// retry after a successful hint POST.
	if !strings.Contains(body, `id="steer-failed-retry-form"`) {
		t.Error("hidden retry form missing — Steer + retry would stop at the hint POST")
	}
	if !strings.Contains(body, `action="/ui/tasks/task_failed/retry"`) {
		t.Error("retry form must target /ui/tasks/{id}/retry — otherwise the second action 404s")
	}
	// JS reads the alsoRetry flag — operator can opt into queue-
	// only with the secondary button.
	if !strings.Contains(body, "submitFailedSteeringHint(true)") {
		t.Error("primary button must call submitFailedSteeringHint(true) — alsoRetry mode")
	}
	if !strings.Contains(body, "submitFailedSteeringHint(false)") {
		t.Error("secondary button must call submitFailedSteeringHint(false) — queue-only mode")
	}
	if !strings.Contains(body, `'steer-failed-retry-form').submit()`) {
		t.Error("alsoRetry branch must submit the retry form after a successful hint POST")
	}
}

// TestTaskDetail_RecoveryActionsHiddenOnSuccess — the card must
// NOT render for COMPLETED tasks. Otherwise an operator opening a
// successful task sees confusing "recovery" UI suggesting
// something's wrong.
func TestTaskDetail_RecoveryActionsHiddenOnSuccess(t *testing.T) {
	srv, taskRepo, _ := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{
			ID:     id,
			Status: persistence.TaskStatusCompleted,
		}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_ok", nil)
	rr := httptest.NewRecorder()
	srv.TaskDetail(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "Recovery actions") {
		t.Error("Recovery actions card must NOT render on a COMPLETED task")
	}
}

// TestLiveTask_HeartbeatAndNowDoingWireup — Upgrade #2 (2026-05-26)
// adds three liveness signals to the live page: a last-event
// timestamp in the header, a "now-doing" line on each step card
// surfacing the in-flight tool call, and a pulse animation on the
// active step card whenever an event arrives. The wire-up depends
// on specific DOM ids + CSS class names that the JS layer reads;
// pinning them here catches a future template edit that breaks
// the contract.
func TestLiveTask_HeartbeatAndNowDoingWireup(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, Status: persistence.TaskStatusRunning}, nil
	}
	currentStep := "research"
	execRepo.ListFunc = func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{
			ID:            "exec_h1",
			TaskID:        "task_h1",
			ProjectID:     "p",
			Status:        persistence.ExecutionStatusRunning,
			CurrentStepID: &currentStep,
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_h1/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_h1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	// Header heartbeat element — JS heartbeat ticker writes to this.
	if !strings.Contains(body, `id="header-last-event"`) {
		t.Error("Heartbeat target id `header-last-event` missing — JS startHeartbeat() can't update the label")
	}
	// CSS classes the JS toggles for liveness colour ladder.
	if !strings.Contains(body, "Last event:") {
		t.Error("Heartbeat label text missing — operator won't see the staleness indicator")
	}
	// Animation keyframe — pulse-on-event depends on this class.
	if !strings.Contains(body, ".pulse-active") {
		t.Error("Pulse-active CSS class missing — active-step border flash won't fire")
	}
	if !strings.Contains(body, "vornik-pulse-border") {
		t.Error("Pulse keyframe name missing")
	}
	// Now-doing dot pulse — visual proof the agent is doing
	// something even between discrete events.
	if !strings.Contains(body, "nowdoing-dot") {
		t.Error("nowdoing-dot class missing — JS expects it on the now-doing line")
	}
	// JS handler hooks the heartbeat from inside handleEventFrame.
	if !strings.Contains(body, "lastEventAt = Date.now()") {
		t.Error("handleEventFrame must stamp lastEventAt — otherwise the heartbeat never advances")
	}
	if !strings.Contains(body, "pulseActiveStep()") {
		t.Error("handleEventFrame must call pulseActiveStep — otherwise no visual confirmation")
	}
	// Boot path wires the heartbeat ticker.
	if !strings.Contains(body, "startHeartbeat()") {
		t.Error("startHeartbeat must be called on boot — otherwise the label stays at `—` forever")
	}
	// Live cost accumulator (2026-05-26 fix): onLLMCallFinished must
	// read p.cost_usd off the payload and update the header counter.
	// Pre-fix the header sat at $0.00 forever while task_llm_usage
	// piled up real spend.
	if !strings.Contains(body, "p.cost_usd") {
		t.Error("onLLMCallFinished must read cost_usd from the payload — without it the header stays at $0.00")
	}
	if !strings.Contains(body, "totalCostUSD += p.cost_usd") {
		t.Error("totalCostUSD must accumulate per-call cost from the live payload")
	}
}

// TestLiveTask_InlineSteeringComposeWireup — Upgrade #3 (2026-05-26)
// replaces the hint modal with an always-visible inline compose box
// carrying a 3-way scope chip toggle. The wire-up depends on:
//   - The compose textarea + send button + scope chips being in the
//     DOM (no modal trigger any more).
//   - The `projectID` JS variable being exposed for task-scope hints.
//   - The submitInlineHint dispatcher recognising all three scopes.
//   - The `?steer=open` query-param auto-focus path.
//
// Pinning here so a future template edit that breaks the contract
// fails loud.
func TestLiveTask_InlineSteeringComposeWireup(t *testing.T) {
	srv, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{
			ID:        id,
			ProjectID: "p1",
			Status:    persistence.TaskStatusRunning,
		}, nil
	}
	execRepo.ListFunc = func(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{
			ID:        "exec_s1",
			TaskID:    "task_s1",
			ProjectID: "p1",
			Status:    persistence.ExecutionStatusRunning,
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/tasks/task_s1/live", nil)
	rr := httptest.NewRecorder()
	srv.TaskLive(rr, req, "task_s1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	body := rr.Body.String()

	// Compose surface is in the DOM.
	for _, want := range []string{
		`id="steer-input"`,
		`id="steer-send"`,
		`data-scope="step"`,
		`data-scope="task"`,
		`data-scope="execution"`,
		"submitInlineHint",
		`Course-correct the next step`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("inline steering compose missing %q — wire-up broken", want)
		}
	}
	// projectID JS variable is required for the task-scope POST URL.
	if !strings.Contains(body, "const projectID =") {
		t.Error("projectID JS variable not exposed — task-scope hints won't be able to construct the URL")
	}
	// The compose must route to the task-scope endpoint when scope=task.
	if !strings.Contains(body, "/projects/' + encodeURIComponent(projectID) +") {
		t.Error("submitInlineHint must call /api/v1/projects/{p}/tasks/{id}/hints when scope=task")
	}
	// `?steer=open` auto-focus path is wired.
	if !strings.Contains(body, `params.get('steer') === 'open'`) {
		t.Error("?steer=open auto-focus path missing — recovery card's Steer+retry button won't land on the compose")
	}
	// `/` keystroke focus is wired.
	if !strings.Contains(body, `e.key !== '/'`) {
		t.Error("`/` keystroke focus shortcut missing — operators lose the GitHub/Linear focus convention")
	}
	// The old hint modal must NOT render (replaced).
	if strings.Contains(body, `id="hint-modal"`) {
		t.Error("hint-modal still rendered — replaced by the inline compose, should be removed")
	}
	// The old fork modal must NOT render — refactor D replaced it
	// with the inline #fork-section details block. Regression
	// guard against accidentally restoring the modal markup.
	if strings.Contains(body, `id="fork-modal"`) {
		t.Error("fork-modal still rendered — replaced by inline #fork-section, should be removed")
	}
	// Clarified scope labels (2026-05-26 follow-up) — operators
	// couldn't tell what "this task" meant. New labels are
	// "this step", "any step", "across retries". Each one MUST be
	// present so the chips visibly distinguish the three behaviours.
	for _, want := range []string{
		">this step<",
		">any step<",
		">across retries<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scope chip label %q missing — operators lose the clarified labels", want)
		}
	}
	// Visible explainer beneath the chips — tooltips alone don't
	// land on mobile. Pinning the explainer element id catches a
	// future template edit that drops the helper text.
	if !strings.Contains(body, `id="steer-scope-explain"`) {
		t.Error("steer-scope-explain element missing — scope explainer line won't render")
	}
	if !strings.Contains(body, "updateSteerScopeExplain") {
		t.Error("updateSteerScopeExplain JS missing — explainer line won't react to chip selection")
	}
}

// TestLiveTask_IsTerminalExecutionStatus — companion check for the
// execution side; PAUSED is non-terminal here even though the task
// may be considered paused-but-not-terminated by the operator.
func TestLiveTask_IsTerminalExecutionStatus(t *testing.T) {
	terminal := []persistence.ExecutionStatus{
		persistence.ExecutionStatusCompleted,
		persistence.ExecutionStatusFailed,
		persistence.ExecutionStatusCancelled,
	}
	for _, s := range terminal {
		if !isTerminalExecutionStatus(s) {
			t.Errorf("expected %q to be terminal", s)
		}
	}
	nonTerminal := []persistence.ExecutionStatus{
		persistence.ExecutionStatusPending,
		persistence.ExecutionStatusRunning,
		persistence.ExecutionStatusPaused,
	}
	for _, s := range nonTerminal {
		if isTerminalExecutionStatus(s) {
			t.Errorf("expected %q to be non-terminal", s)
		}
	}
}
