package config

import "fmt"

// DoctorConfig holds the doctor surface's tunable numeric bounds — the ones an
// operator is expected to retune per deployment.
//
// Every bound in the doctor surface was a Go literal until 2026-09-13, so
// retuning one meant editing Go and rebuilding. That is defensible for bounds
// nobody retunes; it is not for the ones calibrated on ONE fleet's workload,
// which is what most of these are — `unclassified_share`'s default was fitted
// to this deployment's failure mix, and no other deployment has a reason to
// share it.
//
// The invisibility was the real cost. A threshold that does not suit a
// deployment makes its check warn permanently or never warn, and neither reads
// as misconfiguration — so the operator concludes the check is noisy, or that
// the thing it watches never happens.
//
// WHAT IS DELIBERATELY NOT HERE. Only bounds that express a POLICY judgement
// about a workload are tunable. The protective caps stay in Go — row-scan
// limits, max-entries-rendered, file-size ceilings — because they exist to stop
// a check hurting the daemon, not to express an opinion about the fleet, and an
// operator raising one is configuring a self-inflicted outage rather than
// tuning a signal.
//
// A TYPED STRUCT, not a free `map[string]float64`. A map accepts a typo
// silently and the knob then does nothing — the exact failure class this
// change exists to remove. A misspelt key here is a field that does not exist.
//
// Backlog: "Every doctor threshold is a compile-time constant" (2026-08-27).
type DoctorConfig struct {
	// NO doc: tag on this field, deliberately — WalkLeaves treats a
	// doc-tagged struct as a single value and stops descending, which would
	// hide every key below it from the generated config reference and the
	// resolved-config dump. Sections carry no doc; leaves do.
	Thresholds DoctorThresholds `yaml:"thresholds" json:"thresholds"`
}

// Compiled defaults. Each is the value its check shipped with, and the
// reasoning for each lives at the check, not here — this file owns the seam,
// not the calibration.
const (
	// DefaultUnclassifiedShare — re-derived 2026-09-02 against the production
	// ledger after MODEL_UNHEALTHY classification removed five-sixths of the
	// population the old 15% was fitted to. See doctor_unclassified_share.go.
	DefaultUnclassifiedShare = 0.05
	// DefaultUnclassifiedShareWindowDays bounds the check to recent evidence.
	DefaultUnclassifiedShareWindowDays = 30
	// DefaultModelFailureRate is the failed fraction at or above which a model
	// (or model/call-site pair) is flagged.
	DefaultModelFailureRate = 0.5
	// DefaultModelMinSamples is the smallest sample count worth judging a model
	// on: below it, one or two bad calls should not trip an alarm, and an
	// operator who learns to ignore a noisy check is worse off than one with no
	// check.
	DefaultModelMinSamples = 5
	// DefaultCostAttributionFraction is the share of recent cost rows that must
	// come from DB-backed keys before the check is satisfied.
	DefaultCostAttributionFraction = 0.90
	// DefaultCostAttributionMinTotal is the sample floor below which the
	// distribution says nothing.
	DefaultCostAttributionMinTotal = 10
	// DefaultFallbackRungMinAttempts is how many attempts a fallback rung needs
	// before a zero-success record means anything.
	DefaultFallbackRungMinAttempts = 2
)

// DoctorThresholds is the operator's half. Every field is a POINTER because
// zero is a real value for all of them — 0% means "warn on anything", and a
// zero sample floor means "judge on one call" — so zero cannot double as
// "unset" without silently discarding a deliberate setting.
type DoctorThresholds struct {
	UnclassifiedShare *float64 `yaml:"unclassified_share" json:"unclassified_share,omitempty" doc:"Share of classified step failures that may be unclassified before unclassified_step_failures warns (0-1). Default 0.05."`

	UnclassifiedShareWindowDays *int `yaml:"unclassified_share_window_days" json:"unclassified_share_window_days,omitempty" doc:"How many days of failures unclassified_step_failures divides over. Default 30."`

	ModelFailureRate *float64 `yaml:"model_failure_rate" json:"model_failure_rate,omitempty" doc:"Failed fraction at or above which model_health and model_calls_live flag a model (0-1). Default 0.5."`

	ModelMinSamples *int `yaml:"model_min_samples" json:"model_min_samples,omitempty" doc:"Smallest call count model_health and model_calls_live will judge a model on. Default 5."`

	CostAttributionFraction *float64 `yaml:"cost_attribution_fraction" json:"cost_attribution_fraction,omitempty" doc:"Share of recent cost rows that must carry a DB-backed key before cost_attribution is satisfied (0-1). Default 0.9."`

	CostAttributionMinTotal *int `yaml:"cost_attribution_min_total" json:"cost_attribution_min_total,omitempty" doc:"Sample floor below which cost_attribution stays OK regardless of distribution. Default 10."`

	FallbackRungMinAttempts *int `yaml:"fallback_rung_min_attempts" json:"fallback_rung_min_attempts,omitempty" doc:"Attempts a fallback rung needs before a zero-success record is reported. Default 2."`
}

