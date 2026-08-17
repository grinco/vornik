package agentbench

import (
	"math"
	"strings"
	"testing"

	"vornik.io/vornik/internal/quality"
)

func tieredTasks() []TaskSpec {
	return []TaskSpec{
		{ID: "trip", Tier: TaskTierTripwire},
		{ID: "gate-a", Tier: TaskTierGate, Scoring: pinnedPolicy()},
		{ID: "gate-b", Tier: TaskTierGate, Scoring: pinnedPolicy()},
		{ID: "long", Tier: TaskTierExploratory, Scoring: pinnedPolicy()},
	}
}

func releaseTestSHA(ch string) string { return strings.Repeat(ch, 64) }

func TestValidateTaskTiers_RequiresOneKnownTierAndAGradedGateContract(t *testing.T) {
	if err := ValidateTaskTiers(tieredTasks()); err != nil {
		t.Fatalf("valid tiers: %v", err)
	}

	cases := []struct {
		name string
		mut  func([]TaskSpec) []TaskSpec
		want string
	}{
		{"missing", func(in []TaskSpec) []TaskSpec { in[0].Tier = ""; return in }, "no benchmark tier"},
		{"unknown", func(in []TaskSpec) []TaskSpec { in[0].Tier = "blended"; return in }, "unknown benchmark tier"},
		{"gate without score", func(in []TaskSpec) []TaskSpec { in[1].Scoring = nil; return in }, "graded scoring policy"},
		{"duplicate id", func(in []TaskSpec) []TaskSpec { in[1].ID = in[0].ID; return in }, "appears twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tasks := tieredTasks()
			err := ValidateTaskTiers(tc.mut(tasks))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestTierPolicyDigest_IsOrderIndependentAndChangesWhenATaskMovesTier(t *testing.T) {
	a := tieredTasks()
	b := []TaskSpec{a[3], a[1], a[0], a[2]}
	if TierPolicyDigest(a) == "" || TierPolicyDigest(a) != TierPolicyDigest(b) {
		t.Fatalf("tier digest empty or order-dependent: %q vs %q", TierPolicyDigest(a), TierPolicyDigest(b))
	}
	b[0].Tier = TaskTierGate
	if TierPolicyDigest(a) == TierPolicyDigest(b) {
		t.Fatal("moving a task into the gate denominator did not change its policy digest")
	}
}

func calibrationJournal() Journal {
	tiers := map[string]TaskTier{
		"trip": TaskTierTripwire, "gate-a": TaskTierGate,
		"gate-b": TaskTierGate, "long": TaskTierExploratory,
	}
	arm := baseArm()
	arm.TaskSetSHA256, arm.ScoringPolicySHA256, arm.TierPolicySHA256 = "tasks", "scores", "tiers"
	j := Journal{Manifest: RunManifest{RunID: "calibration-1", Arm: arm,
		ArmKey: arm.Key(), TaskTiers: tiers}}
	for _, row := range []struct {
		id     string
		passes []bool
	}{
		{"trip", []bool{true, true, true}},
		{"gate-a", []bool{true, false, true}},
		{"gate-b", []bool{false, true, false}},
		{"long", []bool{false, false, false}},
	} {
		for i, passed := range row.passes {
			errText := ""
			if !passed {
				errText = "criteria not met"
			}
			j.TaskRuns = append(j.TaskRuns, TaskRun{TaskID: row.id, Repeat: i + 1,
				Succeeded: passed, ErrorText: errText, ExecutionIDs: []string{row.id + "-exec"}})
		}
	}
	return j
}

func TestBuildCalibration_AdmitsOnlyDiscriminatingGateTasks(t *testing.T) {
	got, err := BuildCalibration(calibrationJournal(), releaseTestSHA("a"))
	if err != nil {
		t.Fatalf("build calibration: %v", err)
	}
	if got.MinimumAttempts != 3 || len(got.Tasks) != 4 {
		t.Fatalf("calibration = %+v", got)
	}
	byID := map[string]CalibrationTask{}
	for _, task := range got.Tasks {
		byID[task.TaskID] = task
	}
	if byID["gate-a"].PassRate != 2.0/3.0 || byID["trip"].PassRate != 1 {
		t.Fatalf("wrong pass rates: %+v", byID)
	}

	ceiling := calibrationJournal()
	for i := range ceiling.TaskRuns {
		if ceiling.TaskRuns[i].TaskID == "gate-a" {
			ceiling.TaskRuns[i].Succeeded = true
		}
	}
	if _, err := BuildCalibration(ceiling, releaseTestSHA("a")); err == nil || !strings.Contains(err.Error(), "strictly between") {
		t.Fatalf("3/3 gate task was adopted: %v", err)
	}

	brokenTripwire := calibrationJournal()
	brokenTripwire.TaskRuns[0].Succeeded = false
	brokenTripwire.TaskRuns[0].ErrorText = "criteria not met"
	if _, err := BuildCalibration(brokenTripwire, releaseTestSHA("a")); err == nil || !strings.Contains(err.Error(), "tripwire") {
		t.Fatalf("failing tripwire was calibrated: %v", err)
	}
}

func TestBuildCalibration_RefusesMissingDuplicateOrHarnessEvidence(t *testing.T) {
	t.Run("missing repeat", func(t *testing.T) {
		j := calibrationJournal()
		j.TaskRuns = j.TaskRuns[1:]
		if _, err := BuildCalibration(j, releaseTestSHA("a")); err == nil || !strings.Contains(err.Error(), "repeat") {
			t.Fatalf("missing repeat accepted: %v", err)
		}
	})
	t.Run("duplicate repeat", func(t *testing.T) {
		j := calibrationJournal()
		j.TaskRuns = append(j.TaskRuns, j.TaskRuns[0])
		if _, err := BuildCalibration(j, releaseTestSHA("a")); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate repeat accepted: %v", err)
		}
	})
	t.Run("harness failure", func(t *testing.T) {
		j := calibrationJournal()
		j.TaskRuns[3].ErrorText = "could not assemble trace"
		j.TaskRuns[3].Succeeded = false
		if _, err := BuildCalibration(j, releaseTestSHA("a")); err == nil || !strings.Contains(err.Error(), "harness") {
			t.Fatalf("harness failure charged to calibration: %v", err)
		}
	})
}

func noiseFloorJournal(repeats int, gateA, gateB float64) Journal {
	j := calibrationJournal()
	j.TaskScores = nil
	for _, taskID := range []string{"gate-a", "gate-b"} {
		for repeat := 1; repeat <= repeats; repeat++ {
			score := gateA
			if taskID == "gate-b" {
				score = gateB
			}
			j.TaskScores = append(j.TaskScores, TaskScore{TaskID: taskID, Repeat: repeat,
				Kind: quality.ScoreKindPinnedCaseValidation, Score: score})
		}
	}
	return j
}

func TestBuildNoiseFloor_UsesWithinTaskVarianceAndPinsTenRepeats(t *testing.T) {
	a := noiseFloorJournal(10, .4, .6)
	b := noiseFloorJournal(10, .5, .4)
	got, err := BuildNoiseFloor(a, b, releaseTestSHA("a"), releaseTestSHA("b"))
	if err != nil {
		t.Fatalf("build noise floor: %v", err)
	}
	// Per-task deltas are +0.1 and -0.2. Their sample standard deviation
	// is sqrt((.15^2 + .15^2) / 1).
	want := math.Sqrt(.045)
	if math.Abs(got.SigmaD-want) > 1e-12 {
		t.Fatalf("sigma_d = %.12f, want %.12f", got.SigmaD, want)
	}
	if got.SigmaN != 10 || got.GateTaskCount != 2 || got.ArmRepeats != 10 {
		t.Fatalf("noise-floor provenance = %+v", got)
	}

	if _, err := BuildNoiseFloor(noiseFloorJournal(9, .4, .6), b, releaseTestSHA("a"), releaseTestSHA("b")); err == nil || !strings.Contains(err.Error(), "at least 10") {
		t.Fatalf("nine-repeat sigma was accepted: %v", err)
	}
	flat := noiseFloorJournal(10, .5, .5)
	if _, err := BuildNoiseFloor(flat, flat, releaseTestSHA("a"), releaseTestSHA("b")); err == nil || !strings.Contains(err.Error(), "non-positive") {
		t.Fatalf("zero-variance sigma was accepted: %v", err)
	}
}

func TestReleaseGatePolicy_ValidatesAndHashesItsThresholds(t *testing.T) {
	p := ReleaseGatePolicy{Metric: PinnedCaseValidationMetric, MaxScoreRegression: .05,
		MaxStepNoOutputRateIncrease: .02,
		ForbiddenNewErrorClasses:    []string{"context_overflow", "degenerate_loop"}}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	if p.SHA256() == "" {
		t.Fatal("policy has no digest")
	}
	changed := p
	changed.MaxScoreRegression = .10
	if p.SHA256() == changed.SHA256() {
		t.Fatal("changing the release threshold did not change the policy digest")
	}
	changed = p
	changed.ForbiddenNewErrorClasses = []string{"degenerate_loop", "context_overflow"}
	if p.SHA256() != changed.SHA256() {
		t.Fatal("set ordering changed the release-policy digest")
	}
}

func releaseGateFixture(t *testing.T) (Journal, Journal, CalibrationManifest, NoiseFloorManifest, ReleaseGatePolicy) {
	t.Helper()
	tiers := map[string]TaskTier{"trip": TaskTierTripwire, "gate-a": TaskTierGate,
		"gate-b": TaskTierGate, "long": TaskTierExploratory}
	policy := ReleaseGatePolicy{Metric: PinnedCaseValidationMetric, MaxScoreRegression: .05,
		MaxStepNoOutputRateIncrease: .02, ForbiddenNewErrorClasses: []string{"context_overflow"}}
	calibration := CalibrationManifest{HarnessVersion: HarnessVersion, TaskSetSHA256: "tasks",
		TierPolicySHA256: "tiers", ScoringPolicySHA256: "scores", MinimumAttempts: 3,
		SourceRunID: "calibration", SourceArmKey: "calibration-arm", SourceJournalSHA256: releaseTestSHA("c"),
		Tasks: []CalibrationTask{
			{TaskID: "gate-a", Tier: TaskTierGate, Attempts: 3, Passed: 2, PassRate: 2.0 / 3},
			{TaskID: "gate-b", Tier: TaskTierGate, Attempts: 3, Passed: 1, PassRate: 1.0 / 3},
			{TaskID: "long", Tier: TaskTierExploratory, Attempts: 3},
			{TaskID: "trip", Tier: TaskTierTripwire, Attempts: 3, Passed: 3, PassRate: 1},
		}}
	noise := NoiseFloorManifest{HarnessVersion: HarnessVersion, TaskSetSHA256: "tasks",
		TierPolicySHA256: "tiers", ScoringPolicySHA256: "scores", Metric: PinnedCaseValidationMetric,
		GateTaskCount: 2, SigmaN: 10, ArmRepeats: 10, SigmaD: .01,
		SourceRunIDs: []string{"noise-a", "noise-b"}, SourceJournalSHA256: []string{releaseTestSHA("d"), releaseTestSHA("e")}}
	arm := baseArm()
	arm.TaskSetSHA256, arm.TierPolicySHA256, arm.ScoringPolicySHA256 = "tasks", "tiers", "scores"
	noise.SourceArm, noise.SourceArmKey = arm, arm.Key()
	pre := validPreReg()
	pre.Metric = PinnedCaseValidationMetric
	pre.TargetDelta = policy.MaxScoreRegression
	pre.SigmaD, pre.SigmaN = noise.SigmaD, noise.SigmaN
	pre.ComputedPairs, _ = RequiredPairs(pre.SigmaD, pre.TargetDelta)
	pre.IndependentAxes = []string{"binary_sha256", "agent_images"}
	pre.CalibrationSHA256 = calibration.SHA256()
	pre.NoiseFloorSHA256 = noise.SHA256()
	pre.ReleaseGatePolicySHA256 = policy.SHA256()

	preHash, err := pre.Hash()
	if err != nil {
		t.Fatalf("hash pre-registration: %v", err)
	}
	baseline := Journal{Manifest: RunManifest{RunID: "baseline", Arm: arm, ArmKey: arm.Key(),
		PreRegistration: pre, PreRegistrationHash: preHash, TaskTiers: tiers}}
	candidate := baseline
	candidate.Manifest.RunID = "candidate"
	candidate.Manifest.Arm.Name = "candidate"
	candidate.Manifest.Arm.BinarySHA256 = "candidate-bin"
	candidate.Manifest.Arm.AgentImages = map[string]string{"worker": "sha256:candidate"}
	candidate.Manifest.ArmKey = candidate.Manifest.Arm.Key()

	for repeat := 1; repeat <= 10; repeat++ {
		for _, id := range []string{"trip", "gate-a", "gate-b", "long"} {
			run := TaskRun{TaskID: id, Repeat: repeat, Succeeded: true, ExecutionIDs: []string{id}}
			baseline.TaskRuns = append(baseline.TaskRuns, run)
			candidate.TaskRuns = append(candidate.TaskRuns, run)
		}
		for _, id := range []string{"gate-a", "gate-b", "long"} {
			baseline.TaskScores = append(baseline.TaskScores, TaskScore{TaskID: id, Repeat: repeat,
				Kind: quality.ScoreKindPinnedCaseValidation, Score: .5})
			candidate.TaskScores = append(candidate.TaskScores, TaskScore{TaskID: id, Repeat: repeat,
				Kind: quality.ScoreKindPinnedCaseValidation, Score: .6})
		}
	}
	baseline.Records = []ExecutionRecord{
		{TaskID: "gate-a", ExecutionID: "gate-a", Verdicts: []Verdict{{Schema: &SchemaVerdict{
			Terminal: 10, NoOutput: 1, NoOutputByErrorClass: map[string]int{"degenerate_loop": 1}}}}},
		{TaskID: "gate-b", ExecutionID: "gate-b", Verdicts: []Verdict{{Schema: &SchemaVerdict{Terminal: 10}}}},
	}
	candidate.Records = []ExecutionRecord{
		{TaskID: "gate-a", ExecutionID: "gate-a", Verdicts: []Verdict{{Schema: &SchemaVerdict{
			Terminal: 10, NoOutput: 1, NoOutputByErrorClass: map[string]int{"degenerate_loop": 1}}}}},
		{TaskID: "gate-b", ExecutionID: "gate-b", Verdicts: []Verdict{{Schema: &SchemaVerdict{Terminal: 10}}}},
	}
	return baseline, candidate, calibration, noise, policy
}

func TestEvaluateReleaseGate_PassesOnlyCompleteNonRegressingEvidence(t *testing.T) {
	a, b, calibration, noise, policy := releaseGateFixture(t)
	got := EvaluateReleaseGate(a, b, calibration, noise, policy)
	if got.Status != GateStatusPass {
		t.Fatalf("gate = %+v", got)
	}
	if math.Abs(got.GateScore.MeanDelta-.1) > 1e-12 || got.GateScore.PairCount != 2 {
		t.Fatalf("score component = %+v", got.GateScore)
	}
}

func TestValidateReleaseRunPlan_RefusesArtifactOrRepeatDriftBeforeExecution(t *testing.T) {
	a, _, calibration, noise, policy := releaseGateFixture(t)
	if err := ValidateReleaseRunPlan(a.Manifest.PreRegistration, a.Manifest.Arm,
		a.Manifest.TaskTiers, 10, calibration, noise, policy); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	if err := ValidateReleaseRunPlan(a.Manifest.PreRegistration, a.Manifest.Arm,
		a.Manifest.TaskTiers, 9, calibration, noise, policy); err == nil || !strings.Contains(err.Error(), "repeat") {
		t.Fatalf("changed repeat regime accepted: %v", err)
	}
	tampered := calibration
	tampered.MinimumAttempts++
	if err := ValidateReleaseRunPlan(a.Manifest.PreRegistration, a.Manifest.Arm,
		a.Manifest.TaskTiers, 10, tampered, noise, policy); err == nil || !strings.Contains(err.Error(), "calibration sha256") {
		t.Fatalf("changed calibration accepted: %v", err)
	}
	driftedArm := a.Manifest.Arm
	driftedArm.ContextPolicy = "different-policy"
	if err := ValidateReleaseRunPlan(a.Manifest.PreRegistration, driftedArm,
		a.Manifest.TaskTiers, 10, calibration, noise, policy); err == nil || !strings.Contains(err.Error(), "noise-floor source") {
		t.Fatalf("configuration drift from the measured noise source accepted: %v", err)
	}
}

func TestBuildReleasePreRegistration_DerivesCanonicalHashesAndPairCount(t *testing.T) {
	_, _, calibration, noise, policy := releaseGateFixture(t)
	pre, err := BuildReleasePreRegistration([]string{"2026.8.1", "2026.8.2"},
		"refuse an agent-quality regression in the candidate release", calibration, noise, policy)
	if err != nil {
		t.Fatalf("build release pre-registration: %v", err)
	}
	wantPairs, _ := RequiredPairs(noise.SigmaD, policy.MaxScoreRegression)
	if pre.ComputedPairs != wantPairs || pre.NoiseFloorSHA256 != noise.SHA256() ||
		pre.ReleaseGatePolicySHA256 != policy.SHA256() {
		t.Fatalf("derived pre-registration = %+v", pre)
	}
	badNoise := noise
	badNoise.GateTaskCount++
	if _, err := BuildReleasePreRegistration(pre.Arms, pre.Rationale, calibration, badNoise, policy); err == nil {
		t.Fatal("built a pre-registration from mismatched calibration and noise artifacts")
	}
}

func TestEvaluateReleaseGate_FailsTripwireScoreAndStepRegressions(t *testing.T) {
	t.Run("tripwire", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		b.TaskRuns[0].Succeeded = false
		b.TaskRuns[0].ErrorText = "criteria not met"
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusFail || !strings.Contains(got.Reason, "tripwire") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("graded score", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		for i := range b.TaskScores {
			if b.TaskScores[i].TaskID == "gate-a" || b.TaskScores[i].TaskID == "gate-b" {
				b.TaskScores[i].Score = .4
			}
		}
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusFail || !strings.Contains(got.Reason, "score") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("new step error class", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		b.Records[0].Verdicts[0].Schema.NoOutputByErrorClass["context_overflow"] = 1
		b.Records[0].Verdicts[0].Schema.NoOutput++
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusFail || !strings.Contains(got.Reason, "context_overflow") {
			t.Fatalf("gate = %+v", got)
		}
	})
}

func TestEvaluateReleaseGate_RefusesInconclusiveOrMismatchedEvidence(t *testing.T) {
	t.Run("artifact mismatch", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		for _, journal := range []*Journal{&a, &b} {
			journal.Manifest.PreRegistration.CalibrationSHA256 = strings.Repeat("f", 64)
			journal.Manifest.PreRegistrationHash, _ = journal.Manifest.PreRegistration.Hash()
		}
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusRefused || !strings.Contains(got.Reason, "calibration") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("missing step evidence", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		b.Records = b.Records[:1]
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusRefused || !strings.Contains(got.Reason, "schema verdict") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("missing graded repeat", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		b.TaskScores = b.TaskScores[1:]
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusRefused || !strings.Contains(got.Reason, "gate scores") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("unhealthy baseline tripwire", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		a.TaskRuns[0].Succeeded = false
		a.TaskRuns[0].ErrorText = "criteria not met"
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusRefused || !strings.Contains(got.Reason, "baseline tripwire") {
			t.Fatalf("gate = %+v", got)
		}
	})
	t.Run("negative but unresolved", func(t *testing.T) {
		a, b, c, n, p := releaseGateFixture(t)
		p.MaxScoreRegression = .02
		for i := range b.TaskScores {
			if b.TaskScores[i].TaskID == "gate-a" || b.TaskScores[i].TaskID == "gate-b" {
				b.TaskScores[i].Score = .49
			}
		}
		for _, j := range []*Journal{&a, &b} {
			j.Manifest.PreRegistration.TargetDelta = p.MaxScoreRegression
			j.Manifest.PreRegistration.ComputedPairs, _ = RequiredPairs(n.SigmaD, p.MaxScoreRegression)
			j.Manifest.PreRegistration.ReleaseGatePolicySHA256 = p.SHA256()
			j.Manifest.PreRegistrationHash, _ = j.Manifest.PreRegistration.Hash()
		}
		got := EvaluateReleaseGate(a, b, c, n, p)
		if got.Status != GateStatusRefused || !strings.Contains(got.Reason, "inconclusive") {
			t.Fatalf("gate = %+v", got)
		}
	})
}
