package registry

import "testing"

// TestValidate_StageChildArtifactsPlacementGuard exercises the Part-B
// structural placement guard for the stage_child_artifacts step flag
// (delegated-child-artifact-handoff design §3.3).
//
// The flag is valid ONLY on a step that is the post-delegation resume
// consumer: the workflow must be resume_after_children AND the declaring step
// must be reachable after a fan-out origin (the resume_after_children
// entrypoint, or any step pinning delegated_workflow) and must NOT itself be a
// fan-out origin. This refuses the real footguns — the flag on a leaf
// workflow, on the decompose/router step, or on the entrypoint — while
// imposing nothing on git workflows that simply never declare it.
func TestValidate_StageChildArtifactsPlacementGuard(t *testing.T) {
	// base builds a generic fan-out → aggregate → done workflow. The mutator
	// tweaks it per case; every case is otherwise structurally valid so the
	// ONLY thing under test is the placement guard.
	base := func() *Workflow {
		return &Workflow{
			ID:                  "guard-test",
			Entrypoint:          "decompose",
			ResumeAfterChildren: true,
			Steps: map[string]WorkflowStep{
				"decompose": {
					Type:              "agent",
					Role:              "planner",
					Prompt:            "split the work",
					OnSuccess:         "aggregate",
					DelegatedWorkflow: "generic-subtask",
				},
				"aggregate": {
					Type:      "agent",
					Role:      "aggregator",
					Prompt:    "combine artifacts/in",
					OnSuccess: "done",
				},
			},
			Terminals: map[string]WorkflowTerminal{
				"done": {Status: "COMPLETED"},
			},
		}
	}

	setFlag := func(w *Workflow, stepID string) {
		s := w.Steps[stepID]
		s.StageChildArtifacts = true
		w.Steps[stepID] = s
	}

	tests := []struct {
		name      string
		build     func() *Workflow
		wantError bool
	}{
		{
			name: "valid: flag on post-delegation consumer of a resume_after_children workflow",
			build: func() *Workflow {
				w := base()
				setFlag(w, "aggregate")
				return w
			},
			wantError: false,
		},
		{
			name: "valid: no flag anywhere is unaffected (regression — lint must not perturb normal workflows)",
			build: func() *Workflow {
				return base()
			},
			wantError: false,
		},
		{
			name: "invalid: flag on a workflow that is NOT resume_after_children",
			build: func() *Workflow {
				w := base()
				w.ResumeAfterChildren = false
				setFlag(w, "aggregate")
				return w
			},
			wantError: true,
		},
		{
			name: "invalid: flag on the fan-out/decompose step itself (a delegator, not the consumer)",
			build: func() *Workflow {
				w := base()
				setFlag(w, "decompose")
				return w
			},
			wantError: true,
		},
		{
			name: "invalid: flag on the entrypoint of a resume_after_children workflow (the fan-out origin)",
			build: func() *Workflow {
				// A strict-route resume_after_children workflow: the entrypoint
				// is the fan-out origin, the flag belongs on the downstream
				// consumer, not here.
				w := &Workflow{
					ID:                  "guard-entry",
					Entrypoint:          "intake",
					ResumeAfterChildren: true,
					Steps: map[string]WorkflowStep{
						"intake": {
							Type:                "agent",
							Role:                "router",
							Prompt:              "route",
							OnSuccess:           "publish",
							StageChildArtifacts: true,
						},
						"publish": {
							Type:      "agent",
							Role:      "publisher",
							Prompt:    "publish",
							OnSuccess: "done",
						},
					},
					Terminals: map[string]WorkflowTerminal{
						"done": {Status: "COMPLETED"},
					},
				}
				return w
			},
			wantError: true,
		},
		{
			name: "invalid: flag on a leaf (non-delegating, non-resume) workflow",
			build: func() *Workflow {
				w := &Workflow{
					ID:         "guard-leaf",
					Entrypoint: "work",
					Steps: map[string]WorkflowStep{
						"work": {
							Type:                "agent",
							Role:                "coder",
							Prompt:              "do it",
							OnSuccess:           "done",
							StageChildArtifacts: true,
						},
					},
					Terminals: map[string]WorkflowTerminal{
						"done": {Status: "COMPLETED"},
					},
				}
				return w
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.build().Validate(tt.name + ".md")
			if tt.wantError && err == nil {
				t.Errorf("expected a validation error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}
