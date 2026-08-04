package registry

import (
	"path/filepath"
	"testing"
)

// Output + outcome contracts of the SHIPPED workflows.
//
// CUSTOMER REPORT 2026-08-03: a `plan-and-write` task reached COMPLETED with
// `artifacts: []`. The researcher/planner/writer schemas legitimately permit
// `written: false` + a reason (the correct way for a role to decline), every
// agent step transitioned on unconditional `on_success`, and no step declared a
// file-output contract — so a clean refusal walked the whole chain to the
// COMPLETED terminal and the task "succeeded" with nothing to show.
//
// The same class bit US on 2026-08-04: `companion-architectural-review`
// (task_20260804001426_cc1b2817803467fa) returned COMPLETED with no review at
// all, which for a workflow used as a MERGE GATE is indistinguishable from a
// clean pass.
//
// Two mechanisms close it, and these tests pin both against the shipped tree:
//
//  1. `require_output_glob` — already built and enforced (executor
//     container.go) for the identical incident class, but adopted by only three
//     workflows. Every agent step whose prompt promises a NAMED output file now
//     declares it.
//  2. Outcome gates on `plan-and-write` — `written == false` routes to the
//     recovery hop instead of advancing. Note gates and `on_success` are
//     mutually exclusive on an agent step (Validate rejects both, because the
//     executor short-circuits on `on_success` before evaluating gates), so the
//     gated steps carry gates ONLY.

func loadShippedWorkflows(t *testing.T) map[string]*Workflow {
	t.Helper()
	// LoadWorkflows takes the configs ROOT and appends workflows/ itself.
	dir, err := filepath.Abs("../../configs")
	if err != nil {
		t.Fatalf("resolve shipped workflows dir: %v", err)
	}
	wfs, err := LoadWorkflows(dir)
	if err != nil {
		t.Fatalf("LoadWorkflows(%s): %v", dir, err)
	}
	if len(wfs) == 0 {
		t.Fatalf("no shipped workflows loaded from %s", dir)
	}
	return wfs
}

// TestShippedWorkflows_RequireOutputGlob pins the file-output contract for every
// agent step that promises a named artifact.
//
// Globs are deliberately FILENAME-SPECIFIC, never wildcards: in a multi-step
// static workflow the prior steps' outputs are re-staged into this step's
// ephemeral `artifacts/out/` while the step runs, so a wildcard like
// `artifacts/out/*.md` would be satisfied by a re-staged upstream file and pass
// vacuously. A step whose own output name is variable (`<short-slug>.md`) is
// pinned on its deterministic companion file instead (e.g. `summary.txt`).
func TestShippedWorkflows_RequireOutputGlob(t *testing.T) {
	want := map[string]map[string]string{
		"companion-architectural-review": {"review": "artifacts/out/review.md"},
		"companion-data-validation":      {"validate": "artifacts/out/findings.md"},
		"companion-doc-review":           {"review": "artifacts/out/review.md"},
		"companion-report-summarize":     {"summarize": "artifacts/out/summary.md"},
		"companion-research-gather":      {"gather": "artifacts/out/findings.md"},
		"companion-test-coverage-audit":  {"audit": "artifacts/out/review.md"},
		"deep-research":                  {"synthesize": "artifacts/out/deliverable.md"},
		"ingest":                         {"ingest": "artifacts/out/ingestion.md"},
		"parallel-research":              {"synthesize": "artifacts/out/deliverable.md"},
		"research-subtask":               {"research": "artifacts/out/findings.md"},
		"dev-pipeline": {
			"report":             "artifacts/out/CHANGELOG.md",
			"checkpoint-report":  "artifacts/out/CHANGELOG-partial.md",
			"recover-checkpoint": "artifacts/out/CHANGELOG-partial.md",
		},
		"plan-and-write": {
			"research": "artifacts/out/research.md",
			"plan":     "artifacts/out/plan.md",
			// The writer's deliverable name is caller-dependent
			// (`<short-slug>.md`); summary.txt is its deterministic companion
			// and is produced by no other step.
			"write": "artifacts/out/summary.txt",
		},
		"research": {
			"research": "artifacts/out/research.md",
			"write":    "artifacts/out/summary.txt",
		},
		"research-and-publish": {
			"research": "artifacts/out/research.md",
			"write":    "artifacts/out/deliverable.md",
		},
	}

	wfs := loadShippedWorkflows(t)
	for wfID, steps := range want {
		wf, ok := wfs[wfID]
		if !ok {
			t.Errorf("shipped workflow %q not found", wfID)
			continue
		}
		for stepID, glob := range steps {
			step, ok := wf.Steps[stepID]
			if !ok {
				t.Errorf("%s: step %q not found", wfID, stepID)
				continue
			}
			if step.RequireOutputGlob != glob {
				t.Errorf("%s/%s require_output_glob = %q, want %q — a step that promises a file must fail when it writes none",
					wfID, stepID, step.RequireOutputGlob, glob)
			}
		}
	}
}

