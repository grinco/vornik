package agentbench

import (
	"bytes"
	"strings"
	"testing"
)

func validPreReg() PreRegistration {
	return PreRegistration{
		Arms:          []string{"baseline", "suppressed"},
		Metric:        "path coverage",
		TargetDelta:   0.05,
		SigmaD:        0.02,
		SigmaN:        12,
		ComputedPairs: 13,
		Rationale:     "does suppressing canonical-context cost grant quality",
	}
}

func TestPreRegistration_RefusesWhatCommitsToNothing(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PreRegistration)
		want string
	}{
		{"one arm", func(p *PreRegistration) { p.Arms = []string{"only"} }, "two arms"},
		{"an arm twice", func(p *PreRegistration) { p.Arms = []string{"a", "a"} }, "twice"},
		{"an empty arm", func(p *PreRegistration) { p.Arms = []string{"a", ""} }, "empty arm"},
		{"no metric", func(p *PreRegistration) { p.Metric = "" }, "no metric"},
		{"no target delta", func(p *PreRegistration) { p.TargetDelta = 0 }, "no target delta"},
		{"no rationale", func(p *PreRegistration) { p.Rationale = "  " }, "no rationale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := validPreReg()
			c.mut(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("accepted a pre-registration with %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error does not explain the refusal (%q): %v", c.want, err)
			}
		})
	}

	if err := validPreReg().Validate(); err != nil {
		t.Fatalf("rejected a valid pre-registration: %v", err)
	}
}

func TestPreRegistration_HashIsStable(t *testing.T) {
	a, err := validPreReg().Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := validPreReg().Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a != b {
		t.Error("the same pre-registration hashed differently — a report cannot print " +
			"it beside a figure if it moves")
	}
}

func TestJournal_RoundTrips(t *testing.T) {
	j := Journal{
		Manifest: RunManifest{RunID: "r1", ArmKey: "k", PreRegistrationHash: "h"},
		Records: []ExecutionRecord{
			{TaskID: "t1", Succeeded: true, CostUSD: 0.5,
				Verdicts: []Verdict{{Probe: "tool-grant", PathCoverage: 1.0}}},
		},
	}

	var buf bytes.Buffer
	if err := j.Write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadJournal(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Records) != 1 || got.Records[0].TaskID != "t1" {
		t.Fatalf("records did not round-trip: %+v", got.Records)
	}
	// The verdict is what survives the 30-day retention window, so it must
	// survive serialisation.
	if got.Records[0].Verdicts[0].PathCoverage != 1.0 {
		t.Error("the probe verdict did not round-trip — the journal would decay into a " +
			"pointer at nothing once tool_audit_log is pruned")
	}
}

func TestJournal_RollupReadsTheJournalNotTheLedger(t *testing.T) {
	j := Journal{
		Manifest: RunManifest{Arm: ArmFields{Name: "baseline"}},
		Records: []ExecutionRecord{
			{TaskID: "t1", Succeeded: true, CostUSD: 1.0},
			{TaskID: "t2", Succeeded: false, ErrorText: "criteria not met", CostUSD: 1.0},
		},
	}
	r := j.Rollup()
	if r.Arm != "baseline" || r.Attempted != 2 || r.CostPerSuccessUSD != 2.0 {
		t.Errorf("rollup = %+v; want the arm's name, 2 attempts, $2.00 per success", r)
	}
}

func TestJournal_CheckReadableRefusesEachUnsoundShape(t *testing.T) {
	sound := func() Journal {
		return Journal{Manifest: RunManifest{
			PreRegistrationHash: "h",
			Power: PowerCheck{
				SigmaD: 0.02, SigmaN: 12, TargetDelta: 0.05,
				RequiredPairs: 2, AvailablePairs: 20, Adequate: true,
			},
		}}
	}

	if err := sound().CheckReadable(); err != nil {
		t.Fatalf("a sound run was refused: %v", err)
	}

	t.Run("untrustworthy", func(t *testing.T) {
		j := sound()
		j.Manifest.Untrustworthy = true
		j.Manifest.UntrustworthyReason = "daemon restarted mid-run"
		err := j.CheckReadable()
		if err == nil || !strings.Contains(err.Error(), "daemon restarted") {
			t.Errorf("want the reason surfaced, got: %v", err)
		}
	})

	t.Run("no pre-registration", func(t *testing.T) {
		j := sound()
		j.Manifest.PreRegistrationHash = ""
		err := j.CheckReadable()
		if err == nil || !strings.Contains(err.Error(), "exploratory") {
			t.Errorf("an unregistered run must be exploratory-only, got: %v", err)
		}
	})

	t.Run("partial arm key", func(t *testing.T) {
		j := sound()
		j.Manifest.ArmPartial = true
		err := j.CheckReadable()
		if err == nil || !strings.Contains(err.Error(), "PARTIAL") {
			t.Errorf("want a partial-key refusal, got: %v", err)
		}
	})

	t.Run("underpowered", func(t *testing.T) {
		j := sound()
		j.Manifest.Power = PowerCheck{
			SigmaD: 0.05, SigmaN: 12, TargetDelta: 0.01,
			RequiredPairs: 197, AvailablePairs: 20, ResolvableDelta: 0.031,
		}
		err := j.CheckReadable()
		if err == nil || !strings.Contains(err.Error(), "INCONCLUSIVE") {
			t.Errorf("want an underpowered refusal steering to inconclusive, got: %v", err)
		}
	})
}

