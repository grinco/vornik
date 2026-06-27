// Package telegram: tests for the bot's helper methods —
// budgetAlertRecipients (notification routing), resolveLeadSystemPrompt
// (nil-safety branches), getProjectListForUser (allowlist filtering).
package telegram

import (
	"sort"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/registry"
)

// makeBotWithUsers constructs a bot with the AllowedUsers map filled
// in. We don't need a working chat client — option setters / helpers
// don't dial out.
func makeBotWithUsers(t *testing.T, users map[int64]UserAccess) *Bot {
	t.Helper()
	chatClient := chat.NewClient("http://nope.invalid", "k", "m")
	bot, err := NewBot(BotConfig{Token: "x", AllowedUsers: users}, chatClient)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

// --- budgetAlertRecipients -------------------------------------------

func TestBudgetAlertRecipients_NoAllowedUsers(t *testing.T) {
	bot := makeBotWithUsers(t, nil)
	got := bot.budgetAlertRecipients("p1")
	if len(got) != 0 {
		t.Errorf("expected empty recipients with no users; got %v", got)
	}
}

func TestBudgetAlertRecipients_Wildcard(t *testing.T) {
	bot := makeBotWithUsers(t, map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
		222: {Allowed: false, Projects: []string{"*"}}, // not allowed → skipped
	})
	got := bot.budgetAlertRecipients("p1")
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 1 || got[0] != 111 {
		t.Errorf("wildcard: got %v, want [111]", got)
	}
}

func TestBudgetAlertRecipients_ExplicitProject(t *testing.T) {
	bot := makeBotWithUsers(t, map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"p1", "p2"}},
		222: {Allowed: true, Projects: []string{"p2"}},
		333: {Allowed: true, Projects: []string{"p1"}},
	})
	got := bot.budgetAlertRecipients("p1")
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != 2 || got[0] != 111 || got[1] != 333 {
		t.Errorf("got %v, want [111, 333]", got)
	}
}

func TestBudgetAlertRecipients_NotAllowedSkipped(t *testing.T) {
	bot := makeBotWithUsers(t, map[int64]UserAccess{
		111: {Allowed: false, Projects: []string{"p1"}},
	})
	got := bot.budgetAlertRecipients("p1")
	if len(got) != 0 {
		t.Errorf("not-allowed users should be excluded; got %v", got)
	}
}

// --- resolveLeadSystemPrompt -----------------------------------------

func TestResolveLeadSystemPrompt_NoRegistry(t *testing.T) {
	bot := makeBotWithUsers(t, nil)
	if got := bot.resolveLeadSystemPrompt(1, "p1"); got != "" {
		t.Errorf("nil registry: got %q, want empty", got)
	}
}

func TestResolveLeadSystemPrompt_EmptyProjectID(t *testing.T) {
	bot := makeBotWithUsers(t, nil)
	bot.registry = registry.New()
	if got := bot.resolveLeadSystemPrompt(1, ""); got != "" {
		t.Errorf("empty projectID: got %q, want empty", got)
	}
}

func TestResolveLeadSystemPrompt_ProjectNotFound(t *testing.T) {
	bot := makeBotWithUsers(t, nil)
	bot.registry = registry.New()
	if got := bot.resolveLeadSystemPrompt(1, "no-such"); got != "" {
		t.Errorf("missing project: got %q, want empty", got)
	}
}

// --- getProjectListForUser --------------------------------------------

func TestGetProjectListForUser_NoAllowlist_DevMode(t *testing.T) {
	// With nil/empty AllowedUsers, the helper enters dev-mode early
	// return: it returns whatever getProjectList() yields, without
	// per-user filtering. Empty registry → empty slice; pins the
	// branch.
	bot := makeBotWithUsers(t, nil)
	bot.registry = registry.New()
	got := bot.getProjectListForUser(1)
	if len(got) != 0 {
		t.Errorf("empty registry dev mode: got %d projects, want 0", len(got))
	}
}

func TestGetProjectListForUser_UserNotAllowed(t *testing.T) {
	bot := makeBotWithUsers(t, map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	})
	if got := bot.getProjectListForUser(222); got != nil {
		t.Errorf("user not in allowlist: got %v, want nil", got)
	}
}

func TestGetProjectListForUser_UserDenied(t *testing.T) {
	bot := makeBotWithUsers(t, map[int64]UserAccess{
		111: {Allowed: false, Projects: []string{"*"}},
	})
	if got := bot.getProjectListForUser(111); got != nil {
		t.Errorf("explicitly denied user: got %v, want nil", got)
	}
}
