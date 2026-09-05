package api

import (
	"context"
	"fmt"
	"time"
)

// unclassifiedShareWindowDays bounds the check to recent evidence. Long enough
// that a quiet fleet still has failures to divide by, short enough that a
// classifier degrading now is not averaged away by months of healthy history.
const unclassifiedShareWindowDays = 30

// unclassifiedShareThreshold is the share above which the check warns.
//
// A named constant, not a config key: every threshold in the doctor surface is
// one, and this check is not the place to invent a config seam none of its
// neighbours has. Retuning is a one-line change plus a rebuild. The gap —
// doctor thresholds being uniformly unconfigurable — is filed in the backlog
// rather than solved half-way here for one check.
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
const unclassifiedShareThreshold = 0.05

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
	var unclassified, failures int
	err := h.db.QueryRowContext(ctx, `
		SELECT SUM(CASE WHEN error_class = 'unclassified' THEN 1 ELSE 0 END),
		       COUNT(*)
		  FROM execution_step_outcomes
		 WHERE error_class IS NOT NULL AND error_class <> ''
		   AND recorded_at > $1`,
		time.Now().UTC().AddDate(0, 0, -unclassifiedShareWindowDays),
	).Scan(&unclassified, &failures)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("query failed: %v", err)}
	}
	return evaluateUnclassifiedShare(unclassified, failures, unclassifiedShareThreshold)
}

// evaluateUnclassifiedShare turns the two counts into a verdict. Split from the
// query so the contract is testable without a database.
//
// threshold is a parameter rather than a closed-over constant precisely so the
// tests can exercise the boundary (at, above, below) without rebuilding — the
// production caller passes unclassifiedShareThreshold and nothing else does.
//
//nolint:unparam // threshold varies only in tests, deliberately; see above.
func evaluateUnclassifiedShare(unclassified, failures int, threshold float64) DoctorCheck {
	const name = "unclassified_step_failures"

	// No failed steps in the window is NO EVIDENCE, not a healthy classifier.
	// Reporting OK here would be the exact defect Finding A is about — and
	// this check exists to serve that audit, so it must not reproduce it.
	if failures <= 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "SKIPPED",
			Message: fmt.Sprintf("no failed steps in the last %d days; nothing to measure", unclassifiedShareWindowDays),
		}
	}

	share := float64(unclassified) / float64(failures)
	// The denominator is published on EVERY path, passing included: a green
	// check that hides its coverage is the defect this class is about.
	summary := fmt.Sprintf("%d of %d classified step failures (%.1f%%) are unclassified over %d days; threshold %.0f%%",
		unclassified, failures, share*100, unclassifiedShareWindowDays, threshold*100)

	// Strictly above, so a threshold set at the measured steady state does not
	// warn permanently.
	if share > threshold {
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
