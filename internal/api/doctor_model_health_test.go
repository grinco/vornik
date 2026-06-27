package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvalModelHealth_HealthyModelOK: a model with low failure rate and
// healthy completion tokens produces no finding.
func TestEvalModelHealth_HealthyModelOK(t *testing.T) {
	stats := []modelHealthStat{
		{model: "zai.glm-5", samples: 40, failures: 2, medianCompletionTokens: 800},
	}
	findings := evalModelHealth(stats, nil)
	if len(findings) != 0 {
		t.Fatalf("healthy model should produce no findings; got %v", findings)
	}
}

// TestEvalModelHealth_HighFailureRateFlagged: a model failing ≥50% of recent
// steps is flagged ERROR. This is the z-ai/glm-4.5-air:free (100% fail) case.
func TestEvalModelHealth_HighFailureRateFlagged(t *testing.T) {
	stats := []modelHealthStat{
		{model: "z-ai/glm-4.5-air:free", samples: 12, failures: 12, medianCompletionTokens: 300},
	}
	findings := evalModelHealth(stats, map[string]string{"z-ai/glm-4.5-air:free": "zai.glm-5"})
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if findings[0].status != "ERROR" {
		t.Errorf("100%% failure should be ERROR; got %q", findings[0].status)
	}
	if !strings.Contains(findings[0].message, "zai.glm-5") {
		t.Errorf("should RECOMMEND the configured fallback; got %q", findings[0].message)
	}
}

// TestEvalModelHealth_DegenerateTokensFlagged: a model whose median completion
// is degenerate (near-empty output) is flagged even with a sub-50% failure
// rate. This is the local qwen3.6:35b (timeouts + empty output) case.
func TestEvalModelHealth_DegenerateTokensFlagged(t *testing.T) {
	stats := []modelHealthStat{
		{model: "qwen3.6:35b", samples: 20, failures: 4, medianCompletionTokens: 3},
	}
	findings := evalModelHealth(stats, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %v", findings)
	}
	if !strings.Contains(findings[0].message, "degenerate") && !strings.Contains(findings[0].message, "median") {
		t.Errorf("should explain the degenerate-token reason; got %q", findings[0].message)
	}
	if !strings.Contains(findings[0].message, "no fallback") {
		t.Errorf("with no configured fallback the message should say so; got %q", findings[0].message)
	}
}

// TestEvalModelHealth_LowSampleSkipped: a model with too few recent samples is
// not flagged — we don't want a single bad call to trip an alarm.
func TestEvalModelHealth_LowSampleSkipped(t *testing.T) {
	stats := []modelHealthStat{
		{model: "rare.model", samples: 2, failures: 2, medianCompletionTokens: 1},
	}
	findings := evalModelHealth(stats, nil)
	if len(findings) != 0 {
		t.Fatalf("low-sample model should be skipped; got %v", findings)
	}
}

// fakeModelHealthSource returns canned stats / error for the check.
func fakeModelHealthSource(stats []modelHealthStat, err error) func(context.Context) ([]modelHealthStat, error) {
	return func(context.Context) ([]modelHealthStat, error) { return stats, err }
}

// writeModelHealthSwarm lays down a minimal one-role swarm so the check has a
// referenced model to evaluate, and returns the config dir.
func writeModelHealthSwarm(t *testing.T, model, fallback string) string {
	t.Helper()
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	swarm := "---\n" +
		"swarmId: \"s\"\n" +
		"roles:\n" +
		"  - name: \"reviewer\"\n" +
		"    model: \"" + model + "\"\n" +
		"    modelFallback: \"" + fallback + "\"\n" +
		"    runtime:\n" +
		"      image: \"vornik-agent:latest\"\n" +
		"---\n"
	if err := os.WriteFile(filepath.Join(swarmsDir, "s.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	return dir
}

// TestCheckModelHealth_NoSourceSkips: with no data source wired the check
// skips cleanly.
func TestCheckModelHealth_NoSourceSkips(t *testing.T) {
	h := &DoctorHandlers{configDir: "testdata"}
	got := h.checkModelHealth(context.Background())
	if got.Status != "OK" {
		t.Errorf("status = %q, want OK (skip); msg=%q", got.Status, got.Message)
	}
}

// TestCheckModelHealth_SourceErrorWarns: a failing data source downgrades to
// WARNING with the error surfaced.
func TestCheckModelHealth_SourceErrorWarns(t *testing.T) {
	h := &DoctorHandlers{
		configDir:         writeModelHealthSwarm(t, "some.model", "fb.model"),
		modelHealthSource: fakeModelHealthSource(nil, errors.New("db down")),
	}
	got := h.checkModelHealth(context.Background())
	if got.Status != "WARNING" {
		t.Fatalf("status = %q, want WARNING; msg=%q", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "db down") {
		t.Errorf("error should be surfaced; got %q", got.Message)
	}
}

// TestSetModelHealthSource_Overrides confirms the setter installs the source
// and is nil-safe on a nil receiver.
func TestSetModelHealthSource_Overrides(t *testing.T) {
	var nilH *DoctorHandlers
	nilH.SetModelHealthSource(nil) // must not panic

	h := &DoctorHandlers{configDir: writeModelHealthSwarm(t, "some.model", "")}
	h.SetModelHealthSource(fakeModelHealthSource([]modelHealthStat{
		{model: "some.model", samples: 10, failures: 10, medianCompletionTokens: 0},
	}, nil))
	if h.modelHealthSource == nil {
		t.Fatal("source should be installed")
	}
	got := h.checkModelHealth(context.Background())
	if got.Status != "ERROR" {
		t.Errorf("installed source should drive the check; status=%q msg=%q", got.Status, got.Message)
	}
}

// TestCheckModelHealth_Integration: only models referenced by a swarm role are
// evaluated, and the role's configured modelFallback is recommended.
func TestCheckModelHealth_Integration(t *testing.T) {
	dir := t.TempDir()
	swarmsDir := filepath.Join(dir, "swarms")
	if err := os.MkdirAll(swarmsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	swarm := "---\n" +
		"swarmId: \"s\"\n" +
		"roles:\n" +
		"  - name: \"reviewer\"\n" +
		"    model: \"dead.model\"\n" +
		"    modelFallback: \"good.model\"\n" +
		"    runtime:\n" +
		"      image: \"vornik-agent:latest\"\n" +
		"---\n"
	if err := os.WriteFile(filepath.Join(swarmsDir, "s.md"), []byte(swarm), 0o644); err != nil {
		t.Fatalf("write swarm: %v", err)
	}
	// Stats include an unreferenced model that should be ignored, plus the
	// dead referenced one.
	stats := []modelHealthStat{
		{model: "dead.model", samples: 10, failures: 10, medianCompletionTokens: 0},
		{model: "unreferenced.model", samples: 10, failures: 10, medianCompletionTokens: 0},
	}
	h := &DoctorHandlers{
		configDir:         dir,
		modelHealthSource: fakeModelHealthSource(stats, nil),
	}
	got := h.checkModelHealth(context.Background())
	if got.Status != "ERROR" {
		t.Fatalf("status = %q, want ERROR; msg=%q items=%v", got.Status, got.Message, got.Items)
	}
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "dead.model") {
		t.Errorf("referenced dead.model should be flagged; items=%v", got.Items)
	}
	if strings.Contains(joined, "unreferenced.model") {
		t.Errorf("unreferenced model must NOT be flagged; items=%v", got.Items)
	}
	if !strings.Contains(joined, "good.model") {
		t.Errorf("configured fallback good.model should be recommended; items=%v", got.Items)
	}
}