// ResolvedFloat and ResolvedInt carry the value AND where it came from.
//
// The provenance is not decoration. The complaint this seam answers is that a
// mismatched threshold is invisible, so every check renders which of the two
// sources it judged against — "threshold 5% (default)" versus "threshold 25%
// (configured)". Without that, an operator who set a key and typo'd the section
// sees a check behaving exactly as it did before and no way to tell why.
type ResolvedFloat struct {
	Value      float64
	Configured bool
}

// ResolvedInt is ResolvedFloat's integer sibling.
type ResolvedInt struct {
	Value      int
	Configured bool
}

// Source renders the provenance for an operator-facing message.
func (r ResolvedFloat) Source() string { return resolvedSource(r.Configured) }

// Source renders the provenance for an operator-facing message.
func (r ResolvedInt) Source() string { return resolvedSource(r.Configured) }

func resolvedSource(configured bool) string {
	if configured {
		return "configured"
	}
	return "default"
}

// ResolvedDoctorThresholds is the form every check reads.
type ResolvedDoctorThresholds struct {
	UnclassifiedShare           ResolvedFloat
	UnclassifiedShareWindowDays ResolvedInt
	ModelFailureRate            ResolvedFloat
	ModelMinSamples             ResolvedInt
	CostAttributionFraction     ResolvedFloat
	CostAttributionMinTotal     ResolvedInt
	FallbackRungMinAttempts     ResolvedInt
}

// Resolve folds the operator's values over the compiled defaults.
func (d DoctorThresholds) Resolve() ResolvedDoctorThresholds {
	return ResolvedDoctorThresholds{
		UnclassifiedShare:           resolveFloat(d.UnclassifiedShare, DefaultUnclassifiedShare),
		UnclassifiedShareWindowDays: resolveInt(d.UnclassifiedShareWindowDays, DefaultUnclassifiedShareWindowDays),
		ModelFailureRate:            resolveFloat(d.ModelFailureRate, DefaultModelFailureRate),
		ModelMinSamples:             resolveInt(d.ModelMinSamples, DefaultModelMinSamples),
		CostAttributionFraction:     resolveFloat(d.CostAttributionFraction, DefaultCostAttributionFraction),
		CostAttributionMinTotal:     resolveInt(d.CostAttributionMinTotal, DefaultCostAttributionMinTotal),
		FallbackRungMinAttempts:     resolveInt(d.FallbackRungMinAttempts, DefaultFallbackRungMinAttempts),
	}
}

func resolveFloat(v *float64, def float64) ResolvedFloat {
	if v == nil {
		return ResolvedFloat{Value: def}
	}
	return ResolvedFloat{Value: *v, Configured: true}
}

func resolveInt(v *int, def int) ResolvedInt {
	if v == nil {
		return ResolvedInt{Value: def}
	}
	return ResolvedInt{Value: *v, Configured: true}
}

// Validate refuses a bound that makes its check unable to fire.
//
// A fraction above 1 can never be exceeded and a negative one is always
// exceeded; either produces a check that is permanently silent or permanently
// warning, which is precisely the invisible misconfiguration this seam exists
// to remove. Failing the boot is the only way it stays visible — the
// alternative is a daemon that runs with a control that does nothing.
func (d DoctorThresholds) Validate() error {
	for _, f := range []struct {
		key string
		val *float64
	}{
		{"unclassified_share", d.UnclassifiedShare},
		{"model_failure_rate", d.ModelFailureRate},
		{"cost_attribution_fraction", d.CostAttributionFraction},
	} {
		if f.val == nil {
			continue
		}
		if *f.val < 0 || *f.val > 1 {
			return fmt.Errorf("doctor.thresholds.%s must be a fraction between 0 and 1, got %v: "+
				"a value outside that range makes the check permanently silent or permanently warning, "+
				"which is indistinguishable from the check not existing", f.key, *f.val)
		}
	}
	for _, f := range []struct {
		key string
		val *int
		min int
	}{
		{"model_min_samples", d.ModelMinSamples, 0},
		{"cost_attribution_min_total", d.CostAttributionMinTotal, 0},
		{"fallback_rung_min_attempts", d.FallbackRungMinAttempts, 0},
		// A window of zero days divides over nothing, so the check can never
		// have evidence. One day is the smallest span that can.
		{"unclassified_share_window_days", d.UnclassifiedShareWindowDays, 1},
	} {
		if f.val == nil {
			continue
		}
		if *f.val < f.min {
			return fmt.Errorf("doctor.thresholds.%s must be >= %d, got %d", f.key, f.min, *f.val)
		}
	}
	return nil
}
