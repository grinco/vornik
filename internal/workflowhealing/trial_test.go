package workflowhealing

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// ---- fakes -------------------------------------------------------------

// fakeCandidateRepo is an in-memory WorkflowHealingCandidateRepository
// for the trial-runner tests. No DB, no live executions.
type fakeCandidateRepo struct {
	mu            sync.Mutex
	rows          map[string]*persistence.HealingCandidate
	statuses      []persistence.HealingCandidateStatus // status transition log
	setErr        error                                // forced SetStatus error
	getErr        error                                // forced Get error (non-NotFound)
	promoteErr    error                                // forced Promote error
	beginErr      error                                // forced BeginTrial error
	beginConflict bool                                 // force BeginTrial to lose the claim (won=false)
}

func newFakeCandidateRepo() *fakeCandidateRepo {
	return &fakeCandidateRepo{rows: map[string]*persistence.HealingCandidate{}}
}

func (f *fakeCandidateRepo) put(c *persistence.HealingCandidate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *c
	f.rows[c.ID] = &cp
}

func (f *fakeCandidateRepo) Insert(ctx context.Context, c *persistence.HealingCandidate) error {
	if c.ID == "" {
		c.ID = persistence.GenerateID("whc")
	}
	f.put(c)
	return nil
}

func (f *fakeCandidateRepo) Get(ctx context.Context, id string) (*persistence.HealingCandidate, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeCandidateRepo) List(ctx context.Context, filter persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	return nil, nil
}

func (f *fakeCandidateRepo) SetStatus(ctx context.Context, id string, status persistence.HealingCandidateStatus) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = status
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeCandidateRepo) BeginTrial(ctx context.Context, id string) (bool, error) {
	if f.beginErr != nil {
		return false, f.beginErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return false, nil
	}
	if f.beginConflict || c.Status.IsTerminal() || c.Status == persistence.HealingCandidateTrialRunning {
		return false, nil
	}
	c.Status = persistence.HealingCandidateTrialRunning
	f.statuses = append(f.statuses, persistence.HealingCandidateTrialRunning)
	return true, nil
}

func (f *fakeCandidateRepo) Promote(ctx context.Context, id, promotedBy string) error {
	if f.promoteErr != nil {
		return f.promoteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = persistence.HealingCandidatePromoted
	c.PromotedBy = promotedBy
	f.statuses = append(f.statuses, persistence.HealingCandidatePromoted)
	return nil
}

func (f *fakeCandidateRepo) Reject(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	c.Status = persistence.HealingCandidateRejected
	f.statuses = append(f.statuses, persistence.HealingCandidateRejected)
	return nil
}

func (f *fakeCandidateRepo) lastStatus() persistence.HealingCandidateStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.statuses) == 0 {
		return ""
	}
	return f.statuses[len(f.statuses)-1]
}

// fakeTrialRepo is an in-memory WorkflowHealingTrialRepository.
type fakeTrialRepo struct {
	mu         sync.Mutex
	rows       map[string]*persistence.HealingTrial
	insertErr  error
	finishErr  error
	listErr    error
	finishSeen *persistence.HealingTrial // last Finish'd row snapshot
}

func newFakeTrialRepo() *fakeTrialRepo {
	return &fakeTrialRepo{rows: map[string]*persistence.HealingTrial{}}
}

func (f *fakeTrialRepo) Insert(ctx context.Context, tr *persistence.HealingTrial) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	if tr.ID == "" {
		tr.ID = persistence.GenerateID("wht")
	}
	if tr.Verdict == "" {
		tr.Verdict = persistence.HealingTrialPending
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *tr
	f.rows[tr.ID] = &cp
	return nil
}

func (f *fakeTrialRepo) Get(ctx context.Context, id string) (*persistence.HealingTrial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *tr
	return &cp, nil
}

