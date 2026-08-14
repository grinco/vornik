package agentbench

import (
	"strings"
	"testing"
)

func TestBuildGold_RecordsPerRunPathsNotAUnion(t *testing.T) {
	m, err := BuildGold("tset", []UnrestrictedRun{
		{TaskID: "t1", Passed: true, Invoked: []string{"a", "b"}},
		{TaskID: "t1", Passed: true, Invoked: []string{"c", "d"}},
	}, 2)
	if err != nil {
		t.Fatalf("build gold: %v", err)
	}

	g, ok := m.Lookup("t1")
	if !ok {
		t.Fatal("t1 missing from gold")
	}
	if len(g.Paths) != 2 {
		t.Fatalf("paths = %v, want two distinct routes preserved — collapsing them to a "+
			"union or an intersection is what the first two drafts got wrong", g.Paths)
	}
}

// No ground truth exists for a task the unrestricted arm never passed, and we
// cannot tell "the policy was too tight" from "the task is infeasible".
func TestBuildGold_ExcludesATaskTheUnrestrictedArmNeverPassed(t *testing.T) {
	m, err := BuildGold("tset", []UnrestrictedRun{
		{TaskID: "t1", Passed: false},
		{TaskID: "t1", Passed: false},
	}, 2)
	if err != nil {
		t.Fatalf("build gold: %v", err)
	}

	g, ok := m.Lookup("t1")
	if !ok {
		t.Fatal("the failed task vanished entirely — an exclusion nobody can see is " +
			"indistinguishable from a task that was never in the set")
	}
	if !g.Excluded {
		t.Error("a task the arm never passed was kept as scorable ground truth")
	}
	if g.ExcludedReason == "" {
		t.Error("exclusion recorded without a reason")
	}
}

// A task needing no tools cannot exercise a grant policy; scoring it would
// report a perfect coverage nobody earned.
func TestBuildGold_ExcludesATaskThatNeededNoTools(t *testing.T) {
	m, err := BuildGold("tset", []UnrestrictedRun{
		{TaskID: "t1", Passed: true, Invoked: nil},
	}, 1)
	if err != nil {
		t.Fatalf("build gold: %v", err)
	}
	if g, _ := m.Lookup("t1"); !g.Excluded {
		t.Error("a task that invoked no tools was kept as grant-policy ground truth")
	}
}

func TestBuildGold_RefusesWithoutATaskSetHash(t *testing.T) {
	if _, err := BuildGold("", []UnrestrictedRun{{TaskID: "t1", Passed: true, Invoked: []string{"a"}}}, 1); err == nil {
		t.Fatal("built gold with no task-set hash — the regeneration fence would have " +
			"nothing to compare against")
	}
}

// An equal gold set must hash alike regardless of the order runs arrived in, or
// the arm key splits on noise and refuses every comparison.
func TestGoldManifest_HashIsOrderIndependent(t *testing.T) {
	a, err := BuildGold("tset", []UnrestrictedRun{
		{TaskID: "t1", Passed: true, Invoked: []string{"b", "a"}},
		{TaskID: "t2", Passed: true, Invoked: []string{"c"}},
	}, 2)
	if err != nil {
		t.Fatalf("build gold: %v", err)
	}
	b, err := BuildGold("tset", []UnrestrictedRun{
		{TaskID: "t2", Passed: true, Invoked: []string{"c"}},
		{TaskID: "t1", Passed: true, Invoked: []string{"a", "b"}},
	}, 2)
	if err != nil {
		t.Fatalf("build gold: %v", err)
	}

	ha, err := a.SHA256()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	hb, err := b.SHA256()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ha != hb {
		t.Error("the same gold set hashed differently depending on run arrival order")
	}
}

// The fence. Pre-registration stops post-hoc comparison shopping; this stops
// PRE-hoc ground-truth corruption, which nothing else in the design addressed.
func TestCheckRegeneration_RefusesAnUnchangedTaskSet(t *testing.T) {
	pinned := &GoldManifest{TaskSetSHA256: "abc123def456789"}

	err := CheckRegeneration(pinned, "abc123def456789")
	if err == nil {
		t.Fatal("regenerated gold against an unchanged task set — that replaces the " +
			"ground truth the gate measures against, silently")
	}
	// The escape hatch must be named, or an operator who genuinely needs to
	// rebuild will look for a flag instead.
	if !strings.Contains(err.Error(), "delete the pinned manifest") {
		t.Errorf("refusal does not name the reviewable escape hatch: %v", err)
	}
}

func TestCheckRegeneration_AllowsAChangedTaskSet(t *testing.T) {
	pinned := &GoldManifest{TaskSetSHA256: "old"}
	if err := CheckRegeneration(pinned, "new"); err != nil {
		t.Fatalf("refused a genuinely needed rebuild: %v", err)
	}
}

func TestCheckRegeneration_AllowsTheFirstBuild(t *testing.T) {
	if err := CheckRegeneration(nil, "tset"); err != nil {
		t.Fatalf("refused the first gold build: %v", err)
	}
}

func TestCheckRegeneration_RefusesWithoutACurrentHash(t *testing.T) {
	if err := CheckRegeneration(&GoldManifest{TaskSetSHA256: "x"}, ""); err == nil {
		t.Fatal("allowed a rebuild when 'has the task set changed' cannot be answered")
	}
}

// A config-only change does not touch the task set, so its hash is unchanged and
// the fence refuses. This is the property §8 requires be checked in code rather
// than assumed of the operator.
func TestCheckRegeneration_ConfigOnlyChangeCannotRegenerateGold(t *testing.T) {
	ids := []string{"t1", "t2"}
	bodies := map[string]string{"t1": "do a thing", "t2": "do another"}
	digest := TaskSetDigest(ids, bodies)

	pinned := &GoldManifest{TaskSetSHA256: digest}
	// The operator changed a swarm's model and re-ran `gold`. The task set is
	// byte-identical, so nothing about the ground truth may move.
	if err := CheckRegeneration(pinned, TaskSetDigest(ids, bodies)); err == nil {
		t.Fatal("a config-only change regenerated gold — the gate would then be " +
			"measuring against ground truth produced under the very config it is testing")
	}
}
