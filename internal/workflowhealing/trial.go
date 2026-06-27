package workflowhealing

// Trial runner — Self-Healing Workflow Genome v1 (LLD § Trial Runner).
//
// A trial evaluates a repair candidate against a selected set of
// evidence executions and produces a verdict the promotion gate can
// read. v1 ships TWO modes (NO shadow):
//
//   - static  → validate the candidate workflow's shape + policy only.
//               No execution. Always available; the cheapest signal.
//   - replay  → re-run each evidence execution with the candidate
//               genome applied, REUSING the Black Box counterfactual
//               replay engine + its side-effect blocking. Trial
//               executions are NON-PRODUCTION: they never unblock,
//               mutate, or complete the original task, and every
//               side-effecting tool is stubbed by the MCP gate.
//
// Safety invariants enforced here (LLD Goals/Non-Goals + Risks):
//   - Replay is a PROMOTION SIGNAL, not proof. When the replay cannot
//     faithfully reproduce an execution (no recorded inputs, the
//     workflow needs side-effecting tools, too few completed runs),
//     the verdict is 'inconclusive' rather than a false pass/fail.
//   - A minimum evidence count is required; below it the trial is
//     inconclusive (overfitting suppression, LLD § Risks).
//   - The runner never promotes anything. Promotion stays a manual
//     operator action; the runner only stamps a verdict + scorecard.
//
// The runner is operator-triggered (the run-trial endpoint, a later
// unit). There is NO background auto-trial loop — any always-on
// behaviour would violate the LLD's "gated + OFF by default" rule.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/contracts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// DefaultMinEvidence is the smallest evidence set a replay trial will
// accept before it can return a confident pass/fail. Below it the
// trial is inconclusive — too few runs to distinguish a real
// improvement from noise (LLD § Risks: "require minimum evidence").
const DefaultMinEvidence = 2

// DefaultAsyncTrialBound is the wall-clock budget for a detached
// (async) trial run: replays of real workflow executions take minutes
// to tens of minutes each, far past any HTTP handler window. It also
// doubles as the staleness threshold for reclaiming a pending trial a
// crashed daemon (or a pre-async stranded request) left behind.
const DefaultAsyncTrialBound = 45 * time.Minute

var (
	// ErrCandidateNotFound is returned when the candidate the trial
	// targets is absent.
	ErrCandidateNotFound = errors.New("workflowhealing: candidate not found")
	// ErrCandidateTerminal is returned when a trial is requested on a
	// candidate that has already been promoted or rejected.
	ErrCandidateTerminal = errors.New("workflowhealing: candidate is in a terminal state")
	// ErrUnsupportedMode guards modes the v1 runner does not execute
	// (shadow, or any unknown mode string).
	ErrUnsupportedMode = errors.New("workflowhealing: unsupported trial mode")
	// ErrTrialAlreadyRunning is returned when a candidate already has a
	// live pending trial — double-spawning replays would race the
	// transient genome registration and double-charge the evidence set.
	ErrTrialAlreadyRunning = errors.New("workflowhealing: a trial is already running for this candidate")
)

// TrialSummary is the per-genome roll-up over an evidence set (LLD
// § Trial Runner). One summary is produced for the baseline runs and
// one for the candidate runs; the scorecard diffs them. Rates are in
// [0,1].
type TrialSummary struct {
	Runs                     int     `json:"runs"`
	Successes                int     `json:"successes"`
	Failures                 int     `json:"failures"`
	AvgCostUSD               float64 `json:"avg_cost_usd"`
	AvgDurationSeconds       float64 `json:"avg_duration_seconds"`
	HallucinationRate        float64 `json:"hallucination_rate"`
	VerifierFailureRate      float64 `json:"verifier_failure_rate"`
	OperatorInterventionRate float64 `json:"operator_intervention_rate"`
}

// SuccessRate is Successes/Runs, 0 when no runs. Helper for the gate.
func (s TrialSummary) SuccessRate() float64 {
	if s.Runs == 0 {
		return 0
	}
	return float64(s.Successes) / float64(s.Runs)
}

// TrialResult is the trial runner's output before persistence. The
// runner stamps these onto a workflow_healing_trials row via
// WorkflowHealingTrialRepository.Finish and moves the candidate to
// trial_passed / trial_failed accordingly (inconclusive/errored leave
// the candidate eligible for a re-run, so they do NOT advance it).
type TrialResult struct {
	Mode             persistence.HealingTrialMode
	Verdict          persistence.HealingTrialVerdict
	BaselineSummary  TrialSummary
	CandidateSummary TrialSummary
	// Scorecard is the operator-facing baseline-vs-candidate diff.
	Scorecard HealingScorecard
}