func (f *fakeTrialRepo) ListByCandidate(ctx context.Context, candidateID string) ([]*persistence.HealingTrial, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.HealingTrial
	for _, tr := range f.rows {
		if tr.CandidateID != candidateID {
			continue
		}
		cp := *tr
		out = append(out, &cp)
	}
	// Newest first, mirroring the repo contract.
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

func (f *fakeTrialRepo) Finish(ctx context.Context, id string, verdict persistence.HealingTrialVerdict, baselineSummary, candidateSummary, scorecard string) error {
	if f.finishErr != nil {
		return f.finishErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	tr, ok := f.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	tr.Verdict = verdict
	tr.BaselineSummary = baselineSummary
	tr.CandidateSummary = candidateSummary
	tr.Scorecard = scorecard
	now := time.Now().UTC()
	tr.FinishedAt = &now
	cp := *tr
	f.finishSeen = &cp
	return nil
}

// fakeReplayEngine records calls and serves canned traces. It NEVER
// runs a real execution and NEVER touches the original task.
type fakeReplayEngine struct {
	baseline map[string]*contracts.ExecutionTrace
	// replay maps evidenceTaskID -> the candidate trace to return.
	replay map[string]*contracts.ExecutionTrace
	// replayTaskID maps evidenceTaskID -> the spawned trial task id.
	// Defaults to "trial-<evID>" when absent; set to the evID itself
	// to simulate the (forbidden) original-task-mutation bug.
	replayTaskID map[string]string
	baselineErr  map[string]error
	replayErr    map[string]error

	mu             sync.Mutex
	replayedFor    []string
	candidateWFIDs []string
}

func newFakeReplayEngine() *fakeReplayEngine {
	return &fakeReplayEngine{
		baseline:     map[string]*contracts.ExecutionTrace{},
		replay:       map[string]*contracts.ExecutionTrace{},
		replayTaskID: map[string]string{},
		baselineErr:  map[string]error{},
		replayErr:    map[string]error{},
	}
}

func (f *fakeReplayEngine) BaselineTrace(_ context.Context, evID string) (*contracts.ExecutionTrace, error) {
	if e := f.baselineErr[evID]; e != nil {
		return nil, e
	}
	return f.baseline[evID], nil
}

func (f *fakeReplayEngine) ReplayEvidence(_ context.Context, evID, candidateWorkflowID string) (string, *contracts.ExecutionTrace, error) {
	f.mu.Lock()
	f.replayedFor = append(f.replayedFor, evID)
	f.candidateWFIDs = append(f.candidateWFIDs, candidateWorkflowID)
	f.mu.Unlock()
	if e := f.replayErr[evID]; e != nil {
		return "", nil, e
	}
	tid := f.replayTaskID[evID]
	if tid == "" {
		tid = "trial-" + evID
	}
	return tid, f.replay[evID], nil
}

// ---- trace builders ----------------------------------------------------

func traceWith(taskID, status string, costUSD, durSeconds float64, events ...contracts.ExecutionEvent) contracts.ExecutionTrace {
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	return contracts.ExecutionTrace{
		TaskID:      taskID,
		Status:      status,
		StartedAt:   start,
		CompletedAt: start.Add(time.Duration(durSeconds * float64(time.Second))),
		Counts:      contracts.TraceCounts{TotalCostUSD: costUSD},
		Events:      events,
	}
}

func judgeEvent(verdict string) contracts.ExecutionEvent {
	fail := verdict == "fail" || verdict == "hallucination" || verdict == "hallucinated" || verdict == "ungrounded" || verdict == "unsupported"
	return contracts.ExecutionEvent{Kind: eventKindJudgeVerdict, Verdict: verdict, Hallucination: fail}
}

// ---- candidate fixtures ------------------------------------------------

// validCandidateMD is a candidate genome (full WORKFLOW.md) that
// validates clean — used for the static-pass test. It reuses the
// recipe fixture shape.
const validCandidateMD = recipeWorkflowMD

func seedCandidate(t *testing.T, repo *fakeCandidateRepo, status persistence.HealingCandidateStatus, diff string, genomeHash string) *persistence.HealingCandidate {
	t.Helper()
	c := &persistence.HealingCandidate{
		ID:                  persistence.GenerateID("whc"),
		WorkflowID:          "dev-pipeline",
		ProjectID:           "proj",
		TriggerID:           "trg",
		ProposalID:          "wpr_1",
		Status:              status,
		ProposalDiff:        diff,
		CandidateGenomeHash: genomeHash,
		RiskLevel:           persistence.HealingRiskLow,
	}
	repo.put(c)
	return c
}

// fakeRegistrar is a no-op WorkflowRegistrar that records calls so
// replay tests can assert the candidate genome was registered under
// the transient id (and deregistered).
type fakeRegistrar struct {
	mu           sync.Mutex
	registered   map[string]*registry.Workflow
	deregistered []string
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{registered: map[string]*registry.Workflow{}}
}

func (f *fakeRegistrar) RegisterTransient(id string, wf *registry.Workflow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered[id] = wf
	return nil
}

func (f *fakeRegistrar) DeregisterTransient(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregistered = append(f.deregistered, id)
}

// newTestRunner builds a runner wired with a no-op registrar so replay
// trials can route at the candidate genome. Tests that need to assert
// on the registrar build their own via WithRegistrar.
func newTestRunner(cand *fakeCandidateRepo, trials *fakeTrialRepo, eng ReplayEngine, gate GateThresholds, minEv int) *TrialRunner {
	return NewTrialRunner(cand, trials, eng, gate, minEv, zerolog.Nop()).WithRegistrar(newFakeRegistrar())
}

// fakeGateResolver returns canned thresholds and records consultation.
type fakeGateResolver struct {
	thresholds GateThresholds
	calls      int
}

func (f *fakeGateResolver) ResolveForCandidate(_ context.Context, _ *persistence.HealingCandidate) GateThresholds {
	f.calls++
	return f.thresholds
}

// ---- tests -------------------------------------------------------------

// TestRunTrial_StaticPasses: a clean candidate genome passes static
// validation and the candidate advances to trial_passed.
func TestRunTrial_StaticPasses(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	// Compute the genome hash so the static policy check matches.
	h, err := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict = %q, want passed; reasons=%v", res.Verdict, res.Scorecard.Reasons)
	}
	// SAFETY: a static (shape-only) pass must NOT make the candidate
	// promotable. Only a replay-gated pass advances to trial_passed; a
	// static pass leaves it at draft (validated, re-runnable) so an
	// operator can't promote a candidate that bypassed the quantitative
	// gate. The trial row still records the passed static verdict.
	if cands.lastStatus() != persistence.HealingCandidateDraft {
		t.Errorf("candidate status = %q, want draft (static pass is NOT promotable)", cands.lastStatus())
	}
	if trials.finishSeen == nil || trials.finishSeen.Verdict != persistence.HealingTrialPassed {
		t.Error("trial row was not finished with passed verdict")
	}
}

// TestRunTrial_ConcurrentClaimRejected is the hardening regression
// (2026-06-15): when BeginTrial loses the atomic claim (a concurrent
// run-trial already flipped the candidate to trial_running), openTrial
// must abort with ErrTrialAlreadyRunning and NOT insert a duplicate
// trial row.
func TestRunTrial_ConcurrentClaimRejected(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, err := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)
	cands.beginConflict = true // a concurrent opener claimed it first

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil); !errors.Is(err, ErrTrialAlreadyRunning) {
		t.Fatalf("err = %v, want ErrTrialAlreadyRunning", err)
	}
	trials.mu.Lock()
	n := len(trials.rows)
	trials.mu.Unlock()
	if n != 0 {
		t.Errorf("a lost claim must not insert a trial row; got %d", n)
	}
}

