package config

import (
	"strings"
	"testing"
	"time"
)

func TestCostTuningCanaryConfig_ResolvedDefaults(t *testing.T) {
	// An all-zero block resolves to the design §8 defaults.
	r := CostTuningCanaryConfig{}.Resolved()
	if r.Enabled {
		t.Errorf("default Enabled must be false (ships off)")
	}
	if r.ScanInterval != 15*time.Minute {
		t.Errorf("ScanInterval default = %v, want 15m", r.ScanInterval)
	}
	if r.Window != 168*time.Hour {
		t.Errorf("Window default = %v, want 168h", r.Window)
	}
	if r.MinSamples != 20 || r.A2MinSamples != 10 || r.A2Subwindows != 4 {
		t.Errorf("sample defaults = %d/%d/%d, want 20/10/4", r.MinSamples, r.A2MinSamples, r.A2Subwindows)
	}
	if r.MarginA1 != 0.05 || r.MarginA2 != 0.10 || r.MarginCost != 0.15 {
		t.Errorf("margin defaults = %v/%v/%v, want 0.05/0.10/0.15", r.MarginA1, r.MarginA2, r.MarginCost)
	}
	if r.Cooldown != 336*time.Hour || r.MaxCanaryAge != 336*time.Hour {
		t.Errorf("cooldown/max_age defaults = %v/%v, want 336h/336h", r.Cooldown, r.MaxCanaryAge)
	}
}

func TestCostTuningCanaryConfig_ResolvedOverrides(t *testing.T) {
	c := CostTuningCanaryConfig{
		Enabled: true, Swarms: []string{"assistant-swarm"},
		ScanInterval: "5m", Window: "72h", MinSamples: 30, A2MinSamples: 15,
		A2Subwindows: 6, MarginA1: 0.03, MarginA2: 0.08, MarginCost: 0.2,
		Cooldown: "100h", MaxCanaryAge: "200h",
	}
	r := c.Resolved()
	if !r.Enabled || len(r.Swarms) != 1 || r.ScanInterval != 5*time.Minute || r.Window != 72*time.Hour ||
		r.MinSamples != 30 || r.A2MinSamples != 15 || r.A2Subwindows != 6 ||
		r.MarginA1 != 0.03 || r.MarginA2 != 0.08 || r.MarginCost != 0.2 ||
		r.Cooldown != 100*time.Hour || r.MaxCanaryAge != 200*time.Hour {
		t.Fatalf("overrides not applied: %+v", r)
	}
}

func TestCostTuningCanaryConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     CostTuningCanaryConfig
		wantErr string
	}{
		{"empty is valid (defaults)", CostTuningCanaryConfig{}, ""},
		{"valid full", CostTuningCanaryConfig{Enabled: true, ScanInterval: "15m", Window: "168h", MarginA1: 0.05, MarginA2: 0.10}, ""},
		{"bad duration", CostTuningCanaryConfig{Window: "168hours"}, "invalid duration"},
		{"non-positive duration", CostTuningCanaryConfig{ScanInterval: "0s"}, "must be positive"},
		{"negative samples", CostTuningCanaryConfig{MinSamples: -1}, "must be >= 0"},
		{"margin out of range", CostTuningCanaryConfig{MarginA1: 1.5}, "must be in [0,1)"},
		{"a1 >= a2 incoherent", CostTuningCanaryConfig{MarginA1: 0.2, MarginA2: 0.1}, "must be < margin_a2"},
		{"a1 == a2 incoherent", CostTuningCanaryConfig{MarginA1: 0.1, MarginA2: 0.1}, "must be < margin_a2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
