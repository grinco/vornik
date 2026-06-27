// Package telegram: tests for NotifyBudgetBreach — the budget alert
// notification routing. Uses the rewriting-transport rig so the
// outbound telegram API calls hit a captured-stub instead of the
// real bot.
package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/budget"
)

func TestNotifyBudgetBreach_NilBot(t *testing.T) {
	var b *Bot
	// Must not panic.
	b.NotifyBudgetBreach(context.Background(), "p1", "soft", "daily", budget.Decision{})
}

func TestNotifyBudgetBreach_EmptyFields(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	// Empty projectID / level / period should each short-circuit.
	bot.NotifyBudgetBreach(context.Background(), "", "soft", "daily", budget.Decision{})
	bot.NotifyBudgetBreach(context.Background(), "p1", "", "daily", budget.Decision{})
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "", budget.Decision{})
	if len(*calls) != 0 {
		t.Errorf("expected no API calls; got %d", len(*calls))
	}
}

func TestNotifyBudgetBreach_NoRecipients(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	// AllowedUsers is empty → no recipients → no calls.
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "daily", budget.Decision{DailyUSD: 1.23})
	if len(*calls) != 0 {
		t.Errorf("expected no API calls; got %d", len(*calls))
	}
}

func TestNotifyBudgetBreach_SoftDaily(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	d := budget.Decision{DailyUSD: 4.56, Reason: "approaching cap"}
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "daily", d)
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(*calls))
	}
	body := (*calls)[0].Body
	if !strings.Contains(body, "soft cap breached") {
		t.Errorf("expected soft-cap headline; got %q", body)
	}
	if !strings.Contains(body, "4.56") {
		t.Errorf("expected daily figure; got %q", body)
	}
	if !strings.Contains(body, "approaching cap") {
		t.Errorf("expected reason text; got %q", body)
	}
}

func TestNotifyBudgetBreach_HardMonthly(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	d := budget.Decision{MonthlyUSD: 99.99, Reason: "hit ceiling"}
	bot.NotifyBudgetBreach(context.Background(), "p1", "hard", "monthly", d)
	if len(*calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(*calls))
	}
	body := (*calls)[0].Body
	if !strings.Contains(body, "hard cap hit") {
		t.Errorf("expected hard-cap headline; got %q", body)
	}
	if !strings.Contains(body, "99.99") {
		t.Errorf("expected monthly figure; got %q", body)
	}
}

func TestNotifyBudgetBreach_Dedup(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	d := budget.Decision{DailyUSD: 1.0}
	// First call should send; second identical call should dedup.
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "daily", d)
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "daily", d)
	if len(*calls) != 1 {
		t.Errorf("expected 1 call (deduped); got %d", len(*calls))
	}
}

func TestNotifyBudgetBreach_UnknownPeriod(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"*"}},
	}
	// "weekly" isn't in the switch — periodKey defaults to "unknown".
	// The handler still fires the send (alert is best-effort even on
	// unknown buckets).
	d := budget.Decision{DailyUSD: 1.0}
	bot.NotifyBudgetBreach(context.Background(), "p1", "soft", "weekly", d)
	if len(*calls) != 1 {
		t.Errorf("expected 1 call for unknown period; got %d", len(*calls))
	}
}
