// Coverage for the telegram middleware: user access list,
// per-user rate limiting, and the runtime-mutation hooks the
// daemon's signal handlers call (AddAllowedUser, ClearRateLimits,
// etc). Pure data-structure manipulation — no network, no DB.

package telegram

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
)

// newBareTestBot builds a Bot with no chat client wiring beyond
// what NewBot strictly requires. Used by middleware tests that
// only exercise in-memory state.
func newBareTestBot(t *testing.T, cfg BotConfig) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	bot, err := NewBot(cfg, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

// TestAllowedProjectsForUser_NoAllowlistReturnsNil — when no
// allowlist is configured (dev / fully-trusted) the helper
// returns nil so downstream code skips the per-tool check.
func TestAllowedProjectsForUser_NoAllowlistReturnsNil(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if got := bot.AllowedProjectsForUser(42); got != nil {
		t.Errorf("no allowlist: got %v, want nil", got)
	}
}

// TestAllowedProjectsForUser_DeniedUserReturnsEmpty — a user in
// the map but with Allowed=false gets an empty non-nil slice
// (rejected structurally). Belt-and-suspenders behind IsAllowed.
func TestAllowedProjectsForUser_DeniedUserReturnsEmpty(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: false},
		},
	})
	got := bot.AllowedProjectsForUser(42)
	if got == nil || len(got) != 0 {
		t.Errorf("denied user: got %v, want empty non-nil slice", got)
	}
}

// TestAllowedProjectsForUser_WildcardReturnsNil — Projects=["*"]
// is the "any project" sentinel and again returns nil so the
// caller skips the check.
func TestAllowedProjectsForUser_WildcardReturnsNil(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"*"}},
		},
	})
	if got := bot.AllowedProjectsForUser(42); got != nil {
		t.Errorf("wildcard user: got %v, want nil", got)
	}
}

// TestAllowedProjectsForUser_ScopedReturnsCopy — a scoped user
// gets a defensive copy of their project list so the caller
// can't mutate the bot's config.
func TestAllowedProjectsForUser_ScopedReturnsCopy(t *testing.T) {
	original := []string{"alpha", "beta"}
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: original},
		},
	})
	got := bot.AllowedProjectsForUser(42)
	if len(got) != 2 {
		t.Fatalf("scoped user: got %d projects, want 2", len(got))
	}
	got[0] = "MUTATED"
	if original[0] != "alpha" {
		t.Errorf("returned slice must be a copy; caller mutation leaked: %v", original)
	}
}

// TestAllowedProjectsForUser_UnknownReturnsEmpty — userID not in
// the map is treated as denied (Allowed=false default).
func TestAllowedProjectsForUser_UnknownReturnsEmpty(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"alpha"}},
		},
	})
	got := bot.AllowedProjectsForUser(99)
	if got == nil || len(got) != 0 {
		t.Errorf("unknown user: got %v, want empty non-nil slice", got)
	}
}

// TestCheckRateLimit_Disabled covers the "no limit configured"
// path. When RateLimit <= 0, every check passes.
func TestCheckRateLimit_Disabled(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t", RateLimit: 0})
	for i := 0; i < 1000; i++ {
		if !bot.CheckRateLimit(42) {
			t.Fatalf("disabled rate limit blocked iteration %d", i)
		}
	}
}

// TestCheckRateLimit_BlocksAfterLimit walks the counter past the
// limit and verifies the gate flips closed at the right
// iteration. Each successful check increments the counter; the
// (N+1)th call within the window returns false.
func TestCheckRateLimit_BlocksAfterLimit(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t", RateLimit: 3})
	for i := 0; i < 3; i++ {
		if !bot.CheckRateLimit(42) {
			t.Errorf("call %d should pass under limit 3", i)
		}
	}
	if bot.CheckRateLimit(42) {
		t.Error("4th call must be blocked under limit 3")
	}
}

