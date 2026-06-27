package executor

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestTaskWillRetry covers the predicate that gates every
// terminal-FAILED decision in handleFailure. The 2026-05-05 user
// observation — "tasks finalize FAILED after a single execution
// attempt despite MaxAttempts=3" — turned out to be a separate
// path (checkParentUnblock pre-a769cca), but this predicate is
// the canonical place to ensure budget arithmetic stays correct.
// Cheap to test directly because it has no side effects.
func TestTaskWillRetry(t *testing.T) {
	e := &Executor{}

	cases := []struct {
		name      string
		task      *persistence.Task
		wantRetry bool
	}{
		{
			name:      "nil task → no retry",
			task:      nil,
			wantRetry: false,
		},
		{
			name:      "MaxAttempts=0 → no retry (legacy / unconfigured)",
			task:      &persistence.Task{Attempt: 1, MaxAttempts: 0},
			wantRetry: false,
		},
		{
			name:      "MaxAttempts<0 → no retry",
			task:      &persistence.Task{Attempt: 1, MaxAttempts: -1},
			wantRetry: false,
		},
		{
			name:      "first attempt of a 3-attempt task → retry",
			task:      &persistence.Task{Attempt: 1, MaxAttempts: 3},
			wantRetry: true,
		},
		{
			name:      "second attempt of a 3-attempt task → retry",
			task:      &persistence.Task{Attempt: 2, MaxAttempts: 3},
			wantRetry: true,
		},
		{
			name:      "final attempt → no retry",
			task:      &persistence.Task{Attempt: 3, MaxAttempts: 3},
			wantRetry: false,
		},
		{
			name:      "MaxAttempts=1 → no retry on first attempt (operator opted out)",
			task:      &persistence.Task{Attempt: 1, MaxAttempts: 1},
			wantRetry: false,
		},
		{
			name:      "Attempt above MaxAttempts (shouldn't happen but defensive) → no retry",
			task:      &persistence.Task{Attempt: 5, MaxAttempts: 3},
			wantRetry: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := e.taskWillRetry(tc.task)
			if got != tc.wantRetry {
				t.Errorf("taskWillRetry: got %v, want %v (Attempt=%d, MaxAttempts=%d)",
					got, tc.wantRetry,
					attemptOrZero(tc.task), maxAttemptsOrZero(tc.task))
			}
		})
	}
}

func attemptOrZero(t *persistence.Task) int {
	if t == nil {
		return 0
	}
	return t.Attempt
}
func maxAttemptsOrZero(t *persistence.Task) int {
	if t == nil {
		return 0
	}
	return t.MaxAttempts
}
