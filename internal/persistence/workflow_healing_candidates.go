package persistence

import (
	"context"
	"time"
)

// Self-Healing Workflow Genome v1 — candidate + trial ledger.
// The DETECTOR + workflow_healing_triggers shipped under the Black
// Box arc; this file adds the two tables the genome layer needs on
// top: workflow_healing_candidates (a trial-tracking record that
// LINKS to a memetic WorkflowProposal) and workflow_healing_trials
// (one trial run — static or replay — with its scorecard).
//
// See https://docs.vornik.io
// § Data Model. Both tables are Postgres-only in v1 (same Phase B
// discipline as the trigger ledger); the SQLite branch returns a
// stub that signals "unsupported".

// HealingCandidateClass enumerates the structural repair classes a
// candidate can carry. The class is descriptive metadata, not a
// CHECK-constrained enum, so later phases can add classes without a
// schema change. v1 ships the deterministic recipes (retry budget,
// verifier insertion) plus the architect's free-form proposals.
type HealingCandidateClass string

const (
	// HealingCandidateRetryBudget lowers a step's retry budget and
	// adds a failure transition — the deterministic recipe for a
	// retry_loop regression.
	HealingCandidateRetryBudget HealingCandidateClass = "retry_budget"
	// HealingCandidateVerifierInsertion inserts an explicit verifier
	// step ahead of the reviewer — the recipe for a verifier or
	// hallucination regression.
	HealingCandidateVerifierInsertion HealingCandidateClass = "verifier_insertion"
	// HealingCandidateArchitect marks a candidate sourced from the
	// memetic architect rather than a deterministic recipe.
	HealingCandidateArchitect HealingCandidateClass = "architect"
)

// HealingRiskLevel is the operator-facing blast-radius banner on a
// candidate. Descriptive metadata, not CHECK-constrained.
type HealingRiskLevel string

const (
	HealingRiskLow    HealingRiskLevel = "low"
	HealingRiskMedium HealingRiskLevel = "medium"
	HealingRiskHigh   HealingRiskLevel = "high"
)

// HealingCandidateStatus is the candidate lifecycle column. The
// migration CHECK-constrains it to these six values; promotion is
// ALWAYS a manual operator action (no autonomous transition into
// promoted).
type HealingCandidateStatus string

const (
	// HealingCandidateDraft is the initial state — the candidate
	// row exists and links to a WorkflowProposal but no trial has
	// run yet.
	HealingCandidateDraft HealingCandidateStatus = "draft"
	// HealingCandidateTrialRunning means a trial is in flight.
	HealingCandidateTrialRunning HealingCandidateStatus = "trial_running"
	// HealingCandidateTrialPassed means the most recent trial's
	// scorecard cleared the promotion gate — the candidate is
	// eligible for manual promotion.
	HealingCandidateTrialPassed HealingCandidateStatus = "trial_passed"
	// HealingCandidateTrialFailed means the most recent trial did
	// not clear the gate.
	HealingCandidateTrialFailed HealingCandidateStatus = "trial_failed"
	// HealingCandidateRejected means an operator dismissed the
	// candidate without promoting.
	HealingCandidateRejected HealingCandidateStatus = "rejected"
	// HealingCandidatePromoted means an operator promoted the
	// candidate through the memetic apply path (proposal applied).
	HealingCandidatePromoted HealingCandidateStatus = "promoted"
)

// IsTerminal reports whether the candidate is settled — no further
// trial or promotion is expected.
func (s HealingCandidateStatus) IsTerminal() bool {
	return s == HealingCandidateRejected || s == HealingCandidatePromoted
}

// HealingCandidate is one workflow_healing_candidates row. It is a
// trial-tracking record: the proposal content (diff/motivation)
// lives on the linked WorkflowProposal (ProposalID), so this row
// carries only the genome fingerprints and trial lifecycle. The
// ProposalDiff / Motivation / ExpectedEffect fields are denormalised
// copies sourced from the proposal at generation time so the UI can
// render the candidate without a join, but the proposal remains the
// source of truth for the apply path.
type HealingCandidate struct {
	ID                  string
	TriggerID           string
	ProjectID           string
	WorkflowID          string
	ProposalID          string
	BaselineGenomeHash  string
	CandidateGenomeHash string
	CandidateClass      HealingCandidateClass
	ProposalDiff        string
	Motivation          string
	ExpectedEffect      string
	RiskLevel           HealingRiskLevel
	Status              HealingCandidateStatus
	CreatedAt           time.Time
	PromotedAt          *time.Time
	PromotedBy          string
}

// HealingCandidateListFilter drives the operator-facing list query.
type HealingCandidateListFilter struct {
	ProjectID  string
	WorkflowID string
	TriggerID  string
	Status     HealingCandidateStatus
	PageSize   int
}

// HealingTrialMode is the trial execution mode. v1 ships static and
// limited replay; shadow is a later enterprise feature. CHECK-
// constrained in the migration.
type HealingTrialMode string

