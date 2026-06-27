package ui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
)

// TestComputeCacheSummary_HeadlineAndPerRowRatios — the spend dashboard's
// headline cache surface (Cache Hit Ratio + $ saved tile) and per-row
// cache columns flow from one helper so the math stays consistent across
// the page. The ratio is cache_read / (cache_read + prompt_tokens); 0%
// when no cache reads happened.
func TestComputeCacheSummary_HeadlineAndPerRowRatios(t *testing.T) {
	roleRows := []RoleModelSpendRow{
		{Role: "coder", Model: "claude-sonnet-4-6",
			PromptTokens: 200, CacheCreationTokens: 100, CacheReadTokens: 800},
		{Role: "judge", Model: "claude-haiku-4-5",
			PromptTokens: 500, CacheCreationTokens: 0, CacheReadTokens: 0},
	}

	rolesDecorated, totalCreation, totalRead, hitRatioPct := computeCacheHeadline(roleRows)

	// 100 + 0 = 100 cache_creation; 800 + 0 = 800 cache_read.
	// Hit ratio = 800 / (800 + 200 + 500) = 800 / 1500 = 53.33...%
	assert.Equal(t, int64(100), totalCreation)
	assert.Equal(t, int64(800), totalRead)
	assert.InDelta(t, 53.333, hitRatioPct, 0.01)

	// First row hit ratio = 800 / (800 + 200) = 80%
	assert.InDelta(t, 80.0, rolesDecorated[0].CacheHitRatioPct, 0.001)
	// Second row has no cache reads — must be 0%, not NaN.
	assert.Equal(t, 0.0, rolesDecorated[1].CacheHitRatioPct)
}

// TestDecorateCacheRatioPct_BoundaryCases — same divide-by-zero guard
// that protects the per-source / per-project row decoration. Empty rows
// must produce 0%, not NaN.
func TestDecorateCacheRatioPct_BoundaryCases(t *testing.T) {
	assert.Equal(t, 0.0, decorateCacheRatioPct(0, 0))
	assert.Equal(t, 0.0, decorateCacheRatioPct(-1, -1))
	assert.InDelta(t, 75.0, decorateCacheRatioPct(100, 300), 0.001)
	assert.InDelta(t, 100.0, decorateCacheRatioPct(0, 500), 0.001)
	assert.InDelta(t, 0.0, decorateCacheRatioPct(500, 0), 0.001)
}

// TestComputeCacheSummary_AllZeroNoNaN — guard against divide-by-zero.
// When no cohort has any cache or prompt tokens, the ratio must be 0,
// not NaN — the template would render "NaN%" otherwise.
func TestComputeCacheSummary_AllZeroNoNaN(t *testing.T) {
	rows := []RoleModelSpendRow{{Role: "x", Model: "y"}}
	decorated, totalCreation, totalRead, hitRatio := computeCacheHeadline(rows)
	assert.Equal(t, int64(0), totalCreation)
	assert.Equal(t, int64(0), totalRead)
	assert.Equal(t, 0.0, hitRatio)
	assert.Equal(t, 0.0, decorated[0].CacheHitRatioPct)
}

// TestBuildDailyBuckets_FillsMissingDays — the time-series chart
// must show a continuous timeline even when no LLM ran on some
// days. Without zero-fill, three sparse rows would render as
// three crammed bars with no time context — operator can't tell
// "Tuesday cost $5" from "Tuesday I ran nothing and Wednesday
// cost $5."
func TestBuildDailyBuckets_FillsMissingDays(t *testing.T) {
	since := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	now := since.Add(6 * 24 * time.Hour)
	rows := []persistence.DailySpend{
		{Day: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), CostUSD: 5.0, CallCount: 3},
		{Day: time.Date(2026, 4, 28, 0, 0, 0, 0, time.UTC), CostUSD: 12.0, CallCount: 7},
	}
	buckets, max, maxDay := buildDailyBuckets(rows, since, now, 7)
	require.Len(t, buckets, 7, "must produce one bucket per day in the window, even where data is missing")
	assert.InDelta(t, 12.0, max, 0.001, "max must be the busiest day")
	assert.Equal(t, "2026-04-28", maxDay.Format("2006-01-02"), "maxDay must point at the busiest bucket so the UI's peak-day label can show a date")
	// Day 0 (since) gets the $5 row; day 3 gets the $12 row; the
	// rest are zero-filled.
	assert.InDelta(t, 5.0, buckets[0].CostUSD, 0.001)
	assert.Zero(t, buckets[1].CostUSD)
	assert.Zero(t, buckets[2].CostUSD)
	assert.InDelta(t, 12.0, buckets[3].CostUSD, 0.001)
	assert.Zero(t, buckets[4].CostUSD)
}

