package agentbench

import (
	"strings"
	"testing"
)

// chunkJournal builds one chunk of a batched run: the tasks named, each at the
// one repeat index the chunk covers. This is the shape agentbench-reproduce.sh
// writes with --repeat-batch 1 --task-batch N.
func chunkJournal(runID string, repeat int, taskIDs ...string) Journal {
	arm := baseArm()
	arm.TaskSetSHA256, arm.ScoringPolicySHA256, arm.TierPolicySHA256 = "tasks", "scores", "tiers"
	tiers := map[string]TaskTier{}
	for _, id := range taskIDs {
		tiers[id] = TaskTierGate
	}
	j := Journal{Manifest: RunManifest{
		RunID: runID, Arm: arm, ArmKey: arm.Key(), TaskTiers: tiers,
		DaemonBuild: "d0d07a6d1183", HarnessBuild: "d0d07a6d1183",
	}}
	for _, id := range taskIDs {
		j.TaskRuns = append(j.TaskRuns, TaskRun{
			TaskID: id, Repeat: repeat, Succeeded: true,
			ExecutionIDs: []string{id + "-exec"},
		})
		j.TaskScores = append(j.TaskScores, TaskScore{TaskID: id, Repeat: repeat})
	}
	return j
}

func TestMergeJournals_UnionsTiersAndRunsAcrossChunks(t *testing.T) {
	chunks := []Journal{
		chunkJournal("run-c1-tb0", 2, "gate-a", "gate-b"),
		chunkJournal("run-c0-tb0", 1, "gate-a", "gate-b"),
		chunkJournal("run-c0-tb1", 1, "gate-c"),
		chunkJournal("run-c1-tb1", 2, "gate-c"),
	}

	got, err := MergeJournals(chunks...)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got.TaskRuns) != 6 {
		t.Fatalf("TaskRuns = %d, want 6", len(got.TaskRuns))
	}
	if len(got.TaskScores) != 6 {
		t.Fatalf("TaskScores = %d, want 6", len(got.TaskScores))
	}
	if len(got.Manifest.TaskTiers) != 3 {
		t.Fatalf("TaskTiers = %v, want 3 entries", got.Manifest.TaskTiers)
	}
	// RunID keeps MergeJournals' existing <first>+<n> form: provenance naming
	// the first chunk and how many joined it, never identity.
	if got.Manifest.RunID != "run-c1-tb0+3" {
		t.Fatalf("RunID = %q, want the first chunk id plus the joined count", got.Manifest.RunID)
	}
	if got.Manifest.ArmKey != chunks[0].Manifest.ArmKey {
		t.Fatalf("merged arm key drifted from its inputs")
	}
}

// TestMergeJournals_RefusesTheChunkingThatLostTheRepeatIndex is the regression
// test for the 2026-09-20/21 hard-tier calibration passes: twenty hours of
// compute, 200 task runs, every one journaled as `repeat: 1` because
// --repeat-batch 1 makes each invocation stamp repeats from 1. Ten runs all
// claiming to be repeat 1 are indistinguishable from one run journaled ten
// times, so the merge must refuse rather than tally them as ten attempts.
func TestMergeJournals_RefusesTheChunkingThatLostTheRepeatIndex(t *testing.T) {
	var chunks []Journal
	for i := range 10 {
		chunks = append(chunks, chunkJournal("run-c"+string(rune('0'+i))+"-tb0", 1, "gate-a"))
	}
	merged, err := MergeJournals(chunks...)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The refusal lives in calibrationTallies, one layer below the merge: the
	// merge concatenates evidence, and the duplicate rule is a property of the
	// tally. Asserted here rather than only in the tally's own test because
	// this is the path the twenty wasted hours actually took.
	_, err = BuildCalibration(merged, releaseTestSHA("a"))
	if err == nil {
		t.Fatal("ten chunks all stamped repeat 1 were tallied as ten attempts")
	}
	if !strings.Contains(err.Error(), "gate-a") {
		t.Fatalf("error must name the task: %v", err)
	}
	// The cause, not just the symptom: a duplicate pair in a batched run means
	// the chunks restamped their repeat index, and the operator needs the flag
	// that prevents it.
	if !strings.Contains(err.Error(), "repeat-offset") {
		t.Fatalf("error must name the fix (--repeat-offset): %v", err)
	}
}

