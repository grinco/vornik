package service

import (
	"os"
	"testing"
)

// TestApplyContentValidate_SkipsNonYAML is the regression test for the latent
// scaffold-apply bug: the apply engine's cheap pre-write gate YAML-parsed
// every op's content, but a swarm `.md` (YAML frontmatter + a markdown body)
// is not valid single-document YAML — so a scaffold proposal carrying a swarm
// file would be rejected and its whole create bundle reversed before the
// project could be created. Non-YAML paths must skip the gate (reload
// validates them authoritatively).
func TestApplyContentValidate_SkipsNonYAML(t *testing.T) {
	// A real swarm .md fails yaml.Unmarshal (proven empirically) — the gate
	// must not run on it.
	swarm, err := os.ReadFile("../../configs/swarms/assistant-swarm.md")
	if err != nil {
		t.Skipf("assistant-swarm.md not readable: %v", err)
	}
	if err := applyContentValidate("configs/swarms/assistant-swarm.md", string(swarm)); err != nil {
		t.Errorf("swarm .md must skip the YAML gate; got %v", err)
	}
	// An arbitrary markdown body with a mapping-value colon also skips.
	if err := applyContentValidate("configs/swarms/x-swarm.md", "# Title\nrole: not yaml: really"); err != nil {
		t.Errorf(".md content must skip the YAML gate; got %v", err)
	}
}

// TestApplyContentValidate_ChecksYAML pins that genuine YAML targets are still
// syntactically gated — a malformed config.yaml is caught before any write.
func TestApplyContentValidate_ChecksYAML(t *testing.T) {
	if err := applyContentValidate("config.yaml", "valid: true\nnested:\n  ok: 1\n"); err != nil {
		t.Errorf("valid YAML should pass; got %v", err)
	}
	if err := applyContentValidate("configs/projects/x.yaml", "bad:\n  - [unclosed\n"); err == nil {
		t.Error("malformed project YAML should be rejected by the gate")
	}
	// Case-insensitive on the extension.
	if err := applyContentValidate("config.YAML", "k: : :"); err == nil {
		t.Error(".YAML must still be gated (case-insensitive)")
	}
}
