package dispatcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestJaccardTokenSimilarity locks in the threshold tuning that
// the fuzzy dedup relies on. The thresholds in this table were
// chosen to match production cases (the T-af29 / T-6e62 incident
// from 2026-05-10) without merging genuinely-different requests.
func TestJaccardTokenSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		// want is a tight expected range so an over-eager change
		// to the function's metric breaks the test.
		min, max float64
	}{
		{
			name: "identical_strings_return_1",
			a:    "ingest the cv content into project memory",
			b:    "ingest the cv content into project memory",
			min:  1.0, max: 1.0,
		},
		{
			name: "near_dup_with_one_inserted_phrase",
			// Real T-af29 vs T-6e62 case (2026-05-10): one had
			// "(if any)" the other didn't. The two extra tokens
			// drag the metric to ~0.87, which still clears the
			// production 0.85 threshold and triggers dedup.
			a:   "ingest toby sheldon's cv content into project memory if any use tools from the swarm memory module",
			b:   "ingest toby sheldon's cv content into project memory use tools from the swarm memory module",
			min: 0.85, max: 1.0,
		},
		{
			name: "completely_different_requests_low_similarity",
			a:    "build a python web scraper for ebay listings",
			b:    "summarise the latest pull requests on github",
			min:  0.0, max: 0.20,
		},
		{
			name: "empty_input_returns_zero",
			a:    "", b: "anything at all",
			min: 0.0, max: 0.0,
		},
		{
			name: "subset_string_lower_than_threshold",
			// Two prompts about the same topic but very
			// different phrasing should NOT dedup at 0.85.
			a:   "scout the project for unused dependencies",
			b:   "list every package in go.mod and remove ones not imported anywhere",
			min: 0.0, max: 0.30,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jaccardTokenSimilarity(c.a, c.b)
			if got < c.min || got > c.max {
				t.Errorf("jaccardTokenSimilarity(%q, %q) = %v; want in [%v, %v]",
					c.a, c.b, got, c.min, c.max)
			}
		})
	}
}

// TestFindRecentDuplicateTask_DedupsParaphrasedRetry pins the
// exact regression that surfaced as the Toby Sheldon CV
// double-create on 2026-05-10. The dispatcher LLM fired
// create_task twice in 14 seconds with paraphrased prompts that
// differed only in a parenthetical "(if any)". Pre-fix, the
// strict-equality path missed it and a duplicate task hit the
// queue. Post-fix, the second call is suppressed.
func TestFindRecentDuplicateTask_DedupsParaphrasedRetry(t *testing.T) {
	const projectID = "janka"
	firstPrompt := "Ingest Toby Sheldon's CV content into project memory (if any). Use tools from the /swarm/memory module."
	secondPrompt := "Ingest Toby Sheldon's CV content into project memory. Use tools from the /swarm/memory module."

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_existing_first_create",
					ProjectID: projectID,
					Status:    persistence.TaskStatusQueued,
					Payload:   payloadWithPrompt(t, firstPrompt),
					CreatedAt: time.Now().Add(-5 * time.Second),
				},
			}, nil
		},
	}

	got := findRecentDuplicateTask(context.Background(), repo, projectID, secondPrompt, 30*time.Second)
	if got != "task_existing_first_create" {
		t.Errorf("expected dedup to suppress the paraphrased retry, got %q", got)
	}
}

// TestFindRecentDuplicateTask_DistinctRequestsNotDeduped confirms
// the threshold doesn't over-suppress: two genuinely different
// user requests still get their own tasks.
func TestFindRecentDuplicateTask_DistinctRequestsNotDeduped(t *testing.T) {
	const projectID = "vornik-autocoder"

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_first",
					ProjectID: projectID,
					Status:    persistence.TaskStatusRunning,
					Payload:   payloadWithPrompt(t, "Add tests for internal/api/handlers.go targeting the highest-leverage uncovered code"),
					CreatedAt: time.Now().Add(-10 * time.Second),
				},
			}, nil
		},
	}

	other := "Refresh the coverage report and pick the next NEEDS_TESTS file from internal/runtime/"
	got := findRecentDuplicateTask(context.Background(), repo, projectID, other, 30*time.Second)
	if got != "" {
		t.Errorf("distinct requests must not be deduped; got %q", got)
	}
}

// TestFindRecentDuplicateTask_TerminalTasksIgnored verifies the
// active-status guard: a completed/failed task with a matching
// prompt does NOT block a fresh re-run minutes later. Otherwise
// a user re-asking the same question after a failure would
// silently get pointed at the failed task.
func TestFindRecentDuplicateTask_TerminalTasksIgnored(t *testing.T) {
	const projectID = "headmatch"
	prompt := "Run the daily candidate scan"

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_failed_earlier",
					ProjectID: projectID,
					Status:    persistence.TaskStatusFailed,
					Payload:   payloadWithPrompt(t, prompt),
					CreatedAt: time.Now().Add(-5 * time.Second),
				},
			}, nil
		},
	}

	got := findRecentDuplicateTask(context.Background(), repo, projectID, prompt, 30*time.Second)
	if got != "" {
		t.Errorf("terminal tasks must NOT block re-runs; got %q", got)
	}
}