// fakeTrialMetrics captures the (mode, verdict) the runner emits.
type fakeTrialMetrics struct {
	mode, verdict string
	calls         int
}

func (m *fakeTrialMetrics) RecordHealingTrial(mode, verdict string, _ float64) {
	m.mode, m.verdict = mode, verdict
	m.calls++
}

// TestRunTrial_EmitsMetric: a completed trial records trials_total via
// the wired metrics seam (the LLD Observability series). Without this
// the counter/histogram would always read zero.
func TestRunTrial_EmitsMetric(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	m := &fakeTrialMetrics{}
	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0).WithMetrics(m)
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil); err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if m.calls != 1 || m.mode != string(persistence.HealingTrialModeStatic) || m.verdict != string(persistence.HealingTrialPassed) {
		t.Errorf("metric = {calls:%d mode:%q verdict:%q}, want {1 static passed}", m.calls, m.mode, m.verdict)
	}
}

// TestRunTrial_StaticFailsOnInvalidGenome: a malformed candidate genome
// FAILS static (not a runner error) with validator reasons.
func TestRunTrial_StaticFailsOnInvalidGenome(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, "this is not a workflow", "")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial returned an error (should be a FAILED verdict): %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed", res.Verdict)
	}
	if len(res.Scorecard.Reasons) == 0 {
		t.Error("expected failure reasons")
	}
	if cands.lastStatus() != persistence.HealingCandidateTrialFailed {
		t.Errorf("candidate status = %q, want trial_failed", cands.lastStatus())
	}
}

