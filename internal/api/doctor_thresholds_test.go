package api

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
)

// The seam is only worth anything if the operator's value REACHES the check.
// A config key that is parsed, stored, and never read is the "parsed and did
// nothing" class this deployment keeps retiring — and it would be invisible
// here in exactly the way the unconfigurable thresholds were.

func TestDoctorThresholds_ReachTheChecksFromConfig(t *testing.T) {
	share := 0.25
	window := 7
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Doctor: config.DoctorConfig{Thresholds: config.DoctorThresholds{
			UnclassifiedShare:           &share,
			UnclassifiedShareWindowDays: &window,
		}},
	})

	got := h.doctorThresholds()
	if got.UnclassifiedShare.Value != 0.25 || !got.UnclassifiedShare.Configured {
		t.Errorf("UnclassifiedShare = %+v, want 0.25 configured", got.UnclassifiedShare)
	}
	if got.UnclassifiedShareWindowDays.Value != 7 {
		t.Errorf("window = %d, want 7", got.UnclassifiedShareWindowDays.Value)
	}
	// Keys the operator did NOT set still resolve to the shipped defaults, not
	// to zero — a partially configured section must not silently disable the
	// checks it says nothing about.
	if got.ModelFailureRate.Value != config.DefaultModelFailureRate || got.ModelFailureRate.Configured {
		t.Errorf("ModelFailureRate = %+v, want the default, unconfigured", got.ModelFailureRate)
	}
}

// A handler with NO config wired is the case that matters most: zero is a real
// threshold, so falling through to the zero value would make several checks
// warn on everything (0% tolerated) or judge a model on a single call.
func TestDoctorThresholds_UnwiredHandlerGetsDefaultsNotZeroes(t *testing.T) {
	got := (&DoctorHandlers{}).doctorThresholds()
	if got.UnclassifiedShare.Value != config.DefaultUnclassifiedShare {
		t.Errorf("UnclassifiedShare = %v, want the compiled default %v", got.UnclassifiedShare.Value, config.DefaultUnclassifiedShare)
	}
	if got.ModelMinSamples.Value != config.DefaultModelMinSamples {
		t.Errorf("ModelMinSamples = %v, want %v — a zero sample floor judges a model on one call",
			got.ModelMinSamples.Value, config.DefaultModelMinSamples)
	}
	if got.UnclassifiedShare.Configured {
		t.Error("an unwired handler must not claim its bounds were configured")
	}
	// Nil receiver too: several checks are reachable from surfaces that build
	// the handler lazily.
	var nilH *DoctorHandlers
	if v := nilH.doctorThresholds().ModelFailureRate.Value; v != config.DefaultModelFailureRate {
		t.Errorf("nil handler ModelFailureRate = %v, want %v", v, config.DefaultModelFailureRate)
	}
}

// ONE judgement, one knob. model_health and model_calls_live ask the same
// question off different evidence; giving each its own threshold would let a
// deployment answer "is this model failing?" two ways, and the two checks would
// then disagree in public.
func TestDoctorThresholds_ModelChecksShareOneBound(t *testing.T) {
	rate := 0.8
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Doctor: config.DoctorConfig{Thresholds: config.DoctorThresholds{ModelFailureRate: &rate}},
	})
	th := h.doctorThresholds()

	// Both call sites read the same field; this pins that there is only one to
	// read. (The check bodies are exercised by their own tests.)
	if th.ModelFailureRate.Value != 0.8 {
		t.Fatalf("ModelFailureRate = %v, want 0.8", th.ModelFailureRate.Value)
	}
	findings := evalModelHealth([]modelHealthStat{
		{model: "m", samples: 10, failures: 6, medianCompletionTokens: 500},
	}, nil, th)
	if len(findings) != 0 {
		t.Errorf("60%% failures under a configured 80%% threshold must not flag: %+v", findings)
	}
}

// The provenance must survive into what the operator reads, on the PASSING
// path too — that is where a mis-set threshold hides.
func TestDoctorThresholds_ModelCallsMessageNamesTheSource(t *testing.T) {
	stats := chat.NewCallStats()
	for i := 0; i < 6; i++ {
		stats.Record("m1", "dispatcher", nil)
	}
	h := &DoctorHandlers{callStats: stats}
	got := h.checkModelCallsLive()
	if !strings.Contains(got.Message, "(default)") {
		t.Errorf("a passing model_calls_live must name the threshold's source: %q", got.Message)
	}
}
