package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStripInvalidProjects_RejectsGateOnPathNotDeclaredInSchema is
// the end-to-end compat check: a workflow step gates on a path the
// role's outputSchema doesn't declare. stripInvalidProjects must
// catch this at config load and remove the offending project,
// surfacing a clear error message that names role + path.
//
// Item 11 of https://docs.vornik.io Without this
// check the runtime gate evaluation silently goes to "no match" — the
// workflow either stalls or falls through to a default branch
// depending on operator config, and the operator only finds out when
// a task hangs.
func TestStripInvalidProjects_RejectsGateOnPathNotDeclaredInSchema(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, map[string]string{
		"swarms/bad-swarm.md": `---
swarmId: "bad-swarm"
roles:
  - name: "tester"
    runtime: { image: "x" }
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [testing]
      properties:
        testing:
          type: object
          required: [passed]
          properties:
            passed: { type: bool }
---
`,
		"workflows/bad-pipeline.md": `---
workflowId: "bad-pipeline"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "run tests"
    gates:
      - condition: "testing.acceptance_met == true"  # NOT declared
        target: "complete"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
		"projects/bad-project.yaml": `projectId: "bad-project"
swarmId: "bad-swarm"
defaultWorkflowId: "bad-pipeline"
`,
	})

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	verr := r.StripInvalidFromStaged()
	if verr == nil {
		t.Fatal("expected ValidationError, got nil — the schema-vs-gate mismatch was not caught")
	}
	// Error message must name the offending path AND role so the
	// operator can fix it without re-deriving the failing case.
	combined := verr.Error()
	for _, want := range []string{
		"testing.acceptance_met",
		"tester",
		"outputSchema",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("error missing %q; full: %s", want, combined)
		}
	}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	if r.GetProject("bad-project") != nil {
		t.Error("bad-project survived load despite gate/schema mismatch")
	}
}

// TestStripInvalidProjects_AcceptsDeclaredGatePath confirms the happy path: a
// workflow gate referencing a path the schema declares AND requires loads
// cleanly.
//
// REVISED 2026-08-20. This test previously asserted that a merely-DECLARED
// (optional) gate path was acceptable, on the reasoning that "the model can
// still emit them; gate evaluation falls back to no-match when they are absent
// at runtime, which is the intended semantics."
//
// Measurement contradicted that twice. `required` is what binds at decode time
// — the schema becomes the emit_<role>_result tool's parameters — so an
// optional field is one the model may legitimately omit, and it does:
//
//   - review.all_done, optional, gated on by two of dev-pipeline's three
//     conditions: 83 of 97 review steps in the 2026.8.8 profiling arm emitted
//     approved:true with no all_done, matched NO condition, and had their
//     approval discarded (2026-08-20).
//   - testing.cases, optional, similarly gated: emitted in 4 of 50 attempts
//     (2026-08-19). Adding it to `required` moved violations 37/50 to 0/6.
//
// "Falls back to no-match" is only benign if the workflow routes absence
// somewhere useful. dev-pipeline routed it to on_fail → recover-checkpoint →
// a COMPLETED terminal, so a discarded approval looked like success. The
// permissive rule made that reachable by omission rather than by design.
//
// A gate path must now be required at EVERY segment. The alternative
// considered — keep optional paths but require the gate set to be exhaustive
// over their absence — is better modelling and much harder to check; recorded
// in the design as the option not taken.
func TestStripInvalidProjects_AcceptsDeclaredGatePath(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, map[string]string{
		"swarms/good-swarm.md": `---
swarmId: "good-swarm"
roles:
  - name: "reviewer"
    runtime: { image: "x" }
    injectSchemaIntoPrompt: true
    outputSchema:
      type: object
      required: [review]
      properties:
        review:
          type: object
          required: [approved, all_done]
          properties:
            approved: { type: bool }
            all_done: { type: bool }
---
`,
		"workflows/good-pipeline.md": `---
workflowId: "good-pipeline"
entrypoint: "review"
steps:
  review:
    type: "agent"
    role: "reviewer"
    prompt: "review the change"
    gates:
      # Both gate paths are declared AND required, so the model is
      # obliged to emit them and every condition can be evaluated.
      # An optional gate path is refused at load time — see the
      # revision note above for the two measurements behind that.
      - condition: "review.approved == true && review.all_done == true"
        target: "complete"
      - condition: "review.approved == false"
        target: "failed"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
		"projects/good-project.yaml": `projectId: "good-project"
swarmId: "good-swarm"
defaultWorkflowId: "good-pipeline"
`,
	})

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if verr := r.StripInvalidFromStaged(); verr != nil {
		t.Fatalf("unexpected validation error: %v", verr)
	}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	if r.GetProject("good-project") == nil {
		t.Error("good-project was stripped despite valid gate paths")
	}
}

// TestStripInvalidProjects_LegacyRoleSkipsGateCheck preserves the
// pre-outputSchema behaviour: a role without a schema declared keeps
// passing the cross-ref check unchanged. The compat check only fires
// for migrated roles. This guards the staged migration: not every
// shipped swarm has to migrate in lockstep.
func TestStripInvalidProjects_LegacyRoleSkipsGateCheck(t *testing.T) {
	tmp := t.TempDir()
	mustWriteAll(t, tmp, map[string]string{
		"swarms/legacy-swarm.md": `---
swarmId: "legacy-swarm"
roles:
  - name: "tester"
    runtime: { image: "x" }
    requiredOutputKeys: ["testing"]
---
`,
		"workflows/legacy-pipeline.md": `---
workflowId: "legacy-pipeline"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "run tests"
    gates:
      # Path the legacy role couldn't possibly declare — but no
      # outputSchema, so the compat check skips the role entirely.
      - condition: "testing.totally_unknown == true"
        target: "complete"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
		"projects/legacy-project.yaml": `projectId: "legacy-project"
swarmId: "legacy-swarm"
defaultWorkflowId: "legacy-pipeline"
`,
	})

	r := New()
	if err := r.Stage(tmp); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if verr := r.StripInvalidFromStaged(); verr != nil {
		t.Fatalf("legacy-role gate check should be a no-op; got: %v", verr)
	}
	if err := r.ActivateStaged(); err != nil {
		t.Fatalf("ActivateStaged: %v", err)
	}
	if r.GetProject("legacy-project") == nil {
		t.Error("legacy-project stripped despite the role having no outputSchema")
	}
}

// mustWriteAll lays out a config tree from a path → contents map,
// creating intermediate directories. Test-only helper; failure is a
// fatal because the test setup is broken.
func mustWriteAll(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for relPath, contents := range files {
		full := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}
