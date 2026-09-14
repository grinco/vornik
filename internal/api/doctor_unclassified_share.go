package api

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/config"
)

// The window and the threshold are now TUNABLE — `doctor.thresholds` in
// config.yaml, resolved through DoctorHandlers.thresholds (2026-09-13). The
// values below remain the compiled defaults and live in internal/config so the
// generated config reference documents the same number the check uses; the
// calibration reasoning stays here, where the check is.
//
// The window is long enough that a quiet fleet still has failures to divide
// by, short enough that a classifier degrading now is not averaged away by
// months of healthy history.

// The share above which the check warns; the default is
// config.DefaultUnclassifiedShare.
//
// It was a named constant "not a config key: every threshold in the doctor
// surface is one, and this check is not the place to invent a config seam none
// of its neighbours has". That reasoning was right and its premise is now
// false — the seam exists for all of them, uniformly, so this check reads
// `doctor.thresholds.unclassified_share` like its neighbours read theirs.
//
// NOT zero, which would warn permanently: some residual is expected and
// healthy.
//
// RE-DERIVED 2026-09-02, and the old value was measuring something that no
// longer exists. The 2026-08-26 calibration (5.2% all-time, 9.9% over 30 days,
// 0.4% over 7 → threshold 15%) was taken when MODEL_UNHEALTHY was still
// unclassified — and that ONE condition was 387 of the 452 unclassified rows,
// 85.6% of the population the threshold was fitted to. Classifying it
// (2026-09-02-model-unhealthy-classification-design.md) removed five-sixths of
// the denominator, leaving 15% roughly 23-80x above the real baseline: a check
// that could never fire, silently.
//
// Re-measured on the production database after classification, all three
// windows the design asked for:
//
//	all-time:  65 / 19,681  = 0.33%
//	30 days:    7 /  3,944  = 0.18%
//	 7 days:    7 /  1,055  = 0.66%
//
// The 30d/7d spread is denominator arithmetic, not a rate disagreement: it is
// the SAME 7 rows, all recorded in the last two days, over different spans.
//
// 5% sits ~8x above the highest observed window with headroom for a quiet
// period to swing the ratio on small counts, and ~23x below the old value. The
// check is quiet today and can actually fire when a new unnamed failure class
// appears at volume — which is the only signal worth having here.
//
// NOT removed, though the population is small. Its purpose is to notice the
// NEXT unnamed class, and it is doing that already: the 65 remaining rows are
// dominated by "agent fabrication detected", which is nameable and is filed as
// follow-up work. A check whose residual is small is a check that is working.

// checkUnclassifiedShare publishes the denominator behind the residual failure
// bucket.
//
// Finding D of the 2026-08-26 silent-controls audit: a control with a coverage
// boundary must publish a denominator, because "zero findings" and "zero
// coverage" otherwise render identically. `unclassified` is that shape — the
// bucket was 3,027 rows before migration 170 and a bare count said nothing
// without the 5,791 classified failures it was drawn from. Half of every
// classified failure meaning "we do not know" is a finding about the
// classifier, not a baseline to live with.
func (h *DoctorHandlers) checkUnclassifiedShare(ctx context.Context) DoctorCheck {
	const name = "unclassified_step_failures"
	if h.db == nil {
		// Finding A: an unevaluated check reports SKIPPED, never OK.
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database; cannot measure the unclassified share"}
	}

	// PORTABLE (doctor design, Extension 2026-09-04 E2). This query carried
	// two Postgres-only constructs — `COUNT(*) FILTER` and `now() - interval
	// '%d days'` — and was missed by the first inventory because they are
	// spelled in lower case. On SQLite it reported `near "'30 days'": syntax
	// error` as an ERROR verdict on every run. The window is computed in Go
	// and bound; FILTER becomes SUM(CASE …), which is exact for integer counts.
	thresholds := h.doctorThresholds()
	windowDays := thresholds.UnclassifiedShareWindowDays

	var unclassified, failures int
	err := h.db.QueryRowContext(ctx, `
		SELECT SUM(CASE WHEN error_class = 'unclassified' THEN 1 ELSE 0 END),
		       COUNT(*)
		  FROM execution_step_outcomes
		 WHERE error_class IS NOT NULL AND error_class <> ''
		   AND recorded_at > $1`,
		time.Now().UTC().AddDate(0, 0, -windowDays.Value),
	).Scan(&unclassified, &failures)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	return evaluateUnclassifiedShare(unclassified, failures, thresholds.UnclassifiedShare, windowDays)
}

// evaluateUnclassifiedShare turns the two counts into a verdict. Split from the
// query so the contract is testable without a database.
//
// The threshold and window arrive as RESOLVED values — each carrying whether it
// came from config or from the compiled default — because the verdict message
// says which. The nolint:unparam that used to sit here (threshold "varies only
// in tests, deliberately") is gone: it varies in production now.
func evaluateUnclassifiedShare(unclassified, failures int, threshold config.ResolvedFloat, windowDays config.ResolvedInt) DoctorCheck {
	const name = "unclassified_step_failures"

	// No failed steps in the window is NO EVIDENCE, not a healthy classifier.
	// Reporting OK here would be the exact defect Finding A is about — and
	// this check exists to serve that audit, so it must not reproduce it.
	if failures <= 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "SKIPPED",
			Message: fmt.Sprintf("no failed steps in the last %d days (%s); nothing to measure", windowDays.Value, windowDays.Source()),
		}
	}

	share := float64(unclassified) / float64(failures)
	// The denominator is published on EVERY path, passing included: a green
	// check that hides its coverage is the defect this class is about.
	// The threshold's SOURCE is published beside it. The gap this check's
	// tunability closed was that a mismatched bound is invisible — the check
	// warns permanently or never warns, and neither reads as misconfiguration.
	// "(default)" versus "(configured)" is what tells an operator whether the
	// key they edited is the one being used.
	summary := fmt.Sprintf("%d of %d classified step failures (%.1f%%) are unclassified over %d days (%s); threshold %.0f%% (%s)",
		unclassified, failures, share*100, windowDays.Value, windowDays.Source(), threshold.Value*100, threshold.Source())

	// Strictly above, so a threshold set at the measured steady state does not
	// warn permanently.
	if share > threshold.Value {
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: summary,
			Items: []string{
				"A classifier whose modal output is \"unknown\" is the finding, not the baseline.",
				"Group the bucket by container_exit_code and read error_detail on a sample: a recurring shape means a missing arm in refineAgentFailureOutcome (internal/executor/container.go).",
				"Add the arm and a playbook entry — the vocabulary guard will fail the build until the entry exists.",
			},
		}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: summary}
}
