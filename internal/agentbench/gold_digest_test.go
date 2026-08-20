package agentbench

import (
	"strings"
	"testing"
)

// Regression: 2026-08-19. internal/agentbench/tasksets/dev-swarm-gold-v1.json
// was committed with this in its taskSetSha256 field:
//
//	"Benchmark\n\nUsage:\nvornikctl\n\nAvailable\nmemory\n\nFlags:\n-h,\n…"
//
// scripts/agentbench-reproduce.sh computed the digest with a BARE `vornikctl`
// from $PATH rather than the pinned tree binary. That binary's `bench` had no
// `agent` subcommand, so cobra printed `bench`'s own help to stdout, and the
// `| awk '{print $1}'` that was meant to take the hash off a one-line output
// instead reduced the whole help text to its first word per line. Nothing
// downstream looked at the SHAPE of the value, so it was written to disk and
// committed as ground truth.
//
// The gold fence then compared help text against a real digest and refused —
// correctly — which took the tool-grant probe dark from 2026-08-14 until this
// was found. A refusal at USE time was never the gap; the gap was that a
// manifest this malformed could be produced and persisted at all.

func TestGoldManifest_Validate_rejectsAnUnhashedDigest(t *testing.T) {
	m := GoldManifest{
		TaskSetSHA256: "Benchmark\n\nUsage:\nvornikctl\n\nAvailable\nmemory",
		Runs:          19,
	}

	err := m.Validate()

	if err == nil {
		t.Fatal("a manifest whose digest is captured help text was accepted")
	}
	if !strings.Contains(err.Error(), "taskSetSha256") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestGoldManifest_Validate_acceptsARealDigest(t *testing.T) {
	m := GoldManifest{
		TaskSetSHA256: "9b6fffe10fe0fdb6ead82e94bea62a48a9511a38ef2ef7cefe24a97797c98df9",
		Runs:          3,
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a real digest was rejected: %v", err)
	}
}

func TestGoldManifest_Validate_rejectsTheNearMisses(t *testing.T) {
	wantDigest := "9b6fffe10fe0fdb6ead82e94bea62a48a9511a38ef2ef7cefe24a97797c98df9"
	cases := map[string]string{
		"empty":            "",
		"too short":        wantDigest[:63],
		"too long":         wantDigest + "a",
		"uppercase":        strings.ToUpper(wantDigest),
		"non-hex":          strings.Repeat("z", 64),
		"leading space":    " " + wantDigest[1:],
		"embedded newline": wantDigest[:32] + "\n" + wantDigest[33:],
	}
	for name, digest := range cases {
		t.Run(name, func(t *testing.T) {
			m := GoldManifest{TaskSetSHA256: digest, Runs: 1}
			if err := m.Validate(); err == nil {
				t.Errorf("%s digest %q was accepted", name, digest)
			}
		})
	}
}

// BuildGold is the producer. Refusing at construction is what stops a bad
// digest reaching a file — the caller supplies the task-set hash and, before
// this, was trusted with it.
func TestBuildGold_refusesAnUnhashedTaskSetDigest(t *testing.T) {
	runs := []UnrestrictedRun{
		{TaskID: "t1", Passed: true, Invoked: []string{"file_read"}},
	}

	_, err := BuildGold("Benchmark\n\nUsage:\nvornikctl", runs, 1)

	if err == nil {
		t.Fatal("BuildGold accepted captured help text as the task-set digest")
	}
	if !strings.Contains(err.Error(), "taskSetSha256") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestBuildGold_acceptsARealTaskSetDigest(t *testing.T) {
	runs := []UnrestrictedRun{
		{TaskID: "t1", Passed: true, Invoked: []string{"file_read"}},
	}
	wantDigest := "9b6fffe10fe0fdb6ead82e94bea62a48a9511a38ef2ef7cefe24a97797c98df9"

	m, err := BuildGold(wantDigest, runs, 1)

	if err != nil {
		t.Fatalf("BuildGold rejected a real digest: %v", err)
	}
	if m.TaskSetSHA256 != wantDigest {
		t.Errorf("digest not carried through: %q", m.TaskSetSHA256)
	}
}

// MergeGold combines per-batch manifests. A batch carrying a malformed digest
// must not be merged into a set that is otherwise sound.
func TestMergeGold_refusesAManifestWithAnUnhashedDigest(t *testing.T) {
	wantDigest := "9b6fffe10fe0fdb6ead82e94bea62a48a9511a38ef2ef7cefe24a97797c98df9"
	good := GoldManifest{TaskSetSHA256: wantDigest, Runs: 1,
		Entries: []Gold{{TaskID: "t1", Paths: [][]string{{"file_read"}}}}}
	bad := GoldManifest{TaskSetSHA256: "Usage:\nvornikctl", Runs: 1,
		Entries: []Gold{{TaskID: "t2", Paths: [][]string{{"file_read"}}}}}

	if _, err := MergeGold(good, bad); err == nil {
		t.Fatal("MergeGold accepted a batch whose digest is not a sha256")
	}
}