func TestMergeJournals_RefusesInputsThatAreNotOneRun(t *testing.T) {
	ok := func() []Journal {
		return []Journal{
			chunkJournal("run-c0-tb0", 1, "gate-a"),
			chunkJournal("run-c1-tb0", 2, "gate-a"),
		}
	}

	t.Run("empty set", func(t *testing.T) {
		if _, err := MergeJournals(); err == nil ||
			!strings.Contains(err.Error(), "no journals") {
			t.Fatalf("empty input accepted: %v", err)
		}
	})

	t.Run("differing arm key", func(t *testing.T) {
		chunks := ok()
		arm := chunks[1].Manifest.Arm
		arm.TaskSetSHA256 = "other-tasks"
		chunks[1].Manifest.Arm, chunks[1].Manifest.ArmKey = arm, arm.Key()
		if _, err := MergeJournals(chunks...); err == nil ||
			!strings.Contains(err.Error(), "refusing to merge") {
			t.Fatalf("incomparable arms merged: %v", err)
		}
	})

	t.Run("differing pre-registration hash", func(t *testing.T) {
		chunks := ok()
		chunks[1].Manifest.PreRegistrationHash = "different"
		if _, err := MergeJournals(chunks...); err == nil ||
			!strings.Contains(err.Error(), "pre-registration") {
			t.Fatalf("mixed pre-registrations merged: %v", err)
		}
	})

	// No subtest for a differing DaemonBuild: `binary_sha256` is already a
	// keyed arm axis, so two chunks built from different daemons have
	// different arm keys and the check above refuses them. A second guard on
	// the human-readable revision string would be the newer of two
	// implementations of one property, which CLAUDE.md §5 names as the one
	// that is usually wrong.

	// A differing harness build DEGRADES rather than refuses, by existing
	// design: the records are real measurements and discarding them would also
	// discard the evidence of the mismatch. What must not happen is that it
	// passes silently, or that BuildCalibration then reads it as a result.
	t.Run("differing harness build degrades and blocks calibration", func(t *testing.T) {
		chunks := ok()
		chunks[1].Manifest.HarnessBuild = "8610e04c1eb1"
		merged, err := MergeJournals(chunks...)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if !merged.Manifest.Untrustworthy ||
			!strings.Contains(merged.Manifest.UntrustworthyReason, "harness") {
			t.Fatalf("mixed harness builds merged clean: %+v", merged.Manifest)
		}
		if _, err := BuildCalibration(merged, releaseTestSHA("a")); err == nil ||
			!strings.Contains(err.Error(), "untrustworthy") {
			t.Fatalf("calibrated an untrustworthy merge: %v", err)
		}
	})

	// Untrustworthiness is contagious for the same reason.
	t.Run("untrustworthy chunk taints the merge", func(t *testing.T) {
		chunks := ok()
		chunks[1].Manifest.Untrustworthy = true
		chunks[1].Manifest.UntrustworthyReason = "store cleared mid-run"
		merged, err := MergeJournals(chunks...)
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		if !merged.Manifest.Untrustworthy {
			t.Fatal("a degraded batch was laundered by its clean siblings")
		}
		if _, err := BuildCalibration(merged, releaseTestSHA("a")); err == nil {
			t.Fatal("calibrated a merge containing a degraded batch")
		}
	})

	t.Run("partial arm key", func(t *testing.T) {
		chunks := ok()
		chunks[1].Manifest.ArmPartial = true
		if _, err := MergeJournals(chunks...); err == nil ||
			!strings.Contains(err.Error(), "PARTIAL") {
			t.Fatalf("partial arm merged: %v", err)
		}
	})

	// A task at two tiers is a tier-policy change mid-run. The hash comparison
	// above catches it too, but the union re-checks per task so the error names
	// the task rather than only the hash.
	t.Run("conflicting tier for one task", func(t *testing.T) {
		chunks := ok()
		chunks[1].Manifest.TaskTiers["gate-a"] = TaskTierTripwire
		if _, err := MergeJournals(chunks...); err == nil ||
			!strings.Contains(err.Error(), "gate-a") {
			t.Fatalf("conflicting tier merged without naming the task: %v", err)
		}
	})
}

// A merged chunked run must reach a calibration artifact, which is the whole
// point of the merge: BuildCalibration is unchanged and reads the merge as if
// the run had never been chunked.
func TestMergeJournals_FeedsBuildCalibrationUnchanged(t *testing.T) {
	var chunks []Journal
	for repeat := 1; repeat <= MinimumCalibrationAttempts; repeat++ {
		j := chunkJournal("run-c"+string(rune('0'+repeat))+"-tb0", repeat, "gate-a")
		// gate-a must discriminate or §3's admissibility rule refuses it.
		if repeat == 1 {
			j.TaskRuns[0].Succeeded = false
			j.TaskRuns[0].ErrorText = "criteria not met"
		}
		chunks = append(chunks, j)
	}

	merged, err := MergeJournals(chunks...)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	cal, err := BuildCalibration(merged, releaseTestSHA("a"))
	if err != nil {
		t.Fatalf("calibrate a merged chunked run: %v", err)
	}
	if cal.MinimumAttempts != MinimumCalibrationAttempts {
		t.Fatalf("MinimumAttempts = %d, want %d", cal.MinimumAttempts, MinimumCalibrationAttempts)
	}
	if len(cal.Tasks) != 1 || cal.Tasks[0].Attempts != MinimumCalibrationAttempts {
		t.Fatalf("tasks = %+v", cal.Tasks)
	}
}
