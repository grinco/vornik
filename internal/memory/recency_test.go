package memory

import (
	"math"
	"testing"
	"time"
)

// §7 test 8 — the curve's unit-level contract. The incident itself is a
// repository-level regression (tests 1-7), but the four properties below are
// the ones a future edit to the curve is most likely to break silently:
// the half-life reading, the floor, the no-decay sentinel, and the clamp.

// The curve (§5.2), against a chunk's OWN retention window.
//
// §5.1 originally carried a hand-tuned half-life per content class. That was
// corrected during implementation: TTL is overridable per swarm-role and per
// deposit, and the live store shows the overrides are pervasive — `decision` is
// declared never-expiring and holds five distinct finite TTLs, `diagnostic` is
// declared 7 days and ranges 7 to 3650 across twelve values. A per-class
// constant would have been wrong for nearly every class. The half-life is the
// chunk's own `expires_at - created_at`, which is exact at every override level
// and needs no table.

func chunkAt(ageDays, ttlDays int) (created time.Time, expires *time.Time, now time.Time) {
	now = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	created = now.Add(-time.Duration(ageDays) * 24 * time.Hour)
	if ttlDays > 0 {
		e := created.Add(time.Duration(ttlDays) * 24 * time.Hour)
		expires = &e
	}
	return
}

func TestRecencyFreshness_AtOneHalfLifeIsExactlyHalf(t *testing.T) {
	cfg := DefaultRecencyConfig()
	// Age equal to the retention window: one half-life by construction.
	for _, ttl := range []int{7, 30, 90, 365} {
		created, expires, now := chunkAt(ttl, ttl)
		if got := recencyFreshness(created, expires, now, cfg); math.Abs(got-0.5) > 1e-12 {
			t.Errorf("ttl=%dd at full age: freshness = %v, want 0.5", ttl, got)
		}
	}
}

// Shape: an exponential at half a half-life gives 1/sqrt(2) ~ 0.707, where a
// linear ramp would give 0.75. This is what distinguishes the curve actually
// implemented from the rejected linear one.
func TestRecencyFreshness_IsExponentialNotLinear(t *testing.T) {
	cfg := DefaultRecencyConfig()
	created, expires, now := chunkAt(45, 90)
	got := recencyFreshness(created, expires, now, cfg)
	if math.Abs(got-math.Sqrt2/2) > 1e-9 {
		t.Errorf("freshness at half the window = %v, want %v (linear would give 0.75)", got, math.Sqrt2/2)
	}
}

// A chunk with no TTL never decays — §4's rule that ranking must not bury the
// design record, now a CONSEQUENCE of those chunks having no expiry rather than
// a separate table entry that could drift out of agreement with it.
func TestRecencyFreshness_NoTTLNeverDecays(t *testing.T) {
	cfg := DefaultRecencyConfig()
	for _, ageDays := range []int{0, 90, 365, 3650} {
		created, expires, now := chunkAt(ageDays, 0) // expires == nil
		if got := recencyFreshness(created, expires, now, cfg); got != 1.0 {
			t.Errorf("age=%dd with no TTL: freshness = %v, want 1.0", ageDays, got)
		}
	}
}

// THE PROPERTY THAT MAKES THE FLOOR A GUARD RATHER THAN A CLIFF: because the
// half-life IS the retention window, the floor is reached at 1.32x the window —
// past the point where the chunk is hard-excluded from search. No chunk
// saturates while it is still retrievable, at any TTL.
func TestRecencyFreshness_FloorIsNeverReachedBeforeExpiry(t *testing.T) {
	cfg := DefaultRecencyConfig()
	floor := cfg.effectiveMinFreshness()
	for _, ttl := range []int{7, 14, 30, 90, 365, 3650} {
		created, expires, now := chunkAt(ttl, ttl) // the last instant before exclusion
		got := recencyFreshness(created, expires, now, cfg)
		if got <= floor {
			t.Errorf("ttl=%dd: freshness at expiry = %v, already at/below the floor %v — the curve "+
				"saturates while the chunk is still searchable", ttl, got, floor)
		}
	}
	// It does bind eventually, well past expiry.
	created, expires, now := chunkAt(300, 90)
	if got := recencyFreshness(created, expires, now, cfg); got != floor {
		t.Errorf("far past expiry: freshness = %v, want the floor %v", got, floor)
	}
}

