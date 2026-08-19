package agentbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"vornik.io/vornik/internal/quality"
)

// TaskTier separates deterministic alarms, calibrated release evidence, and
// deliberately non-gating exploratory work.
type TaskTier string

const (
	// TaskTierTripwire alarms on any task failure and never enters a score denominator.
	TaskTierTripwire TaskTier = "tripwire"
	// TaskTierGate contains calibrated graded tasks that decide a release.
	TaskTierGate TaskTier = "gate"
	// TaskTierExploratory is reported but can never change a release verdict.
	TaskTierExploratory TaskTier = "exploratory"
)

// TaskRun is one submitted benchmark task repeat. Execution records cannot
// stand in for this unit because a retry or fork may produce several executions.
type TaskRun struct {
	TaskID       string   `json:"taskId"`
	Repeat       int      `json:"repeat"`
	Succeeded    bool     `json:"succeeded"`
	ErrorText    string   `json:"errorText,omitempty"`
	ExecutionIDs []string `json:"executionIds,omitempty"`
}

// ValidateTaskTiers refuses a blended/implicit task set before it can be used
// by a release gate.
func ValidateTaskTiers(tasks []TaskSpec) error {
	seen := map[string]bool{}
	for _, task := range tasks {
		if strings.TrimSpace(task.ID) == "" {
			return fmt.Errorf("benchmark task has an empty id")
		}
		if seen[task.ID] {
			return fmt.Errorf("task id %q appears twice", task.ID)
		}
		seen[task.ID] = true
		switch task.Tier {
		case TaskTierTripwire, TaskTierExploratory:
		case TaskTierGate:
			if task.Scoring == nil {
				return fmt.Errorf("gate task %q needs a graded scoring policy", task.ID)
			}
		default:
			if task.Tier == "" {
				return fmt.Errorf("task %q has no benchmark tier", task.ID)
			}
			return fmt.Errorf("task %q has unknown benchmark tier %q", task.ID, task.Tier)
		}
	}
	return nil
}