// TestRunTrial_StaticFailsOnParseError: a candidate whose diff is not
// parseable WORKFLOW.md (malformed YAML frontmatter) FAILS static with
// a parse reason — distinct from the validation-failure path.
func TestRunTrial_StaticFailsOnParseError(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	// Open the frontmatter fence but feed broken YAML so the parser
	// errors rather than producing an empty-but-valid struct.
	broken := "---\nworkflowId: \"x\"\n  bad: : indent\n: :\n---\n# x\n"
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, broken, "")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial returned an error (should be a FAILED verdict): %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed", res.Verdict)
	}
	joined := ""
	for _, rsn := range res.Scorecard.Reasons {
		joined += rsn
	}
	if joined == "" {
		t.Error("expected a parse-failure reason")
	}
}

// TestRunTrial_StaticFailsOnValidationError: a candidate that PARSES
// cleanly but fails the registry validator (here: a missing
// description) FAILS static with the validator findings as reasons —
// the validation-failure branch distinct from the parse-error branch.
func TestRunTrial_StaticFailsOnValidationError(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	// validWorkflowMD (genome_test.go) has no `description`, which the
	// validator flags as an ERROR — but it parses fine.
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validWorkflowMD, "")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed on validation error", res.Verdict)
	}
	joined := ""
	for _, rsn := range res.Scorecard.Reasons {
		joined += rsn + " "
	}
	if !strings.Contains(joined, "description") {
		t.Errorf("reasons %q should cite the missing description", joined)
	}
}

// TestRunTrial_StaticFailsOnHashMismatch: a candidate whose stored
// genome hash disagrees with its diff fails the policy check.
func TestRunTrial_StaticFailsOnHashMismatch(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "deadbeefdeadbeef")

	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed on hash mismatch", res.Verdict)
	}
}

// TestRunTrial_ReplayCollectsSummaryAndPasses: replay re-runs evidence,
// collects baseline + candidate summaries, and passes the gate when the
// candidate is strictly better.
func TestRunTrial_ReplayCollectsSummaryAndPasses(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	// Two evidence executions. Baseline: one failed, expensive; one ok.
	// Candidate: both succeed, cheaper. Strict improvement → pass.
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict = %q, want passed; reasons=%v", res.Verdict, res.Scorecard.Reasons)
	}
	// Summaries collected for BOTH arms.
	if res.BaselineSummary.Runs != 2 || res.CandidateSummary.Runs != 2 {
		t.Fatalf("runs baseline=%d candidate=%d, want 2/2", res.BaselineSummary.Runs, res.CandidateSummary.Runs)
	}
	if res.BaselineSummary.Successes != 1 {
		t.Errorf("baseline successes = %d, want 1", res.BaselineSummary.Successes)
	}
	if res.CandidateSummary.Successes != 2 {
		t.Errorf("candidate successes = %d, want 2", res.CandidateSummary.Successes)
	}
	if res.Scorecard.SuccessDelta <= 0 {
		t.Errorf("success delta = %f, want positive", res.Scorecard.SuccessDelta)
	}
	if res.Scorecard.CostDeltaPct >= 0 {
		t.Errorf("cost delta pct = %f, want negative (cheaper)", res.Scorecard.CostDeltaPct)
	}
	if len(eng.replayedFor) != 2 {
		t.Errorf("replayed %d evidence, want 2", len(eng.replayedFor))
	}
	// Candidate workflow id must be the transient candidate genome id,
	// NOT the live workflow id.
	for _, wfid := range eng.candidateWFIDs {
		if wfid == "dev-pipeline" {
			t.Error("replay routed at the LIVE workflow id; must use a transient candidate id")
		}
	}
}