// TestFindRecentDuplicateTask_OutsideWindowIgnored — a stale
// matching task outside the dedup window is past the point
// where coalescing helps the operator. A second creation is the
// right answer.
func TestFindRecentDuplicateTask_OutsideWindowIgnored(t *testing.T) {
	const projectID = "snake"
	prompt := "Run the daily training pass"

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_old_running",
					ProjectID: projectID,
					Status:    persistence.TaskStatusRunning,
					Payload:   payloadWithPrompt(t, prompt),
					CreatedAt: time.Now().Add(-2 * time.Minute), // outside the 30s window
				},
			}, nil
		},
	}

	got := findRecentDuplicateTask(context.Background(), repo, projectID, prompt, 30*time.Second)
	if got != "" {
		t.Errorf("matches outside the window must not dedup; got %q", got)
	}
}

// TestFindSameTurnDuplicateTask_DedupsCompletedSibling reproduces
// the 2026-05-21 watchlist incident: the dispatcher created T-0918
// inside one turn, T-0918 COMPLETED, and 38 s later the same turn
// fired create_task again for nearly the same prompt — past the
// 30 s window of findRecentDuplicateTask. findSameTurnDuplicateTask
// must catch this hit regardless of age + status, because the dispatcher
// should be re-reading T-0918's artifacts rather than re-scheduling.
func TestFindSameTurnDuplicateTask_DedupsCompletedSibling(t *testing.T) {
	const projectID = "ibkr-trader"
	const turn = "chat_20260521190824_aaaa"
	firstPrompt := "Add high-volatility symbols TQQQ, GME, SQQQ, SOXX, QQQ to the ibkr-trader watchlist (replacing TM, SAP, INFY, JPM, SHEL)."
	secondPrompt := "Add high-volatility symbols TQQQ, GME, SQQQ, SOXX, QQQ to the ibkr-trader watchlist, replacing TM, SAP, INFY, JPM, SHEL."

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			turnPtr := turn
			return []*persistence.Task{
				{
					ID:         "task_0918_completed",
					ProjectID:  projectID,
					Status:     persistence.TaskStatusCompleted,
					Payload:    payloadWithPrompt(t, firstPrompt),
					CreatedAt:  time.Now().Add(-2 * time.Minute), // outside the 30s window
					ChatTurnID: &turnPtr,
				},
			}, nil
		},
	}

	got := findSameTurnDuplicateTask(context.Background(), repo, projectID, turn, secondPrompt)
	if got != "task_0918_completed" {
		t.Errorf("same-turn dedup must catch completed sibling regardless of age; got %q", got)
	}
}

// TestFindSameTurnDuplicateTask_DifferentTurnIgnored — a matching
// prompt from a DIFFERENT chat turn must not dedup, even if the
// other task is still active. Otherwise asking the same question
// in a fresh conversation would silently get pointed at someone
// else's old task.
func TestFindSameTurnDuplicateTask_DifferentTurnIgnored(t *testing.T) {
	const projectID = "ibkr-trader"
	prompt := "Run a trading tick"

	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			otherTurn := "chat_OTHER_turn"
			return []*persistence.Task{
				{
					ID:         "task_other_turn",
					ProjectID:  projectID,
					Status:     persistence.TaskStatusRunning,
					Payload:    payloadWithPrompt(t, prompt),
					CreatedAt:  time.Now().Add(-5 * time.Second),
					ChatTurnID: &otherTurn,
				},
			}, nil
		},
	}

	got := findSameTurnDuplicateTask(context.Background(), repo, projectID, "chat_MY_turn", prompt)
	if got != "" {
		t.Errorf("different-turn match must not dedup; got %q", got)
	}
}

// TestFindSameTurnDuplicateTask_NoTurnIDStillSafe — when the
// candidate task has nil ChatTurnID (legacy / API-initiated), it
// must NOT match the same-turn check. Only tasks explicitly tagged
// with the same turn id qualify.
func TestFindSameTurnDuplicateTask_NoTurnIDStillSafe(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{
				{
					ID:        "task_legacy_no_turn",
					ProjectID: "p1",
					Status:    persistence.TaskStatusRunning,
					Payload:   payloadWithPrompt(t, "do thing"),
					CreatedAt: time.Now(),
					// ChatTurnID: nil
				},
			}, nil
		},
	}
	got := findSameTurnDuplicateTask(context.Background(), repo, "p1", "chat_turn", "do thing")
	if got != "" {
		t.Errorf("task with nil ChatTurnID must never satisfy same-turn dedup; got %q", got)
	}
}

// TestFindSameTurnDuplicateTask_GuardEmptyInputs — empty turn id or
// empty prompt short-circuits to "" without hitting the repo. This
// is what makes the legacy window-based dedup the fallback for
// non-chat call paths (no behaviour change for API/autonomy).
func TestFindSameTurnDuplicateTask_GuardEmptyInputs(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, _ persistence.TaskFilter) ([]*persistence.Task, error) {
			t.Fatal("repo.List should not be called when guards trip")
			return nil, nil
		},
	}
	if got := findSameTurnDuplicateTask(context.Background(), repo, "p", "", "prompt"); got != "" {
		t.Errorf("empty turn id should short-circuit; got %q", got)
	}
	if got := findSameTurnDuplicateTask(context.Background(), repo, "p", "turn", ""); got != "" {
		t.Errorf("empty prompt should short-circuit; got %q", got)
	}
	if got := findSameTurnDuplicateTask(context.Background(), nil, "p", "turn", "x"); got != "" {
		t.Errorf("nil repo should short-circuit; got %q", got)
	}
}

func payloadWithPrompt(t *testing.T, prompt string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"prompt": prompt,
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}
