package registry

import "testing"

// TestCanCall_DefaultAllowAll: an unset canCallProjects allows calling
// any project (back-compatible — the callee's acceptCallsFrom is still
// the binding consent gate).
func TestCanCall_DefaultAllowAll(t *testing.T) {
	p := &Project{ID: "caller"}
	for _, callee := range []string{"a", "team-x", "anything"} {
		if !p.CanCall(callee) {
			t.Errorf("CanCall(%q) on empty allowlist = false, want true (default allow-all)", callee)
		}
	}
	// Empty callee and nil receiver are refused.
	if p.CanCall("") {
		t.Error("CanCall(\"\") must be false")
	}
	var np *Project
	if np.CanCall("a") {
		t.Error("nil receiver must be false")
	}
}

// TestCanCall_Restricted: a non-empty list restricts outbound calls to
// exact + glob matches; everything else is refused.
func TestCanCall_Restricted(t *testing.T) {
	p := &Project{ID: "trading", CanCallProjects: []string{"market-data", "ta-*"}}
	for _, ok := range []string{"market-data", "ta-momentum", "ta-"} {
		if !p.CanCall(ok) {
			t.Errorf("CanCall(%q) = false, want true", ok)
		}
	}
	for _, no := range []string{"assistant", "ta", "market-data-x"} {
		if p.CanCall(no) {
			t.Errorf("CanCall(%q) = true, want false (not in allowlist)", no)
		}
	}
}

// TestCanCall_Wildcard: an explicit "*" entry allows any callee.
func TestCanCall_Wildcard(t *testing.T) {
	p := &Project{ID: "orchestrator", CanCallProjects: []string{"*"}}
	if !p.CanCall("anything") {
		t.Error("wildcard should allow any callee")
	}
}