// ReplayEngine is the narrow seam the trial runner needs from the
// counterfactual replay engine. Production wires this to a thin
// adapter over contracts.HealingApplier (spawn a candidate-genome replay
// of one evidence execution, poll for settlement, assemble the resulting
// trace). Tests supply a fake — no live executions, no real models.
//
// ReplayEvidence runs ONE evidence execution under the candidate
// genome and blocks until the trial task settles, returning the
// assembled candidate trace. It MUST guarantee the trial execution is
// non-production: side-effecting tools blocked/stubbed, and the
// ORIGINAL task untouched (never unblocked/mutated/completed). The
// runner defends the second half: it refuses to count a replay whose
// returned replayTaskID equals the original evidence task ID.
type ReplayEngine interface {
	// BaselineTrace assembles the recorded trace of the original
	// evidence execution (the baseline arm of the comparison). No new
	// task is created.
	BaselineTrace(ctx context.Context, evidenceTaskID string) (*contracts.ExecutionTrace, error)

	// ReplayEvidence spawns a non-production candidate-genome replay of
	// the evidence execution, waits for it to settle, and returns the
	// candidate trace. candidateWorkflowID is the transient workflow ID
	// the candidate genome was registered under. The returned
	// replayTaskID is the spawned trial task (NOT the original) so the
	// runner can assert it differs from the original.
	ReplayEvidence(ctx context.Context, evidenceTaskID, candidateWorkflowID string) (replayTaskID string, candidate *contracts.ExecutionTrace, err error)
}

// HealingScorecard is the LLD's promotion-gate scorecard (LLD
// § Trial Runner "Scorecard"). It is the aggregate baseline-vs-
// candidate verdict, distinct from blackbox.Scorecard (which is a
// single-pair trace diff). Persisted as the trial row's scorecard
// JSON blob.
type HealingScorecard struct {
	SuccessDelta       float64  `json:"success_delta"`
	CostDeltaPct       float64  `json:"cost_delta_pct"`
	LatencyDeltaPct    float64  `json:"latency_delta_pct"`
	HallucinationDelta float64  `json:"hallucination_delta"`
	VerifierDelta      float64  `json:"verifier_delta"`
	RiskLevel          string   `json:"risk_level"`
	Verdict            string   `json:"verdict"`
	Reasons            []string `json:"reasons"`
	// Inconclusive surfaces replay-fidelity limitations (LLD § Risks:
	// "show replay mode limitations in the scorecard"). When true, the
	// deltas above are unreliable and the verdict is inconclusive.
	Inconclusive       bool   `json:"inconclusive"`
	InconclusiveReason string `json:"inconclusive_reason,omitempty"`
}

// TrialRunner evaluates candidates. Constructed once; the deps are
// safe for concurrent use. Metrics is optional (nil-safe).
type TrialRunner struct {
	candidates   persistence.WorkflowHealingCandidateRepository
	trials       persistence.WorkflowHealingTrialRepository
	engine       ReplayEngine
	registrar    WorkflowRegistrar
	gateResolver GateResolver
	triggers     TriggerEvidenceSource
	gate         GateThresholds
	minEvidence  int
	asyncBound   time.Duration
	log          zerolog.Logger
	metrics      TrialMetrics
}

// TriggerEvidenceSource is the narrow seam RunTrial uses to fall back
// to the candidate trigger's evidence_execution_ids when the caller
// supplied none — neither the UI form nor the default API body carries
// explicit evidence IDs, so without this fallback every replay trial
// ran with zero evidence and tripped the min-evidence gate (the
// 2026-06-06 "replay always inconclusive" incident). Optional
// (WithTriggers); persistence.WorkflowHealingTriggerRepository
// satisfies it.
type TriggerEvidenceSource interface {
	Get(ctx context.Context, id string) (*persistence.HealingTrigger, error)
}

// WithTriggers wires the trigger-evidence fallback source and returns
// the runner for chaining (like WithMetrics/WithRegistrar — kept off
// the constructor so existing call sites + tests don't churn).
func (r *TrialRunner) WithTriggers(src TriggerEvidenceSource) *TrialRunner {
	if r != nil {
		r.triggers = src
	}
	return r
}

// WorkflowRegistrar is the narrow seam the replay path uses to make a
// candidate genome resolvable by the dispatcher for the duration of a
// trial: register the parsed candidate *registry.Workflow under the
// transient "<wf>-candidate-<hash>" id, then deregister it. *registry.
// Registry satisfies it. Optional (WithRegistrar): a runner without a
// registrar can only run static trials — a replay then can't route at
// the candidate genome, so it errors cleanly rather than silently
// replaying the baseline.
type WorkflowRegistrar interface {
	RegisterTransient(id string, wf *registry.Workflow) error
	DeregisterTransient(id string)
}

