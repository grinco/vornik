package config

import (
	"strings"
	"testing"
)

func TestComposerDefaultsApplied(t *testing.T) {
	c := DefaultConfig()
	if c.Composer.Enabled {
		t.Error("composer should default to disabled")
	}
	got := c.Composer
	if got.MaxTier != ComposerDefaultMaxTier {
		t.Errorf("MaxTier = %d, want %d", got.MaxTier, ComposerDefaultMaxTier)
	}
	if got.MaxTier3Turns != ComposerDefaultMaxTier3Turns {
		t.Errorf("MaxTier3Turns = %d, want %d", got.MaxTier3Turns, ComposerDefaultMaxTier3Turns)
	}
	if got.DefaultBudget.DailySoftUSD != 1.00 || got.DefaultBudget.DailyHardUSD != 3.00 {
		t.Errorf("daily budget = %+v, want soft 1.00 hard 3.00", got.DefaultBudget)
	}
	if got.DefaultBudget.MonthlySoftUSD != 15.00 || got.DefaultBudget.MonthlyHardUSD != 40.00 {
		t.Errorf("monthly budget = %+v, want soft 15.00 hard 40.00", got.DefaultBudget)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("default composer config should validate: %v", err)
	}
}

func TestComposerApplyDefaultsFillsZeros(t *testing.T) {
	c := ComposerConfig{Enabled: true} // everything else zero
	c.applyDefaults()
	if c.MaxTier != 3 || c.MaxTier3Turns != 10 {
		t.Errorf("caps not filled: %+v", c)
	}
	if c.DefaultBudget.DailyHardUSD != 3.00 {
		t.Errorf("budget not filled: %+v", c.DefaultBudget)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("filled config should validate: %v", err)
	}
}

func TestComposerValidateSoftExceedsHard(t *testing.T) {
	c := ComposerConfig{MaxTier: 3, MaxTier3Turns: 10, DefaultBudget: ComposerBudget{
		DailySoftUSD: 5, DailyHardUSD: 3, MonthlySoftUSD: 15, MonthlyHardUSD: 40,
	}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "daily_soft_usd cannot exceed") {
		t.Errorf("expected daily soft>hard error, got %v", err)
	}

	c2 := ComposerConfig{MaxTier: 3, MaxTier3Turns: 10, DefaultBudget: ComposerBudget{
		DailySoftUSD: 1, DailyHardUSD: 3, MonthlySoftUSD: 50, MonthlyHardUSD: 40,
	}}
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "monthly_soft_usd cannot exceed") {
		t.Errorf("expected monthly soft>hard error, got %v", err)
	}
}

func TestComposerValidateZeroCapRejected(t *testing.T) {
	c := ComposerConfig{MaxTier: 3, MaxTier3Turns: 10, DefaultBudget: ComposerBudget{
		DailySoftUSD: 1, DailyHardUSD: 0, MonthlySoftUSD: 15, MonthlyHardUSD: 40,
	}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must be > 0") {
		t.Errorf("expected zero-cap error, got %v", err)
	}
}

func TestComposerValidateNegative(t *testing.T) {
	c := ComposerConfig{MaxTier: 3, MaxTier3Turns: -1, DefaultBudget: ComposerBudget{
		DailySoftUSD: 1, DailyHardUSD: 3, MonthlySoftUSD: 15, MonthlyHardUSD: 40,
	}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_tier3_turns cannot be negative") {
		t.Errorf("expected negative-turns error, got %v", err)
	}

	c2 := ComposerConfig{MaxTier: 3, MaxTier3Turns: 10, DefaultBudget: ComposerBudget{
		DailySoftUSD: -1, DailyHardUSD: 3, MonthlySoftUSD: 15, MonthlyHardUSD: 40,
	}}
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Errorf("expected negative-cap error, got %v", err)
	}
}

func TestComposerValidateMaxTierRange(t *testing.T) {
	for _, tier := range []int{0, 4, -1} {
		c := ComposerConfig{MaxTier: tier, MaxTier3Turns: 10, DefaultBudget: ComposerBudget{
			DailySoftUSD: 1, DailyHardUSD: 3, MonthlySoftUSD: 15, MonthlyHardUSD: 40,
		}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_tier must be between 1 and 3") {
			t.Errorf("tier %d: expected range error, got %v", tier, err)
		}
	}
}

func TestComposerParseFromYAML(t *testing.T) {
	yaml := `
server:
  address: ":8080"
database:
  driver: postgres
  host: localhost
  port: 5432
  name: db
  user: u
api:
  auth_enabled: false
composer:
  enabled: true
  max_tier: 2
  max_tier3_turns: 5
  default_budget:
    daily_soft_usd: 0.50
    daily_hard_usd: 2.00
    monthly_soft_usd: 10.00
    monthly_hard_usd: 30.00
`
	if err := ValidateBytes([]byte(yaml)); err != nil {
		t.Fatalf("valid composer YAML failed: %v", err)
	}
}

func TestComposerEnabledOnlyGetsDefaultBudget(t *testing.T) {
	// An operator who flips only enabled must still get a valid,
	// non-zero default budget (ValidateBytes runs applyDefaults).
	yaml := `
server:
  address: ":8080"
database:
  driver: postgres
  host: localhost
  port: 5432
  name: db
  user: u
api:
  auth_enabled: false
composer:
  enabled: true
`
	if err := ValidateBytes([]byte(yaml)); err != nil {
		t.Fatalf("enabled-only composer YAML failed: %v", err)
	}
}
