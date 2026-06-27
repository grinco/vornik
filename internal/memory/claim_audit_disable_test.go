package memory

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// TestClaimAudit_PerProjectDisable is the hardening regression
// (2026-06-15): when a project is listed in ClaimAuditDisabledProjects
// the pipeline skips the claim-audit AuditLookup entirely (the gate then
// auto-allows on zero results); other projects still run it.
func TestClaimAudit_PerProjectDisable(t *testing.T) {
	// Content with a backtick command → ExtractClaims yields >=1 claim,
	// so the AuditLookup would fire unless the project is disabled.
	const content = "Ran `go test ./...` to verify the fix works end to end."

	newPipe := func(disabled []string) (*Pipeline, *bool) {
		called := false
		cfg := PipelineConfig{
			Logger:                     zerolog.Nop(),
			ClaimAuditDisabledProjects: disabled,
			AuditLookup: func(_ context.Context, _ string, claims []Claim) ([]ClaimMatch, error) {
				called = true
				return nil, nil
			},
		}
		return NewPipeline(nil, cfg), &called
	}

	t.Run("disabled project skips the lookup", func(t *testing.T) {
		p, called := newPipe([]string{"p-disabled"})
		_ = p.DryRunWithExecution("p-disabled", "s.md", "reviewer", "exec-1", content)
		if *called {
			t.Error("AuditLookup ran for a disabled project")
		}
	})

	t.Run("other project still runs the lookup", func(t *testing.T) {
		p, called := newPipe([]string{"p-disabled"})
		_ = p.DryRunWithExecution("p-enabled", "s.md", "reviewer", "exec-1", content)
		if !*called {
			t.Error("AuditLookup did not run for an enabled project")
		}
	})

	// Hot-reload (2026-06-15): UpdateGates swaps the disabled-projects set
	// live, so a config.yaml reload takes effect on the next ingest without
	// rebuilding the pipeline.
	t.Run("UpdateGates flips claim-audit disable live", func(t *testing.T) {
		p, called := newPipe(nil) // starts enabled for all projects

		_ = p.DryRunWithExecution("p-x", "s.md", "reviewer", "exec-1", content)
		if !*called {
			t.Fatal("baseline: AuditLookup should run when no project is disabled")
		}

		// Reload adds p-x to the disabled set → next ingest skips the lookup.
		*called = false
		p.UpdateGates("", []string{"p-x"}, nil)
		_ = p.DryRunWithExecution("p-x", "s.md", "reviewer", "exec-2", content)
		if *called {
			t.Error("after UpdateGates disabled p-x, AuditLookup should be skipped")
		}

		// Reload removes it again → lookup resumes.
		*called = false
		p.UpdateGates("", nil, nil)
		_ = p.DryRunWithExecution("p-x", "s.md", "reviewer", "exec-3", content)
		if !*called {
			t.Error("after UpdateGates re-enabled p-x, AuditLookup should run again")
		}
	})
}