// WithRegistrar wires the transient-workflow registrar used to route
// replay trials at the candidate genome. Kept off the constructor so
// existing call sites + tests don't churn. Returns the runner for
// chaining.
func (r *TrialRunner) WithRegistrar(reg WorkflowRegistrar) *TrialRunner {
	if r != nil {
		r.registrar = reg
	}
	return r
}

// GateResolver sources per-candidate promotion-gate thresholds — e.g.
// from the per-(project, workflow, trigger-class) overrides repo via
// GateThresholdResolver. Optional: a runner without one uses its static
// gate (DefaultGateThresholds unless overridden at construction).
// Implemented in the service layer where the triggers + overrides repos
// are available (the runner deliberately doesn't import them).
type GateResolver interface {
	ResolveForCandidate(ctx context.Context, cand *persistence.HealingCandidate) GateThresholds
}

// WithGateResolver wires per-candidate gate-threshold resolution. Kept
// off the constructor (chaining, like WithMetrics/WithRegistrar) so
// existing call sites + tests don't churn. Returns the runner.
func (r *TrialRunner) WithGateResolver(res GateResolver) *TrialRunner {
	if r != nil {
		r.gateResolver = res
	}
	return r
}

// TrialMetrics is the narrow metrics seam the runner emits to on every
// completed trial. *blackbox.Metrics satisfies it (via its
// RecordHealingTrial method). Nil-safe: a runner with no metrics wired
// simply doesn't emit (the LLD Observability series then read zero,
// which is the unwired-deployment signal).
type TrialMetrics interface {
	RecordHealingTrial(mode, verdict string, durationSeconds float64)
}

// WithMetrics wires the trials_total / trial_duration_seconds emitter
// and returns the runner for chaining. Kept off the constructor so the
// existing call sites (and tests) don't churn.
func (r *TrialRunner) WithMetrics(m TrialMetrics) *TrialRunner {
	if r != nil {
		r.metrics = m
	}
	return r
}

// NewTrialRunner constructs a runner. candidates + trials are
// required; engine may be nil for a static-only runner (replay trials
// then error out cleanly). gate carries the promotion thresholds
// (sourced from the overrides repo by the caller); the zero value
// falls back to DefaultGateThresholds. minEvidence <= 0 falls back to
// DefaultMinEvidence.
func NewTrialRunner(
	candidates persistence.WorkflowHealingCandidateRepository,
	trials persistence.WorkflowHealingTrialRepository,
	engine ReplayEngine,
	gate GateThresholds,
	minEvidence int,
	log zerolog.Logger,
) *TrialRunner {
	if gate.IsZero() {
		gate = DefaultGateThresholds()
	}
	if minEvidence <= 0 {
		minEvidence = DefaultMinEvidence
	}
	return &TrialRunner{
		candidates:  candidates,
		trials:      trials,
		engine:      engine,
		gate:        gate,
		minEvidence: minEvidence,
		asyncBound:  DefaultAsyncTrialBound,
		log:         log,
	}
}

// RunTrial executes a trial of the given candidate in the given mode
// and persists the result. It:
//
//  1. loads the candidate (404 → ErrCandidateNotFound; terminal →
//     ErrCandidateTerminal);
//  2. opens a pending trial row + flips the candidate to trial_running;
//  3. runs the mode-specific evaluation;
//  4. stamps the verdict + summaries + scorecard on the trial row;
//  5. advances the candidate to trial_passed / trial_failed (only on a
//     confident verdict — inconclusive/errored leave it re-runnable).
//
// evidenceIDs is the evidence execution set to evaluate against. An
// empty set falls back to the candidate trigger's evidence_execution_ids
// when a TriggerEvidenceSource is wired (WithTriggers) — the UI and the
// default API body never carry explicit IDs. It is required for replay;
// for static it is recorded but unused.
func (r *TrialRunner) RunTrial(ctx context.Context, candidateID string, mode persistence.HealingTrialMode, evidenceIDs []string) (*TrialResult, error) {
	cand, trial, evidence, err := r.openTrial(ctx, candidateID, mode, evidenceIDs)
	if err != nil {
		return nil, err
	}
	return r.execute(ctx, cand, trial, evidence)
}