// TestGetRateLimitStatus covers the three branches:
//   - rate limiting disabled
//   - user not yet seen (returns count=0, full window left)
//   - user under limit (returns current count + remaining)
func TestGetRateLimitStatus(t *testing.T) {
	disabled := newBareTestBot(t, BotConfig{Token: "t", RateLimit: 0})
	count, reset, limited := disabled.GetRateLimitStatus(42)
	if count != 0 || reset != 0 || limited {
		t.Errorf("disabled: got count=%d reset=%v limited=%v, want 0/0/false", count, reset, limited)
	}

	bot := newBareTestBot(t, BotConfig{Token: "t", RateLimit: 5})
	count, reset, limited = bot.GetRateLimitStatus(42)
	if count != 0 || reset != time.Minute || limited {
		t.Errorf("unseen user: got count=%d reset=%v limited=%v, want 0/1m/false", count, reset, limited)
	}

	_ = bot.CheckRateLimit(42)
	_ = bot.CheckRateLimit(42)
	count, reset, limited = bot.GetRateLimitStatus(42)
	if count != 2 {
		t.Errorf("after 2 calls: got count=%d, want 2", count)
	}
	if reset <= 0 || reset > time.Minute {
		t.Errorf("after 2 calls: reset=%v, want >0 and <=1m", reset)
	}
	if limited {
		t.Error("after 2/5 calls: must not be limited yet")
	}
}

// TestClearRateLimits — operator hook for "clear all rate
// limits" via signal handler. After clearing, a previously
// limited user is fresh.
func TestClearRateLimits(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t", RateLimit: 1})
	bot.CheckRateLimit(42) // count=1
	if bot.CheckRateLimit(42) {
		t.Fatal("precondition: 42 should be limited")
	}
	bot.ClearRateLimits()
	if !bot.CheckRateLimit(42) {
		t.Error("after ClearRateLimits, 42 should pass again")
	}
}

// TestAddAllowedUser_AndGetAllowedUsers covers the operator
// hooks for runtime allowlist mutation.
func TestAddAllowedUser_AndGetAllowedUsers(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if got := bot.GetAllowedUsers(); len(got) != 0 {
		t.Errorf("initial: got %v users, want empty", got)
	}
	bot.AddAllowedUser(101)
	bot.AddAllowedUser(202)
	got := bot.GetAllowedUsers()
	if len(got) != 2 {
		t.Errorf("after adding 2: got %d, want 2 (list=%v)", len(got), got)
	}
}

// TestRemoveAllowedUser deletes a user from the allowlist and
// verifies the change.
func TestRemoveAllowedUser(t *testing.T) {
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	bot.AddAllowedUser(42)
	bot.RemoveAllowedUser(42)
	if got := bot.GetAllowedUsers(); len(got) != 0 {
		t.Errorf("after remove: got %v, want empty", got)
	}
}

// TestUserAccessHelpers covers UserAccess.Wildcard +
// CanAccessProject across the three shapes. These are pure
// methods on a small struct; locking them down so the matrix
// stays stable.
func TestUserAccessHelpers(t *testing.T) {
	tests := []struct {
		name      string
		ua        UserAccess
		project   string
		wantWild  bool
		wantAllow bool
	}{
		{
			"wildcard allowed",
			UserAccess{Allowed: true, Projects: []string{"*"}},
			"any-project",
			true, true,
		},
		{
			"scoped match",
			UserAccess{Allowed: true, Projects: []string{"alpha", "beta"}},
			"beta",
			false, true,
		},
		{
			"scoped miss",
			UserAccess{Allowed: true, Projects: []string{"alpha"}},
			"gamma",
			false, false,
		},
		{
			"denied user even with projects",
			UserAccess{Allowed: false, Projects: []string{"alpha"}},
			"alpha",
			false, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ua.Wildcard(); got != tc.wantWild {
				t.Errorf("Wildcard: got %v, want %v", got, tc.wantWild)
			}
			if got := tc.ua.CanAccessProject(tc.project); got != tc.wantAllow {
				t.Errorf("CanAccessProject(%q): got %v, want %v", tc.project, got, tc.wantAllow)
			}
		})
	}
}