// TestBuildDailyBuckets_HeightPctScalesAgainstMax — the bar
// renderer reads HeightPct directly. Verify it lands as a
// percentage of the busiest day, not absolute.
func TestBuildDailyBuckets_HeightPctScalesAgainstMax(t *testing.T) {
	since := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	now := since.Add(24 * time.Hour)
	rows := []persistence.DailySpend{
		{Day: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), CostUSD: 25.0},
		{Day: time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC), CostUSD: 100.0},
	}
	buckets, _, _ := buildDailyBuckets(rows, since, now, 2)
	require.Len(t, buckets, 2)
	assert.InDelta(t, 25.0, buckets[0].HeightPct, 0.001, "smaller day at 25% of the max")
	assert.InDelta(t, 100.0, buckets[1].HeightPct, 0.001, "busiest day pegs at 100%")
}

func TestBuildDailyBuckets_IncludesTodayForRollingWindow(t *testing.T) {
	now := time.Date(2026, 4, 30, 13, 30, 0, 0, time.UTC)
	since := now.Add(-7 * 24 * time.Hour)

	buckets, _, _ := buildDailyBuckets(nil, since, now, 7)

	require.Len(t, buckets, 7)
	assert.Equal(t, "2026-04-24", buckets[0].Day.Format("2006-01-02"))
	assert.Equal(t, "2026-04-30", buckets[6].Day.Format("2006-01-02"))
}

// TestBuildDailyBuckets_AllZeroNoNaN — defensive: when every day
// is zero, the divide-by-max path could produce NaN. Catch it.
func TestBuildDailyBuckets_AllZeroNoNaN(t *testing.T) {
	since := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	buckets, max, maxDay := buildDailyBuckets(nil, since, since.Add(3*24*time.Hour), 3)
	require.Len(t, buckets, 3)
	assert.Zero(t, max)
	assert.True(t, maxDay.IsZero(), "no data → no peak day; UI should suppress the date label")
	for _, b := range buckets {
		assert.Zero(t, b.HeightPct, "all-zero series must produce 0%, not NaN")
	}
}

// TestDisplaySource_FriendlyLabels — operator-facing labels.
// Empty source ("") gets a clear "Unattributed" rather than a
// blank cell; unknown sources pass through verbatim so a future
// source name surfaces without code changes.
func TestDisplaySource_FriendlyLabels(t *testing.T) {
	cases := map[string]string{
		"workflow_step":     "Workflow steps",
		"dispatcher":        "Chat dispatcher",
		"memory_titler":     "Memory topic labels",
		"memory_classifier": "Memory content classification",
		// New (customer-rename request): underscored / abbreviated
		// raw sources get operator-facing names on the spend page.
		"_authoring":    "Config assistant",
		"external_api":  "3rd-party requests",
		"":              "Unattributed",
		"future_source": "future_source",
	}
	for in, want := range cases {
		assert.Equal(t, want, displaySource(in), "displaySource(%q)", in)
	}
}

// TestDisplayRole_FriendlyLabels — operator-facing role labels.
// All known background-consumer roles must return a "Memory · …" or
// service-name label so the spend tables don't show raw column data.
func TestDisplayRole_FriendlyLabels(t *testing.T) {
	cases := map[string]string{
		"kg_extractor":      "Memory · Entity Extractor",
		"kg_resolver":       "Memory · Entity Resolver",
		"kg_relationship":   "Memory · Relationship Extractor",
		"kg_validator":      "Memory · Faithfulness Validator",
		"memory_titler":     "Memory · Topic Titler",
		"memory_classifier": "Memory · Content Classifier",
		"judge":             "Hallucination Judge",
		"researcher":        "researcher", // workflow role names pass through
	}
	for in, want := range cases {
		assert.Equal(t, want, displayRole(in), "displayRole(%q)", in)
	}
}

// TestHumanizeDuration — sanity test the column formatter for
// the top-tasks Duration column.
func TestHumanizeDuration(t *testing.T) {
	assert.Equal(t, "—", humanizeDuration(0))
	assert.Equal(t, "—", humanizeDuration(-1*time.Second))
	assert.Equal(t, "45s", humanizeDuration(45*time.Second))
	assert.Equal(t, "5m 30s", humanizeDuration(5*time.Minute+30*time.Second))
	assert.Equal(t, "2h 15m", humanizeDuration(2*time.Hour+15*time.Minute))
}

// shortenID was removed in 2026-04-30 in favour of the shared
// `shortID` template helper used by /ui/tasks, /ui/audit, and the
// landing page's active-tasks tile. This test is intentionally
// left as a placeholder so a future re-add of a per-page shortening
// algorithm has to take a test slot, prompting the author to think
// about consistency before duplicating logic.
