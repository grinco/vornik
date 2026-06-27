package memetic

import (
	"context"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// TelemetrySource is the narrow interface this package needs from
// the workflow-telemetry layer. The concrete *workflowtelemetry.Service
// satisfies it via a one-line adapter; tests stub the interface
// directly.
type TelemetrySource interface {
	ForWorkflow(ctx context.Context, workflowID string, since time.Time) (*workflowtelemetry.Rollup, error)
}

// WorkflowSource fetches the current YAML for a workflow. Splitting
// it from a generic "filesystem reader" lets tests stub a
// canonical-fixture without touching disk and lets the production
// adapter resolve the configs/workflows/<id>.md path with the same
// logic the registry already uses.
type WorkflowSource interface {
	Load(ctx context.Context, workflowID string) ([]byte, error)
}

// ExecutionLookup answers "does run R belong to workflow W?" — the
// evidence-validity check the design doc requires. Implemented over
// the executions table by the production adapter; tests pass a map
// stub.
type ExecutionLookup interface {
	// BelongsTo returns true iff each executionID in `ids` is an
	// execution of `workflowID`. Returns ([]validIDs, ok) so the
	// caller can both 1) decide whether to reject the proposal,
	// and 2) include the validated subset in the audit record.
	BelongsTo(ctx context.Context, workflowID string, ids []string) (valid []string, allValid bool, err error)
}

// ProposalSink is the narrow write surface. The concrete
// persistence.WorkflowProposalRepository satisfies it.
type ProposalSink interface {
	Insert(ctx context.Context, p *persistence.WorkflowProposal) error
}

// InstinctSource is the narrow read surface the architect uses to
// consult workflow-domain instincts as evidence priors (Consumer B —
// instinct.consumers.architect_priors). nil → the architect runs
// exactly as before (no priors injected), which is the gate-off and
// not-wired behaviour. The concrete persistence.InstinctRepository
// satisfies it; tests pass a fake.
//
// Advisory only: the priors are surfaced in the prompt + cited in the
// proposal's motivation/evidence/confidence. They do NOT widen the
// architect's structural-edit scope (steps / terminals / transitions
// only) and never auto-apply — every proposal still passes the full
// validation pipeline and lands as a pending row for operator review.
type InstinctSource interface {
	List(ctx context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error)
}

// InstinctSink is the narrow write surface used to record a rejected
// proposal as a workflow-domain contradiction instinct (source
// 'architect-reject'), so the confidence model learns to stop
// re-proposing what operators decline. nil → no write-back. The
// concrete persistence.InstinctRepository satisfies it.
type InstinctSink interface {
	Upsert(ctx context.Context, in *persistence.Instinct) (id string, err error)
	AddEvidence(ctx context.Context, ev *persistence.InstinctEvidence) (inserted bool, err error)
}

// ApplicationWriter is the narrow write surface used to log an
// instinct_applications row each time the architect consults a prior on
// a propose turn (review item W2 — the architect-evidence surface of the
// continuous-learning feedback loop, slice 7). nil → no application
// logging (gate off / not wired), which is byte-for-byte the prior
// behaviour. The concrete persistence.InstinctRepository satisfies it.
//
// Recording is strictly best-effort: a write error never fails the
// propose turn. The architect records accepted when a proposal lands and
// rejected when the turn fails after the priors were already loaded.
type ApplicationWriter interface {
	RecordApplication(ctx context.Context, app *persistence.InstinctApplication) error
}

// ArchitectOutput is the JSON shape the LLM is contractually
// required to emit. Strictly typed so a malformed object surfaces
// as a parse error rather than silently dropping fields.
//
// Note on shape: design doc § Slice 2 lists `proposal_diff` (a
// unified-diff string). v1 ships `proposed_yaml` (the full new
// file) instead — easier to validate, easier to render in the
// approval UI. Slice 4 computes the diff at git-commit time. This
// is documented in CHANGELOG and tracked in BACKLOG; revisit when
// proposal sizes outgrow the LLM's emit budget.
type ArchitectOutput struct {
	WorkflowID     string   `json:"workflow_id"`
	ProposedYAML   string   `json:"proposed_yaml"`
	Motivation     string   `json:"motivation"`
	EvidenceRunIDs []string `json:"evidence_run_ids"`
	Confidence     float32  `json:"confidence"`
	// Kind is the structural-edit class (add_step, change_timeout, …
	// — see persistence.WorkflowProposalKind). OPTIONAL on the wire:
	// the parser uses DisallowUnknownFields, so the field must be
	// declared, but the architect prompt isn't yet required to emit
	// it. Empty defaults to the "unspecified" sentinel at the call
	// site. Reliable population is a tracked LLM-output follow-on
	// (mitigation plan §8.5); the persistence + filter + per-class
	// kill switch already consume it.
	Kind string `json:"kind,omitempty"`
}

// Config tunes the architect's behaviour at construction. Zero
// values are NOT acceptable defaults — use DefaultConfig and
// override selectively. Pinning explicit defaults at construction
// avoids drift between docs and code.
type Config struct {
	// MinEvidenceRunIDs is the lower bound on evidence cites.
	// Design doc says 3. Anything below is rejected before SQL.
	MinEvidenceRunIDs int
	// MinConfidence is the lower bound on the LLM's self-reported
	// confidence (0.0 - 1.0). Design doc says 0.6. Below this,
	// the proposal is dropped; operators don't get woken for
	// low-signal suggestions.
	MinConfidence float32
	// Lookback is how far back the telemetry rollup runs. Design
	// doc default: 7 days. Wider windows surface more failure
	// classes but dilute "recent regression" signal.
	Lookback time.Duration
	// SystemPrompt overrides the built-in architect role prompt.
	// Empty → use defaultSystemPrompt. Operators tune this per
	// deployment via the admin config.
	SystemPrompt string
	// MaxOutputBytes caps the LLM's emitted response. JSON parser
	// rejects anything past this — guards against a runaway
	// completion emitting a megabyte of YAML.
	MaxOutputBytes int
	// Paused, when true, short-circuits Propose with
	// ErrArchitectPaused before any LLM call. Operator-controllable
	// kill switch (Slice 5 of the memetic-workflows arc) — wired
	// from the VORNIK_ARCHITECT_PAUSED env / admin config. The
	// admin endpoint maps the sentinel to 503 ARCHITECT_PAUSED so
	// operators see why the propose call short-circuited rather
	// than getting a confusing 500.
	//
	// LEVEL 1 of the three-level kill switch. LEVEL 2 is per-workflow
	// (`architect_enabled: false` in the workflow frontmatter, read
	// at Propose time). LEVEL 3 is per-proposal-class (DisabledKinds).
	Paused bool
	// DisabledKinds is the per-proposal-class kill switch (LEVEL 3).
	// A proposal whose kind is in this set is dropped with
	// ErrProposalKindDisabled before insert — lets an operator accept
	// `change_timeout` proposals while rejecting all `add_step` ones,
	// for instance. Empty = no class is blocked. Keys are
	// persistence.WorkflowProposalKind values (as strings).
	DisabledKinds map[string]bool
}

// DefaultConfig returns the config the design doc pins. Caller
// can override fields after construction.
func DefaultConfig() Config {
	return Config{
		MinEvidenceRunIDs: 3,
		MinConfidence:     0.6,
		Lookback:          7 * 24 * time.Hour,
		SystemPrompt:      "", // resolved to defaultSystemPrompt
		MaxOutputBytes:    256 * 1024,
	}
}