// RunTrialAsync opens the trial synchronously (validation, evidence
// fallback, pending trial row, trial_running flip — so the caller gets
// the trial ID and any open error immediately) and runs the evaluation
// in a DETACHED goroutine bounded by the runner's async budget. Built
// for replay trials: real replays take minutes to tens of minutes per
// evidence execution, far past any HTTP handler window — the
// synchronous path stranded the candidate at trial_running with a
// pending row when the request context expired (2026-06-06).
//
// Still strictly operator-triggered — this is the SAME single trial
// the operator asked for, finishing after the response; it is not a
// background trial loop (LLD non-negotiable #5).
func (r *TrialRunner) RunTrialAsync(ctx context.Context, candidateID string, mode persistence.HealingTrialMode, evidenceIDs []string) (*persistence.HealingTrial, error) {
	cand, trial, evidence, err := r.openTrial(ctx, candidateID, mode, evidenceIDs)
	if err != nil {
		return nil, err
	}
	go func() {
		// WithoutCancel: keep the caller's values (trace/log metadata)
		// but survive its cancellation — the whole point of async.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.asyncBound)
		defer cancel()
		if _, err := r.execute(dctx, cand, trial, evidence); err != nil {
			r.log.Warn().Err(err).
				Str("candidate_id", cand.ID).
				Str("trial_id", trial.ID).
				Msg("workflowhealing: async trial finished with error")
		}
	}()
	return trial, nil
}

// openTrial is the shared synchronous prologue: validates the mode and
// candidate, guards against a concurrent live trial (reclaiming stale
// pending rows), resolves the evidence set, opens the pending trial
// row, and flips the candidate to trial_running.
func (r *TrialRunner) openTrial(ctx context.Context, candidateID string, mode persistence.HealingTrialMode, evidenceIDs []string) (*persistence.HealingCandidate, *persistence.HealingTrial, []string, error) {
	if mode != persistence.HealingTrialModeStatic && mode != persistence.HealingTrialModeReplay {
		return nil, nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedMode, mode)
	}

	cand, err := r.candidates.Get(ctx, candidateID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrCandidateNotFound, candidateID)
		}
		return nil, nil, nil, fmt.Errorf("workflowhealing: load candidate: %w", err)
	}
	if cand.Status.IsTerminal() {
		return nil, nil, nil, fmt.Errorf("%w: %s (status=%s)", ErrCandidateTerminal, candidateID, cand.Status)
	}
	if cand.Status == persistence.HealingCandidateTrialRunning {
		// Already running: reclaimStaleTrial errors if a live trial exists,
		// or reclaims a stranded one and lets us proceed (status stays
		// trial_running).
		if err := r.reclaimStaleTrial(ctx, candidateID); err != nil {
			return nil, nil, nil, err
		}
	} else {
		// Atomically claim the candidate. The CAS flip to trial_running
		// only succeeds from a trial-eligible state, so two concurrent
		// openTrial calls that both observed a non-running status can't
		// both proceed — the loser gets won=false and aborts WITHOUT
		// inserting a duplicate trial. (Hardening 2026-06-15.)
		won, err := r.candidates.BeginTrial(ctx, candidateID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("workflowhealing: claim candidate: %w", err)
		}
		if !won {
			return nil, nil, nil, fmt.Errorf("%w: %s (claimed concurrently)", ErrTrialAlreadyRunning, candidateID)
		}
	}

	// No explicit evidence set → fall back to the trigger's
	// evidence_execution_ids (the regressing runs the detector opened the
	// trigger on). Explicit caller IDs always win. Best-effort: a failed
	// lookup degrades to the no-evidence path (replay → inconclusive,
	// re-runnable) rather than erroring the trial.
	if len(evidenceIDs) == 0 && r.triggers != nil && cand.TriggerID != "" {
		trig, terr := r.triggers.Get(ctx, cand.TriggerID)
		switch {
		case terr != nil:
			r.log.Warn().Err(terr).Str("candidate_id", candidateID).Str("trigger_id", cand.TriggerID).
				Msg("workflowhealing: trigger-evidence fallback lookup failed; running with no evidence")
		case trig != nil && len(trig.EvidenceExecutionIDs) > 0:
			evidenceIDs = append([]string(nil), trig.EvidenceExecutionIDs...)
			r.log.Debug().Str("candidate_id", candidateID).Str("trigger_id", cand.TriggerID).
				Int("evidence", len(evidenceIDs)).
				Msg("workflowhealing: evidence resolved from trigger")
		}
	}

	trial := &persistence.HealingTrial{
		CandidateID:          candidateID,
		Mode:                 mode,
		EvidenceExecutionIDs: append([]string(nil), evidenceIDs...),
		Verdict:              persistence.HealingTrialPending,
	}
	if err := r.trials.Insert(ctx, trial); err != nil {
		return nil, nil, nil, fmt.Errorf("workflowhealing: open trial: %w", err)
	}
	// The candidate is already trial_running here — either it was running
	// (reclaim path) or BeginTrial flipped it as the atomic claim above —
	// so no extra status write is needed.
	return cand, trial, evidenceIDs, nil
}

