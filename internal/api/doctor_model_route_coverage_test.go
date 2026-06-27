package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/pricing"
)

// loadPricingFromString writes the YAML to a temp file and loads it, so the
// route-coverage tests can build small pricing tables without touching the
// real configs/pricing.yaml.
func loadPricingFromString(t *testing.T, yaml string) *pricing.Table {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write pricing: %v", err)
	}
	table, err := pricing.Load(p)
	if err != nil {
		t.Fatalf("load pricing: %v", err)
	}
	return table
}

// TestModelRouteCoverage_Pure_FullyCovered: a model with both a matching
// route prefix and a pricing entry produces no findings.
func TestModelRouteCoverage_Pure_FullyCovered(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  zai.glm-5:\n    input: 1\n    output: 2\n")
	refs := []modelRef{{model: "zai.glm-5", swarm: "s", role: "reviewer"}}
	findings := evalModelRouteCoverage(refs, []string{"zai."}, table)
	if len(findings) != 0 {
		t.Fatalf("expected no findings, got %v", findings)
	}
}

// TestModelRouteCoverage_Pure_Unrouted: a model that matches no route prefix
// is flagged as unrouted.
func TestModelRouteCoverage_Pure_Unrouted(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  mistral.large:\n    input: 1\n    output: 2\n")
	refs := []modelRef{{model: "mistral.large", swarm: "s", role: "coder"}}
	findings := evalModelRouteCoverage(refs, []string{"zai."}, table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0], "unrouted") {
		t.Errorf("finding should call out unrouted; got %q", findings[0])
	}
}

// TestModelRouteCoverage_Pure_Unpriced: a routed model with no pricing entry
// is flagged as unpriced.
func TestModelRouteCoverage_Pure_Unpriced(t *testing.T) {
	table := pricing.Empty()
	refs := []modelRef{{model: "zai.glm-5", swarm: "s", role: "reviewer"}}
	findings := evalModelRouteCoverage(refs, []string{"zai."}, table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0], "unpriced") {
		t.Errorf("finding should call out unpriced; got %q", findings[0])
	}
}

// TestModelRouteCoverage_Pure_FallbackChecked: modelFallback is covered too —
// a fully-routed/priced primary with an unrouted fallback still flags.
func TestModelRouteCoverage_Pure_FallbackChecked(t *testing.T) {
	table := loadPricingFromString(t, "models:\n  zai.glm-5:\n    input: 1\n    output: 2\n  bad.fallback:\n    input: 1\n    output: 2\n")
	refs := []modelRef{
		{model: "zai.glm-5", swarm: "s", role: "reviewer"},
		{model: "bad.fallback", swarm: "s", role: "reviewer", isFallback: true},
	}
	findings := evalModelRouteCoverage(refs, []string{"zai."}, table)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding for the fallback, got %v", findings)
	}
	if !strings.Contains(findings[0], "bad.fallback") || !strings.Contains(findings[0], "fallback") {
		t.Errorf("finding should name the fallback model; got %q", findings[0])
	}
}

// TestCheckModelRouteCoverage_Integration exercises the full check end-to-end
// from a temp config dir + pricing file + injected route prefixes.
func TestCheckModelRouteCoverage_Integration(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	swarm := "---\n" +
		"swarmId: \"s\"\n" +
		"roles:\n" +
		"  - name: \"reviewer\"\n" +
		"    model: \"zai.glm-5\"\n" +
		"    modelFallback: \"orphan.model\"\n" +
		"    runtime:\n" +
		"      image: \"vornik-agent:latest\"\n" +
		"---\n"
	if err := os.WriteFile(filepath.Join(swarmsDir, "s.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	pricingPath := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(pricingPath, []byte("models:\n  zai.glm-5:\n    input: 1\n    output: 2\n"), 0o644); err != nil {
		t.Fatalf("write pricing: %v", err)
	}

	h := &DoctorHandlers{
		configDir:         dir,
		pricingPath:       pricingPath,
		chatRoutePrefixes: []string{"zai."},
	}
	got := h.checkModelRouteCoverage()
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING; msg=%q items=%v", got.Status, got.Message, got.Items)
	}
	// orphan.model is both unrouted and unpriced → one finding naming it.
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "orphan.model") {
		t.Errorf("orphan.model should be flagged; items=%v", got.Items)
	}
	if strings.Contains(joined, "zai.glm-5") {
		t.Errorf("covered primary should not be flagged; items=%v", got.Items)
	}
}

// TestCheckModelRouteCoverage_NoRoutesSkips: with no route prefixes wired the
// check skips (can't meaningfully assert coverage).
func TestCheckModelRouteCoverage_NoRoutesSkips(t *testing.T) {
	h := &DoctorHandlers{configDir: "testdata"}
	got := h.checkModelRouteCoverage()
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK (skip); msg=%q", got.Status, got.Message)
	}
}
