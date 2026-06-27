package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// vrd is a tiny constructor for one verdict row — keeps tests
// terse without hiding the fields that matter (role, model,
// verdict, confidence, cost, recorded_at).
func vrd(role, model, verdict string, conf, cost float64, at time.Time) *persistence.TaskJudgeVerdict {
	return &persistence.TaskJudgeVerdict{
		Role:       role,
		Model:      model,
		Verdict:    verdict,
		Confidence: conf,
		CostUSD:    cost,
		RecordedAt: at,
	}
}

// TestAggregateHallucinationVerdicts_GroupsByRoleModel — same
// (role, model) verdicts collapse into one row with summed
// counts, summed cost, and a derived pass-rate. The headline
// shape the rollup tile renders.
func TestAggregateHallucinationVerdicts_GroupsByRoleModel(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	since := now.Add(-7 * 24 * time.Hour)
	in := []*persistence.TaskJudgeVerdict{
		vrd("judge", "gemma", persistence.JudgeVerdictPass, 0.8, 0.001, now.Add(-1*time.Hour)),
		vrd("judge", "gemma", persistence.JudgeVerdictFail, 0.6, 0.001, now.Add(-2*time.Hour)),
		vrd("judge", "gemma", persistence.JudgeVerdictAbstain, 0.4, 0.0005, now.Add(-3*time.Hour)),
		vrd("judge", "gemma", persistence.JudgeVerdictPass, 0.9, 0.0012, now.Add(-30*time.Minute)),
		vrd("judge", "gpt-oss", persistence.JudgeVerdictPass, 0.95, 0.005, now.Add(-2*time.Hour)),
	}
	out := aggregateHallucinationVerdicts(in, since)
	require.Len(t, out, 2)

	// gemma row first (4 verdicts > 1 verdict).
	gemma := out[0]
	assert.Equal(t, "gemma", gemma.Model)
	assert.Equal(t, 4, gemma.Total)
	assert.Equal(t, 2, gemma.Pass)
	assert.Equal(t, 1, gemma.Fail)
	assert.Equal(t, 1, gemma.Abstain)
	assert.InDelta(t, 50.0, gemma.PassRatePct, 0.01)
	// (0.8+0.6+0.4+0.9) / 4 = 0.675
	assert.InDelta(t, 0.675, gemma.MeanConfidence, 0.001)
	// 0.001+0.001+0.0005+0.0012 = 0.0037
	assert.InDelta(t, 0.0037, gemma.TotalCostUSD, 0.0001)
	assert.InDelta(t, 0.0037/4, gemma.CostPerTaskUSD, 0.0001)
	// LastRecordedAt is the most recent verdict in the cohort.
	assert.Equal(t, now.Add(-30*time.Minute), gemma.LastRecordedAt)

	gpt := out[1]
	assert.Equal(t, "gpt-oss", gpt.Model)
	assert.Equal(t, 1, gpt.Total)
	assert.Equal(t, 1, gpt.Pass)
	assert.InDelta(t, 100.0, gpt.PassRatePct, 0.01)
}

// TestAggregateHallucinationVerdicts_FiltersBySince — verdicts
// older than the window must be excluded so the headline numbers
// match the operator's selected ?window=24h/7d/30d.
func TestAggregateHallucinationVerdicts_FiltersBySince(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	since := now.Add(-24 * time.Hour)
	in := []*persistence.TaskJudgeVerdict{
		vrd("judge", "gemma", persistence.JudgeVerdictPass, 0.8, 0.001, now.Add(-2*time.Hour)),
		vrd("judge", "gemma", persistence.JudgeVerdictFail, 0.6, 0.001, now.Add(-100*time.Hour)),
	}
	out := aggregateHallucinationVerdicts(in, since)
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].Total, "only the in-window verdict counts")
}

// TestAggregateHallucinationVerdicts_SortsByVolumeThenPassRate —
// rollup tile lists biggest-volume cohorts first; ties break on
// the worst pass rate so the operator's eye lands on the
// problem cohort.
func TestAggregateHallucinationVerdicts_SortsByVolumeThenPassRate(t *testing.T) {
	now := time.Now().UTC()
	in := []*persistence.TaskJudgeVerdict{
		// Cohort A: 3 verdicts, all pass (100%).
		vrd("judge", "model-A", persistence.JudgeVerdictPass, 0.9, 0.001, now),
		vrd("judge", "model-A", persistence.JudgeVerdictPass, 0.9, 0.001, now),
		vrd("judge", "model-A", persistence.JudgeVerdictPass, 0.9, 0.001, now),
		// Cohort B: 3 verdicts, 1 pass (33%).
		vrd("judge", "model-B", persistence.JudgeVerdictPass, 0.5, 0.001, now),
		vrd("judge", "model-B", persistence.JudgeVerdictFail, 0.5, 0.001, now),
		vrd("judge", "model-B", persistence.JudgeVerdictFail, 0.5, 0.001, now),
		// Cohort C: 1 verdict (lowest volume → comes last).
		vrd("judge", "model-C", persistence.JudgeVerdictPass, 0.5, 0.001, now),
	}
	out := aggregateHallucinationVerdicts(in, now.Add(-time.Hour))
	require.Len(t, out, 3)
	// model-B and model-A both have 3 verdicts; B's worse pass
	// rate sorts it ahead. C with 1 verdict comes last.
	assert.Equal(t, "model-B", out[0].Model, "lowest pass rate at equal volume sorts first")
	assert.Equal(t, "model-A", out[1].Model)
	assert.Equal(t, "model-C", out[2].Model)
}

// TestAggregateHallucinationVerdicts_EmptyInput — defensive: a
// project with no verdicts produces nil rather than an
// empty-but-non-nil slice that the template would render as
// an empty table.
func TestAggregateHallucinationVerdicts_EmptyInput(t *testing.T) {
	out := aggregateHallucinationVerdicts(nil, time.Now())
	assert.Nil(t, out)
}

// TestAggregateHallucinationVerdicts_CapsAt12Rows — beyond
// 12 cohorts the panel becomes a wall of badges. Real
// deployments rarely cross this; the cap is defensive.
func TestAggregateHallucinationVerdicts_CapsAt12Rows(t *testing.T) {
	now := time.Now().UTC()
	var in []*persistence.TaskJudgeVerdict
	// 20 distinct (role, model) pairs, one verdict each.
	for i := 0; i < 20; i++ {
		role := "r" + string(rune('A'+i))
		in = append(in, vrd(role, "m", persistence.JudgeVerdictPass, 0.5, 0.001, now))
	}
	out := aggregateHallucinationVerdicts(in, now.Add(-time.Hour))
	assert.Len(t, out, 12)
}

// TestAggregateHallucinationVerdicts_HandlesNilEntries —
// defensive: a corrupt nil entry in the verdict slice (slice
// resize race, log replay error) must not panic and must not
// pollute the aggregation.
func TestAggregateHallucinationVerdicts_HandlesNilEntries(t *testing.T) {
	now := time.Now().UTC()
	in := []*persistence.TaskJudgeVerdict{
		nil,
		vrd("judge", "m", persistence.JudgeVerdictPass, 0.8, 0.001, now),
		nil,
	}
	out := aggregateHallucinationVerdicts(in, now.Add(-time.Hour))
	require.Len(t, out, 1)
	assert.Equal(t, 1, out[0].Total)
}
