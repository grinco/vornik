package executor

// TDD for item 5 of the dynamic-tool-budget follow-ups
// (https://docs.vornik.io §5):
// an operator-initiated task must keep operator budget headroom across a
// checkpoint-retry. resolveBudgetAutonomous resolves the ORIGIN creation source
// by walking the checkpoint parent chain; anything unresolvable fails safe to
// autonomous (true) so a lookup miss can never WIDEN the budget.

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

func ptr(s string) *string { return &s }

// fakeTaskGetter serves tasks from a map; a missing id returns an error so the
// fail-safe path is exercised.
func fakeTaskGetter(m map[string]*persistence.Task) taskGetter {
	return func(_ context.Context, id string) (*persistence.Task, error) {
		if t, ok := m[id]; ok {
			return t, nil
		}
		return nil, errors.New("task not found")
	}
}

func TestResolveBudgetAutonomous(t *testing.T) {
	// Chain fixtures.
	userRoot := &persistence.Task{ID: "u1", CreationSource: persistence.TaskCreationSourceUser}
	autoRoot := &persistence.Task{ID: "a1", CreationSource: persistence.TaskCreationSourceAutonomous}
	// one-hop checkpoints
	cpOfUser := &persistence.Task{ID: "c1", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("u1")}
	cpOfAuto := &persistence.Task{ID: "c2", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("a1")}
	// two-deep checkpoint chain rooted at the user task: cpDeep -> cpOfUser -> userRoot
	cpDeep := &persistence.Task{ID: "c3", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("c1")}
	// checkpoint whose parent id is missing from the store (lookup miss)
	cpOrphan := &persistence.Task{ID: "c4", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("gone")}
	// checkpoint with no parent at all
	cpNoParent := &persistence.Task{ID: "c5", CreationSource: persistence.TaskCreationSourceCheckpoint}
	// cycle: c6 -> c7 -> c6 (both checkpoints) — must not loop forever, fails safe
	cpCycleA := &persistence.Task{ID: "c6", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("c7")}
	cpCycleB := &persistence.Task{ID: "c7", CreationSource: persistence.TaskCreationSourceCheckpoint, ParentTaskID: ptr("c6")}

	store := map[string]*persistence.Task{
		"u1": userRoot, "a1": autoRoot,
		"c1": cpOfUser, "c2": cpOfAuto, "c3": cpDeep,
		"c6": cpCycleA, "c7": cpCycleB,
	}
	get := fakeTaskGetter(store)
	log := zerolog.Nop()

	cases := []struct {
		name string
		task *persistence.Task
		want bool // want autonomous?
	}{
		{"user task is not autonomous", userRoot, false},
		{"autonomy task is autonomous", autoRoot, true},
		{"checkpoint of user keeps operator headroom", cpOfUser, false},
		{"checkpoint of autonomy stays autonomous", cpOfAuto, true},
		{"two-deep checkpoint rooted at user", cpDeep, false},
		{"checkpoint with missing parent fails safe", cpOrphan, true},
		{"checkpoint with no parent fails safe", cpNoParent, true},
		{"checkpoint cycle fails safe (no infinite loop)", cpCycleA, true},
		{"nil task fails safe", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBudgetAutonomous(context.Background(), tc.task, get, log)
			if got != tc.want {
				t.Errorf("resolveBudgetAutonomous(%v) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