const (
	// HealingTrialModeStatic validates the candidate workflow shape
	// and policy only — no execution.
	HealingTrialModeStatic HealingTrialMode = "static"
	// HealingTrialModeReplay re-runs selected evidence executions
	// with side-effecting tools blocked (reuses the Black Box
	// counterfactual replay engine).
	HealingTrialModeReplay HealingTrialMode = "replay"
	// HealingTrialModeShadow routes future matching tasks through
	// candidate and baseline in parallel — NOT in v1.
	HealingTrialModeShadow HealingTrialMode = "shadow"
)

// HealingTrialVerdict is the trial outcome. CHECK-constrained in the
// migration. inconclusive surfaces replay-fidelity limitations (the
// scorecard couldn't produce a confident comparison).
type HealingTrialVerdict string

const (
	// HealingTrialPending — the trial row was created but the runner
	// has not produced a verdict yet.
	HealingTrialPending HealingTrialVerdict = "pending"
	// HealingTrialPassed — the scorecard cleared the promotion gate.
	HealingTrialPassed HealingTrialVerdict = "passed"
	// HealingTrialFailed — the scorecard did not clear the gate.
	HealingTrialFailed HealingTrialVerdict = "failed"
	// HealingTrialInconclusive — replay fidelity was too low (too
	// many stubbed side-effecting tools, or insufficient evidence)
	// to render a confident verdict.
	HealingTrialInconclusive HealingTrialVerdict = "inconclusive"
	// HealingTrialErrored — the trial runner itself failed.
	HealingTrialErrored HealingTrialVerdict = "errored"
)

// IsTerminal reports whether a verdict is settled.
func (v HealingTrialVerdict) IsTerminal() bool {
	return v == HealingTrialPassed || v == HealingTrialFailed ||
		v == HealingTrialInconclusive || v == HealingTrialErrored
}

// HealingTrial is one workflow_healing_trials row — a single trial
// run of a candidate against a selected evidence set. The summary
// and scorecard blobs are opaque JSON produced by the trial runner
// (internal/workflowhealing); the persistence layer stores them
// verbatim. EvidenceExecutionIDs is the evidence set the trial ran
// against.
type HealingTrial struct {
	ID                   string
	CandidateID          string
	Mode                 HealingTrialMode
	EvidenceExecutionIDs []string
	// BaselineSummary / CandidateSummary / Scorecard are JSON blobs
	// (TrialSummary / Scorecard from the LLD). Stored as raw JSON
	// text; empty means '{}'.
	BaselineSummary  string
	CandidateSummary string
	Scorecard        string
	Verdict          HealingTrialVerdict
	StartedAt        time.Time
	FinishedAt       *time.Time
}

// WorkflowHealingCandidateRepository persists candidate rows. The
// candidate LINKS to a WorkflowProposal (ProposalID) generated by
// the memetic architect — the repo does not own proposal content.
type WorkflowHealingCandidateRepository interface {
	// Insert writes a new candidate. ID is generated when empty;
	// CreatedAt defaults to now; Status defaults to draft.
	Insert(ctx context.Context, c *HealingCandidate) error

	// Get returns one row by id; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*HealingCandidate, error)

	// List returns rows matching the filter, newest first.
	List(ctx context.Context, filter HealingCandidateListFilter) ([]*HealingCandidate, error)

	// SetStatus moves the candidate to the given non-terminal trial
	// status (trial_running / trial_passed / trial_failed). Returns
	// ErrNotFound on a missing row. Refuses to overwrite a terminal
	// status (promoted/rejected) — those go through Promote/Reject.
	SetStatus(ctx context.Context, id string, status HealingCandidateStatus) error

	// BeginTrial atomically claims a candidate for a trial: it flips the
	// status to trial_running IFF the row is currently trial-eligible
	// (not terminal and not already trial_running). Returns won=true when
	// this caller made the transition, won=false when it didn't (a
	// concurrent opener already claimed it, or the row is terminal /
	// missing). This is the compare-and-set that stops two concurrent
	// run-trial calls from both opening a trial for the same candidate.
	BeginTrial(ctx context.Context, id string) (won bool, err error)

	// Promote stamps status=promoted + promoted_at + promoted_by on
	// a non-terminal row. Returns ErrNotFound on a missing or
	// already-terminal row. The actual workflow apply happens on the
	// memetic path; this only records the operator decision.
	Promote(ctx context.Context, id, promotedBy string) error

	// Reject stamps status=rejected on a non-terminal row. Same
	// not-found contract as Promote.
	Reject(ctx context.Context, id string) error
}

// WorkflowHealingTrialRepository persists trial rows. Each trial
// belongs to one candidate (FK candidate_id).
type WorkflowHealingTrialRepository interface {
	// Insert writes a new trial. ID is generated when empty;
	// StartedAt defaults to now; Verdict defaults to pending.
	Insert(ctx context.Context, tr *HealingTrial) error

	// Get returns one row by id; ErrNotFound when missing.
	Get(ctx context.Context, id string) (*HealingTrial, error)

	// ListByCandidate returns all trials for a candidate, newest
	// first.
	ListByCandidate(ctx context.Context, candidateID string) ([]*HealingTrial, error)

	// Finish stamps the verdict + summaries + scorecard + finished_at
	// on a pending trial. Returns ErrNotFound on a missing row.
	Finish(ctx context.Context, id string, verdict HealingTrialVerdict, baselineSummary, candidateSummary, scorecard string) error
}
