package config

import (
	"strings"
	"testing"
	"time"
)

func TestCostTuningAutoApplyConfig_ResolvedDefaults(t *testing.T) {
	r := CostTuningAutoApplyConfig{}.Resolved()
	if r.Enabled {
		t.Error("default must be disabled")
	}
	if len(r.Swarms) != 0 {
		t.Error("default allow-list must be empty (= NONE)")
	}
	if r.MinPassedCanaries != 2 {
		t.Errorf("default K = %d, want 2", r.MinPassedCanaries)
	}
	if r.ScanInterval != 15*time.Minute {
		t.Errorf("default scan_interval = %v, want 15m", r.ScanInterval)
	}
}

func TestCostTuningAutoApplyConfig_ResolvedOverrides(t *testing.T) {
	r := CostTuningAutoApplyConfig{
		Enabled: true, Swarms: []string{"assistant-swarm"},
		MinPassedCanaries: 3, ScanInterval: "5m",
	}.Resolved()
	if !r.Enabled || r.MinPassedCanaries != 3 || r.ScanInterval != 5*time.Minute || len(r.Swarms) != 1 {
		t.Errorf("overrides not applied: %+v", r)
	}
	// K floor: an explicit 0 resolves to the default 2 (never auto-applies an
	// unproven knob).
	if k := (CostTuningAutoApplyConfig{MinPassedCanaries: 0}).Resolved().MinPassedCanaries; k != 2 {
		t.Errorf("K=0 must resolve to 2, got %d", k)
	}
}

func TestCostTuningAutoApplyConfig_Validate(t *testing.T) {
	if err := (CostTuningAutoApplyConfig{}).Validate(); err != nil {
		t.Errorf("empty block must be valid, got %v", err)
	}
	if err := (CostTuningAutoApplyConfig{MinPassedCanaries: 5, ScanInterval: "10m"}).Validate(); err != nil {
		t.Errorf("valid block rejected: %v", err)
	}
	if err := (CostTuningAutoApplyConfig{MinPassedCanaries: -1}).Validate(); err == nil || !strings.Contains(err.Error(), "min_passed_canaries") {
		t.Errorf("negative K must be rejected, got %v", err)
	}
	if err := (CostTuningAutoApplyConfig{ScanInterval: "nope"}).Validate(); err == nil {
		t.Error("unparseable scan_interval must be rejected")
	}
	if err := (CostTuningAutoApplyConfig{ScanInterval: "-5m"}).Validate(); err == nil {
		t.Error("non-positive scan_interval must be rejected")
	}
}
