package memory

// Regression tests for the per-project enforcement-mode
// override added 2026.5.9 (Phase D follow-on of the
// Policy-Aware Memory Firewall LLD). Pins:
//   - FirewallDeps.ModeForProject precedence over daemon default
//   - Unwired ModeForProject falls through to daemon default
//   - Resolver returning (_, false) falls through cleanly

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/memoryfirewall"
)

// TestSetFirewallMetrics_WiresOntoDeps confirms the post-boot metrics
// setter (drift-mitigation §8.3) installs the recall-side collectors
// onto an already-wired FirewallDeps and is nil-safe when the
// firewall (or the searcher) isn't wired.
func TestSetFirewallMetrics_WiresOntoDeps(t *testing.T) {
	// nil searcher → no panic.
	var nilS *Searcher
	nilS.SetFirewallMetrics(nil)

	// searcher with no firewall → no-op (no panic).
	s := &Searcher{}
	s.SetFirewallMetrics(memoryfirewall.NewMetrics(prometheus.NewRegistry()))
	assert.Nil(t, s.firewall, "setter must not fabricate FirewallDeps")

	// searcher with firewall → metrics land on the deps.
	s.firewall = &FirewallDeps{Evaluator: memoryfirewall.NewEvaluator()}
	m := memoryfirewall.NewMetrics(prometheus.NewRegistry())
	s.SetFirewallMetrics(m)
	assert.Same(t, m, s.firewall.Metrics)
}

func TestPerProjectModeResolver_PrecedenceContract(t *testing.T) {
	// Directly test the resolution logic that lives inline in
	// applyFirewall: per-project resolver wins; no resolver +
	// no override → daemon default.
	daemon := memoryfirewall.EnforcementAdvisory

	t.Run("nil resolver → daemon default", func(t *testing.T) {
		fw := &FirewallDeps{EnforcementMode: daemon, ModeForProject: nil}
		got := resolveMode(fw, "any-project")
		assert.Equal(t, daemon, got)
	})

	t.Run("resolver returns false → daemon default", func(t *testing.T) {
		fw := &FirewallDeps{
			EnforcementMode: daemon,
			ModeForProject: func(string) (memoryfirewall.EnforcementMode, bool) {
				return memoryfirewall.EnforcementEnforce, false
			},
		}
		got := resolveMode(fw, "p1")
		assert.Equal(t, daemon, got, "false signal must NOT promote the override")
	})

	t.Run("resolver returns true → override wins", func(t *testing.T) {
		fw := &FirewallDeps{
			EnforcementMode: memoryfirewall.EnforcementOff,
			ModeForProject: func(p string) (memoryfirewall.EnforcementMode, bool) {
				if p == "compliance-project" {
					return memoryfirewall.EnforcementEnforce, true
				}
				return memoryfirewall.EnforcementOff, false
			},
		}
		assert.Equal(t, memoryfirewall.EnforcementEnforce, resolveMode(fw, "compliance-project"))
		// Other projects fall through to daemon default.
		assert.Equal(t, memoryfirewall.EnforcementOff, resolveMode(fw, "other-project"))
	})

	t.Run("override can downgrade as well as upgrade", func(t *testing.T) {
		// A daemon-default of enforce can be downgraded to off
		// per project (e.g. legacy projects pending migration).
		fw := &FirewallDeps{
			EnforcementMode: memoryfirewall.EnforcementEnforce,
			ModeForProject: func(p string) (memoryfirewall.EnforcementMode, bool) {
				return memoryfirewall.EnforcementOff, p == "legacy"
			},
		}
		assert.Equal(t, memoryfirewall.EnforcementOff, resolveMode(fw, "legacy"))
		assert.Equal(t, memoryfirewall.EnforcementEnforce, resolveMode(fw, "modern"))
	})
}
