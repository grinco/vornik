package service

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
)

// TestApplyHotConfig_PushesGateKnobsToLivePipeline — the config.yaml
// hot-reload activator step. applyHotConfig must push the staged memory
// gate knobs onto the live pipeline (no restart) and clear the staging slot.
func TestApplyHotConfig_PushesGateKnobsToLivePipeline(t *testing.T) {
	// Content with a backtick command → ExtractClaims yields a claim, so the
	// AuditLookup fires unless the project is in the disabled set.
	const content = "Ran `go test ./...` to verify the fix end to end."

	auditCalled := false
	pipe := memory.NewPipeline(nil, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		AuditLookup: func(_ context.Context, _ string, _ []memory.Claim) ([]memory.ClaimMatch, error) {
			auditCalled = true
			return nil, nil
		},
	})

	// Baseline: project enabled → AuditLookup fires.
	_ = pipe.DryRunWithExecution("p-x", "s.md", "reviewer", "exec-0", content)
	if !auditCalled {
		t.Fatal("baseline: AuditLookup should run before the hot-reload disables p-x")
	}

	c := &Container{
		Logger:         zerolog.Nop(),
		memoryPipeline: pipe,
		stagedConfig: &config.Config{
			Memory: config.MemoryConfig{
				PromptInjectionScan:        "quarantine",
				ClaimAuditDisabledProjects: []string{"p-x"},
			},
		},
	}

	c.applyHotConfig()

	if c.stagedConfig != nil {
		t.Error("applyHotConfig must clear the staging slot after applying")
	}

	// After the hot-reload, p-x is disabled → AuditLookup is skipped.
	auditCalled = false
	_ = pipe.DryRunWithExecution("p-x", "s.md", "reviewer", "exec-1", content)
	if auditCalled {
		t.Error("after applyHotConfig disabled p-x, AuditLookup should be skipped on the live pipeline")
	}
}

// TestApplyHotConfig_PushesDenyPatternsToLivePipeline — backlog (batch-2
// RAG/memory follow-up): the deny_patterns deny-list must hot-reload like the
// other ingest-gate knobs. A config.yaml reload that adds a deny pattern
// quarantines matching content on the live pipeline without a restart.
func TestApplyHotConfig_PushesDenyPatternsToLivePipeline(t *testing.T) {
	body := "this note holds the BLOCKED-PHRASE marker " + strings.Repeat("word ", 30)

	pipe := memory.NewPipeline(nil, memory.PipelineConfig{Logger: zerolog.Nop()})

	// Baseline: no deny-list → the gate does not quarantine.
	if res := pipe.DryRun("p-x", "s.md", "researcher", body); res.Final.Action == memory.GateQuarantine {
		t.Fatalf("baseline: content must not be quarantined before deny-list applied: %+v", res.Final)
	}

	c := &Container{
		Logger:         zerolog.Nop(),
		memoryPipeline: pipe,
		stagedConfig: &config.Config{
			Memory: config.MemoryConfig{
				DenyPatterns: []string{"BLOCKED-PHRASE"},
			},
		},
	}
	c.applyHotConfig()
	if c.stagedConfig != nil {
		t.Error("applyHotConfig must clear the staging slot after applying")
	}

	// After the hot-reload the live pipeline quarantines via policy_match.
	res := pipe.DryRun("p-x", "s.md", "researcher", body)
	if res.Final.Action != memory.GateQuarantine || res.Final.Gate != memory.GatePolicyMatch {
		t.Fatalf("after deny-list hot-reload want quarantine via policy_match, got %+v", res.Final)
	}
}

// TestApplyHotConfig_NilSafe — no staged config and/or no pipeline must be a
// no-op, never a panic (a reload triggered before the pipeline is wired, or
// with config.yaml unchanged).
func TestApplyHotConfig_NilSafe(t *testing.T) {
	// No staged config.
	c := &Container{Logger: zerolog.Nop()}
	c.applyHotConfig()

	// Staged config but no pipeline (memory subsystem disabled).
	c = &Container{Logger: zerolog.Nop(), stagedConfig: &config.Config{}}
	c.applyHotConfig()
	if c.stagedConfig != nil {
		t.Error("applyHotConfig must clear the staging slot even when the pipeline is nil")
	}
}