// Clock skew or a future-dated row must not score above 1.0.
func TestRecencyFreshness_FutureDatedClampsToOne(t *testing.T) {
	cfg := DefaultRecencyConfig()
	created, expires, now := chunkAt(-5, 90)
	if got := recencyFreshness(created, expires, now, cfg); got != 1.0 {
		t.Errorf("future-dated chunk: freshness = %v, want 1.0", got)
	}
}

// A backdated or already-expired row reaching the ranker must not produce a
// negative half-life. Such a chunk is hard-excluded from search anyway; the
// curve stays total rather than relying on that.
func TestRecencyFreshness_NonPositiveWindowDoesNotDecay(t *testing.T) {
	cfg := DefaultRecencyConfig()
	now := time.Now().UTC()
	created := now.Add(-48 * time.Hour)
	expired := created.Add(-1 * time.Hour) // expires BEFORE it was created
	if got := recencyFreshness(created, &expired, now, cfg); got != 1.0 {
		t.Errorf("non-positive retention window: freshness = %v, want 1.0", got)
	}
}

func TestDominanceThreshold_MatchesDesignTable(t *testing.T) {
	cases := []struct {
		perArmCap int
		want      float64
	}{
		{5, 0.469},
		{10, 0.436},
		{15, 0.407},
		{20, 0.381},
		{50, 0.277},
		{100, 0.191},
	}
	for _, tc := range cases {
		got := dominanceThreshold(rrfConstantK, tc.perArmCap)
		if math.Abs(got-tc.want) > 0.001 {
			t.Errorf("dominanceThreshold(60, %d) = %.4f, want %.3f (design §5.3b)", tc.perArmCap, got, tc.want)
		}
	}
	// The direction that matters (§5.3b): a DEEPER pool lowers the
	// threshold. A sign flip here would invert the safety argument.
	if dominanceThreshold(rrfConstantK, 50) >= dominanceThreshold(rrfConstantK, 20) {
		t.Error("deeper per-arm pool must LOWER the dominance threshold")
	}
}

// TestComputedMinFreshness_TracksTheConstants — the floor is derived from the
// live k and pool depth, not copied. §5.3b names "a constant derived from
// another constant, copied rather than referenced" as the failure this design
// keeps hitting.
func TestComputedMinFreshness_TracksTheConstants(t *testing.T) {
	// At the shipped constants this is the design's ~0.40.
	got := computedMinFreshness(rrfConstantK, perArmCandidateCap)
	if math.Abs(got-0.400) > 0.005 {
		t.Errorf("computedMinFreshness(60, 20) = %.4f, want ~0.400", got)
	}
	// 1.05 safety margin: strictly above the threshold it guards.
	if th := dominanceThreshold(rrfConstantK, perArmCandidateCap); got <= th {
		t.Errorf("computed floor %.4f must exceed the dominance threshold %.4f", got, th)
	}
	// The clamp bounds are what make 0.0 safe as the "unset" sentinel
	// (§5.6): no legitimate computed or overridden value can be 0.
	if lo := computedMinFreshness(1, 1_000_000); lo != 0.05 {
		t.Errorf("computedMinFreshness clamped low = %v, want 0.05", lo)
	}
	// The UPPER clamp is defensive only and is documented as such:
	// dominanceThreshold((k+1)/(2(k+D))) approaches 0.5 from below as D
	// falls toward 0, so the largest attainable computed floor is
	// 0.5 × 1.05 = 0.525 and the 0.95 ceiling cannot be reached with any
	// positive (k, D). Pinned so that a future change to the formula which
	// CAN exceed 0.95 shows up here rather than silently relying on a
	// clamp nobody has ever exercised.
	if hi := computedMinFreshness(1_000_000, 1); hi > minFreshnessCeilBound {
		t.Errorf("computedMinFreshness = %v, must not exceed the ceiling %v", hi, minFreshnessCeilBound)
	}
	if hi := computedMinFreshness(1_000_000, 1); math.Abs(hi-0.525) > 1e-6 {
		t.Errorf("computedMinFreshness(1e6, 1) = %v, want the attainable max 0.525", hi)
	}
}