// TestRunTrial_Replay_GateResolverOverridesStaticGate: when a gate
// resolver is wired, ITS thresholds drive the verdict, not the runner's
// static gate. A strictly-better candidate that PASSES under defaults is
// failed by a resolver demanding an impossible success uplift.
func TestRunTrial_Replay_GateResolverOverridesStaticGate(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	strict := DefaultGateThresholds()
	strict.SuccessUplift = 5.0 // impossible → must fail the gate
	resolver := &fakeGateResolver{thresholds: strict}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2).WithGateResolver(resolver)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if resolver.calls == 0 {
		t.Error("gate resolver was not consulted")
	}
	if res.Verdict == persistence.HealingTrialPassed {
		t.Errorf("verdict = passed; the resolver's strict threshold should have prevented a pass")
	}
}

// TestRunTrial_ReplayFailsGateOnRegression: candidate regresses success
// → gate fails the trial.
func TestRunTrial_ReplayFailsGateOnRegression(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.10, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 10)
		eng.baseline["ev2"] = &v
	}
	// Candidate makes one of them FAIL → success regression.
	{
		v := traceWith("ev1", "failed", 0.10, 10)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 10)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed", res.Verdict)
	}
	if cands.lastStatus() != persistence.HealingCandidateTrialFailed {
		t.Errorf("candidate status = %q, want trial_failed", cands.lastStatus())
	}
}

// TestRunTrial_ReplayFailsGateOnHallucinationIncrease: even with the
// same success rate, a hallucination increase fails the gate.
func TestRunTrial_ReplayFailsGateOnHallucinationIncrease(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	// Baseline: clean judge verdicts. Candidate: hallucination verdicts.
	{
		v := traceWith("ev1", "completed", 0.10, 10, judgeEvent("grounded"))
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 10, judgeEvent("grounded"))
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 10, judgeEvent("hallucination"))
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 10, judgeEvent("hallucination"))
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict = %q, want failed on hallucination increase", res.Verdict)
	}
	if res.Scorecard.HallucinationDelta <= 0 {
		t.Errorf("hallucination delta = %f, want positive", res.Scorecard.HallucinationDelta)
	}
}

// TestRunTrial_MinEvidenceGate: below the minimum evidence count, a
// replay trial is inconclusive (overfitting suppression) and the
// candidate is NOT advanced to a passed/failed terminal-ish state.
func TestRunTrial_MinEvidenceGate(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "completed", 0.10, 10)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.05, 5)
		eng.replay["ev1"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 3) // require 3
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive", res.Verdict)
	}
	if !res.Scorecard.Inconclusive || res.Scorecard.InconclusiveReason == "" {
		t.Error("scorecard should flag inconclusive with a reason")
	}
	// The engine should not even have been asked to replay (gated before
	// the loop).
	if len(eng.replayedFor) != 0 {
		t.Errorf("replayed %d evidence, want 0 (gated before loop)", len(eng.replayedFor))
	}
	// Candidate reset to draft (re-runnable), not trial_passed/failed.
	if cands.lastStatus() != persistence.HealingCandidateDraft {
		t.Errorf("candidate status = %q, want draft (re-runnable)", cands.lastStatus())
	}
}

