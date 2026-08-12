// Package llmspend is the single seam through which billed LLM calls reach the
// task_llm_usage ledger.
//
// It exists because "every model call's cost is attributed to a project" was an
// operator requirement that nothing enforced. Three components reached production
// making real provider calls and recording nothing — the instinct distiller (its
// field was assigned only in a test), the memory reranker (no field at all), and
// the embedder (no recording at all on any provider) — and every one was found by
// an operator reading a bill rather than by the test suite. As late as
// 2026-08-12, deleting one line from the service container left `go test ./...`
// green while a component silently stopped billing.
//
// The mechanism is a REQUIRED constructor parameter. A component that can call a
// provider takes a Recorder to be built, so the compiler asks "what happens to
// this component's spend?" at every construction site. That is the same mechanism
// that worked for memory.EmbedScope, where making the parameter required turned
// the compiler into a call-site audit and found three entry points a grep could
// not see.
//
// Design of record: https://docs.vornik.io
//
// Import boundary: only internal/persistence, internal/pricing and stdlib. No
// internal/enterprise (the CE→EE law would fail the build), no internal/chat, no
// internal/memory. Enterprise components import this package; it imports nothing
// edition-specific.
package llmspend

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// UsageRepo is the ledger write surface. An interface so tests can inject a fake
// while Recorder itself stays a value type.
type UsageRepo interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
	// Upsert overwrites the row with the caller's stable ID. Used by the
	// streaming path, where an agent reports cumulative usage per iteration and
	// each report must replace the last rather than add a row.
	Upsert(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// PricingTable turns token counts into dollars. Mirrors *pricing.Table.CostUSD;
// declared locally so this package does not depend on the pricing package's full
// surface.
type PricingTable interface {
	CostUSD(model string, promptTokens, completionTokens int) float64
}

// FailureSink counts ledger-write failures. Satisfied by a Prometheus CounterVec
// with one "source" label. Optional — nil means the warn log is the only signal.
type FailureSink interface {
	Inc(source string)
}

// Recorder carries everything a billed call needs to land a ledger row.
//
// It is a VALUE, not an interface, and that is the whole point: a nil interface is
// precisely the failure mode this package removes. There is no way to hold a
// Recorder that is accidentally absent — an absent recorder must be spelled
// Disabled(), which is greppable, classifiable and visible in review.
//
// The zero value is NOT usable: it is neither enabled nor deliberately disabled,
// so Record on it reports a programming error rather than silently doing nothing.
// Construct with New or Disabled.
type Recorder struct {
	repo    UsageRepo
	pricing PricingTable
	sink    FailureSink
	logger  zerolog.Logger
	// source and role are the ledger's task_llm_usage.source / .role. Held here
	// rather than passed per call so a component cannot report itself
	// inconsistently across its own call sites.
	source string
	role   string
	// state distinguishes the three cases the zero value would otherwise
	// conflate: configured, deliberately disabled, and never constructed.
	state recorderState
}

type recorderState uint8

const (
	// stateUnset is the zero value: never constructed. Record treats it as a
	// programming error, because silently accepting it would reintroduce exactly
	// the "absent recorder does nothing quietly" behaviour this package exists
	// to eliminate.
	stateUnset recorderState = iota
	stateEnabled
	stateDisabled
)

// New returns a Recorder that writes rows for the given source and role.
//
// A nil repo yields a DISABLED recorder rather than a half-built one: some
// deployments legitimately run without a ledger repo, and the alternative
// (an enabled recorder holding nil) is the nil dereference this package exists to
// prevent. The caller's intent is still recorded as "wanted billing", so the
// disabled-path log names the source.
func New(repo UsageRepo, pricing PricingTable, source, role string, opts ...Option) Recorder {
	r := Recorder{
		repo:    repo,
		pricing: pricing,
		source:  source,
		role:    role,
		state:   stateEnabled,
	}
	if repo == nil {
		r.state = stateDisabled
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

// Disabled returns a Recorder that deliberately writes nothing.
//
// Legitimate uses: tests, and production paths whose spend is knowingly
// unattributed (the memetic architect is the one such case today, and the
// chat call-site registry already classifies it).
//
// Calling this in production code is a decision, and slice D of the design adds a
// law requiring every non-test call site to appear in an allowlist with a note.
// That is the difference from a nil recorder: absence is invisible, whereas
// Disabled() is a thing a reviewer can see and a linter can find.
func Disabled() Recorder {
	return Recorder{state: stateDisabled}
}

// Option customises a Recorder at construction.
type Option func(*Recorder)

// WithFailureSink wires the counter incremented when a ledger write fails.
func WithFailureSink(s FailureSink) Option {
	return func(r *Recorder) { r.sink = s }
}

// WithLogger wires the logger used for ledger-write failures.
func WithLogger(l zerolog.Logger) Option {
	return func(r *Recorder) { r.logger = l }
}

// Enabled reports whether this Recorder will write rows. Exposed so a component
// can skip assembling an Input it would only throw away.
func (r Recorder) Enabled() bool { return r.state == stateEnabled }

// Source exposes the ledger source this Recorder writes, for tests and diagnostics.
func (r Recorder) Source() string { return r.source }

// Role exposes the ledger role this Recorder writes, for tests and diagnostics.
func (r Recorder) Role() string { return r.role }

// Input is one billed provider call.
//
// The row's SHAPE stops being each caller's problem — source, role, id
// generation, cost computation and the TaskID-nil convention all live here. What
// remains the caller's responsibility is the CONTENTS: passing the right project,
// the right token counts, and calling Record before the response is judged.
type Input struct {
	// ProjectID is who pays. Empty is allowed only for InfraScope work.
	ProjectID string
	// Model is the provider model id actually used.
	Model string
	// PromptTokens / CompletionTokens are the billed counts. Embedding calls
	// generate no output, so CompletionTokens is zero for them.
	PromptTokens     int
	CompletionTokens int
	// TokensEstimated marks counts DERIVED from text length because the provider
	// reported none (Bedrock Cohere reports none for embeddings). It exists so a
	// bill reconciliation can tell a measurement from an inference; presenting a
	// guess as a measurement is how a ledger stops being evidence.
	TokensEstimated bool
	// TaskID / ExecutionID are nil for spend that is not task-scoped —
	// retrieval, ingest and background workers. A synthetic id is deliberately
	// NOT invented: one could later collide with a real task and corrupt
	// per-task attribution.
	TaskID      *string
	ExecutionID *string
	// StepID names the unit of work when exactly one is responsible (a chunk id
	// for a titling call). Empty when the call spans many, like a rerank over a
	// candidate set or an embed over a batch.
	StepID string
	// CacheCreationTokens / CacheReadTokens carry provider prompt-cache
	// observability where the provider reports it.
	CacheCreationTokens int
	CacheReadTokens     int

	// ---- fields only some callers need ----

	// RoleOverride replaces the Recorder's fixed role for this one call. Empty
	// keeps the Recorder's role, which is the normal case.
	//
	// It exists for the agent streaming path, where the ROLE is reported by the
	// agent per call (a step runs as researcher, coder, reviewer …) rather than
	// being a property of the component doing the recording. A Recorder with a
	// fixed role cannot express that.
	RoleOverride string
	// CostUSD overrides the pricing-table computation. nil means "compute it",
	// which is what every component doing its own provider call wants.
	//
	// Non-nil is for callers who were TOLD the cost rather than deriving it —
	// again the agent path, which reports a figure computed inside the
	// container from its own token accounting.
	CostUSD *float64
	// APIKeyID attributes the row to the credential that authenticated the
	// call. Only the API-facing paths have one.
	APIKeyID *string
	// SessionID ties the row to a chat conversation. Only the dispatcher path
	// has one — a chat turn is billed against a session rather than a task.
	SessionID *string
	// Iterations is the tool-calling loop count for a streamed step. 0 becomes
	// 1, matching every non-streaming caller.
	Iterations int
	// CacheHit marks a stage served from the LLM RESPONSE cache: no provider was
	// reached, so cost and tokens are zero and the row exists only to keep the
	// stage visible rather than vanishing on a hit.
	//
	// It is the one case where a ZERO-TOKEN row is still written. Billing a hit's
	// would-have-been tokens over-reported spend until 2026-08-05; dropping the
	// row instead would hide the stage entirely, which the response-cache design
	// explicitly does not want. So: row written, tokens zero, flag set.
	CacheHit bool
}

// Record writes one ledger row for one billed provider call.
//
// CALL THIS BEFORE JUDGING THE RESPONSE. The provider charged the moment it
// returned, so a row written only on the success path turns every degrade into a
// silent spender. That is not a hypothetical: the KG extractor laundered ~83% of
// its spend by classifying before billing, and the reranker's spend hid inside a
// degrade path that was working exactly as designed. This package cannot enforce
// the ordering — only a per-component test can, by asserting that an unusable
// response still bills — but it is the one place the rule can be stated.
//
// Returns nothing, deliberately. By the time Record runs, the provider has
// already charged: failing the caller would lose the work as well without
// un-charging anything, and there is no pending intent to retry. A dropped row is
// a fidelity loss, not an integrity violation.
//
// What replaces the silence is the failure sink and a warn log. Nineteen call
// sites used to swallow this error with no shared place to count it; now there is
// one, and the counter is alarmed rather than merely exported.
func (r Recorder) Record(ctx context.Context, in Input) {
	switch r.state {
	case stateDisabled:
		return
	case stateUnset:
		// A zero-value Recorder reached a billed call. Loud, because the quiet
		// version of this is the defect the package exists to remove.
		r.logger.Error().
			Str("project_id", in.ProjectID).
			Str("model", in.Model).
			Msg("llmspend: zero-value Recorder used for a billed call — construct with " +
				"llmspend.New or llmspend.Disabled; this call's spend is NOT in the ledger")
		if r.sink != nil {
			r.sink.Inc("__unset__")
		}
		return
	}

	if in.PromptTokens <= 0 && in.CompletionTokens <= 0 && !in.CacheHit {
		// Nothing was billed (or the provider reported nothing and the caller
		// chose not to estimate). An empty row would only pollute the dashboard.
		//
		// A CACHE HIT is the exception and is written anyway: zero tokens there
		// means "served without reaching a provider", which is information, not
		// absence. Skipping it would make a cached stage indistinguishable from
		// one that never ran.
		return
	}

	row := r.row(persistence.GenerateID("llm"), in)
	if err := r.repo.Record(ctx, row); err != nil {
		if r.sink != nil {
			r.sink.Inc(r.source)
		}
		r.logger.Warn().Err(err).
			Str("source", r.source).
			Str("role", r.role).
			Str("project_id", in.ProjectID).
			Msg("llmspend: ledger write failed — this call's spend is not attributed")
	}
}

// Upsert writes a row under the caller's STABLE id, replacing any previous one.
//
// This is the streaming shape: an agent reports cumulative usage per iteration,
// and each report must overwrite the last rather than add a row. It is also what
// gives a cancelled task a cost — the most recent report is already persisted when
// the container is killed, whereas a record-at-finalize path leaves it at $0.
//
// UNLIKE Record, this RETURNS AN ERROR, and the asymmetry is deliberate. Record is
// called after a provider already charged, with nobody to tell and nothing to
// retry. Upsert is called by an HTTP handler whose client is the agent itself: it
// can retry, the existing contract already answers 500 on failure, and swallowing
// the error would silently lose the usage the agent went out of its way to report.
func (r Recorder) Upsert(ctx context.Context, id string, in Input) error {
	if id == "" {
		return fmt.Errorf("llmspend: Upsert requires a stable id")
	}
	switch r.state {
	case stateDisabled:
		return nil
	case stateUnset:
		r.logger.Error().
			Str("project_id", in.ProjectID).
			Msg("llmspend: zero-value Recorder used for a streamed usage report — " +
				"construct with llmspend.New or llmspend.Disabled")
		if r.sink != nil {
			r.sink.Inc("__unset__")
		}
		return fmt.Errorf("llmspend: recorder not configured")
	}
	if err := r.repo.Upsert(ctx, r.row(id, in)); err != nil {
		if r.sink != nil {
			r.sink.Inc(r.source)
		}
		return err
	}
	return nil
}

// row builds the ledger row. One place, so Record and Upsert cannot drift on the
// conventions (role, source, cost, the TaskID-nil rule) that nineteen call sites
// each used to decide for themselves.
func (r Recorder) row(id string, in Input) *persistence.TaskLLMUsage {
	role := r.role
	if in.RoleOverride != "" {
		role = in.RoleOverride
	}
	costUSD := 0.0
	switch {
	case in.CostUSD != nil:
		costUSD = *in.CostUSD
	case r.pricing != nil:
		costUSD = r.pricing.CostUSD(in.Model, in.PromptTokens, in.CompletionTokens)
	}
	iterations := in.Iterations
	if iterations <= 0 {
		iterations = 1
	}
	return &persistence.TaskLLMUsage{
		ID: id,
		// Stamped here rather than left for the repository to default: a row that
		// leaves this seam should be complete, so an observer (a fake in a test, a
		// future in-memory sink) sees the same shape the database would store.
		RecordedAt:          time.Now().UTC(),
		ProjectID:           in.ProjectID,
		TaskID:              in.TaskID,
		ExecutionID:         in.ExecutionID,
		StepID:              in.StepID,
		Role:                role,
		Model:               in.Model,
		PromptTokens:        int64(in.PromptTokens),
		CompletionTokens:    int64(in.CompletionTokens),
		Iterations:          iterations,
		CostUSD:             costUSD,
		Source:              r.source,
		TokensEstimated:     in.TokensEstimated,
		CacheHit:            in.CacheHit,
		APIKeyID:            in.APIKeyID,
		SessionID:           in.SessionID,
		CacheCreationTokens: int64(in.CacheCreationTokens),
		CacheReadTokens:     int64(in.CacheReadTokens),
	}
}
