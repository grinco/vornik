package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckSchemaGateDrift covers the doctor-side surface of the
// workflow-gate schema compat check (item 5 of
// https://docs.vornik.io). The strip path in
// registry already refuses bad configs at load (item 11); this
// doctor check surfaces the same findings as a WARNING so an
// operator can spot them BEFORE restarting the daemon and hitting
// the hard refusal.
//
// Three cases mirror the registry-side tests:
//   - happy path: clean config produces OK
//   - schema-gate drift: produces WARNING with role + path in items
//   - other strip reasons (missing swarm/workflow/role) don't
//     pollute this check — they belong to config_validation
func TestCheckSchemaGateDrift(t *testing.T) {
	t.Run("happy path: gate paths declared by schema → OK", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(t, dir, map[string]string{
			"swarms/s.md": `---
swarmId: "s"
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
			"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
    gates:
      - condition: "testing.passed == true"
        target: "complete"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
			"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
`,
		})

		h := &DoctorHandlers{configDir: dir}
		got := h.checkSchemaGateDrift()
		if got.Status != "OK" {
			t.Errorf("status = %q, want OK; items = %v", got.Status, got.Items)
		}
	})

	t.Run("gate references undeclared path → WARNING", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(t, dir, map[string]string{
			"swarms/s.md": `---
swarmId: "s"
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
			"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
    gates:
      - condition: "testing.acceptance_met == true"  # NOT declared
        target: "complete"
    on_fail: "failed"
terminals:
  complete: { status: "COMPLETED" }
  failed:   { status: "FAILED" }
---
`,
			"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
`,
		})

		h := &DoctorHandlers{configDir: dir}
		got := h.checkSchemaGateDrift()
		if got.Status != "WARNING" {
			t.Fatalf("status = %q, want WARNING; items = %v", got.Status, got.Items)
		}
		if len(got.Items) != 1 {
			t.Fatalf("expected 1 item; got %d: %v", len(got.Items), got.Items)
		}
		// Operator-facing message must name the role + the path so
		// they don't have to grep the YAML to find what's wrong.
		for _, want := range []string{"tester", "testing.acceptance_met", "outputSchema"} {
			if !strings.Contains(got.Items[0], want) {
				t.Errorf("item missing %q; full: %s", want, got.Items[0])
			}
		}
	})

	t.Run("missing-swarm strip findings don't pollute this check", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(t, dir, map[string]string{
			// No swarm file — project's SwarmID won't resolve.
			"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "anything"
    prompt: "x"
    on_fail: "failed"
terminals:
  failed:   { status: "FAILED" }
---
`,
			"projects/p.yaml": `projectId: "p"
swarmId: "missing-swarm"
defaultWorkflowId: "w"
`,
		})

		h := &DoctorHandlers{configDir: dir}
		got := h.checkSchemaGateDrift()
		// The project gets stripped (missing-swarm reason), but
		// that's config_validation's surface — schema_gate_drift
		// must stay OK so we don't double-report.
		if got.Status != "OK" {
			t.Errorf("status = %q, want OK (missing-swarm should be config_validation's surface, not ours); items = %v",
				got.Status, got.Items)
		}
	})
}

func writeAll(t *testing.T, root string, files map[string]string) {
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
