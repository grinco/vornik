package registry

import "testing"

// TestAcceptsCallsFrom_ClosedByDefault asserts a project with
// no acceptCallsFrom field set refuses every caller. This is
// the security default — cross-project orchestration is opt-in.
func TestAcceptsCallsFrom_ClosedByDefault(t *testing.T) {
	p := &Project{ID: "callee"}
	for _, caller := range []string{"marketing", "architect", ""} {
		if p.AcceptsCallsFrom(caller) {
			t.Errorf("AcceptsCallsFrom(%q) on empty allowlist = true, want false (default closed)", caller)
		}
	}
}

// TestAcceptsCallsFrom_ExactMatch covers the most common
// allowlist entry shape: a literal project ID.
func TestAcceptsCallsFrom_ExactMatch(t *testing.T) {
	p := &Project{ID: "callee", AcceptCallsFrom: []string{"marketing", "architect"}}
	if !p.AcceptsCallsFrom("marketing") {
		t.Error("marketing should be allowed")
	}
	if !p.AcceptsCallsFrom("architect") {
		t.Error("architect should be allowed")
	}
	if p.AcceptsCallsFrom("attacker") {
		t.Error("unlisted caller must be rejected")
	}
}

// TestAcceptsCallsFrom_Glob covers the prefix-style entries
// the LLD calls out: "team-*" admits any caller with that
// prefix, useful for spawned-project naming conventions.
func TestAcceptsCallsFrom_Glob(t *testing.T) {
	p := &Project{ID: "callee", AcceptCallsFrom: []string{"team-*", "exact-match"}}
	for _, ok := range []string{"team-a", "team-frontend", "team-"} {
		if !p.AcceptsCallsFrom(ok) {
			t.Errorf("AcceptsCallsFrom(%q) = false, want true (matches team-*)", ok)
		}
	}
	for _, no := range []string{"teamx", "ream-a", "other"} {
		if p.AcceptsCallsFrom(no) {
			t.Errorf("AcceptsCallsFrom(%q) = true, want false", no)
		}
	}
}

// TestAcceptsCallsFrom_Wildcard covers the "*" escape hatch.
// Documented as discouraged in the LLD but supported for
// single-tenant trust-everything deployments.
func TestAcceptsCallsFrom_Wildcard(t *testing.T) {
	p := &Project{ID: "callee", AcceptCallsFrom: []string{"*"}}
	for _, caller := range []string{"anything", "marketing", "team-a"} {
		if !p.AcceptsCallsFrom(caller) {
			t.Errorf("wildcard should allow %q", caller)
		}
	}
	// Even with wildcard, empty caller (a misconfigured call
	// with no caller_project) is still rejected.
	if p.AcceptsCallsFrom("") {
		t.Error("empty caller must be rejected even under wildcard")
	}
}

// TestAcceptsCallsFrom_NilReceiver asserts the matcher
// doesn't panic when the project lookup returned nil (the
// registry sometimes returns nil for unknown IDs).
func TestAcceptsCallsFrom_NilReceiver(t *testing.T) {
	var p *Project
	if p.AcceptsCallsFrom("anything") {
		t.Error("nil receiver should reject all callers")
	}
}

// TestAcceptsCallsFrom_WhitespaceTrimmed asserts hand-edited
// YAML with stray whitespace ("  marketing  ") still matches.
// Operators paste lists from notes; trimming each entry keeps
// the matcher tolerant.
func TestAcceptsCallsFrom_WhitespaceTrimmed(t *testing.T) {
	p := &Project{ID: "callee", AcceptCallsFrom: []string{"  marketing  ", "", "architect"}}
	if !p.AcceptsCallsFrom("marketing") {
		t.Error("whitespace-padded entry should match")
	}
	if !p.AcceptsCallsFrom("architect") {
		t.Error("architect should match after blank-entry skip")
	}
}

// TestAcceptsCallsFrom_MalformedGlobNotPanic ensures a broken
// glob pattern (e.g. unclosed brackets) is treated as a non-
// match rather than crashing the runtime.
func TestAcceptsCallsFrom_MalformedGlobNotPanic(t *testing.T) {
	p := &Project{ID: "callee", AcceptCallsFrom: []string{"[broken"}}
	if p.AcceptsCallsFrom("anything") {
		t.Error("malformed glob should not match")
	}
}