// reclaimStaleTrial decides whether a trial_running candidate may open
// a new trial. A pending trial younger than the async budget is
// genuinely in flight → ErrTrialAlreadyRunning. A pending trial OLDER
// than the budget is a strand (daemon crash, or the pre-async request-
// context bug) — it is flipped to errored so the audit shows the
// strand, and the new trial proceeds. No pending rows at all means the
// status mirror itself was stale → proceed.
func (r *TrialRunner) reclaimStaleTrial(ctx context.Context, candidateID string) error {
	rows, err := r.trials.ListByCandidate(ctx, candidateID)
	if err != nil {
		// Fail safe: if we can't inspect the trials we must assume one
		// is live rather than double-spawn replays.
		return fmt.Errorf("%w (could not inspect existing trials: %v)", ErrTrialAlreadyRunning, err)
	}
	for _, tr := range rows {
		if tr == nil || tr.Verdict != persistence.HealingTrialPending {
			continue
		}
		if time.Since(tr.StartedAt) < r.asyncBound {
			return fmt.Errorf("%w: trial %s started %s ago", ErrTrialAlreadyRunning, tr.ID, time.Since(tr.StartedAt).Round(time.Second))
		}
		sc := mustJSON(HealingScorecard{
			Verdict: string(persistence.HealingTrialErrored),
			Reasons: []string{"trial stranded in pending past the async budget; reclaimed by a new run-trial"},
		})
		if ferr := r.trials.Finish(ctx, tr.ID, persistence.HealingTrialErrored, "{}", "{}", sc); ferr != nil {
			r.log.Warn().Err(ferr).Str("trial_id", tr.ID).Msg("workflowhealing: could not reclaim stale trial")
		}
	}
	return nil
}

// execute runs the mode-specific evaluation and persists the verdict +
// summaries + scorecard. The persistence finalization runs on a
// context that survives the evaluation context's cancellation — a
// timed-out replay must still record errored/inconclusive rather than
// strand the candidate at trial_running with a pending row (the
// 2026-06-06 stranded-state bug).
func (r *TrialRunner) execute(ctx context.Context, cand *persistence.HealingCandidate, trial *persistence.HealingTrial, evidenceIDs []string) (*TrialResult, error) {
	started := time.Now()
	mode := trial.Mode
	candidateID := cand.ID

	res := &TrialResult{Mode: mode}
	switch mode {
	case persistence.HealingTrialModeStatic:
		r.runStatic(cand, res)
	case persistence.HealingTrialModeReplay:
		r.runReplay(ctx, cand, evidenceIDs, res)
	}

	// Cancel-independent finalization window.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// Persist the verdict + summaries + scorecard. Marshal failures
	// degrade to "{}" rather than aborting — the verdict is the
	// load-bearing field.
	baselineJSON := mustJSON(res.BaselineSummary)
	candidateJSON := mustJSON(res.CandidateSummary)
	scorecardJSON := mustJSON(res.Scorecard)
	if err := r.trials.Finish(pctx, trial.ID, res.Verdict, baselineJSON, candidateJSON, scorecardJSON); err != nil {
		// Still try to free the candidate — a failed Finish must not
		// leave trial_running behind.
		r.advance(pctx, candidateID, persistence.HealingCandidateDraft)
		return res, fmt.Errorf("workflowhealing: finish trial: %w", err)
	}

	// Advance the candidate. Only a REPLAY pass — which cleared the
	// quantitative promotion gate (success/cost/latency/hallucination/
	// verifier) — makes the candidate promotable (trial_passed). A static
	// pass validates shape + policy ONLY; treating it as promotable would
	// let an operator promote a candidate that bypassed the gate
	// entirely (LLD: promotion requires the replay comparison). So a
	// static pass leaves the candidate at draft — validated and
	// re-runnable, but NOT promotable. The trial row still records the
	// passed static verdict for the audit/UI.
	switch {
	case res.Verdict == persistence.HealingTrialPassed && mode == persistence.HealingTrialModeReplay:
		r.advance(pctx, candidateID, persistence.HealingCandidateTrialPassed)
	case res.Verdict == persistence.HealingTrialFailed:
		r.advance(pctx, candidateID, persistence.HealingCandidateTrialFailed)
	default:
		// static pass, inconclusive, or errored: keep the candidate
		// re-runnable (and non-promotable). Reset to draft so the UI
		// doesn't show a stuck trial_running.
		r.advance(pctx, candidateID, persistence.HealingCandidateDraft)
	}

	if r.metrics != nil {
		r.metrics.RecordHealingTrial(string(mode), string(res.Verdict), time.Since(started).Seconds())
	}

	r.log.Info().
		Str("candidate_id", candidateID).
		Str("trial_id", trial.ID).
		Str("mode", string(mode)).
		Str("verdict", string(res.Verdict)).
		Int("evidence", len(evidenceIDs)).
		Msg("workflowhealing: trial complete")

	return res, nil
}

