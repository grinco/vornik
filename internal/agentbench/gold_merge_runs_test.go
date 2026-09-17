package agentbench

import (
	"strings"
	"testing"
)

// MergeGold summed every batch's Runs unconditionally. That is right for REPEAT
// batching, where each batch re-runs the SAME tasks for a subset of the repeats
// and the targets genuinely add up. It is wrong for TASK batching, where the
// batches are disjoint and each declares the same per-task target.
//
// Measured 2026-09-17 on the 2026.9.4 gold pass: 10 task-batches at --runs 3
// merged to a manifest declaring runs=30 while every one of its 30 entries held
// exactly 3 paths. shortGoldReport then called all 30 entries short and printed
//
//	scripts/agentbench-reproduce.sh --topup --runs 30
//
// against a COMPLETE gold set. Following that advice is 27 further runs on each
// of 30 tasks — 810 task-runs, tens of hours and tens of dollars — to fix
// nothing. The gold data was correct; only the header lied.
//
// The rule that covers both batchings: a task's target is the sum of the Runs
// declared by the manifests that CONTAIN it, and the manifest's target is the
// largest of those.
func TestMergeGold_TaskBatchedRunsIsPerTaskTargetNotTheSum(t *testing.T) {
	batch := func(runs int, ids ...string) GoldManifest {
		m := GoldManifest{TaskSetSHA256: strings.Repeat("a", 64), Runs: runs}
		for _, id := range ids {
			m.Entries = append(m.Entries, Gold{TaskID: id, Paths: [][]string{{"file_read"}, {"file_read"}, {"file_read"}}[:runs]})
		}
		return m
	}

	// TASK batching: disjoint tasks, same per-task target. Merged target is 3.
	got, err := MergeGold(batch(3, "t1", "t2"), batch(3, "t3", "t4"), batch(3, "t5"))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.Runs != 3 {
		t.Errorf("task-batched merge declared runs=%d, want 3 — summing turns a complete "+
			"gold set into one the harness reports as 100%% short", got.Runs)
	}
	if len(got.Entries) != 5 {
		t.Errorf("entries=%d, want 5", len(got.Entries))
	}
	if n := len(got.ShortEntries()); n != 0 {
		t.Errorf("%d entries reported short against a complete set", n)
	}

	// REPEAT batching: the SAME tasks in every batch, one run each. The targets
	// do add up, and this must keep working.
	got, err = MergeGold(batch(1, "t1", "t2"), batch(1, "t1", "t2"), batch(1, "t1", "t2"))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got.Runs != 3 {
		t.Errorf("repeat-batched merge declared runs=%d, want 3 (1+1+1)", got.Runs)
	}
	if n := len(got.ShortEntries()); n != 0 {
		t.Errorf("%d entries short after a complete repeat-batched merge", n)
	}
}
