package toolbudget

import "fmt"

// RoleLintView is the tiny projection of a swarm role that LintRoles needs. The
// caller fills it from registry.SwarmRole so this package stays dependency-free
// and unit-testable without the registry or a daemon.
type RoleLintView struct {
	Name          string
	RuntimePolicy string // "warm" | "ephemeral" | "" (defaults to ephemeral)
	HasBaseLimit  bool   // does the role pin a base VORNIK_MAX_TOOL_ITERATIONS?
}

// LintRoles returns human-readable warnings for roles whose configuration
// interacts badly with an ENABLED tool budget (LLD §2). It is advisory only —
// the caller logs each string at WARN; nothing here is fatal. Returns nil when
// the feature is disabled or every role is clean.
//
// Two foot-guns are surfaced:
//   - a role with runtimePolicy=warm keeps STATIC budgets by design (dynamic
//     scaling is ephemeral-only), so an operator expecting it to scale won't;
//   - a role that pins no base VORNIK_MAX_TOOL_ITERATIONS silently falls back to
//     the daemon default before scaling.
func LintRoles(cfg Config, roles []RoleLintView) []string {
	if !cfg.Enabled {
		return nil
	}
	var warnings []string
	for _, r := range roles {
		if r.RuntimePolicy == "warm" {
			warnings = append(warnings, fmt.Sprintf(
				"tool_budget: role %q is runtimePolicy=warm; its tool budget stays static "+
					"(dynamic scaling is ephemeral-only) while tool_budget.enabled=true", r.Name))
		}
		if !r.HasBaseLimit {
			warnings = append(warnings, fmt.Sprintf(
				"tool_budget: role %q pins no base VORNIK_MAX_TOOL_ITERATIONS; dynamic scaling "+
					"will use the daemon default as the base", r.Name))
		}
	}
	return warnings
}