func (r *TrialRunner) advance(ctx context.Context, candidateID string, status persistence.HealingCandidateStatus) {
	if err := r.candidates.SetStatus(ctx, candidateID, status); err != nil {
		r.log.Warn().Err(err).Str("candidate_id", candidateID).Str("status", string(status)).
			Msg("workflowhealing: could not advance candidate status")
	}
}

// runStatic validates the candidate genome's shape + policy. It re-
// parses the candidate's ProposalDiff (the full candidate WORKFLOW.md)
// and runs the registry validator. A genome that parses + validates
// clean PASSES; anything else FAILS with the validator's findings as
// the scorecard reasons. Static never produces inconclusive (it is a
// deterministic check) and never errored (parse/validate failures are
// FAILED verdicts, not runner errors).
func (r *TrialRunner) runStatic(cand *persistence.HealingCandidate, res *TrialResult) {
	sc := HealingScorecard{RiskLevel: string(cand.RiskLevel)}

	wf, err := registry.ParseWorkflowMarkdown([]byte(cand.ProposalDiff), cand.WorkflowID+".md")
	if err != nil {
		sc.Verdict = string(persistence.HealingTrialFailed)
		sc.Reasons = []string{"candidate genome failed to parse: " + err.Error()}
		res.Verdict = persistence.HealingTrialFailed
		res.Scorecard = sc
		return
	}

	report := registry.ValidateWorkflowMarkdown([]byte(cand.ProposalDiff), cand.WorkflowID+".md")
	if report.HasErrors() {
		for _, f := range report.Findings {
			if f.Severity == registry.SeverityError {
				sc.Reasons = append(sc.Reasons, f.Code+": "+f.Message)
			}
		}
		sc.Verdict = string(persistence.HealingTrialFailed)
		res.Verdict = persistence.HealingTrialFailed
		res.Scorecard = sc
		return
	}

	// Policy check: confirm the candidate genome's hash matches the
	// hash denormalised onto the candidate row. A mismatch means the
	// stored diff and the recorded candidate_genome_hash disagree —
	// the candidate is internally inconsistent and must not pass.
	if h := GenomeHash(wf); cand.CandidateGenomeHash != "" && h != cand.CandidateGenomeHash {
		sc.Verdict = string(persistence.HealingTrialFailed)
		sc.Reasons = []string{fmt.Sprintf("candidate genome hash mismatch: diff hashes to %q but row records %q", h, cand.CandidateGenomeHash)}
		res.Verdict = persistence.HealingTrialFailed
		res.Scorecard = sc
		return
	}

	sc.Verdict = string(persistence.HealingTrialPassed)
	sc.Reasons = []string{"candidate workflow validates clean (static shape + policy check passed)"}
	res.Verdict = persistence.HealingTrialPassed
	res.Scorecard = sc
}