// TestDefaultRecencyConfig_ShippedDefaults — §5.6's table.
func TestDefaultRecencyConfig_ShippedDefaults(t *testing.T) {
	d := DefaultRecencyConfig()
	if !d.Enabled {
		t.Error("Enabled should default true")
	}
	if d.RecencyWeight != 1.0 {
		t.Errorf("RecencyWeight = %v, want 1.0", d.RecencyWeight)
	}
	if d.SeriesSupersededWeight != 0.25 {
		t.Errorf("SeriesSupersededWeight = %v, want 0.25", d.SeriesSupersededWeight)
	}
	if d.OverFetchFactor != 3 {
		t.Errorf("OverFetchFactor = %d, want 3", d.OverFetchFactor)
	}
	if math.Abs(d.MinFreshness-computedMinFreshness(rrfConstantK, perArmCandidateCap)) > 1e-12 {
		t.Errorf("MinFreshness = %v, want the computed floor", d.MinFreshness)
	}
}

// TestRecencyConfig_ApplyDefaults_FillsZeroKnobs mirrors
// RetrievalRoutingConfig's contract: non-zero operator values win, zero knobs
// fall back.
func TestRecencyConfig_ApplyDefaults_FillsZeroKnobs(t *testing.T) {
	got := RecencyConfig{}.applyDefaults()
	if got != DefaultRecencyConfig() {
		t.Errorf("zero config applyDefaults() = %+v, want %+v", got, DefaultRecencyConfig())
	}

	partial := RecencyConfig{RecencyWeight: 0.5, MinFreshness: 0.10, OverFetchFactor: 5}
	eff := partial.applyDefaults()
	if eff.RecencyWeight != 0.5 || eff.MinFreshness != 0.10 || eff.OverFetchFactor != 5 {
		t.Errorf("operator values overwritten: %+v", eff)
	}
	if eff.SeriesSupersededWeight != 0.25 {
		t.Errorf("SeriesSupersededWeight = %v, want the default 0.25", eff.SeriesSupersededWeight)
	}
	if !eff.Enabled {
		t.Error("Enabled should default true when not explicitly wired")
	}
}

// TestRecencyConfig_ApplyDefaults_ExplicitFalseSurvives is the bug the
// Set*-plus-shadow-bool pattern exists to prevent: the kill switch is the one
// knob whose intended value is false, so a plain zero-check would make it
// impossible to turn the feature off from config.
func TestRecencyConfig_ApplyDefaults_ExplicitFalseSurvives(t *testing.T) {
	var c RecencyConfig
	c.SetEnabled(false)
	if eff := c.applyDefaults(); eff.Enabled {
		t.Error("explicitly-set Enabled=false was overwritten by the default true")
	}

	// A bare field assignment is NOT an explicit wire — it looks like
	// "unset" and gets the default. Pinning this stops someone
	// "simplifying" the setter away.
	bare := RecencyConfig{Enabled: false}
	if eff := bare.applyDefaults(); !eff.Enabled {
		t.Error("bare Enabled=false should read as unset and take the default true")
	}

	var on RecencyConfig
	on.SetEnabled(true)
	if eff := on.applyDefaults(); !eff.Enabled {
		t.Error("explicitly-set Enabled=true was lost")
	}
}
