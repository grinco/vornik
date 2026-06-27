package telegram

import (
	"context"
	"strings"
	"testing"
)

// TestHandleCallbackQuery_RejectsNonAllowlistedUser guards the fix that the
// inline-keyboard callback surface enforces the SAME IsAllowed gate as text
// messages. Pre-fix a non-allowlisted user's button taps reached the action
// handlers (e.g. switching the active project) unauthenticated + unthrottled.
func TestHandleCallbackQuery_RejectsNonAllowlistedUser(t *testing.T) {
	rig := newCallbackRig(t)
	rig.bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	// User 999 is NOT on the allowlist.
	cq := buildCallback(t, "cb-deny", 100, 999, "project:select:whatever")
	if err := rig.bot.handleCallbackQuery(context.Background(), cq); err != nil {
		t.Fatalf("handleCallbackQuery returned err: %v", err)
	}
	calls := rig.callsTo("answerCallbackQuery")
	if len(calls) == 0 {
		t.Fatal("expected an answerCallbackQuery toast for the rejected user")
	}
	if !strings.Contains(string(calls[len(calls)-1].body), "not authorized") {
		t.Errorf("expected 'not authorized' toast, got: %s", calls[len(calls)-1].body)
	}
	// The project action must NOT have run → active project unchanged.
	if got := rig.bot.getActiveProject(100); got != "" {
		t.Errorf("rejected callback still mutated active project: %q", got)
	}
}

// TestProjectSelectCallback_ScopeEnforced guards the project:select IDOR fix:
// a scoped user cannot pin a project outside their allowed set (closing the
// divergence from the /project text path), but CAN select an allowed one.
func TestProjectSelectCallback_ScopeEnforced(t *testing.T) {
	rig := newCallbackRig(t)
	rig.bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"proj-a"}},
	}
	const chatID = int64(100)

	// Denied: user 111 is not cleared for proj-b → no mutation.
	if err := rig.bot.handleProjectCallback(context.Background(), chatID, 111, "cb-x", "select", "proj-b"); err != nil {
		t.Fatalf("handleProjectCallback(deny) err: %v", err)
	}
	if got := rig.bot.getActiveProject(chatID); got == "proj-b" {
		t.Errorf("scoped user pinned an unauthorized project: %q", got)
	}

	// Allowed: user 111 is cleared for proj-a (nil registry → existence
	// check skipped) → active project set.
	if err := rig.bot.handleProjectCallback(context.Background(), chatID, 111, "cb-y", "select", "proj-a"); err != nil {
		t.Fatalf("handleProjectCallback(allow) err: %v", err)
	}
	if got := rig.bot.getActiveProject(chatID); got != "proj-a" {
		t.Errorf("authorized select did not set active project: %q", got)
	}
}
