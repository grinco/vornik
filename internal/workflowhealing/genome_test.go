package workflowhealing

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// validWorkflowMD is a minimal WORKFLOW.md the genome helper can
// parse + fingerprint. Mirrors the registry test fixture shape.
const validWorkflowMD = `---
workflowId: "heal-flow"
displayName: "Heal Flow"
version: "1.0"
entrypoint: "plan"
maxStepVisits: 3
maxWallClock: "1h"
steps:
  plan:
    type: "agent"
    role: "lead"
    on_success: "complete"
    on_fail: "failed"
terminals:
  complete:
    status: "success"
  failed:
    status: "failed"
---

# Heal Flow

## Prompts

### plan

Plan the work and hand off.
`

func TestGenomeHash_NilWorkflow(t *testing.T) {
	if got := GenomeHash(nil); got != "" {
		t.Errorf("GenomeHash(nil) = %q, want empty", got)
	}
}

func TestGenomeHash_MatchesRegistryHash(t *testing.T) {
	wf, err := registry.ParseWorkflowMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if GenomeHash(wf) != wf.Hash() {
		t.Errorf("GenomeHash diverged from registry.Workflow.Hash")
	}
	if GenomeHash(wf) == "" {
		t.Error("GenomeHash of a valid workflow should be non-empty")
	}
}

func TestGenomeHashFromMarkdown_Deterministic(t *testing.T) {
	h1, err := GenomeHashFromMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("GenomeHashFromMarkdown: %v", err)
	}
	h2, err := GenomeHashFromMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("GenomeHashFromMarkdown#2: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
}

func TestGenomeHashFromMarkdown_StructuralChangeChangesHash(t *testing.T) {
	base, err := GenomeHashFromMarkdown([]byte(validWorkflowMD), "heal.md")
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	// Change maxStepVisits 3 -> 5: a structural change, must rehash.
	mutated := strings.Replace(validWorkflowMD, "maxStepVisits: 3", "maxStepVisits: 5", 1)
	got, err := GenomeHashFromMarkdown([]byte(mutated), "heal.md")
	if err != nil {
		t.Fatalf("mutated: %v", err)
	}
	if got == base {
		t.Error("structural change did not change the genome hash")
	}
}

func TestGenomeHashFromMarkdown_ParseErrorSurfaces(t *testing.T) {
	if _, err := GenomeHashFromMarkdown([]byte("not a workflow"), "bad.md"); err == nil {
		t.Fatal("expected parse error for malformed markdown")
	}
}