// TestShippedWorkflows_NoVacuousOutputGlob keeps the wildcard hazard out: a
// glob containing `*` in a multi-step workflow can be satisfied by a re-staged
// upstream artifact, which is a contract that always passes.
func TestShippedWorkflows_NoVacuousOutputGlob(t *testing.T) {
	for wfID, wf := range loadShippedWorkflows(t) {
		for stepID, step := range wf.Steps {
			g := step.RequireOutputGlob
			if g == "" {
				continue
			}
			for _, c := range g {
				if c == '*' || c == '?' {
					t.Errorf("%s/%s require_output_glob = %q contains a wildcard — upstream artifacts re-staged into artifacts/out/ during this step would satisfy it vacuously; pin the exact filename",
						wfID, stepID, g)
					break
				}
			}
		}
	}
}

// TestPlanAndWrite_GatesDeclinedOutcomes is the direct regression for the
// customer's report: each agent step must route `written == false` to the
// recovery hop and must NOT carry an unconditional `on_success`.
func TestPlanAndWrite_GatesDeclinedOutcomes(t *testing.T) {
	wf, ok := loadShippedWorkflows(t)["plan-and-write"]
	if !ok {
		t.Fatal("plan-and-write not found in the shipped tree")
	}

	for _, tc := range []struct {
		step, field, onTrue string
	}{
		{"research", "research.written", "plan"},
		{"plan", "planning.written", "write"},
		{"write", "writing.written", "done"},
	} {
		step, ok := wf.Steps[tc.step]
		if !ok {
			t.Errorf("step %q missing", tc.step)
			continue
		}
		// on_success would short-circuit BEFORE the gates (executor
		// workflow.go) — Validate rejects both, and silently shadowed gates
		// were the 2026-06-13 resume-gate incident.
		if step.OnSuccess != "" {
			t.Errorf("%s: on_success = %q must be empty when gates decide the transition", tc.step, step.OnSuccess)
		}
		if len(step.Gates) < 2 {
			t.Errorf("%s: want gates for both written==true and written==false, got %d", tc.step, len(step.Gates))
			continue
		}
		var sawTrue, sawFalse bool
		for _, g := range step.Gates {
			switch g.Condition {
			case tc.field + " == true":
				sawTrue = true
				if g.Target != tc.onTrue {
					t.Errorf("%s: written==true routes to %q, want %q", tc.step, g.Target, tc.onTrue)
				}
			case tc.field + " == false":
				sawFalse = true
				if g.Target != "recover" {
					t.Errorf("%s: written==false routes to %q, want recover — a declared refusal must never reach a COMPLETED terminal", tc.step, g.Target)
				}
			}
		}
		if !sawTrue || !sawFalse {
			t.Errorf("%s: missing gate (%s==true: %v, ==false: %v)", tc.step, tc.field, sawTrue, sawFalse)
		}
		// The recovery hop must still exist for hard failures.
		if step.OnFail != "recover" {
			t.Errorf("%s: on_fail = %q, want recover", tc.step, step.OnFail)
		}
	}
}