// TierPolicyDigest pins task-to-tier interpretation separately from task text.
func TierPolicyDigest(tasks []TaskSpec) string {
	type row struct {
		TaskID string   `json:"taskId"`
		Tier   TaskTier `json:"tier"`
	}
	rows := make([]row, 0, len(tasks))
	for _, task := range tasks {
		if task.ID == "" || task.Tier == "" {
			return ""
		}
		rows = append(rows, row{TaskID: task.ID, Tier: task.Tier})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
	return digestJSON(rows)
}

// CalibrationTask is the observed adoption evidence for one candidate task.
type CalibrationTask struct {
	TaskID   string   `json:"taskId"`
	Tier     TaskTier `json:"tier"`
	Attempts int      `json:"attempts"`
	Passed   int      `json:"passed"`
	PassRate float64  `json:"passRate"`
}

// CalibrationManifest proves task difficulty on the exact tiered task set.
type CalibrationManifest struct {
	HarnessVersion      string            `json:"harnessVersion"`
	TaskSetSHA256       string            `json:"taskSetSha256"`
	TierPolicySHA256    string            `json:"tierPolicySha256"`
	ScoringPolicySHA256 string            `json:"scoringPolicySha256,omitempty"`
	SourceRunID         string            `json:"sourceRunId"`
	SourceArmKey        string            `json:"sourceArmKey"`
	SourceJournalSHA256 string            `json:"sourceJournalSha256"`
	MinimumAttempts     int               `json:"minimumAttempts"`
	Tasks               []CalibrationTask `json:"tasks"`
}

// SHA256 returns the immutable identity a pre-registration pins.
func (m CalibrationManifest) SHA256() string { return digestJSON(m) }

// BuildCalibration derives and validates calibration without rewriting tiers.
func BuildCalibration(j Journal, sourceJournalSHA256 string) (CalibrationManifest, error) {
	if !validSHA256(sourceJournalSHA256) {
		return CalibrationManifest{}, fmt.Errorf("source journal sha256 must be 64 hexadecimal characters")
	}
	if j.Manifest.Untrustworthy {
		return CalibrationManifest{}, fmt.Errorf("calibration source journal is untrustworthy: %s", j.Manifest.UntrustworthyReason)
	}
	if j.Manifest.ArmPartial {
		return CalibrationManifest{}, fmt.Errorf("calibration source journal has a partial arm key")
	}
	if j.Manifest.ArmKey == "" || j.Manifest.ArmKey != j.Manifest.Arm.Key() {
		return CalibrationManifest{}, fmt.Errorf("calibration source journal arm key is missing or inconsistent")
	}
	if len(j.Manifest.TaskTiers) == 0 || j.Manifest.Arm.TierPolicySHA256 == "" {
		return CalibrationManifest{}, fmt.Errorf("journal has no task-tier policy")
	}
	byTask, err := calibrationTallies(j.TaskRuns, j.Manifest.TaskTiers)
	if err != nil {
		return CalibrationManifest{}, err
	}

	manifest := CalibrationManifest{
		HarnessVersion:      j.Manifest.Arm.HarnessVersion,
		TaskSetSHA256:       j.Manifest.Arm.TaskSetSHA256,
		TierPolicySHA256:    j.Manifest.Arm.TierPolicySHA256,
		ScoringPolicySHA256: j.Manifest.Arm.ScoringPolicySHA256,
		SourceRunID:         j.Manifest.RunID, SourceArmKey: j.Manifest.ArmKey,
		SourceJournalSHA256: sourceJournalSHA256,
	}
	manifest.Tasks, manifest.MinimumAttempts, err = calibratedTasks(j.Manifest.TaskTiers, byTask)
	if err != nil {
		return CalibrationManifest{}, err
	}
	return manifest, nil
}

type calibrationTally struct {
	seen   map[int]bool
	passed int
}

func calibrationTallies(runs []TaskRun, tiers map[string]TaskTier) (map[string]*calibrationTally, error) {
	byTask := map[string]*calibrationTally{}
	for _, run := range runs {
		if _, ok := tiers[run.TaskID]; !ok {
			return nil, fmt.Errorf("task run %q has no journaled tier", run.TaskID)
		}
		if run.Repeat < 1 {
			return nil, fmt.Errorf("task %q has invalid repeat %d", run.TaskID, run.Repeat)
		}
		if err := refuseCalibrationOperationalFailure(run); err != nil {
			return nil, err
		}
		tally := byTask[run.TaskID]
		if tally == nil {
			tally = &calibrationTally{seen: map[int]bool{}}
			byTask[run.TaskID] = tally
		}
		if tally.seen[run.Repeat] {
			return nil, fmt.Errorf("duplicate task/repeat pair %s/%d", run.TaskID, run.Repeat)
		}
		tally.seen[run.Repeat] = true
		if run.Succeeded {
			tally.passed++
		}
	}
	return byTask, nil
}

func refuseCalibrationOperationalFailure(run TaskRun) error {
	if run.Succeeded {
		return nil
	}
	switch ClassifyFailure(false, run.ErrorText) {
	case FailureHarness:
		return fmt.Errorf("task %q repeat %d is a harness failure", run.TaskID, run.Repeat)
	case FailureInfra:
		return fmt.Errorf("task %q repeat %d is an infra failure", run.TaskID, run.Repeat)
	default:
		return nil
	}
}

func calibratedTasks(tiers map[string]TaskTier, byTask map[string]*calibrationTally) ([]CalibrationTask, int, error) {
	ids := make([]string, 0, len(tiers))
	for id := range tiers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	minimum := 0
	tasks := make([]CalibrationTask, 0, len(ids))
	for _, id := range ids {
		tally := byTask[id]
		if tally == nil {
			return nil, 0, fmt.Errorf("task %q has no calibration repeats", id)
		}
		attempts := len(tally.seen)
		if err := validateCalibrationRepeats(id, attempts, tally.seen); err != nil {
			return nil, 0, err
		}
		if err := validateCalibrationTier(id, tiers[id], attempts, tally.passed); err != nil {
			return nil, 0, err
		}
		if minimum == 0 || attempts < minimum {
			minimum = attempts
		}
		tasks = append(tasks, CalibrationTask{TaskID: id, Tier: tiers[id], Attempts: attempts,
			Passed: tally.passed, PassRate: float64(tally.passed) / float64(attempts)})
	}
	return tasks, minimum, nil
}

func validateCalibrationRepeats(taskID string, attempts int, seen map[int]bool) error {
	if attempts < 3 {
		return fmt.Errorf("task %q has %d repeats; calibration requires at least 3", taskID, attempts)
	}
	for repeat := 1; repeat <= attempts; repeat++ {
		if !seen[repeat] {
			return fmt.Errorf("task %q is missing repeat %d", taskID, repeat)
		}
	}
	return nil
}

func validateCalibrationTier(taskID string, tier TaskTier, attempts, passed int) error {
	switch tier {
	case TaskTierGate:
		if passed == 0 || passed == attempts {
			return fmt.Errorf("gate task %q pass rate must be strictly between 0 and 1; got %d/%d", taskID, passed, attempts)
		}
	case TaskTierTripwire:
		if passed != attempts {
			return fmt.Errorf("tripwire task %q failed calibration (%d/%d)", taskID, passed, attempts)
		}
	case TaskTierExploratory:
	default:
		return fmt.Errorf("task %q has invalid tier %q", taskID, tier)
	}
	return nil
}

// NoiseFloorManifest pins the measured variance used to size release arms.
type NoiseFloorManifest struct {
	HarnessVersion      string    `json:"harnessVersion"`
	TaskSetSHA256       string    `json:"taskSetSha256"`
	TierPolicySHA256    string    `json:"tierPolicySha256"`
	ScoringPolicySHA256 string    `json:"scoringPolicySha256"`
	SourceRunIDs        []string  `json:"sourceRunIds"`
	SourceArmKey        string    `json:"sourceArmKey"`
	SourceArm           ArmFields `json:"sourceArm"`
	SourceJournalSHA256 []string  `json:"sourceJournalSha256"`
	Metric              string    `json:"metric"`
	GateTaskCount       int       `json:"gateTaskCount"`
	SigmaN              int       `json:"sigmaN"`
	ArmRepeats          int       `json:"armRepeats"`
	SigmaD              float64   `json:"sigmaD"`
}

// SHA256 returns the immutable identity a pre-registration pins.
func (m NoiseFloorManifest) SHA256() string { return digestJSON(m) }

// BuildNoiseFloor measures sigma_d directly from the per-task differences of
// two same-configuration arms. A single arm's within-task variance cannot
// recover this quantity and previously manufactured false adequacy.
func BuildNoiseFloor(a, b Journal, sourceASHA256, sourceBSHA256 string) (NoiseFloorManifest, error) {
	if !validSHA256(sourceASHA256) || !validSHA256(sourceBSHA256) {
		return NoiseFloorManifest{}, fmt.Errorf("both source journal sha256 values must be 64 hexadecimal characters")
	}
	if a.Manifest.Untrustworthy || b.Manifest.Untrustworthy {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor source journal is untrustworthy")
	}
	if a.Manifest.ArmPartial || b.Manifest.ArmPartial {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor source journal has a partial arm key")
	}
	if a.Manifest.ArmKey == "" || b.Manifest.ArmKey == "" ||
		a.Manifest.ArmKey != a.Manifest.Arm.Key() || b.Manifest.ArmKey != b.Manifest.Arm.Key() {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor source journal arm key is missing or inconsistent")
	}
	if err := CheckComparable(a.Manifest.Arm, b.Manifest.Arm); err != nil {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor arms must have identical configuration: %w", err)
	}
	if !sameTaskTiers(a.Manifest.TaskTiers, b.Manifest.TaskTiers) {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor journals have different task-tier maps")
	}
	left, repeatsA, err := gateTaskMeans(a)
	if err != nil {
		return NoiseFloorManifest{}, fmt.Errorf("first noise-floor journal: %w", err)
	}
	right, repeatsB, err := gateTaskMeans(b)
	if err != nil {
		return NoiseFloorManifest{}, fmt.Errorf("second noise-floor journal: %w", err)
	}
	if repeatsA != repeatsB {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor repeat regimes differ: %d vs %d", repeatsA, repeatsB)
	}
	if len(left) != len(right) {
		return NoiseFloorManifest{}, fmt.Errorf("noise-floor gate task sets differ: %d vs %d", len(left), len(right))
	}
	ids := make([]string, 0, len(left))
	for id := range left {
		if _, ok := right[id]; !ok {
			return NoiseFloorManifest{}, fmt.Errorf("gate task %q is missing from the second noise-floor journal", id)
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	diffs := make([]float64, 0, len(ids))
	for _, id := range ids {
		diffs = append(diffs, right[id]-left[id])
	}
	mean := meanFloat(diffs)
	sigmaD := sampleStdDev(diffs, mean)
	if math.IsNaN(sigmaD) || math.IsInf(sigmaD, 0) || sigmaD <= 0 {
		return NoiseFloorManifest{}, fmt.Errorf("noise floor produced non-positive sigma_d; zero variance is not proof of stability")
	}
	return NoiseFloorManifest{
		HarnessVersion:      a.Manifest.Arm.HarnessVersion,
		TaskSetSHA256:       a.Manifest.Arm.TaskSetSHA256,
		TierPolicySHA256:    a.Manifest.Arm.TierPolicySHA256,
		ScoringPolicySHA256: a.Manifest.Arm.ScoringPolicySHA256,
		SourceRunIDs:        []string{a.Manifest.RunID, b.Manifest.RunID},
		SourceArmKey:        a.Manifest.ArmKey, SourceArm: a.Manifest.Arm,
		SourceJournalSHA256: []string{sourceASHA256, sourceBSHA256},
		Metric:              PinnedCaseValidationMetric, GateTaskCount: len(ids),
		SigmaN: repeatsA, ArmRepeats: repeatsA, SigmaD: sigmaD,
	}, nil
}

func gateTaskMeans(j Journal) (map[string]float64, int, error) {
	gateIDs := map[string]bool{}
	for id, tier := range j.Manifest.TaskTiers {
		if tier == TaskTierGate {
			gateIDs[id] = true
		}
	}
	if len(gateIDs) < 2 {
		return nil, 0, fmt.Errorf("need at least two gate-tier tasks to measure sample sigma_d")
	}
	all, err := taskRepeatScores(j.TaskScores, quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]float64{}
	repeats := 0
	for id := range gateIDs {
		scores := all[id]
		if len(scores) < MinimumSigmaRuns {
			return nil, 0, fmt.Errorf("gate task %q has %d scored repeats; need at least 10", id, len(scores))
		}
		for repeat := 1; repeat <= len(scores); repeat++ {
			if _, ok := scores[repeat]; !ok {
				return nil, 0, fmt.Errorf("gate task %q is missing repeat %d", id, repeat)
			}
		}
		if repeats == 0 {
			repeats = len(scores)
		} else if repeats != len(scores) {
			return nil, 0, fmt.Errorf("gate tasks use different repeat regimes: %d vs %d", repeats, len(scores))
		}
		out[id] = meanScore(scores)
	}
	return out, repeats, nil
}

func sameTaskTiers(a, b map[string]TaskTier) bool {
	if len(a) != len(b) {
		return false
	}
	for id, tier := range a {
		if b[id] != tier {
			return false
		}
	}
	return true
}

// ReleaseGatePolicy is committed before results and hashed into the
// pre-registration. It deliberately contains no per-invocation override.
type ReleaseGatePolicy struct {
	Metric                      string   `json:"metric"`
	MaxScoreRegression          float64  `json:"maxScoreRegression"`
	MaxStepNoOutputRateIncrease float64  `json:"maxStepNoOutputRateIncrease"`
	ForbiddenNewErrorClasses    []string `json:"forbiddenNewErrorClasses,omitempty"`
}

// GateStatus is deliberately three-valued. Missing or statistically
// insufficient evidence is never collapsed into a passing release.
type GateStatus string

const (
	// GateStatusPass means all committed evidence is complete and non-regressing.
	GateStatusPass GateStatus = "PASS"
	// GateStatusFail means the candidate has a measured policy violation.
	GateStatusFail GateStatus = "FAIL"
	// GateStatusRefused means integrity or statistical sufficiency was not established.
	GateStatusRefused GateStatus = "REFUSED"
)

// GateScoreResult is the paired, repeat-averaged score evidence used by the
// verdict. ResolvableFloor is derived from the pre-measured noise artifact,
// not re-estimated from whichever release result happened to arrive.
type GateScoreResult struct {
	MeanDelta       float64 `json:"meanDelta"`
	PairCount       int     `json:"pairCount"`
	ResolvableFloor float64 `json:"resolvableFloor"`
}

// GateStepResult keeps the reliability half of schema conformance visible.
type GateStepResult struct {
	BaselineTerminal      int            `json:"baselineTerminal"`
	CandidateTerminal     int            `json:"candidateTerminal"`
	BaselineNoOutput      int            `json:"baselineNoOutput"`
	CandidateNoOutput     int            `json:"candidateNoOutput"`
	BaselineNoOutputRate  float64        `json:"baselineNoOutputRate"`
	CandidateNoOutputRate float64        `json:"candidateNoOutputRate"`
	RateDelta             float64        `json:"rateDelta"`
	BaselineErrorClasses  map[string]int `json:"baselineErrorClasses,omitempty"`
	CandidateErrorClasses map[string]int `json:"candidateErrorClasses,omitempty"`
}

// ExploratoryScore is published for diagnosis but cannot affect Status.
type ExploratoryScore struct {
	MeanDelta       float64 `json:"meanDelta"`
	PairCount       int     `json:"pairCount"`
	ResolvableFloor float64 `json:"resolvableFloor,omitempty"`
	Inconclusive    bool    `json:"inconclusive"`
}

// ReleaseGateDecision is a self-contained operator artifact.
type ReleaseGateDecision struct {
	Status      GateStatus        `json:"status"`
	Reason      string            `json:"reason"`
	GateScore   GateScoreResult   `json:"gateScore"`
	StepHealth  GateStepResult    `json:"stepHealth"`
	Exploratory *ExploratoryScore `json:"exploratory,omitempty"`
}

// BuildReleasePreRegistration turns reviewed artifacts into the exact commit
// both release arms must share. Keeping this derivation in code prevents an
// operator from copying sigma correctly but computing n from a different
// threshold (or hashing the policy file bytes instead of its canonical form).
func BuildReleasePreRegistration(arms []string, rationale string, calibration CalibrationManifest,
	noise NoiseFloorManifest, policy ReleaseGatePolicy) (PreRegistration, error) {
	if err := policy.Validate(); err != nil {
		return PreRegistration{}, err
	}
	if calibration.HarnessVersion != noise.HarnessVersion ||
		calibration.TaskSetSHA256 != noise.TaskSetSHA256 ||
		calibration.TierPolicySHA256 != noise.TierPolicySHA256 ||
		calibration.ScoringPolicySHA256 != noise.ScoringPolicySHA256 {
		return PreRegistration{}, fmt.Errorf("calibration and noise-floor artifacts describe different benchmark contracts")
	}
	if err := validateArtifactProvenance(calibration, noise); err != nil {
		return PreRegistration{}, err
	}
	tiers := make(map[string]TaskTier, len(calibration.Tasks))
	for _, task := range calibration.Tasks {
		tiers[task.TaskID] = task.Tier
	}
	gate, _, _, err := calibratedTierSets(calibration, tiers)
	if err != nil {
		return PreRegistration{}, fmt.Errorf("invalid calibration artifact: %w", err)
	}
	if noise.GateTaskCount != len(gate) || noise.Metric != policy.Metric ||
		noise.SigmaN != noise.ArmRepeats || noise.SigmaN < MinimumSigmaRuns {
		return PreRegistration{}, fmt.Errorf("noise-floor task count, metric, or repeat provenance is invalid")
	}
	if noise.SourceArmKey == "" || noise.SourceArmKey != noise.SourceArm.Key() || noise.SourceArm.Partial() {
		return PreRegistration{}, fmt.Errorf("noise-floor source arm provenance is missing or inconsistent")
	}
	required, err := RequiredPairs(noise.SigmaD, policy.MaxScoreRegression)
	if err != nil {
		return PreRegistration{}, err
	}
	if len(gate) < required {
		floor, _ := ResolvableDelta(noise.SigmaD, len(gate))
		return PreRegistration{}, fmt.Errorf("calibrated gate has %d tasks but the measured noise needs %d; current floor is %.4f",
			len(gate), required, floor)
	}
	pre := PreRegistration{
		Arms: arms, Metric: policy.Metric, TargetDelta: policy.MaxScoreRegression,
		SigmaD: noise.SigmaD, SigmaN: noise.SigmaN, ComputedPairs: required,
		Rationale: rationale, IndependentAxes: []string{"binary_sha256", "agent_images"},
		CalibrationSHA256: calibration.SHA256(), NoiseFloorSHA256: noise.SHA256(),
		ReleaseGatePolicySHA256: policy.SHA256(),
	}
	if err := pre.Validate(); err != nil {
		return PreRegistration{}, err
	}
	return pre, nil
}

// ValidateReleaseRunPlan is the pre-spend half of the gate. It verifies that a
// run is about to use the exact task/scoring contract, thresholds, sigma, and
// repeat regime pinned in its pre-registration.
func ValidateReleaseRunPlan(pre PreRegistration, arm ArmFields, taskTiers map[string]TaskTier, repeats int,
	calibration CalibrationManifest, noise NoiseFloorManifest, policy ReleaseGatePolicy) error {
	if err := pre.Validate(); err != nil {
		return err
	}
	if !pre.ReleaseGateEnabled() {
		return fmt.Errorf("pre-registration does not pin release gate artifacts")
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := validateArtifactProvenance(calibration, noise); err != nil {
		return err
	}
	if pre.CalibrationSHA256 != calibration.SHA256() {
		return fmt.Errorf("calibration sha256 differs from pre-registration")
	}
	if pre.NoiseFloorSHA256 != noise.SHA256() {
		return fmt.Errorf("noise-floor sha256 differs from pre-registration")
	}
	if pre.ReleaseGatePolicySHA256 != policy.SHA256() {
		return fmt.Errorf("release-policy sha256 differs from pre-registration")
	}
	if pre.Metric != policy.Metric || pre.TargetDelta != policy.MaxScoreRegression {
		return fmt.Errorf("metric or score threshold differs from release policy")
	}
	if pre.SigmaD != noise.SigmaD || pre.SigmaN != noise.SigmaN {
		return fmt.Errorf("pre-registered sigma differs from noise-floor artifact")
	}
	required, err := RequiredPairs(noise.SigmaD, policy.MaxScoreRegression)
	if err != nil {
		return err
	}
	if pre.ComputedPairs != required {
		return fmt.Errorf("pre-registered pair count is %d; artifact-derived requirement is %d", pre.ComputedPairs, required)
	}
	if repeats != noise.ArmRepeats || noise.SigmaN != noise.ArmRepeats || noise.SigmaN < MinimumSigmaRuns {
		return fmt.Errorf("release repeats=%d must exactly match the measured repeat regime=%d", repeats, noise.ArmRepeats)
	}
	if arm.HarnessVersion != calibration.HarnessVersion || arm.HarnessVersion != noise.HarnessVersion ||
		arm.TaskSetSHA256 != calibration.TaskSetSHA256 || arm.TaskSetSHA256 != noise.TaskSetSHA256 ||
		arm.TierPolicySHA256 != calibration.TierPolicySHA256 || arm.TierPolicySHA256 != noise.TierPolicySHA256 ||
		arm.ScoringPolicySHA256 != calibration.ScoringPolicySHA256 || arm.ScoringPolicySHA256 != noise.ScoringPolicySHA256 {
		return fmt.Errorf("run arm identity differs from calibration or noise-floor artifacts")
	}
	if noise.SourceArmKey == "" || noise.SourceArmKey != noise.SourceArm.Key() || noise.SourceArm.Partial() {
		return fmt.Errorf("noise-floor source arm provenance is missing or inconsistent")
	}
	if err := CheckComparableExcept(noise.SourceArm, arm, []string{"binary_sha256", "agent_images"}); err != nil {
		return fmt.Errorf("run differs from noise-floor source configuration: %w", err)
	}
	gate, _, _, err := calibratedTierSets(calibration, taskTiers)
	if err != nil {
		return fmt.Errorf("invalid calibration artifact: %w", err)
	}
	if len(gate) != noise.GateTaskCount || noise.Metric != policy.Metric {
		return fmt.Errorf("noise-floor task count or metric differs from the calibrated gate")
	}
	power, err := CheckPower(noise.SigmaD, noise.SigmaN, policy.MaxScoreRegression, len(gate))
	if err != nil {
		return err
	}
	if err := power.Refuse(); err != nil {
		return err
	}
	return nil
}

// EvaluateReleaseGate evaluates only journaled evidence against artifacts
// committed by both arms before execution. Integrity errors are REFUSED;
// measured regressions are FAIL; PASS requires both complete and sufficiently
// powered evidence.
func EvaluateReleaseGate(baseline, candidate Journal, calibration CalibrationManifest, noise NoiseFloorManifest, policy ReleaseGatePolicy) ReleaseGateDecision {
	refuse := func(format string, args ...any) ReleaseGateDecision {
		return ReleaseGateDecision{Status: GateStatusRefused, Reason: fmt.Sprintf(format, args...)}
	}
	fail := func(reason string, score GateScoreResult, step GateStepResult, exploratory *ExploratoryScore) ReleaseGateDecision {
		return ReleaseGateDecision{Status: GateStatusFail, Reason: reason, GateScore: score, StepHealth: step, Exploratory: exploratory}
	}

	tiers, err := validateReleaseJournalPair(baseline, candidate, calibration, noise, policy)
	if err != nil {
		return refuse("%v", err)
	}
	if tripwireDecision := releaseTripwireDecision(baseline, candidate, tiers.tripwire); tripwireDecision != nil {
		return *tripwireDecision
	}
	if err := validateGateScores(baseline, tiers.gate, noise.ArmRepeats); err != nil {
		return refuse("baseline gate scores: %v", err)
	}
	if err := validateGateScores(candidate, tiers.gate, noise.ArmRepeats); err != nil {
		return refuse("candidate gate scores: %v", err)
	}

	gateComparison, err := CompareTaskScores(filterTaskScores(baseline.TaskScores, tiers.gate),
		filterTaskScores(candidate.TaskScores, tiers.gate), quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		return refuse("gate score evidence: %v", err)
	}
	power, err := CheckPower(noise.SigmaD, noise.SigmaN, policy.MaxScoreRegression, gateComparison.PairCount)
	if err != nil {
		return refuse("gate power calculation: %v", err)
	}
	if err := power.Refuse(); err != nil {
		return refuse("gate score evidence is underpowered: %v", err)
	}
	score := GateScoreResult{MeanDelta: gateComparison.MeanDelta, PairCount: gateComparison.PairCount,
		ResolvableFloor: power.ResolvableDelta}
	exploratory := exploratoryComparison(baseline, candidate, tiers.exploratory, noise.SigmaD)
	step, err := compareGateStepHealth(baseline, candidate, tiers.gate)
	if err != nil {
		return refuse("gate step evidence: %v", err)
	}
	for _, class := range policy.ForbiddenNewErrorClasses {
		if step.BaselineErrorClasses[class] == 0 && step.CandidateErrorClasses[class] > 0 {
			return fail(fmt.Sprintf("candidate introduced forbidden step error class %q", class), score, step, exploratory)
		}
	}
	if step.RateDelta > policy.MaxStepNoOutputRateIncrease {
		return fail(fmt.Sprintf("step no-output rate increased by %.4f (limit %.4f)", step.RateDelta,
			policy.MaxStepNoOutputRateIncrease), score, step, exploratory)
	}
	if score.MeanDelta < -policy.MaxScoreRegression {
		return fail(fmt.Sprintf("gate score regressed by %.4f (limit %.4f)", -score.MeanDelta,
			policy.MaxScoreRegression), score, step, exploratory)
	}
	if score.MeanDelta-score.ResolvableFloor <= -policy.MaxScoreRegression {
		return ReleaseGateDecision{Status: GateStatusRefused,
			Reason: fmt.Sprintf("gate score is inconclusive: delta %.4f with floor %.4f cannot exclude regression beyond %.4f",
				score.MeanDelta, score.ResolvableFloor, policy.MaxScoreRegression),
			GateScore: score, StepHealth: step, Exploratory: exploratory}
	}
	return ReleaseGateDecision{Status: GateStatusPass, Reason: "all pre-registered release gates passed",
		GateScore: score, StepHealth: step, Exploratory: exploratory}
}

type releaseTierSets struct {
	gate        map[string]bool
	tripwire    map[string]bool
	exploratory map[string]bool
}

func validateReleaseJournalPair(baseline, candidate Journal, calibration CalibrationManifest,
	noise NoiseFloorManifest, policy ReleaseGatePolicy) (releaseTierSets, error) {
	if err := policy.Validate(); err != nil {
		return releaseTierSets{}, fmt.Errorf("invalid release gate policy: %w", err)
	}
	if err := validateReleaseJournal("baseline", baseline); err != nil {
		return releaseTierSets{}, err
	}
	if err := validateReleaseJournal("candidate", candidate); err != nil {
		return releaseTierSets{}, err
	}
	if baseline.Manifest.PreRegistrationHash != candidate.Manifest.PreRegistrationHash {
		return releaseTierSets{}, fmt.Errorf("release journals were run under different pre-registrations")
	}
	if !sameTaskTiers(baseline.Manifest.TaskTiers, candidate.Manifest.TaskTiers) {
		return releaseTierSets{}, fmt.Errorf("release journals have different task-tier maps")
	}
	if err := CheckComparableExcept(baseline.Manifest.Arm, candidate.Manifest.Arm,
		[]string{"binary_sha256", "agent_images"}); err != nil {
		return releaseTierSets{}, fmt.Errorf("release arms are not comparable: %w", err)
	}
	if err := validateReleaseArtifacts(baseline, candidate, calibration, noise, policy); err != nil {
		return releaseTierSets{}, fmt.Errorf("release artifact mismatch: %w", err)
	}
	gate, tripwire, exploratory, err := calibratedTierSets(calibration, baseline.Manifest.TaskTiers)
	if err != nil {
		return releaseTierSets{}, fmt.Errorf("invalid calibration artifact: %w", err)
	}
	if len(gate) != noise.GateTaskCount {
		return releaseTierSets{}, fmt.Errorf("noise-floor gate task count is %d; calibration contains %d", noise.GateTaskCount, len(gate))
	}
	if err := validateReleaseRuns(baseline, gate, tripwire, exploratory, noise.ArmRepeats); err != nil {
		return releaseTierSets{}, fmt.Errorf("baseline task evidence: %w", err)
	}
	if err := validateReleaseRuns(candidate, gate, tripwire, exploratory, noise.ArmRepeats); err != nil {
		return releaseTierSets{}, fmt.Errorf("candidate task evidence: %w", err)
	}
	return releaseTierSets{gate: gate, tripwire: tripwire, exploratory: exploratory}, nil
}

func validateReleaseJournal(label string, journal Journal) error {
	if journal.Manifest.Untrustworthy {
		return fmt.Errorf("%s journal is untrustworthy: %s", label, journal.Manifest.UntrustworthyReason)
	}
	if journal.Manifest.ArmPartial {
		return fmt.Errorf("%s journal has a partial arm key", label)
	}
	if journal.Manifest.ArmKey == "" || journal.Manifest.ArmKey != journal.Manifest.Arm.Key() {
		return fmt.Errorf("%s journal arm key does not match its observed arm fields", label)
	}
	if err := journal.Manifest.PreRegistration.Validate(); err != nil {
		return fmt.Errorf("%s pre-registration is invalid: %w", label, err)
	}
	if !journal.Manifest.PreRegistration.ReleaseGateEnabled() {
		return fmt.Errorf("%s pre-registration does not pin release gate artifacts", label)
	}
	preHash, err := journal.Manifest.PreRegistration.Hash()
	if err != nil || journal.Manifest.PreRegistrationHash == "" || journal.Manifest.PreRegistrationHash != preHash {
		return fmt.Errorf("%s pre-registration hash is missing or does not match its contents", label)
	}
	return nil
}

// tripwireContractUnmet reports whether a tripwire repeat declared a contract
// and failed to meet it, with the reason.
//
// A tripwire asks "did we break this workflow outright", and TASK STATUS cannot
// answer that for any workflow with a recovery path. Measured 2026-08-18:
// dev-pipeline routes test's on_fail to recover-checkpoint, which parks the
// work and exits via a COMPLETED terminal, so three repeats read 3/3 TRIPWIRE
// OK while the scorer recorded missing_verifier_step (7 pinned cases) twice and
// no_pinned_cases once. The workflow's verification step had failed every
// attempt.
//
// Where the workflow declares a contract, the contract is the signal. Where it
// declares none the score is not_applicable and task status remains the only
// thing to assert — which is most tripwires, so this must not fail them.
//
// The tier is binary by construction (validateCalibrationTier demands
// passed == attempts), so a partially met contract is not a pass either.
func tripwireContractUnmet(j Journal, taskID string, repeat int) (string, bool) {
	for _, score := range j.TaskScores {
		if score.TaskID != taskID || score.Repeat != repeat {
			continue
		}
		if score.Status == quality.ScoreStatusNotApplicable {
			return "", false // nothing declared; task status is the signal
		}
		if score.Status == quality.ScoreStatusScored && score.Score >= 1 {
			return "", false
		}
		reason := string(score.Status)
		if score.Diagnostic != "" {
			reason += " (" + score.Diagnostic + ")"
		}
		return reason, true
	}
	return "", false
}

func releaseTripwireDecision(baseline, candidate Journal, tripwireIDs map[string]bool) *ReleaseGateDecision {
	for _, run := range baseline.TaskRuns {
		if !tripwireIDs[run.TaskID] {
			continue
		}
		if !run.Succeeded {
			return &ReleaseGateDecision{Status: GateStatusRefused,
				Reason: fmt.Sprintf("baseline tripwire %q failed repeat %d; the comparison environment is not healthy", run.TaskID, run.Repeat)}
		}
		if why, unmet := tripwireContractUnmet(baseline, run.TaskID, run.Repeat); unmet {
			return &ReleaseGateDecision{Status: GateStatusRefused,
				Reason: fmt.Sprintf("baseline tripwire %q repeat %d reached a terminal but left its declared contract unmet: %s; the comparison environment is not healthy", run.TaskID, run.Repeat, why)}
		}
	}
	for _, run := range candidate.TaskRuns {
		if !tripwireIDs[run.TaskID] {
			continue
		}
		if run.Succeeded {
			if why, unmet := tripwireContractUnmet(candidate, run.TaskID, run.Repeat); unmet {
				return &ReleaseGateDecision{Status: GateStatusFail,
					Reason: fmt.Sprintf("candidate tripwire %q repeat %d reached a terminal but left its declared contract unmet: %s", run.TaskID, run.Repeat, why)}
			}
			continue
		}
		class := ClassifyFailure(false, run.ErrorText)
		if class == FailureHarness || class == FailureInfra {
			return &ReleaseGateDecision{Status: GateStatusRefused,
				Reason: fmt.Sprintf("candidate tripwire %q repeat %d is %s failure", run.TaskID, run.Repeat, class)}
		}
		return &ReleaseGateDecision{Status: GateStatusFail,
			Reason: fmt.Sprintf("candidate tripwire %q failed repeat %d", run.TaskID, run.Repeat)}
	}
	return nil
}

func validateReleaseArtifacts(a, b Journal, calibration CalibrationManifest, noise NoiseFloorManifest, policy ReleaseGatePolicy) error {
	if err := validateArtifactProvenance(calibration, noise); err != nil {
		return err
	}
	preA, preB := a.Manifest.PreRegistration, b.Manifest.PreRegistration
	if preA.CalibrationSHA256 != calibration.SHA256() || preB.CalibrationSHA256 != calibration.SHA256() {
		return fmt.Errorf("calibration sha256 does not match both pre-registrations")
	}
	if preA.NoiseFloorSHA256 != noise.SHA256() || preB.NoiseFloorSHA256 != noise.SHA256() {
		return fmt.Errorf("noise-floor sha256 does not match both pre-registrations")
	}
	if preA.ReleaseGatePolicySHA256 != policy.SHA256() || preB.ReleaseGatePolicySHA256 != policy.SHA256() {
		return fmt.Errorf("release-policy sha256 does not match both pre-registrations")
	}
	if preA.Metric != policy.Metric || preB.Metric != policy.Metric ||
		preA.TargetDelta != policy.MaxScoreRegression || preB.TargetDelta != policy.MaxScoreRegression {
		return fmt.Errorf("metric or score threshold differs from the committed policy")
	}
	if preA.SigmaD != noise.SigmaD || preB.SigmaD != noise.SigmaD ||
		preA.SigmaN != noise.SigmaN || preB.SigmaN != noise.SigmaN {
		return fmt.Errorf("pre-registered sigma differs from noise-floor artifact")
	}
	required, err := RequiredPairs(noise.SigmaD, policy.MaxScoreRegression)
	if err != nil {
		return err
	}
	if preA.ComputedPairs != required || preB.ComputedPairs != required {
		return fmt.Errorf("pre-registered pair count differs from artifact-derived requirement")
	}
	for _, item := range []struct {
		label string
		arm   ArmFields
	}{{label: "baseline", arm: a.Manifest.Arm}, {label: "candidate", arm: b.Manifest.Arm}} {
		label, arm := item.label, item.arm
		if arm.HarnessVersion != calibration.HarnessVersion || arm.HarnessVersion != noise.HarnessVersion ||
			arm.TaskSetSHA256 != calibration.TaskSetSHA256 || arm.TaskSetSHA256 != noise.TaskSetSHA256 ||
			arm.TierPolicySHA256 != calibration.TierPolicySHA256 || arm.TierPolicySHA256 != noise.TierPolicySHA256 ||
			arm.ScoringPolicySHA256 != calibration.ScoringPolicySHA256 || arm.ScoringPolicySHA256 != noise.ScoringPolicySHA256 {
			return fmt.Errorf("%s arm identity differs from calibration or noise-floor identity", label)
		}
		if noise.SourceArmKey == "" || noise.SourceArmKey != noise.SourceArm.Key() || noise.SourceArm.Partial() {
			return fmt.Errorf("noise-floor source arm provenance is missing or inconsistent")
		}
		if err := CheckComparableExcept(noise.SourceArm, arm, []string{"binary_sha256", "agent_images"}); err != nil {
			return fmt.Errorf("%s arm differs from noise-floor source configuration: %w", label, err)
		}
	}
	if noise.Metric != policy.Metric || noise.SigmaN < MinimumSigmaRuns || noise.ArmRepeats < MinimumSigmaRuns || noise.SigmaN != noise.ArmRepeats {
		return fmt.Errorf("noise-floor metric or repeat provenance is invalid")
	}
	return nil
}

func validateArtifactProvenance(calibration CalibrationManifest, noise NoiseFloorManifest) error {
	if calibration.SourceRunID == "" || calibration.SourceArmKey == "" || !validSHA256(calibration.SourceJournalSHA256) {
		return fmt.Errorf("calibration source provenance is missing or invalid")
	}
	if len(noise.SourceRunIDs) != 2 || len(noise.SourceJournalSHA256) != 2 ||
		noise.SourceRunIDs[0] == "" || noise.SourceRunIDs[1] == "" ||
		!validSHA256(noise.SourceJournalSHA256[0]) || !validSHA256(noise.SourceJournalSHA256[1]) {
		return fmt.Errorf("noise-floor source provenance is missing or invalid")
	}
	return nil
}

func calibratedTierSets(calibration CalibrationManifest, tiers map[string]TaskTier) (map[string]bool, map[string]bool, map[string]bool, error) {
	gate, tripwire, exploratory := map[string]bool{}, map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	minimumAttempts := 0
	for _, task := range calibration.Tasks {
		if task.TaskID == "" || seen[task.TaskID] || tiers[task.TaskID] != task.Tier {
			return nil, nil, nil, fmt.Errorf("task %q is duplicate or has a tier mismatch", task.TaskID)
		}
		seen[task.TaskID] = true
		if task.Attempts < 3 || task.Passed < 0 || task.Passed > task.Attempts ||
			math.Abs(task.PassRate-float64(task.Passed)/float64(task.Attempts)) > 1e-12 {
			return nil, nil, nil, fmt.Errorf("task %q has invalid calibration counts", task.TaskID)
		}
		if minimumAttempts == 0 || task.Attempts < minimumAttempts {
			minimumAttempts = task.Attempts
		}
		switch task.Tier {
		case TaskTierGate:
			if task.Passed == 0 || task.Passed == task.Attempts {
				return nil, nil, nil, fmt.Errorf("gate task %q is not discriminating", task.TaskID)
			}
			gate[task.TaskID] = true
		case TaskTierTripwire:
			if task.Passed != task.Attempts {
				return nil, nil, nil, fmt.Errorf("tripwire task %q did not pass calibration", task.TaskID)
			}
			tripwire[task.TaskID] = true
		case TaskTierExploratory:
			exploratory[task.TaskID] = true
		default:
			return nil, nil, nil, fmt.Errorf("task %q has invalid tier %q", task.TaskID, task.Tier)
		}
	}
	if len(seen) != len(tiers) {
		return nil, nil, nil, fmt.Errorf("calibration task set differs from journal task set")
	}
	if calibration.MinimumAttempts != minimumAttempts {
		return nil, nil, nil, fmt.Errorf("calibration minimumAttempts=%d does not match task evidence minimum=%d",
			calibration.MinimumAttempts, minimumAttempts)
	}
	return gate, tripwire, exploratory, nil
}

func validateReleaseRuns(j Journal, gate, tripwire, exploratory map[string]bool, repeats int) error {
	want := map[string]bool{}
	for id := range gate {
		want[id] = true
	}
	for id := range tripwire {
		want[id] = true
	}
	for id := range exploratory {
		want[id] = true
	}
	seen := map[string]map[int]bool{}
	for _, run := range j.TaskRuns {
		if !want[run.TaskID] || run.Repeat < 1 || run.Repeat > repeats {
			return fmt.Errorf("unexpected task/repeat %q/%d", run.TaskID, run.Repeat)
		}
		if len(run.ExecutionIDs) == 0 {
			return fmt.Errorf("task %q repeat %d has no execution provenance", run.TaskID, run.Repeat)
		}
		if seen[run.TaskID] == nil {
			seen[run.TaskID] = map[int]bool{}
		}
		if seen[run.TaskID][run.Repeat] {
			return fmt.Errorf("duplicate task/repeat %q/%d", run.TaskID, run.Repeat)
		}
		seen[run.TaskID][run.Repeat] = true
		if !run.Succeeded {
			class := ClassifyFailure(false, run.ErrorText)
			if class == FailureHarness || class == FailureInfra {
				return fmt.Errorf("task %q repeat %d is %s failure", run.TaskID, run.Repeat, class)
			}
		}
	}
	for id := range want {
		for repeat := 1; repeat <= repeats; repeat++ {
			if !seen[id][repeat] {
				return fmt.Errorf("task %q is missing repeat %d", id, repeat)
			}
		}
	}
	return nil
}

func filterTaskScores(scores []TaskScore, ids map[string]bool) []TaskScore {
	out := make([]TaskScore, 0, len(scores))
	for _, score := range scores {
		if ids[score.TaskID] {
			out = append(out, score)
		}
	}
	return out
}

func validateGateScores(j Journal, gateIDs map[string]bool, repeats int) error {
	scores, err := taskRepeatScores(filterTaskScores(j.TaskScores, gateIDs), quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		return err
	}
	if len(scores) != len(gateIDs) {
		return fmt.Errorf("scored gate task set has %d tasks; expected %d", len(scores), len(gateIDs))
	}
	for id := range gateIDs {
		byRepeat := scores[id]
		if len(byRepeat) != repeats {
			return fmt.Errorf("gate task %q has %d scores; expected %d", id, len(byRepeat), repeats)
		}
		for repeat := 1; repeat <= repeats; repeat++ {
			if _, ok := byRepeat[repeat]; !ok {
				return fmt.Errorf("gate task %q is missing score repeat %d", id, repeat)
			}
		}
	}
	return nil
}

func exploratoryComparison(a, b Journal, ids map[string]bool, sigmaD float64) *ExploratoryScore {
	if len(ids) == 0 {
		return nil
	}
	comparison, err := CompareTaskScores(filterTaskScores(a.TaskScores, ids), filterTaskScores(b.TaskScores, ids), quality.ScoreKindPinnedCaseValidation)
	if err != nil {
		return &ExploratoryScore{Inconclusive: true}
	}
	floor, err := ResolvableDelta(sigmaD, comparison.PairCount)
	if err != nil {
		return &ExploratoryScore{MeanDelta: comparison.MeanDelta, PairCount: comparison.PairCount, Inconclusive: true}
	}
	return &ExploratoryScore{MeanDelta: comparison.MeanDelta, PairCount: comparison.PairCount, ResolvableFloor: floor,
		Inconclusive: math.Abs(comparison.MeanDelta) < floor}
}

func compareGateStepHealth(a, b Journal, gateIDs map[string]bool) (GateStepResult, error) {
	at, an, ac, err := gateStepTotals(a, gateIDs)
	if err != nil {
		return GateStepResult{}, fmt.Errorf("baseline: %w", err)
	}
	bt, bn, bc, err := gateStepTotals(b, gateIDs)
	if err != nil {
		return GateStepResult{}, fmt.Errorf("candidate: %w", err)
	}
	if at <= 0 || bt <= 0 {
		return GateStepResult{}, fmt.Errorf("both arms need terminal gate-step verdicts")
	}
	ar, br := float64(an)/float64(at), float64(bn)/float64(bt)
	return GateStepResult{BaselineTerminal: at, CandidateTerminal: bt, BaselineNoOutput: an,
		CandidateNoOutput: bn, BaselineNoOutputRate: ar, CandidateNoOutputRate: br,
		RateDelta: br - ar, BaselineErrorClasses: ac, CandidateErrorClasses: bc}, nil
}

func gateStepTotals(j Journal, gateIDs map[string]bool) (int, int, map[string]int, error) {
	terminal, noOutput, classes := 0, 0, map[string]int{}
	expected := map[string]bool{}
	for _, run := range j.TaskRuns {
		if !gateIDs[run.TaskID] {
			continue
		}
		for _, id := range run.ExecutionIDs {
			expected[id] = true
		}
	}
	observed := map[string]bool{}
	for _, record := range j.Records {
		if !gateIDs[record.TaskID] {
			continue
		}
		for _, verdict := range record.Verdicts {
			if verdict.Schema == nil {
				continue
			}
			observed[record.ExecutionID] = true
			terminal += verdict.Schema.Terminal
			noOutput += verdict.Schema.NoOutput
			for class, n := range verdict.Schema.NoOutputByErrorClass {
				classes[class] += n
			}
		}
	}
	for id := range expected {
		if !observed[id] {
			return 0, 0, nil, fmt.Errorf("gate execution %q has no journaled schema verdict", id)
		}
	}
	return terminal, noOutput, classes, nil
}

// Validate checks bounded thresholds and the only currently supported graded metric.
func (p ReleaseGatePolicy) Validate() error {
	if p.Metric != PinnedCaseValidationMetric {
		return fmt.Errorf("release gate metric must be %q", PinnedCaseValidationMetric)
	}
	for name, value := range map[string]float64{
		"maxScoreRegression":          p.MaxScoreRegression,
		"maxStepNoOutputRateIncrease": p.MaxStepNoOutputRateIncrease,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return fmt.Errorf("%s must be a finite fraction in [0,1]", name)
		}
	}
	seen := map[string]bool{}
	for _, class := range p.ForbiddenNewErrorClasses {
		trimmed := strings.TrimSpace(class)
		if trimmed == "" {
			return fmt.Errorf("forbidden error class is empty")
		}
		if trimmed != class {
			return fmt.Errorf("forbidden error class %q has surrounding whitespace", class)
		}
		if seen[class] {
			return fmt.Errorf("forbidden error class %q appears twice", class)
		}
		seen[class] = true
	}
	return nil
}

// SHA256 is order-independent for the forbidden-class set.
func (p ReleaseGatePolicy) SHA256() string {
	p.ForbiddenNewErrorClasses = sortedCopy(p.ForbiddenNewErrorClasses)
	return digestJSON(p)
}

func digestJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
