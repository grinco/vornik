package registry

import (
	"strings"
	"testing"
)

// These tests pin the project- and swarm-level semantic-validation rules
// and the inter-project consent/depth helpers that gate cross-project
// orchestration. They construct structs directly so each reject branch is
// exercised in isolation with its operator-facing Field/Message asserted.
// Value-add coverage that LoadProjects/LoadSwarms YAML round-trips don't
// reach: the trading.mode/cap range rejections, the call/spawn allowlist
// matchers, and the swarm role/lead-role rules.

// helper: a minimal valid project we can mutate per-case.
func validProject() *Project {
	return &Project{
		ID:                "p",
		SwarmID:           "s",
		DefaultWorkflowID: "w",
	}
}

// --- trading block semantic validation ---

func TestProjectValidate_TradingModeRejectsBogusValue(t *testing.T) {
	p := validProject()
	p.Trading.Mode = "casino"
	err := p.Validate("p.yaml")
	if err == nil || !strings.Contains(err.Error(), "paper") {
		t.Fatalf("bogus trading.mode should be rejected, got: %v", err)
	}
	if verr, ok := err.(ProjectValidationError); ok && verr.Field != "trading.mode" {
		t.Errorf("field should be trading.mode, got %q", verr.Field)
	}
}

func TestProjectValidate_TradingModeAcceptsValidValues(t *testing.T) {
	for _, mode := range []string{"", "paper", "live"} {
		t.Run("mode="+mode, func(t *testing.T) {
			p := validProject()
			p.Trading.Mode = mode
			if err := p.Validate("p.yaml"); err != nil {
				t.Fatalf("trading.mode %q should be valid, got: %v", mode, err)
			}
		})
	}
}

func TestProjectValidate_DrawdownCapOutOfRange_Rejected(t *testing.T) {
	for _, pct := range []float64{-1, 100.1} {
		p := validProject()
		p.Trading.Caps.DrawdownCircuitBreakerPct = pct
		err := p.Validate("p.yaml")
		if err == nil || !strings.Contains(err.Error(), "between 0 and 100") {
			t.Fatalf("drawdown pct %v should be rejected, got: %v", pct, err)
		}
	}
}

func TestProjectValidate_DailyLossCapOutOfRange_Rejected(t *testing.T) {
	p := validProject()
	p.Trading.Caps.DailyLossCircuitBreakerPct = 150
	err := p.Validate("p.yaml")
	if err == nil || !strings.Contains(err.Error(), "daily_loss_circuit_breaker_pct") {
		t.Fatalf("daily-loss pct out of range should be rejected, got: %v", err)
	}
}

func TestProjectValidate_NegativeTradingCaps_Rejected(t *testing.T) {
	cases := map[string]func(*Project){
		"max_position_usd":       func(p *Project) { p.Trading.Caps.MaxPositionUSD = -1 },
		"max_daily_turnover_usd": func(p *Project) { p.Trading.Caps.MaxDailyTurnoverUSD = -1 },
		"max_orders_per_hour":    func(p *Project) { p.Trading.Caps.MaxOrdersPerHour = -1 },
		"max_orders_per_minute":  func(p *Project) { p.Trading.Caps.MaxOrdersPerMinute = -1 },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			p := validProject()
			mutate(p)
			err := p.Validate("p.yaml")
			if err == nil || !strings.Contains(err.Error(), "cannot be negative") {
				t.Fatalf("negative %s should be rejected, got: %v", field, err)
			}
		})
	}
}

// --- inter-project consent gates (CanCall, AcceptsCallsFrom, spawn) ---

func TestProject_CanCall_Matrix(t *testing.T) {
	tests := []struct {
		name   string
		allow  []string
		callee string
		want   bool
	}{
		{"empty list allows all", nil, "anything", true},
		{"exact match", []string{"reports"}, "reports", true},
		{"non-match exact", []string{"reports"}, "trading", false},
		{"wildcard star", []string{"*"}, "trading", true},
		{"glob prefix match", []string{"team-*"}, "team-alpha", true},
		{"glob prefix non-match", []string{"team-*"}, "alpha-team", false},
		{"empty callee refused", []string{"*"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Project{CanCallProjects: tt.allow}
			if got := p.CanCall(tt.callee); got != tt.want {
				t.Errorf("CanCall(%q) with %v: got %v want %v", tt.callee, tt.allow, got, tt.want)
			}
		})
	}
}

func TestProject_CanCall_NilReceiver(t *testing.T) {
	var p *Project
	if p.CanCall("x") {
		t.Error("nil project should not be able to call")
	}
}

