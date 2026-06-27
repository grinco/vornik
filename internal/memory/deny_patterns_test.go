package memory

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// Backlog (batch-2 RAG/memory follow-up): "Wire memory deny_patterns from
// YAML ... the substring deny-list is ReDoS-immune by construction ... only
// the YAML wiring is the remaining gap." These tests pin the deny-list to the
// hot-reloadable gateOverrides snapshot so a config.yaml reload swaps it the
// same way prompt_injection_scan and claim_audit_disabled_projects do.

// TestPipeline_DenyPatterns_SnapshotSourced — NewPipeline publishes the
// construction-time deny patterns into the snapshot, and the ingest gate reads
// them from there (not from the now-removed static cfg field).
func TestPipeline_DenyPatterns_SnapshotSourced(t *testing.T) {
	p := NewPipeline(nil, PipelineConfig{
		Logger:       zerolog.Nop(),
		DenyPatterns: []string{"forbidden", ""}, // empty entry must be dropped
	})
	got := p.denyPatternsSnapshot()
	if len(got) != 1 || got[0] != "forbidden" {
		t.Fatalf("denyPatternsSnapshot() = %v, want [forbidden]", got)
	}
}

// TestPipeline_DenyPatterns_HotReload — UpdateGates swaps the deny-list live
// without rebuilding the pipeline. An empty/absent list is a no-op (no deny).
func TestPipeline_DenyPatterns_HotReload(t *testing.T) {
	p := NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop()})

	// Absent list initially → no patterns.
	if got := p.denyPatternsSnapshot(); len(got) != 0 {
		t.Fatalf("initial denyPatternsSnapshot() = %v, want empty", got)
	}

	// Reload with a deny-list.
	p.UpdateGates("", nil, []string{"secret-token"})
	if got := p.denyPatternsSnapshot(); len(got) != 1 || got[0] != "secret-token" {
		t.Fatalf("after UpdateGates(deny), snapshot = %v, want [secret-token]", got)
	}

	// Reload back to empty — deny-list clears.
	p.UpdateGates("", nil, nil)
	if got := p.denyPatternsSnapshot(); len(got) != 0 {
		t.Fatalf("after UpdateGates(nil), snapshot = %v, want empty", got)
	}

	// A bare &Pipeline{} (no published snapshot) must not panic.
	var bare Pipeline
	if got := bare.denyPatternsSnapshot(); len(got) != 0 {
		t.Errorf("bare pipeline deny snapshot = %v, want empty", got)
	}
}

// TestPipeline_DenyPatterns_GateUsesHotSnapshot — the live ingest path
// (RunStandardGates via DryRun) honours a hot-swapped deny-list: matching
// content is quarantined; after clearing the list the same content is allowed.
func TestPipeline_DenyPatterns_GateUsesHotSnapshot(t *testing.T) {
	p := NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop()})
	body := "this note contains the magic-deny phrase " + strings.Repeat("word ", 30)

	// No deny-list → allowed.
	if res := p.DryRun("p", "s.md", "researcher", body); res.Final.Action == GateQuarantine {
		t.Fatalf("with no deny-list the gate must not quarantine: %+v", res.Final)
	}

	// Hot-apply a deny-list that matches → quarantine via policy_match.
	p.UpdateGates("", nil, []string{"magic-deny"})
	res := p.DryRun("p", "s.md", "researcher", body)
	if res.Final.Action != GateQuarantine || res.Final.Gate != GatePolicyMatch {
		t.Fatalf("with deny-list match want quarantine via policy_match, got %+v", res.Final)
	}

	// Clear the list → allowed again, proving the swap is live.
	p.UpdateGates("", nil, nil)
	if res := p.DryRun("p", "s.md", "researcher", body); res.Final.Action == GateQuarantine {
		t.Fatalf("after clearing deny-list the gate must not quarantine: %+v", res.Final)
	}
}