// awaitTrialVerdict polls the fake trial repo until the trial reaches a
// non-pending verdict or the deadline passes. Used by the async tests.
func awaitTrialVerdict(t *testing.T, trials *fakeTrialRepo, trialID string, within time.Duration) persistence.HealingTrialVerdict {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		trials.mu.Lock()
		row := trials.rows[trialID]
		var v persistence.HealingTrialVerdict
		if row != nil {
			v = row.Verdict
		}
		trials.mu.Unlock()
		if v != "" && v != persistence.HealingTrialPending {
			return v
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("trial %s never left pending within %s", trialID, within)
	return ""
}

// TestRunTrialAsync_SurvivesCallerCancellation: regression for the
// 2026-06-06 stranded-state bug — the synchronous run-trial path ran
// the whole replay (plus the Finish write and candidate advance) on
// the HTTP request context, so the 120s handler deadline stranded the
// candidate at trial_running with a pending trial row. The async path
// must complete the trial on a DETACHED context even when the caller's
// context is cancelled immediately after the call returns.
func TestRunTrialAsync_SurvivesCallerCancellation(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	ctx, cancel := context.WithCancel(context.Background())
	trial, err := r.RunTrialAsync(ctx, c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	cancel() // caller (HTTP handler) goes away immediately
	if err != nil {
		t.Fatalf("RunTrialAsync: %v", err)
	}
	if trial == nil || trial.ID == "" || trial.Verdict != persistence.HealingTrialPending {
		t.Fatalf("trial = %+v, want a pending row with an id", trial)
	}
	if v := awaitTrialVerdict(t, trials, trial.ID, 5*time.Second); v != persistence.HealingTrialPassed {
		t.Fatalf("async verdict = %q, want passed despite caller cancellation", v)
	}
	// Candidate advanced to trial_passed by the detached goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cands.lastStatus() == persistence.HealingCandidateTrialPassed {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("candidate status = %q, want trial_passed", cands.lastStatus())
}

// TestRunTrialAsync_ConcurrentTrialRejected: a candidate with a live
// pending trial refuses a second run (ErrTrialAlreadyRunning) instead
// of double-spawning replays.
func TestRunTrialAsync_ConcurrentTrialRejected(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateTrialRunning, validCandidateMD, "")
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID:          "wht-live",
		CandidateID: c.ID,
		Mode:        persistence.HealingTrialModeReplay,
		Verdict:     persistence.HealingTrialPending,
		StartedAt:   time.Now().UTC(),
	})
	r := newTestRunner(cands, trials, newFakeReplayEngine(), DefaultGateThresholds(), 2)
	if _, err := r.RunTrialAsync(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"}); !errors.Is(err, ErrTrialAlreadyRunning) {
		t.Fatalf("err = %v, want ErrTrialAlreadyRunning", err)
	}
	// Same guard on the sync path.
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil); !errors.Is(err, ErrTrialAlreadyRunning) {
		t.Fatalf("sync err = %v, want ErrTrialAlreadyRunning", err)
	}
}

// TestRunTrialAsync_StaleRunningReclaimed: a pending trial older than
// the async bound (daemon crash, stranded request) must not lock the
// candidate out forever — the stale row is flipped to errored and the
// new trial proceeds.
func TestRunTrialAsync_StaleRunningReclaimed(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateTrialRunning, validCandidateMD, h)
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		ID:          "wht-stale",
		CandidateID: c.ID,
		Mode:        persistence.HealingTrialModeReplay,
		Verdict:     persistence.HealingTrialPending,
		StartedAt:   time.Now().UTC().Add(-2 * time.Hour),
	})
	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	trial, err := r.RunTrialAsync(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrialAsync: %v", err)
	}
	if v := awaitTrialVerdict(t, trials, trial.ID, 5*time.Second); v != persistence.HealingTrialPassed {
		t.Fatalf("verdict = %q, want passed after stale reclaim", v)
	}
	// The stale row was flipped to errored, not left pending.
	trials.mu.Lock()
	stale := trials.rows["wht-stale"]
	trials.mu.Unlock()
	if stale == nil || stale.Verdict != persistence.HealingTrialErrored {
		t.Errorf("stale trial verdict = %v, want errored", stale)
	}
}

// fakeTriggerSource serves canned HealingTrigger rows for the
// evidence-fallback tests.
type fakeTriggerSource struct {
	rows   map[string]*persistence.HealingTrigger
	getErr error
	calls  int
}

