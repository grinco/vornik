package api

import "testing"

// The button value is the only thing tying a tap back to a task. Task ids
// contain no colon, so the index is after the LAST one — the same rule the
// Telegram decoder applies. A malformed value must be refused, never guessed at.
func TestParseSteerAction(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantTask string
		wantIdx  int
		wantOK   bool
	}{
		{"steer:c:task_20260806212011_77b90a7d:2", "task_20260806212011_77b90a7d", 2, true},
		{"steer:c:task_1:0", "task_1", 0, true},
		{"steer:approve:task_1", "", 0, false}, // different action namespace
		{"steer:c:task_1", "", 0, false},       // no index
		{"steer:c:task_1:", "", 0, false},      // empty index
		{"steer:c:task_1:x", "", 0, false},     // non-numeric index
		{"steer:c::1", "", 0, false},           // empty task id
		{"", "", 0, false},
		{"nonsense", "", 0, false},
	} {
		gotTask, gotIdx, ok := parseSteerAction(tc.in)
		if ok != tc.wantOK || gotTask != tc.wantTask || gotIdx != tc.wantIdx {
			t.Errorf("parseSteerAction(%q) = (%q,%d,%v), want (%q,%d,%v)",
				tc.in, gotTask, gotIdx, ok, tc.wantTask, tc.wantIdx, tc.wantOK)
		}
	}
}

// A negative index cannot be produced by parseSteerAction (the digit loop
// rejects '-'), which is what keeps slackOptionIDAt's bounds check from ever
// seeing one. Pinned because the option lookup trusts it.
func TestParseSteerAction_RejectsNegativeIndex(t *testing.T) {
	if _, _, ok := parseSteerAction("steer:c:task_1:-1"); ok {
		t.Error("a negative index must not parse — the option lookup relies on it")
	}
}

// An unauthorized clicker and an unknown task must be indistinguishable. A
// distinct "not authorized" would confirm the task exists and that somebody
// else owns it — and this button is visible to everyone in the channel.
func TestRefusalText_DoesNotDiscloseExistenceOrOwnership(t *testing.T) {
	for _, leak := range []string{"authoriz", "owner", "permission", "belongs", "denied"} {
		if containsFold(refusalText, leak) {
			t.Errorf("refusal text %q leaks %q — it must read the same as an unknown task",
				refusalText, leak)
		}
	}
}

func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 || len(n) > len(h) {
		return false
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		match := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
