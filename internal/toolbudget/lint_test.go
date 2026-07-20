package toolbudget

// TDD for item 2 of the dynamic-tool-budget follow-ups
// (https://docs.vornik.io §2):
// a pure LintRoles helper surfaces two silent foot-guns at daemon startup when
// tool_budget is enabled — a warm role (budgets stay static) and a role with no
// configured base limit (falls back to the daemon default). Warnings only,
// never errors; empty when the feature is disabled or the roles are clean.

import (
	"strings"
	"testing"
)

func TestLintRoles(t *testing.T) {
	enabled := Config{Enabled: true}
	disabled := Config{Enabled: false}

	warm := RoleLintView{Name: "reviewer", RuntimePolicy: "warm", HasBaseLimit: true}
	noBase := RoleLintView{Name: "coder", RuntimePolicy: "ephemeral", HasBaseLimit: false}
	clean := RoleLintView{Name: "lead", RuntimePolicy: "ephemeral", HasBaseLimit: true}
	warmNoBase := RoleLintView{Name: "tester", RuntimePolicy: "warm", HasBaseLimit: false}

	t.Run("disabled -> no warnings", func(t *testing.T) {
		if w := LintRoles(disabled, []RoleLintView{warm, noBase}); len(w) != 0 {
			t.Errorf("disabled must produce no warnings, got %v", w)
		}
	})
	t.Run("warm role warns", func(t *testing.T) {
		w := LintRoles(enabled, []RoleLintView{warm})
		if len(w) != 1 || !strings.Contains(w[0], "reviewer") || !strings.Contains(w[0], "warm") {
			t.Errorf("want one warm warning naming the role, got %v", w)
		}
	})
	t.Run("missing base warns", func(t *testing.T) {
		w := LintRoles(enabled, []RoleLintView{noBase})
		if len(w) != 1 || !strings.Contains(w[0], "coder") || !strings.Contains(w[0], "VORNIK_MAX_TOOL_ITERATIONS") {
			t.Errorf("want one missing-base warning naming the role, got %v", w)
		}
	})
	t.Run("clean ephemeral role -> no warnings", func(t *testing.T) {
		if w := LintRoles(enabled, []RoleLintView{clean}); len(w) != 0 {
			t.Errorf("clean role must produce no warnings, got %v", w)
		}
	})
	t.Run("warm + no base -> two warnings", func(t *testing.T) {
		w := LintRoles(enabled, []RoleLintView{warmNoBase})
		if len(w) != 2 {
			t.Errorf("warm role with no base must produce both warnings, got %v", w)
		}
	})
}
