package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func baseWF() *Workflow {
	return &Workflow{
		ID:           "research",
		Entrypoint:   "scout",
		MaxWallClock: "1h", MaxStepVisits: 3, MaxIterations: 20,
		Steps: map[string]WorkflowStep{
			"scout": {
				Type: "agent", Role: "researcher", Prompt: "go",
				OnSuccess: "write", Timeout: "30m",
				RetryPolicy: WorkflowRetryPolicy{MaxRetries: 2, Backoff: "1m"},
			},
		},
	}
}

func TestWorkflowChangeIsStructural_TuningOnly(t *testing.T) {
	// janka incident cpp_20260723033332: a reclaim-timeout edit changes only
	// a step Timeout and must NOT read as structural (it can apply live).
	a := baseWF()
	b := baseWF()
	st := b.Steps["scout"]
	st.Timeout = "10m" // reclaim over-provisioned timeout
	b.Steps["scout"] = st
	if workflowChangeIsStructural(a, b) {
		t.Fatal("step-timeout-only change must not be structural")
	}
}

func TestWorkflowChangeIsStructural_TuningFields(t *testing.T) {
	for _, mut := range []func(*Workflow){
		func(w *Workflow) { w.MaxWallClock = "2h" },
		func(w *Workflow) { w.MaxStepVisits = 5 },
		func(w *Workflow) { w.MaxIterations = 40 },
		func(w *Workflow) {
			st := w.Steps["scout"]
			st.RetryPolicy = WorkflowRetryPolicy{MaxRetries: 9, Backoff: "5m"}
			w.Steps["scout"] = st
		},
	} {
		a, b := baseWF(), baseWF()
		mut(b)
		if workflowChangeIsStructural(a, b) {
			t.Errorf("tuning-only mutation must not be structural: %+v", b)
		}
	}
}

func TestWorkflowChangeIsStructural_Structural(t *testing.T) {
	for name, mut := range map[string]func(*Workflow){
		"prompt":      func(w *Workflow) { st := w.Steps["scout"]; st.Prompt = "different"; w.Steps["scout"] = st },
		"role":        func(w *Workflow) { st := w.Steps["scout"]; st.Role = "writer"; w.Steps["scout"] = st },
		"entrypoint":  func(w *Workflow) { w.Entrypoint = "write" },
		"add-step":    func(w *Workflow) { w.Steps["extra"] = WorkflowStep{Type: "agent", Role: "x"} },
		"remove-step": func(w *Workflow) { delete(w.Steps, "scout") },
	} {
		a, b := baseWF(), baseWF()
		mut(b)
		if !workflowChangeIsStructural(a, b) {
			t.Errorf("%s change must be structural", name)
		}
	}
}

func TestWorkflowChangeIsStructural_TuningPlusStructural(t *testing.T) {
	// Fail-closed: a timeout change bundled with a prompt change is structural.
	a, b := baseWF(), baseWF()
	st := b.Steps["scout"]
	st.Timeout = "5m"
	st.Prompt = "changed too"
	b.Steps["scout"] = st
	if !workflowChangeIsStructural(a, b) {
		t.Fatal("tuning+structural must be structural (fail-closed)")
	}
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func TestDiffStaged_TimeoutOnlyNotStructural(t *testing.T) {
	// Mirror the staging setup of TestRegistryDiffStaged (registry_test.go):
	// write configs to a temp dir, Load, then rewrite ONE workflow's step
	// timeout, Stage the same dir, DiffStaged.
	tmpDir, err := os.MkdirTemp("", "registry-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	writeRegistryFixture(t, tmpDir, map[string]string{
		"swarms/test.md": `---
swarmId: "test-swarm"
roles:
  - name: "coder"
    runtime:
      image: "test:latest"
---
`,
		"workflows/research.md": `---
workflowId: "research"
entrypoint: "scout"
steps:
  scout:
    type: "agent"
    prompt: "go"
    role: "coder"
    timeout: "30m"
terminals:
  done:
    status: "COMPLETED"
---
`,
	})

	reg := New()
	if err := reg.Load(tmpDir); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "workflows", "research.md"), []byte(`---
workflowId: "research"
entrypoint: "scout"
steps:
  scout:
    type: "agent"
    prompt: "go"
    role: "coder"
    timeout: "10m"
terminals:
  done:
    status: "COMPLETED"
---
`), 0644); err != nil {
		t.Fatalf("failed to rewrite workflow: %v", err)
	}

	if err := reg.Stage(tmpDir); err != nil {
		t.Fatalf("Stage failed: %v", err)
	}

	diff, err := reg.DiffStaged()
	if err != nil {
		t.Fatalf("DiffStaged failed: %v", err)
	}

	if !contains(diff.ChangedWorkflows, "research") {
		t.Fatalf("expected \"research\" in ChangedWorkflows, got %#v", diff.ChangedWorkflows)
	}
	if contains(diff.StructurallyChangedWorkflows, "research") {
		t.Fatalf("timeout-only change must not be in StructurallyChangedWorkflows, got %#v", diff.StructurallyChangedWorkflows)
	}
}