// runReplay re-runs each evidence execution under the candidate
// genome, aggregates baseline + candidate summaries, diffs them, and
// applies the promotion gate. Inconclusive is returned when:
//   - fewer than minEvidence evidence IDs were supplied;
//   - the replay engine is not wired;
//   - too few replays completed to compare confidently;
//   - the per-run scorecards flagged the replay as heavily stubbed
//     (low fidelity).
func (r *TrialRunner) runReplay(ctx context.Context, cand *persistence.HealingCandidate, evidenceIDs []string, res *TrialResult) {
	if len(evidenceIDs) < r.minEvidence {
		res.Verdict = persistence.HealingTrialInconclusive
		res.Scorecard = HealingScorecard{
			RiskLevel:          string(cand.RiskLevel),
			Verdict:            string(persistence.HealingTrialInconclusive),
			Inconclusive:       true,
			InconclusiveReason: fmt.Sprintf("only %d evidence execution(s) supplied; need >= %d to compare confidently", len(evidenceIDs), r.minEvidence),
		}
		return
	}
	if r.engine == nil {
		res.Verdict = persistence.HealingTrialErrored
		res.Scorecard = HealingScorecard{
			RiskLevel: string(cand.RiskLevel),
			Verdict:   string(persistence.HealingTrialErrored),
			Reasons:   []string{"replay engine not wired on this deployment"},
		}
		return
	}

	candidateWorkflowID := cand.WorkflowID + "-candidate-" + cand.CandidateGenomeHash

	// Make the candidate genome resolvable by the dispatcher for the
	// duration of this trial so the VariableWorkflow replay routes at
	// it. Parse the stored diff (the full candidate WORKFLOW.md), then
	// register it under the transient id; deregister on the way out.
	// Without a registrar (static-only deployments) a replay can't
	// route at the candidate — fail closed rather than silently
	// replaying the baseline genome.
	if r.registrar == nil {
		res.Verdict = persistence.HealingTrialErrored
		res.Scorecard = HealingScorecard{
			RiskLevel: string(cand.RiskLevel),
			Verdict:   string(persistence.HealingTrialErrored),
			Reasons:   []string{"replay engine wired without a workflow registrar; cannot route at the candidate genome"},
		}
		return
	}
	candWF, perr := registry.ParseWorkflowMarkdown([]byte(cand.ProposalDiff), cand.WorkflowID+".md")
	if perr != nil {
		res.Verdict = persistence.HealingTrialErrored
		res.Scorecard = HealingScorecard{
			RiskLevel: string(cand.RiskLevel),
			Verdict:   string(persistence.HealingTrialErrored),
			Reasons:   []string{"candidate genome failed to parse for replay: " + perr.Error()},
		}
		return
	}
	if rerr := r.registrar.RegisterTransient(candidateWorkflowID, candWF); rerr != nil {
		res.Verdict = persistence.HealingTrialErrored
		res.Scorecard = HealingScorecard{
			RiskLevel: string(cand.RiskLevel),
			Verdict:   string(persistence.HealingTrialErrored),
			Reasons:   []string{"could not register candidate genome for replay: " + rerr.Error()},
		}
		return
	}
	defer r.registrar.DeregisterTransient(candidateWorkflowID)

	var (
		baselineAgg     = newAggregator()
		candidateAgg    = newAggregator()
		anyInconclusive bool
		replayedOK      int
	)

	for _, evID := range evidenceIDs {
		baseTrace, berr := r.engine.BaselineTrace(ctx, evID)
		if berr != nil {
			r.log.Warn().Err(berr).Str("evidence_task_id", evID).Msg("workflowhealing: baseline trace unavailable; skipping evidence")
			continue
		}
		replayTaskID, candTrace, rerr := r.engine.ReplayEvidence(ctx, evID, candidateWorkflowID)
		if rerr != nil {
			r.log.Warn().Err(rerr).Str("evidence_task_id", evID).Msg("workflowhealing: replay failed; skipping evidence")
			continue
		}
		// SAFETY: the replay must target a NEW task, never the original.
		// A runner that returned the original task ID would mean the
		// trial mutated production — fail closed.
		if replayTaskID == "" || replayTaskID == evID {
			r.log.Error().Str("evidence_task_id", evID).Str("replay_task_id", replayTaskID).
				Msg("workflowhealing: replay returned the original task id; refusing to count it (non-production invariant)")
			continue
		}
		if baseTrace == nil || candTrace == nil {
			continue
		}

		baselineAgg.add(baseTrace)
		candidateAgg.add(candTrace)

		// Low-fidelity signal: the EE adapter sets Inconclusive when the
		// replay was heavily stubbed (side-effecting tools blocked by the MCP
		// gate). CE reads the flag directly — no blackbox.Compare needed.
		if candTrace.Inconclusive {
			anyInconclusive = true
		}
		replayedOK++
	}

	res.BaselineSummary = baselineAgg.summary()
	res.CandidateSummary = candidateAgg.summary()

	// Not enough runs actually completed to compare — inconclusive.
	if replayedOK < r.minEvidence {
		res.Verdict = persistence.HealingTrialInconclusive
		res.Scorecard = HealingScorecard{
			RiskLevel:          string(cand.RiskLevel),
			Verdict:            string(persistence.HealingTrialInconclusive),
			Inconclusive:       true,
			InconclusiveReason: fmt.Sprintf("only %d of %d evidence replays produced a comparable trace; need >= %d", replayedOK, len(evidenceIDs), r.minEvidence),
		}
		return
	}

	// Heavily-stubbed replay → low fidelity → inconclusive (LLD § Risks
	// "false confidence from weak replay").
	if anyInconclusive {
		res.Verdict = persistence.HealingTrialInconclusive
		res.Scorecard = HealingScorecard{
			RiskLevel:          string(cand.RiskLevel),
			Verdict:            string(persistence.HealingTrialInconclusive),
			Inconclusive:       true,
			InconclusiveReason: "one or more replays were heavily stubbed (side-effecting tools blocked); deltas are unreliable",
		}
		return
	}

	// Confident comparison: apply the promotion gate. When a per-
	// candidate gate resolver is wired (the overrides-threshold path),
	// it sources the thresholds for this candidate's (project, workflow,
	// trigger-class) tuple; otherwise the runner's static gate is used.
	gate := r.gate
	if r.gateResolver != nil {
		gate = r.gateResolver.ResolveForCandidate(ctx, cand)
	}
	sc := gate.Evaluate(res.BaselineSummary, res.CandidateSummary, string(cand.RiskLevel))
	res.Scorecard = sc
	if sc.Verdict == string(persistence.HealingTrialPassed) {
		res.Verdict = persistence.HealingTrialPassed
	} else {
		res.Verdict = persistence.HealingTrialFailed
	}
}