func (f *fakeTriggerSource) Get(ctx context.Context, id string) (*persistence.HealingTrigger, error) {
	f.calls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	t, ok := f.rows[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// TestRunTrial_FallsBackToTriggerEvidence: regression for the
// 2026-06-06 "replay trials always inconclusive" bug. Neither the UI
// nor the default API body carries explicit evidence IDs, so every
// replay trial ran with zero evidence and tripped the min-evidence
// gate — even though the candidate's trigger held the regressing
// execution set the whole time. With a trigger source wired, an empty
// evidence set must fall back to the trigger's evidence_execution_ids.
func TestRunTrial_FallsBackToTriggerEvidence(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	{
		v := traceWith("ev1", "failed", 0.20, 30)
		eng.baseline["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.20, 30)
		eng.baseline["ev2"] = &v
	}
	{
		v := traceWith("ev1", "completed", 0.10, 15)
		eng.replay["ev1"] = &v
	}
	{
		v := traceWith("ev2", "completed", 0.10, 15)
		eng.replay["ev2"] = &v
	}

	trigs := &fakeTriggerSource{rows: map[string]*persistence.HealingTrigger{
		"trg": {ID: "trg", EvidenceExecutionIDs: []string{"ev1", "ev2"}},
	}}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2).WithTriggers(trigs)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict = %q, want passed via trigger-evidence fallback; scorecard=%+v", res.Verdict, res.Scorecard)
	}
	if len(eng.replayedFor) != 2 {
		t.Errorf("replayed %d evidence, want 2 (the trigger's set)", len(eng.replayedFor))
	}
	// The trial row must record the resolved evidence set for the audit.
	trials.mu.Lock()
	defer trials.mu.Unlock()
	if len(trials.rows) != 1 {
		t.Fatalf("trial rows = %d, want 1", len(trials.rows))
	}
	for _, row := range trials.rows {
		if len(row.EvidenceExecutionIDs) != 2 {
			t.Errorf("trial row evidence = %v, want the trigger's [ev1 ev2]", row.EvidenceExecutionIDs)
		}
	}
}

// TestRunTrial_ExplicitEvidenceWinsOverTrigger: caller-supplied IDs are
// an override; the trigger source must not be consulted.
func TestRunTrial_ExplicitEvidenceWinsOverTrigger(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, _ := GenomeHashFromMarkdown([]byte(validCandidateMD), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, h)

	eng := newFakeReplayEngine()
	{
		v := traceWith("evA", "failed", 0.20, 30)
		eng.baseline["evA"] = &v
	}
	{
		v := traceWith("evB", "completed", 0.20, 30)
		eng.baseline["evB"] = &v
	}
	{
		v := traceWith("evA", "completed", 0.10, 15)
		eng.replay["evA"] = &v
	}
	{
		v := traceWith("evB", "completed", 0.10, 15)
		eng.replay["evB"] = &v
	}

	trigs := &fakeTriggerSource{rows: map[string]*persistence.HealingTrigger{
		"trg": {ID: "trg", EvidenceExecutionIDs: []string{"ev1", "ev2"}},
	}}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2).WithTriggers(trigs)
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"evA", "evB"}); err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if trigs.calls != 0 {
		t.Errorf("trigger source consulted %d times, want 0 (explicit IDs override)", trigs.calls)
	}
	if len(eng.replayedFor) != 2 || eng.replayedFor[0] != "evA" {
		t.Errorf("replayed %v, want the explicit [evA evB]", eng.replayedFor)
	}
}

// TestRunTrial_TriggerLookupFailureDegrades: a failing trigger lookup
// must not error the trial — it degrades to the no-evidence path
// (inconclusive), which is re-runnable.
func TestRunTrial_TriggerLookupFailureDegrades(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	trigs := &fakeTriggerSource{getErr: errors.New("db down")}
	r := newTestRunner(cands, trials, newFakeReplayEngine(), DefaultGateThresholds(), 2).WithTriggers(trigs)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive (degraded, re-runnable)", res.Verdict)
	}
	if cands.lastStatus() != persistence.HealingCandidateDraft {
		t.Errorf("candidate status = %q, want draft (re-runnable)", cands.lastStatus())
	}
}

// TestRunTrial_InconclusiveOnStubbedReplay: when a per-run scorecard is
// inconclusive (heavily-stubbed side-effecting tools), the trial is
// inconclusive rather than a false pass/fail.
func TestRunTrial_InconclusiveOnStubbedReplay(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	// A candidate trace whose tool calls are all stubbed by the
	// counterfactual gate → EE adapter sets Inconclusive=true on the trace.
	for _, ev := range []string{"ev1", "ev2"} {
		bt := traceWith(ev, "completed", 0.10, 10)
		eng.baseline[ev] = &bt
		ct := traceWith(ev, "completed", 0.10, 10)
		ct.Inconclusive = true // simulates EE adapter detecting heavily-stubbed replay
		eng.replay[ev] = &ct
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive on stubbed replay", res.Verdict)
	}
	if !res.Scorecard.Inconclusive {
		t.Error("scorecard should be flagged inconclusive")
	}
}

