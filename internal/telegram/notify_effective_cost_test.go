// Package telegram: tests for NotifyEffectiveCostDrift — the
// $/success model-regression notifier.
package telegram

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/budget"
)

func TestNotifyEffectiveCostDrift_NilBot(t *testing.T) {
	var b *Bot
	b.NotifyEffectiveCostDrift(context.Background(), budget.EffectiveCostAlert{Role: "coder", Model: "x"})
}

func TestNotifyEffectiveCostDrift_EmptyRoleOrModel(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.NotifyEffectiveCostDrift(context.Background(), budget.EffectiveCostAlert{Model: "x"})
	bot.NotifyEffectiveCostDrift(context.Background(), budget.EffectiveCostAlert{Role: "x"})
	if len(*calls) != 0 {
		t.Errorf("expected no calls; got %d", len(*calls))
	}
}

func TestNotifyEffectiveCostDrift_NoRecipients(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.NotifyEffectiveCostDrift(context.Background(), budget.EffectiveCostAlert{
		Role: "coder", Model: "gpt-4o", Ratio: 3.0,
	})
	if len(*calls) != 0 {
		t.Errorf("no allowed users → no calls; got %d", len(*calls))
	}
}

func TestNotifyEffectiveCostDrift_SendsToAllAllowed(t *testing.T) {
	bot, calls, cleanup := makeAutopilotBot(t)
	defer cleanup()
	bot.config.AllowedUsers = map[int64]UserAccess{
		111: {Allowed: true, Projects: []string{"p1"}},
		222: {Allowed: true, Projects: []string{"*"}},
		333: {Allowed: false}, // skipped
	}
	bot.NotifyEffectiveCostDrift(context.Background(), budget.EffectiveCostAlert{
		Role: "coder", Model: "gpt-4o", Ratio: 3.2,
		Current24hUSDPerSuccess: 0.45, Baseline7dUSDPerSuccess: 0.14,
		Spend24hUSD: 4.50, Successes24h: 10,
	})
	if len(*calls) != 2 {
		t.Errorf("expected 2 calls (one per allowed user); got %d", len(*calls))
	}
	if !strings.Contains((*calls)[0].Body, "effective-cost drift") {
		t.Errorf("expected drift headline; got %q", (*calls)[0].Body)
	}
	if !strings.Contains((*calls)[0].Body, "coder") || !strings.Contains((*calls)[0].Body, "gpt-4o") {
		t.Errorf("expected role+model; got %q", (*calls)[0].Body)
	}
}
