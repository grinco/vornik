package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoConfigPath resolves a path inside the repo's configs/ tree from this
// package's directory (internal/registry).
func repoConfigPath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "configs"}, parts...)...)
}

// The shipped vision workflow must parse and reference a role the assistant
// swarm actually provides.
//
// This pins the gap that made T-1df3 unanswerable: the swarm had a fully
// configured `vision` role — multimodal model, ImageMagick, a written
// prompt — and NO workflow anywhere contained a step with `role: vision`,
// so the adaptive router could never select it and a photo was routed to a
// blind researcher instead.
//
// see LLD § https://docs.vornik.io §4.5
func TestShippedVisionWorkflowResolvesToARealRole(t *testing.T) {
	wfBytes, err := os.ReadFile(repoConfigPath("workflows", "vision.md"))
	if err != nil {
		t.Fatalf("read shipped vision workflow: %v", err)
	}
	wf, err := ParseWorkflowMarkdown(wfBytes, "vision.md")
	if err != nil {
		t.Fatalf("shipped vision workflow does not parse: %v", err)
	}
	if err := wf.Validate("vision.md"); err != nil {
		t.Fatalf("shipped vision workflow fails validation: %v", err)
	}

	swarmBytes, err := os.ReadFile(repoConfigPath("swarms", "assistant-swarm.md"))
	if err != nil {
		t.Fatalf("read assistant swarm: %v", err)
	}
	swarm, err := ParseSwarmMarkdown(swarmBytes, "assistant-swarm.md")
	if err != nil {
		t.Fatalf("assistant swarm does not parse: %v", err)
	}

	roles := make(map[string]bool, len(swarm.Roles))
	for _, r := range swarm.Roles {
		roles[r.Name] = true
	}

	sawAgentStep := false
	for id, step := range wf.Steps {
		if step.Type != "agent" {
			continue
		}
		sawAgentStep = true
		if !roles[step.Role] {
			t.Errorf("step %q references role %q which assistant-swarm does not define", id, step.Role)
		}
	}
	if !sawAgentStep {
		t.Error("the vision workflow must contain at least one agent step, or it cannot reach the seeing role")
	}
}

// The shipped assistant swarm must itself satisfy the requiredModalities
// check. If this fails, the deployment's own config would be refused at
// load — the failure mode of a validation rule that is stricter than the
// configuration it ships alongside.
func TestShippedAssistantSwarmPassesModalityValidation(t *testing.T) {
	swarmBytes, err := os.ReadFile(repoConfigPath("swarms", "assistant-swarm.md"))
	if err != nil {
		t.Fatalf("read assistant swarm: %v", err)
	}
	swarm, err := ParseSwarmMarkdown(swarmBytes, "assistant-swarm.md")
	if err != nil {
		t.Fatalf("assistant swarm does not parse: %v", err)
	}
	if err := swarm.Validate("assistant-swarm.md"); err != nil {
		t.Fatalf("shipped assistant swarm fails validation: %v", err)
	}

	// And the declaration is actually present on the vision role — without
	// it the validation above passes trivially and the protection is absent.
	var found bool
	for _, r := range swarm.Roles {
		if r.Name != "vision" {
			continue
		}
		found = true
		if len(r.RequiredModalities) == 0 {
			t.Error("the vision role must declare requiredModalities, or a blind model can be wired to it silently")
		}
	}
	if !found {
		t.Fatal("assistant-swarm no longer defines a vision role")
	}
}

// The Art 5 refusal clauses must be present in BOTH places a vision agent
// gets its instructions: the swarm role prompt and the workflow step prompt.
// Either one alone leaves a path where the guardrail is absent — the role is
// runnable from any workflow, and the workflow prompt is what a step actually
// receives.
//
// This is a conformity regression test, not a style check. Shipping media
// perception made three EU AI Act Art 5 prohibitions live (identification,
// emotion inference, deduction of sensitive characteristics) and put
// GDPR Art 9 special-category inference within reach of an ordinary "what's
// in this photo" request. The prompt is the only place that limit can bite,
// because the model is a general-purpose one that will otherwise answer.
//
// see LLD § https://docs.vornik.io
func TestShippedVisionPromptsCarryArt5Refusals(t *testing.T) {
	// One probe per prohibition, chosen to fail if the clause is softened
	// into a preference rather than deleted outright.
	probes := []string{
		"Do not identify people",
		"Putting a name to a face is not",
		"Do not infer emotion or inner state as fact",
		"Do not deduce sensitive characteristics",
		"refusals, not preferences",
		// The OCR carve-out matters as much as the prohibitions: without
		// it the clauses read as "do not process documents with people in
		// them", which would break the dossier workflow the operator
		// actually relies on.
		"Reading text out of a photographed document (OCR) is unaffected",
	}

	for _, path := range []string{
		repoConfigPath("swarms", "assistant-swarm.md"),
		repoConfigPath("workflows", "vision.md"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(data)
		for _, probe := range probes {
			if !strings.Contains(body, probe) {
				t.Errorf("%s is missing the Art 5 clause %q", filepath.Base(path), probe)
			}
		}
	}
}
