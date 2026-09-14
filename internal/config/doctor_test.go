package config

import (
	"reflect"
	"strings"
	"testing"
)

// DOCTOR THRESHOLDS (BACKLOG 2026-08-27). Every numeric bound in the doctor
// surface was a Go literal, so retuning one meant editing Go and rebuilding —
// and the bounds that most need retuning are exactly the ones calibrated on a
// single fleet's workload, which no other deployment has a reason to share.
// Worse, the mismatch was invisible: the check either warned permanently or
// never warned, and neither reads as misconfiguration.

func TestDoctorThresholds_UnsetResolvesToTheCompiledDefault(t *testing.T) {
	var d DoctorConfig
	got := d.Thresholds.Resolve()

	if got.UnclassifiedShare.Value != DefaultUnclassifiedShare {
		t.Errorf("UnclassifiedShare = %v, want the compiled default %v", got.UnclassifiedShare.Value, DefaultUnclassifiedShare)
	}
	if got.UnclassifiedShare.Configured {
		t.Error("an unset threshold must not claim to be configured — that is the difference the check reports")
	}
	if got.ModelFailureRate.Value != DefaultModelFailureRate {
		t.Errorf("ModelFailureRate = %v, want %v", got.ModelFailureRate.Value, DefaultModelFailureRate)
	}
}

// Zero is a REAL value for a fraction (0% = warn on anything), so it cannot
// double as "unset". The fields are pointers for that reason, and this pins it:
// an explicit 0 must survive as 0 rather than resolving to the default.
func TestDoctorThresholds_ExplicitZeroIsNotUnset(t *testing.T) {
	zero := 0.0
	d := DoctorConfig{Thresholds: DoctorThresholds{UnclassifiedShare: &zero}}
	got := d.Thresholds.Resolve()

	if got.UnclassifiedShare.Value != 0 {
		t.Errorf("an explicit 0 resolved to %v; zero-as-unset would silently discard a deliberate setting", got.UnclassifiedShare.Value)
	}
	if !got.UnclassifiedShare.Configured {
		t.Error("an explicitly set threshold must report as configured")
	}
}

func TestDoctorThresholds_ConfiguredValueWins(t *testing.T) {
	v := 0.25
	n := 12
	d := DoctorConfig{Thresholds: DoctorThresholds{UnclassifiedShare: &v, ModelMinSamples: &n}}
	got := d.Thresholds.Resolve()

	if got.UnclassifiedShare.Value != 0.25 || !got.UnclassifiedShare.Configured {
		t.Errorf("UnclassifiedShare = %+v, want 0.25 configured", got.UnclassifiedShare)
	}
	if got.ModelMinSamples.Value != 12 || !got.ModelMinSamples.Configured {
		t.Errorf("ModelMinSamples = %+v, want 12 configured", got.ModelMinSamples)
	}
}

// A fraction outside [0,1] produces a check that can never fire — exactly the
// invisible failure this seam exists to remove. It must fail the boot, loudly,
// naming the key.
func TestDoctorThresholds_ValidateRejectsAnImpossibleFraction(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  DoctorThresholds
		want string
	}{
		{"share above one", DoctorThresholds{UnclassifiedShare: floatPtr(1.5)}, "doctor.thresholds.unclassified_share"},
		{"negative share", DoctorThresholds{UnclassifiedShare: floatPtr(-0.1)}, "doctor.thresholds.unclassified_share"},
		{"failure rate above one", DoctorThresholds{ModelFailureRate: floatPtr(2)}, "doctor.thresholds.model_failure_rate"},
		{"negative sample floor", DoctorThresholds{ModelMinSamples: intPtr(-1)}, "doctor.thresholds.model_min_samples"},
		{"zero window", DoctorThresholds{UnclassifiedShareWindowDays: intPtr(0)}, "doctor.thresholds.unclassified_share_window_days"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted; a bound that makes a check unable to fire must not boot", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the key %q — an operator cannot fix what is not named", err, tc.want)
			}
		})
	}
}

func TestDoctorThresholds_ValidateAcceptsTheBoundaries(t *testing.T) {
	ok := DoctorThresholds{
		UnclassifiedShare:           floatPtr(0),
		ModelFailureRate:            floatPtr(1),
		ModelMinSamples:             intPtr(0),
		CostAttributionFraction:     floatPtr(0.9),
		UnclassifiedShareWindowDays: intPtr(1),
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("boundary values rejected: %v", err)
	}
}

// Every tunable must be reachable from YAML AND documented, or it is a knob
// that exists only in Go — the thing this change is removing. WalkLeaves is
// what the generated config reference and the resolved-config dump both use,
// so a field it cannot see is a field neither surface will ever mention.
func TestDoctorThresholds_EveryFieldCarriesAYAMLKeyAndDoc(t *testing.T) {
	seen := 0
	WalkLeaves(reflect.ValueOf(DoctorConfig{}), func(key string, f reflect.StructField, _ reflect.Value) {
		if !strings.HasPrefix(key, "thresholds.") {
			return
		}
		seen++
		if f.Tag.Get("doc") == "" {
			t.Errorf("doctor.%s has no doc: tag; it would be absent from the generated config reference", key)
		}
	})
	if seen < 5 {
		t.Fatalf("WalkLeaves found %d threshold leaves; the section is not being enumerated", seen)
	}
}

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }
