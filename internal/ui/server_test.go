package ui

import "testing"

func TestParsePlanSubRole(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// canonical cases — role names from the default dev-swarm
		{"analyst", "plan_0_analyst", "analyst"},
		{"coder", "plan_1_coder", "coder"},
		{"reviewer", "plan_2_reviewer", "reviewer"},

		// role names the operator may define in any swarm YAML —
		// these MUST survive through the pill (this is what the user
		// called out: "it can be plan_x_anything")
		{"role with underscore", "plan_0_code_reviewer", "code_reviewer"},
		{"role with multiple underscores", "plan_3_senior_backend_engineer", "senior_backend_engineer"},
		{"role with digit suffix", "plan_0_analyst42", "analyst42"},
		{"role with digits and underscores", "plan_1_role_v2_prototype", "role_v2_prototype"},
		{"single-char role", "plan_7_x", "x"},

		// double-digit indexes (a plan could have >10 roles in principle)
		{"double-digit index", "plan_12_coder", "coder"},

		// non-synthetic IDs — MUST return empty so the pill hides and
		// regular workflow step lookup wins
		{"regular step id", "plan", ""},
		{"two-part id", "plan_done", ""},
		{"non-numeric middle", "plan_first_coder", ""},
		{"empty role part", "plan_0_", ""},
		{"empty string", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePlanSubRole(tc.in)
			if got != tc.want {
				t.Errorf("parsePlanSubRole(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSessionUserGlobalConfigPath_A4_FailSafe is the regression test for
// audit finding A4 (2026-06-10): the RoleUser global-route protection
// was a hand-maintained deny-list mixing exact matches and prefixes, so
// a newly added SUBPATH under a global-authoring root (e.g.
// /swarms/x/new, /mcp/import, /audit/export) was reachable until someone
// extended the list. The check is now uniformly prefix-based (fail-safe).
//
// The load-bearing assertions are the "new subpath" cases marked below:
// they fail pre-fix for the exact-match roots (/audit, /mcp,
// /assistant/draft) and pass post-fix.
func TestSessionUserGlobalConfigPath_A4_FailSafe(t *testing.T) {
	tests := []struct {
		name string
		path string
		deny bool
	}{
		// Previously-listed roots — behaviour must be identical.
		{"swarms root", "/swarms", true},
		{"workflows root", "/workflows", true},
		{"projects new", "/projects/new", true},
		{"assistant draft", "/assistant/draft", true},
		{"audit root", "/audit", true},
		{"mcp root", "/mcp", true},
		{"swarms subpath", "/swarms/swarm-1/edit", true},

		// A4 load-bearing: hypothetical NEW subpaths under each root
		// must default-deny (these slipped through the old exact-match
		// entries for /audit, /mcp, /assistant/draft).
		{"new swarms subpath", "/swarms/anything/new", true},
		{"new mcp subpath", "/mcp/import", true},
		{"new audit subpath", "/audit/export", true},
		{"new assistant subpath", "/assistant/draft/anything", true},
		{"new projects-new subpath", "/projects/new/wizard", true},

		// Non-authoring / project-scoped paths must remain reachable.
		{"projects list", "/projects", false},
		{"a project page", "/projects/assistant", false},
		{"project keys", "/projects/assistant/keys", false},
		{"dashboard", "/", false},
		{"tasks", "/tasks", false},
		// Must NOT over-match a path that merely shares a prefix string
		// but isn't under the authoring root.
		{"swarmsomething not under swarms", "/swarmsomething", false},
		{"auditlog not under audit", "/auditlog", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionUserGlobalConfigPath(tc.path); got != tc.deny {
				t.Errorf("sessionUserGlobalConfigPath(%q) = %v, want %v", tc.path, got, tc.deny)
			}
		})
	}
}
