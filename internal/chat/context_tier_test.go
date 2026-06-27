package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTierFromUsage_BandBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		used, limit int
		want        ContextTier
	}{
		{"empty conversation", 0, 200_000, TierPeak},
		{"1% used", 2_000, 200_000, TierPeak},
		{"49.9% used", 99_800, 200_000, TierPeak},
		{"exactly 50% — GOOD starts", 100_000, 200_000, TierGood},
		{"74.9% used", 149_800, 200_000, TierGood},
		{"exactly 75% — DEGRADING starts", 150_000, 200_000, TierDegrading},
		{"89.9% used", 179_800, 200_000, TierDegrading},
		{"exactly 90% — POOR starts", 180_000, 200_000, TierPoor},
		{"99% used", 198_000, 200_000, TierPoor},
		{"overshoot (used > limit)", 250_000, 200_000, TierPoor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, TierFromUsage(c.used, c.limit),
				"used=%d limit=%d", c.used, c.limit)
		})
	}
}

func TestTierFromUsage_NoSignalReturnsPeak(t *testing.T) {
	// No configured budget — we can't degrade on a signal we don't
	// have. PEAK is the safe default (no behavioural changes).
	assert.Equal(t, TierPeak, TierFromUsage(50_000, 0))
	assert.Equal(t, TierPeak, TierFromUsage(50_000, -1))
	assert.Equal(t, TierPeak, TierFromUsage(0, 200_000))
	assert.Equal(t, TierPeak, TierFromUsage(-5, 200_000))
}

func TestContextTier_IsDegraded(t *testing.T) {
	assert.False(t, TierPeak.IsDegraded(), "PEAK is not degraded")
	assert.False(t, TierGood.IsDegraded(), "GOOD is not degraded")
	assert.True(t, TierDegrading.IsDegraded(), "DEGRADING IS degraded")
	assert.True(t, TierPoor.IsDegraded(), "POOR IS degraded")
}

func TestContextTier_String(t *testing.T) {
	assert.Equal(t, "peak", TierPeak.String())
	assert.Equal(t, "good", TierGood.String())
	assert.Equal(t, "degrading", TierDegrading.String())
	assert.Equal(t, "poor", TierPoor.String())
	assert.Equal(t, "unknown", ContextTier(99).String())
}

func TestHeadroomPct_Boundaries(t *testing.T) {
	cases := []struct {
		name        string
		used, limit int
		want        float64
	}{
		{"empty conversation", 0, 200_000, 100},
		{"no signal — limit zero", 50_000, 0, 100},
		{"no signal — negative limit", 50_000, -1, 100},
		{"no signal — negative used", -5, 200_000, 100},
		{"25% used → 75% headroom", 50_000, 200_000, 75},
		{"50% used → 50% headroom", 100_000, 200_000, 50},
		{"99% used → 1% headroom", 198_000, 200_000, 1},
		{"100% used → 0% headroom", 200_000, 200_000, 0},
		{"overshoot clamps to 0", 250_000, 200_000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.InDelta(t, c.want, HeadroomPct(c.used, c.limit), 0.001,
				"used=%d limit=%d", c.used, c.limit)
		})
	}
}
