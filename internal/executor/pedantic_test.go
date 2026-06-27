package executor

import (
	"testing"

	"vornik.io/vornik/internal/registry"
)

// TestResolvePedantic_PrecedenceLadder covers the narrower-wins
// ladder from swarm-recovery-design.md §6:
//
//	task > workflow > project > default(false).
//
// "Set" means the *bool is non-nil; an absent field reads as nil
// and defers to the next-broader scope.
func TestResolvePedantic_PrecedenceLadder(t *testing.T) {
	pT := true
	pF := false

	cases := []struct {
		name     string
		payload  []byte
		workflow *registry.Workflow
		project  *registry.Project
		want     bool
	}{
		{
			name: "all nil → default false",
			want: false,
		},
		{
			name:    "project true, nothing narrower → true",
			project: &registry.Project{Pedantic: &pT},
			want:    true,
		},
		{
			name:     "workflow false overrides project true",
			workflow: &registry.Workflow{Pedantic: &pF},
			project:  &registry.Project{Pedantic: &pT},
			want:     false,
		},
		{
			name:     "task true overrides workflow false + project true",
			payload:  []byte(`{"context":{"pedantic":true}}`),
			workflow: &registry.Workflow{Pedantic: &pF},
			project:  &registry.Project{Pedantic: &pT},
			want:     true,
		},
		{
			name:    "task false overrides project true",
			payload: []byte(`{"context":{"pedantic":false}}`),
			project: &registry.Project{Pedantic: &pT},
			want:    false,
		},
		{
			name:     "workflow true wins over absent project",
			workflow: &registry.Workflow{Pedantic: &pT},
			want:     true,
		},
		{
			name:     "malformed payload falls through to workflow",
			payload:  []byte("not json"),
			workflow: &registry.Workflow{Pedantic: &pT},
			want:     true,
		},
		{
			name:    "payload without pedantic key falls through to project",
			payload: []byte(`{"context":{"prompt":"hi"}}`),
			project: &registry.Project{Pedantic: &pT},
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePedantic(tc.payload, tc.workflow, tc.project)
			if got != tc.want {
				t.Errorf("resolvePedantic(%q, %+v, %+v) = %v, want %v",
					tc.payload, tc.workflow, tc.project, got, tc.want)
			}
		})
	}
}

// TestResolvePedantic_NilSafe makes sure we don't panic on nil
// inputs — the executor calls this from the on_fail hot path and
// any nil deref there bricks the entire execution.
func TestResolvePedantic_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("resolvePedantic panicked on nil inputs: %v", r)
		}
	}()
	if resolvePedantic(nil, nil, nil) {
		t.Error("nil inputs must resolve to false (default: recovery on)")
	}
}
