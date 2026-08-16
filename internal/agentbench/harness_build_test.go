package agentbench

import (
	"strings"
	"testing"
)

// Regression, 2026-08-16. A long-horizon arm journaled durationMs=0 for all 14
// records while execution_step_outcomes held the durations (57 rows, avg 85.2s,
// no nulls). The read code was correct and present; the vornikctl RUNNING it was
// 27 commits stale and predated the fix.
//
// Nothing in the manifest could show that. The arm key pins the daemon's binary
// sha — which was current — plus the scoring CONTRACT version, which matched.
// Two runs could agree on every keyed axis and still differ in which metrics
// they were capable of recording at all.
func TestHarnessBuild_IsRecorded(t *testing.T) {
	got := HarnessBuild()
	if got == "" {
		t.Fatal("HarnessBuild must never be empty: an absent stamp is " +
			"indistinguishable from a journal written before provenance existed")
	}
}

func TestHarnessBuildTrustworthy(t *testing.T) {
	cases := map[string]bool{
		"872a06b969ae":       true,
		"":                   false,
		UnknownHarnessBuild:  false,
		"872a06b969ae+dirty": false,
	}
	for build, want := range cases {
		if got := HarnessBuildTrustworthy(build); got != want {
			t.Errorf("HarnessBuildTrustworthy(%q) = %v, want %v", build, got, want)
		}
	}
}

// Chunks of one experiment must have been scored by one apparatus. Merging a
// chunk scored by a build that records a metric with one that cannot produces a
// rollup where that metric is "defined" over a subset — the exact
// partial-denominator failure the Defined flags exist to prevent.
func TestMergeJournals_DifferentHarnessBuildsDegradesTheMerge(t *testing.T) {
	mk := func(runID, build string) Journal {
		return Journal{Manifest: RunManifest{
			RunID:               runID,
			HarnessBuild:        build,
			PreRegistrationHash: "preregA",
			Arm:                 comparableArm(),
		}}
	}

	merged, err := MergeJournals(mk("c0", "aaaaaaaaaaaa"), mk("c1", "bbbbbbbbbbbb"))
	if err != nil {
		t.Fatalf("merge must not REFUSE a build difference — the records are real: %v", err)
	}
	if !merged.Manifest.Untrustworthy {
		t.Fatal("a merge across different harness builds must be marked untrustworthy")
	}
	if !strings.Contains(merged.Manifest.UntrustworthyReason, "aaaaaaaaaaaa") ||
		!strings.Contains(merged.Manifest.UntrustworthyReason, "bbbbbbbbbbbb") {
		t.Errorf("the reason must name both builds, got %q", merged.Manifest.UntrustworthyReason)
	}
	if err := merged.CheckReadable(); err == nil {
		t.Error("CheckReadable must surface the degraded merge")
	}
}

func TestMergeJournals_SameHarnessBuildMergesCleanly(t *testing.T) {
	mk := func(runID string) Journal {
		return Journal{Manifest: RunManifest{
			RunID:               runID,
			HarnessBuild:        "aaaaaaaaaaaa",
			PreRegistrationHash: "preregA",
			Arm:                 comparableArm(),
		}}
	}

	merged, err := MergeJournals(mk("c0"), mk("c1"))
	if err != nil {
		t.Fatalf("MergeJournals: %v", err)
	}
	if merged.Manifest.Untrustworthy {
		t.Fatalf("identical builds must merge clean, got %q", merged.Manifest.UntrustworthyReason)
	}
}

// Journals written before provenance was recorded carry no stamp. Treating
// "absent" as a difference would mark every merge involving historical data
// untrustworthy on no evidence at all.
func TestMergeJournals_AbsentBuildStampIsNotADifference(t *testing.T) {
	withBuild := Journal{Manifest: RunManifest{
		RunID: "c0", HarnessBuild: "aaaaaaaaaaaa",
		PreRegistrationHash: "preregA", Arm: comparableArm(),
	}}
	legacy := Journal{Manifest: RunManifest{
		RunID: "c1", PreRegistrationHash: "preregA", Arm: comparableArm(),
	}}

	merged, err := MergeJournals(withBuild, legacy)
	if err != nil {
		t.Fatalf("MergeJournals: %v", err)
	}
	if merged.Manifest.Untrustworthy {
		t.Fatalf("an absent stamp is unknown, not a mismatch; got %q",
			merged.Manifest.UntrustworthyReason)
	}
}

// comparableArm is a fully-populated arm so MergeJournals' partial-key and
// comparability guards pass and the build check is what the test exercises.
func comparableArm() ArmFields {
	return ArmFields{
		HarnessVersion: HarnessVersion,
		Name:           "arm",
		BinarySHA256:   "binsha",
		ConfigSHA256:   "cfgsha",
		Models:         map[string]string{"coder": "m1"},
		ContextPolicy:  "suppression=none",
		TaskSetSHA256:  "tasksha",
	}
}
