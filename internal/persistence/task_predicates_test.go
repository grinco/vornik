package persistence

import "testing"

func strPtr(s string) *string { return &s }

func TestTask_EndedUnsuccessfully(t *testing.T) {
	boom := strPtr("step implement visited 4 times (max 3) — likely infinite rework loop")
	cases := []struct {
		name string
		task *Task
		want bool
	}{
		{"nil", nil, false},
		{"failed", &Task{Status: TaskStatusFailed}, true},
		{"cancelled", &Task{Status: TaskStatusCancelled}, true},
		{"closed with LastError", &Task{Status: TaskStatusClosed, LastError: boom}, true},
		{"closed with LastErrorClass", &Task{Status: TaskStatusClosed, LastErrorClass: strPtr("MERGE_FAILED")}, true},
		{"closed with empty LastError", &Task{Status: TaskStatusClosed, LastError: strPtr("")}, false},
		{"closed clean (success-close)", &Task{Status: TaskStatusClosed}, false},
		{"completed", &Task{Status: TaskStatusCompleted}, false},
		{"running", &Task{Status: TaskStatusRunning}, false},
		{"queued", &Task{Status: TaskStatusQueued}, false},
		{"awaiting input", &Task{Status: TaskStatusAwaitingInput}, false},
		{"awaiting external", &Task{Status: TaskStatusAwaitingExternal}, false},
		{"paused", &Task{Status: TaskStatusPaused}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.task.EndedUnsuccessfully(); got != tc.want {
				t.Errorf("EndedUnsuccessfully() = %v, want %v", got, tc.want)
			}
		})
	}
}