func TestProject_AcceptsCallsFrom_Matrix(t *testing.T) {
	tests := []struct {
		name   string
		accept []string
		caller string
		want   bool
	}{
		{"empty list closed", nil, "anyone", false},
		{"exact match", []string{"orchestrator"}, "orchestrator", true},
		{"wildcard", []string{"*"}, "orchestrator", true},
		{"glob match", []string{"team-*"}, "team-beta", true},
		{"no match", []string{"team-*"}, "solo", false},
		{"empty caller refused", []string{"*"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Project{AcceptCallsFrom: tt.accept}
			if got := p.AcceptsCallsFrom(tt.caller); got != tt.want {
				t.Errorf("AcceptsCallsFrom(%q) with %v: got %v want %v", tt.caller, tt.accept, got, tt.want)
			}
		})
	}
}

func TestProject_AllowsSpawnTemplate_Matrix(t *testing.T) {
	tests := []struct {
		name     string
		allow    []string
		template string
		want     bool
	}{
		{"empty list secure default deny", nil, "sales-campaign", false},
		{"exact match", []string{"sales-campaign"}, "sales-campaign", true},
		{"wildcard", []string{"*"}, "anything", true},
		{"glob match", []string{"sales-*"}, "sales-q3", true},
		{"glob non-match", []string{"sales-*"}, "ops-runbook", false},
		{"empty template refused", []string{"*"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Project{AllowSpawn: ProjectAllowSpawn{Templates: tt.allow}}
			if got := p.AllowsSpawnTemplate(tt.template); got != tt.want {
				t.Errorf("AllowsSpawnTemplate(%q) with %v: got %v want %v", tt.template, tt.allow, got, tt.want)
			}
		})
	}
}

func TestProject_EffectiveMaxCallDepth(t *testing.T) {
	if d := validProject().EffectiveMaxCallDepth(); d != DefaultMaxCallDepth {
		t.Errorf("unset maxCallDepth should default to %d, got %d", DefaultMaxCallDepth, d)
	}
	p := validProject()
	p.MaxCallDepth = 3
	if d := p.EffectiveMaxCallDepth(); d != 3 {
		t.Errorf("explicit maxCallDepth should win, got %d", d)
	}
	p.MaxCallDepth = -5 // negative falls back to default (secure)
	if d := p.EffectiveMaxCallDepth(); d != DefaultMaxCallDepth {
		t.Errorf("negative maxCallDepth should fall back to %d, got %d", DefaultMaxCallDepth, d)
	}
	var nilP *Project
	if d := nilP.EffectiveMaxCallDepth(); d != DefaultMaxCallDepth {
		t.Errorf("nil project should get default depth, got %d", d)
	}
}

// --- swarm semantic validation ---

func validSwarm() *Swarm {
	return &Swarm{
		ID: "s",
		Roles: []SwarmRole{
			{Name: "coder", Runtime: SwarmRoleRuntime{Image: "img:latest"}},
		},
	}
}

func TestSwarmValidate_DuplicateRoleName_Rejected(t *testing.T) {
	s := validSwarm()
	s.Roles = append(s.Roles, SwarmRole{Name: "coder", Runtime: SwarmRoleRuntime{Image: "img:latest"}})
	err := s.Validate("s.md")
	if err == nil || !strings.Contains(err.Error(), "duplicate role name") {
		t.Fatalf("duplicate role name should be rejected, got: %v", err)
	}
}

func TestSwarmValidate_RoleMissingImage_Rejected(t *testing.T) {
	s := validSwarm()
	s.Roles[0].Runtime.Image = ""
	err := s.Validate("s.md")
	if err == nil || !strings.Contains(err.Error(), "runtime image is required") {
		t.Fatalf("role without runtime image should be rejected, got: %v", err)
	}
}

func TestSwarmValidate_BadRuntimePolicy_Rejected(t *testing.T) {
	s := validSwarm()
	s.Roles[0].RuntimePolicy = "lukewarm"
	err := s.Validate("s.md")
	if err == nil || !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("invalid runtimePolicy should be rejected, got: %v", err)
	}
}

func TestSwarmValidate_LeadRoleMustExist(t *testing.T) {
	s := validSwarm()
	s.LeadRole = "ghost"
	err := s.Validate("s.md")
	if err == nil || !strings.Contains(err.Error(), "leadRole") {
		t.Fatalf("unknown leadRole should be rejected, got: %v", err)
	}

	// A leadRole that names a real role is accepted.
	s.LeadRole = "coder"
	if err := s.Validate("s.md"); err != nil {
		t.Fatalf("valid leadRole should be accepted, got: %v", err)
	}
}

func TestSwarmValidate_OutputSchemaConflictsWithLegacyFields(t *testing.T) {
	// outputSchema is the single source of truth: pairing it with the
	// legacy requiredOutputKeys block is the exact drift the schema was
	// added to prevent, and Validate must refuse it.
	s := validSwarm()
	s.Roles[0].OutputSchema = &OutputSchema{}
	s.Roles[0].RequiredOutputKeys = []string{"status"}
	err := s.Validate("s.md")
	if err == nil || !strings.Contains(err.Error(), "outputSchema") {
		t.Fatalf("outputSchema + requiredOutputKeys should conflict, got: %v", err)
	}
}
