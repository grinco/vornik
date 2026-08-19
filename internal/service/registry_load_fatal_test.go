package service

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Regression, measured 2026-08-18 on the benchmark host.
//
// Registry.Load is Stage → StripInvalidFromStaged → ActivateStaged, and BOTH a
// stage failure and a project-strip return a *registry.ValidationError. The
// service container treated every ValidationError as a warning ("invalid
// projects skipped") — correct for the strip, catastrophic for the stage: a
// stage failure returns BEFORE anything is activated, so the registry holds no
// swarms and no workflows.
//
// One bad role in one swarm therefore produced: warning logged, daemon serves,
// executor.resolveExecutionPlan substitutes a synthetic single-step "worker"
// workflow for every task, and the ledger still records the workflow_id the
// task ASKED for. Three benchmark tasks reported COMPLETED having run a
// "Process task <id>" prompt instead of the workflow they named — a measurement
// that scored a workflow which never executed.
//
// This pins the distinction: a registry that activated nothing must refuse to
// start, whatever error class it returned.
func TestRegistryLoad_StageFailureLeavesNothingActivated(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A swarm that fails validation: a role naming a step type the loader
	// rejects is enough — the point is that STAGING fails.
	bad := "---\nswarmId: broken\nleadRole: \"nope\"\nroles: []\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "broken.md"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	reg := registry.New()
	err := reg.Load(dir)
	if err == nil {
		t.Skip("this fixture no longer fails staging; the guard below still holds for real failures")
	}

	// The load failed. The invariant the container now enforces:
	if len(reg.ListSwarms()) != 0 || len(reg.ListWorkflows()) != 0 {
		t.Fatalf("expected nothing activated after a stage failure, got %d swarms / %d workflows",
			len(reg.ListSwarms()), len(reg.ListWorkflows()))
	}
	// A container seeing this MUST refuse to start rather than warn — serving
	// here means every task silently runs the synthetic worker workflow.
	if !registryActivatedNothing(reg) {
		t.Error("a registry with no swarms and no workflows must be judged unusable")
	}
}

// A registry that DID activate real content stays serviceable even when the
// load reported stripped projects — the tolerance the original warning was
// written for, which this fix must not remove.
func TestRegistryLoad_ActivatedRegistryStaysServiceable(t *testing.T) {
	reg := registry.New()
	if err := reg.Load(filepath.Join("..", "..", "configs")); err != nil {
		t.Logf("shipped configs loaded with warnings: %v", err)
	}
	if len(reg.ListSwarms()) == 0 || len(reg.ListWorkflows()) == 0 {
		t.Fatal("shipped configs must activate swarms and workflows")
	}
	if registryActivatedNothing(reg) {
		t.Error("a registry with shipped swarms and workflows must be serviceable")
	}
}