// aggregator accumulates per-run trace metrics into a TrialSummary.
type aggregator struct {
	runs           int
	successes      int
	failures       int
	costSum        float64
	durationSum    float64
	durationCount  int
	judgeVerdicts  int
	hallucinations int
	verifierSteps  int
	verifierFails  int
	interventions  int
}

func newAggregator() *aggregator { return &aggregator{} }

// event kind constants — stable strings matching blackbox.EventKind*
// so tests that round-trip through the EE adapter see the same values.
// Defined here to avoid importing internal/blackbox.
const (
	eventKindStep         = "step"
	eventKindJudgeVerdict = "judge_verdict"
	eventKindOperatorOp   = "operator_op"
)

// add folds one trace into the running aggregate.
func (a *aggregator) add(t *contracts.ExecutionTrace) {
	if t == nil {
		return
	}
	a.runs++
	// Execution status is stored UPPERCASE in the DB (persistence
	// .ExecutionStatus* = "COMPLETED"/"FAILED"/"CANCELLED") and the
	// EE adapter carries it through verbatim onto ExecutionTrace.Status —
	// so normalise case before matching, or every run scores 0/0.
	switch strings.ToLower(strings.TrimSpace(t.Status)) {
	case "completed", "success":
		a.successes++
	case "failed", "error", "cancelled", "canceled":
		a.failures++
	}
	a.costSum += t.Counts.TotalCostUSD
	if !t.StartedAt.IsZero() && !t.CompletedAt.IsZero() {
		a.durationSum += t.CompletedAt.Sub(t.StartedAt).Seconds()
		a.durationCount++
	}
	for _, e := range t.Events {
		switch e.Kind {
		case eventKindJudgeVerdict:
			a.judgeVerdicts++
			if isHallucinationVerdict(e) {
				a.hallucinations++
			}
		case eventKindStep:
			if isVerifierStep(e) {
				a.verifierSteps++
				if isFailedStep(e) {
					a.verifierFails++
				}
			}
		case eventKindOperatorOp:
			if isIntervention(e) {
				a.interventions++
			}
		}
	}
}

// summary finalises the aggregate into a TrialSummary. Rates are
// computed against the natural denominator (judge verdicts for
// hallucination, verifier steps for verifier failure, runs for
// intervention) and are 0 when the denominator is 0.
func (a *aggregator) summary() TrialSummary {
	s := TrialSummary{
		Runs:      a.runs,
		Successes: a.successes,
		Failures:  a.failures,
	}
	if a.runs > 0 {
		s.AvgCostUSD = a.costSum / float64(a.runs)
		s.OperatorInterventionRate = float64(a.interventions) / float64(a.runs)
	}
	if a.durationCount > 0 {
		s.AvgDurationSeconds = a.durationSum / float64(a.durationCount)
	}
	if a.judgeVerdicts > 0 {
		s.HallucinationRate = float64(a.hallucinations) / float64(a.judgeVerdicts)
	}
	if a.verifierSteps > 0 {
		s.VerifierFailureRate = float64(a.verifierFails) / float64(a.verifierSteps)
	}
	return s
}

// isHallucinationVerdict inspects a judge-verdict event for a failing
// signal. The EE adapter sets ExecutionEvent.Hallucination = true and
// ExecutionEvent.Verdict to the verdict string; this function reads both
// the pre-computed flag (fast path) and the vocabulary-matched string
// (tolerant fallback for older EE builds).
func isHallucinationVerdict(e contracts.ExecutionEvent) bool {
	if e.Hallucination {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(e.Verdict)) {
	case "fail", "hallucination", "hallucinated", "ungrounded", "unsupported":
		return true
	}
	return false
}

// isVerifierStep reports whether a step event was produced by a
// verifier role. The EE adapter sets ExecutionEvent.Role on step events.
func isVerifierStep(e contracts.ExecutionEvent) bool {
	return e.Role == "verifier"
}

// isFailedStep reports whether a step terminal outcome failed. The EE
// adapter sets ExecutionEvent.Outcome to the stepoutcome vocabulary value
// ("ok", "parse_error", "schema_violation", "refused", …). Any non-"ok"
// non-empty outcome is a failure.
func isFailedStep(e contracts.ExecutionEvent) bool {
	oc := strings.ToLower(strings.TrimSpace(e.Outcome))
	return oc != "" && oc != "ok"
}

// isIntervention reports whether an operator-op event is a manual
// intervention (cancel/retry/fork/hint). The EE adapter sets
// ExecutionEvent.Detail to the op value for operator_op events.
func isIntervention(e contracts.ExecutionEvent) bool {
	switch e.Detail {
	case "intervention", "cancel", "retry", "fork", "hint":
		return true
	}
	return false
}

// mustJSON marshals v, falling back to "{}" on error so a marshal
// failure never blocks the load-bearing verdict from being persisted.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