func TestCompareJournals_RefusesIncomparableArms(t *testing.T) {
	a := Journal{Manifest: RunManifest{Arm: baseArm()}}
	bArm := baseArm()
	bArm.BinarySHA256 = "different"
	b := Journal{Manifest: RunManifest{Arm: bArm}}

	if _, err := CompareJournals(a, b, 0.1); err == nil {
		t.Fatal("diffed two runs against different binaries")
	}
}

// A difference below what the runs can resolve is INCONCLUSIVE — and still
// carries its number, because suppressing a real measurement is its own
// dishonesty. What inconclusive forbids is the claim, not the figure.
func TestCompareJournals_BelowTheFloorIsInconclusiveAndStillPrintsTheNumber(t *testing.T) {
	mk := func(floor float64) Journal {
		return Journal{Manifest: RunManifest{
			Arm:   baseArm(),
			Power: PowerCheck{ResolvableDelta: floor},
		}}
	}

	got, err := CompareJournals(mk(0.10), mk(0.10), 0.08)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !strings.Contains(got, "INCONCLUSIVE") {
		t.Errorf("a sub-floor delta was presented as a result: %q", got)
	}
	if !strings.Contains(got, "0.0800") {
		t.Errorf("the measured delta was suppressed: %q", got)
	}
}

// A comparison is only as resolvable as its least-powered side.
func TestCompareJournals_TheWeakerRunGovernsTheFloor(t *testing.T) {
	strong := Journal{Manifest: RunManifest{Arm: baseArm(), Power: PowerCheck{ResolvableDelta: 0.01}}}
	weak := Journal{Manifest: RunManifest{Arm: baseArm(), Power: PowerCheck{ResolvableDelta: 0.20}}}

	got, err := CompareJournals(strong, weak, 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !strings.Contains(got, "INCONCLUSIVE") {
		t.Errorf("the strong run's floor was used; a comparison is only as resolvable "+
			"as its weaker side: %q", got)
	}
}

func TestCompareJournals_AboveTheFloorReportsAResult(t *testing.T) {
	mk := func(floor float64) Journal {
		return Journal{Manifest: RunManifest{Arm: baseArm(), Power: PowerCheck{ResolvableDelta: floor}}}
	}
	got, err := CompareJournals(mk(0.01), mk(0.01), 0.05)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if strings.Contains(got, "INCONCLUSIVE") {
		t.Errorf("a resolvable delta was called inconclusive: %q", got)
	}
}

func jrnl(runID string, mutate func(*Journal)) Journal {
	j := Journal{
		Manifest: RunManifest{
			RunID:               runID,
			Arm:                 baseArm(),
			ArmKey:              baseArm().Key(),
			PreRegistrationHash: "prereghash",
		},
		Records: []ExecutionRecord{{TaskID: "t-" + runID}},
	}
	if mutate != nil {
		mutate(&j)
	}
	return j
}

// A batched scoring pass is only usable if the batches can be recombined.
func TestMergeJournals_ConcatenatesRecordsOfTheSameArm(t *testing.T) {
	m, err := MergeJournals(jrnl("a", nil), jrnl("b", nil), jrnl("c", nil))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(m.Records) != 3 {
		t.Errorf("got %d records, want 3", len(m.Records))
	}
	if m.Manifest.RunID != "a+2" {
		t.Errorf("runID = %q; a merged journal must not pass as one of its parts", m.Manifest.RunID)
	}
}

// Averaging two arms produces a number describing neither system.
func TestMergeJournals_RefusesDifferentArms(t *testing.T) {
	other := jrnl("b", func(j *Journal) {
		j.Manifest.Arm.ContextPolicy = "suppression=canonical-context;advert=gated"
	})
	_, err := MergeJournals(jrnl("a", nil), other)
	if err == nil {
		t.Fatal("merged two different arms")
	}
	if !strings.Contains(err.Error(), "context_policy") {
		t.Errorf("error does not name the differing axis: %v", err)
	}
}

// "Unverified" twice over is not evidence that two runs matched.
func TestMergeJournals_RefusesPartialKeys(t *testing.T) {
	partial := jrnl("b", func(j *Journal) { j.Manifest.ArmPartial = true })
	if _, err := MergeJournals(jrnl("a", nil), partial); err == nil {
		t.Fatal("merged a journal with a PARTIAL arm key")
	}
	// ...including when it is the FIRST one, which a loop starting at index 1 misses.
	if _, err := MergeJournals(partial, jrnl("a", nil)); err == nil {
		t.Fatal("a partial FIRST journal was accepted")
	}
}

func TestMergeJournals_RefusesDifferentPreRegistrations(t *testing.T) {
	other := jrnl("b", func(j *Journal) { j.Manifest.PreRegistrationHash = "different" })
	_, err := MergeJournals(jrnl("a", nil), other)
	if err == nil || !strings.Contains(err.Error(), "pre-registration") {
		t.Fatalf("want a refusal naming the pre-registration, got: %v", err)
	}
}

// A degraded batch must not be laundered by the clean ones it merges with.
func TestMergeJournals_UntrustworthinessIsContagious(t *testing.T) {
	bad := jrnl("b", func(j *Journal) {
		j.Manifest.Untrustworthy = true
		j.Manifest.UntrustworthyReason = "ledger gap"
	})
	m, err := MergeJournals(jrnl("a", nil), bad)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !m.Manifest.Untrustworthy {
		t.Fatal("a merged journal containing a degraded batch read as clean")
	}
	if !strings.Contains(m.Manifest.UntrustworthyReason, "ledger gap") {
		t.Errorf("reason lost the original cause: %q", m.Manifest.UntrustworthyReason)
	}
}

func TestMergeJournals_RefusesAnEmptySet(t *testing.T) {
	if _, err := MergeJournals(); err == nil {
		t.Fatal("merging nothing produced a journal")
	}
}