// TestRunTrial_NonProductionNeverMutatesOriginal: if the replay engine
// (buggy) returned the ORIGINAL task id, the runner refuses to count
// that run — protecting the non-production invariant. With both
// evidence runs poisoned this way, the trial is inconclusive (no
// comparable runs) rather than silently scoring a production mutation.
func TestRunTrial_NonProductionNeverMutatesOriginal(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	eng := newFakeReplayEngine()
	for _, ev := range []string{"ev1", "ev2"} {
		ev := ev // capture for closure
		{
			v := traceWith(ev, "completed", 0.10, 10)
			eng.baseline[ev] = &v
		}
		{
			v := traceWith(ev, "completed", 0.05, 5)
			eng.replay[ev] = &v
		}
		// Simulate the forbidden bug: replay returns the original id.
		eng.replayTaskID[ev] = ev
	}

	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	// No comparable runs were counted → inconclusive, NOT passed.
	if res.Verdict != persistence.HealingTrialInconclusive {
		t.Fatalf("verdict = %q, want inconclusive (no run may mutate the original)", res.Verdict)
	}
	if res.CandidateSummary.Runs != 0 {
		t.Errorf("candidate summary runs = %d, want 0 (all poisoned runs rejected)", res.CandidateSummary.Runs)
	}
}

// TestRunTrial_ReplayEngineNotWired: a replay trial with no engine
// errors cleanly (errored verdict), not a panic.
func TestRunTrial_ReplayEngineNotWired(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")

	r := newTestRunner(cands, trials, nil, DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeReplay, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialErrored {
		t.Fatalf("verdict = %q, want errored", res.Verdict)
	}
}

// TestRunTrial_CandidateNotFound: a missing candidate returns
// ErrCandidateNotFound.
func TestRunTrial_CandidateNotFound(t *testing.T) {
	r := newTestRunner(newFakeCandidateRepo(), newFakeTrialRepo(), nil, GateThresholds{}, 0)
	_, err := r.RunTrial(context.Background(), "nope", persistence.HealingTrialModeStatic, nil)
	if !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("err = %v, want ErrCandidateNotFound", err)
	}
}

// TestRunTrial_TerminalCandidateRejected: a promoted/rejected candidate
// cannot be re-tried.
func TestRunTrial_TerminalCandidateRejected(t *testing.T) {
	cands := newFakeCandidateRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidatePromoted, validCandidateMD, "")
	r := newTestRunner(cands, newFakeTrialRepo(), nil, GateThresholds{}, 0)
	_, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if !errors.Is(err, ErrCandidateTerminal) {
		t.Fatalf("err = %v, want ErrCandidateTerminal", err)
	}
}

// TestRunTrial_UnsupportedMode: shadow (and unknown) modes are rejected.
func TestRunTrial_UnsupportedMode(t *testing.T) {
	cands := newFakeCandidateRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")
	r := newTestRunner(cands, newFakeTrialRepo(), nil, GateThresholds{}, 0)
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeShadow, nil); !errors.Is(err, ErrUnsupportedMode) {
		t.Fatalf("err = %v, want ErrUnsupportedMode for shadow", err)
	}
}

// TestRunTrial_InsertErrorSurfaces: a failure to open the trial row is
// surfaced as a runner error (the trial cannot proceed un-recorded).
func TestRunTrial_InsertErrorSurfaces(t *testing.T) {
	cands := newFakeCandidateRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, validCandidateMD, "")
	trials := newFakeTrialRepo()
	trials.insertErr = errors.New("db down")
	r := newTestRunner(cands, trials, nil, GateThresholds{}, 0)
	if _, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil); err == nil {
		t.Fatal("expected an error when the trial row can't be opened")
	}
}
