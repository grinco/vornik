package registry

import "testing"

// TestResolveUserContextFilePath pins the path-safety contract
// so a misconfigured project YAML can never read host files
// outside the workspace via traversal — and so the executor's
// VORNIK_USER_CONTEXT_PATH stamp is never set to a value that
// would resolve outside the per-task bind mount.
func TestResolveUserContextFilePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty_returns_empty", "", ""},
		{"whitespace_returns_empty", "   ", ""},
		{"hidden_dir_path_kept", ".autonomy/USER_GUIDANCE.md", ".autonomy/USER_GUIDANCE.md"},
		{"root_relative_kept", "USER_GUIDANCE.md", "USER_GUIDANCE.md"},
		{"trims_whitespace", "  guidance.md  ", "guidance.md"},
		{"absolute_path_rejected", "/etc/passwd", ""},
		{"parent_dir_rejected", "..", ""},
		{"parent_traversal_rejected", "../etc/secrets", ""},
		{"nested_traversal_rejected", "subdir/../../etc/secrets", ""},
		{"clean_collapses_dot", "./.autonomy/USER_GUIDANCE.md", ".autonomy/USER_GUIDANCE.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Project{Autonomy: ProjectAutonomy{UserContextFilePath: c.in}}
			got := p.ResolveUserContextFilePath()
			if got != c.want {
				t.Errorf("ResolveUserContextFilePath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveUserContextFilePath_NilProject — defensive nil
// handling. Caller code in the executor passes plan.project
// which is normally non-nil, but the safety net keeps this from
// panicking if a future refactor changes that assumption.
func TestResolveUserContextFilePath_NilProject(t *testing.T) {
	var p *Project
	if got := p.ResolveUserContextFilePath(); got != "" {
		t.Errorf("nil project must return empty path, got %q", got)
	}
}
