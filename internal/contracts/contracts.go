// Package contracts defines the narrow CE/EE seam interfaces and plain-data
// DTOs for the three engine-level IP capabilities: replay-safety classification,
// instinct budget resolution, and healing plan application.
//
// Contracts is a neutral leaf package — it imports nothing from internal/enterprise,
// internal/blackbox, or internal/instinct. CE packages import contracts to depend on
// interfaces; EE packages implement the interfaces and inject them via the provider seam.
//
// Design reference: https://docs.vornik.io §2.2
package contracts

import (
	"context"
	"errors"
	"io"
	"time"

	"vornik.io/vornik/internal/storage"
)

// Sentinel errors for the BlackBox seam. Enterprise adapters wrap their
// internal blackbox.Err* values into these so CE handlers can
// errors.Is-check without importing internal/blackbox.
var (
	ErrBlackBoxTaskNotFound           = errors.New("blackbox: task not found")
	ErrBlackBoxVariableNotImplemented = errors.New("counterfactual variable not yet implemented in v1")
	ErrBlackBoxMissingOriginal        = errors.New("blackbox: original task not found")
)

// BlackBoxReplayPlan is the CE-side wire shape for a counterfactual
// replay request. Defined here (in contracts) so both the api package
// (interface definition) and the enterprise/blackbox package (adapter
// implementation) can reference it without creating an import cycle.
// The enterprise adapter converts to its internal blackbox.Plan at the
// boundary.
type BlackBoxReplayPlan struct {
	OriginalTaskID string
	Variable       string
	Value          string
	Role           string
	Label          string
}

// ReplaySafetyClassifier is the deny-by-default gate for which tools may
// execute live during a counterfactual replay. The policy (which tools are
// safe) is IP; the interface is CE.
//
// Nil-receiver safety: all call sites must guard with a nil check before
// calling — nil means "no classifier wired; fail closed in CE caller".
type ReplaySafetyClassifier interface {
	// IsReplaySafe reports whether the named tool is on the active allow-list
	// and may therefore execute live during a counterfactual replay.
	// called from api/mcp_counterfactual_gate.go:181
	IsReplaySafe(toolName string) bool
}

// LogForwarder is the CE-visible handle to the EE log-shipping router
// (internal/enterprise/logship). It is the seam through which the CE service
// container drives centralised log forwarding without naming any logship type.
//
// Nil in Community: a Community build wires no LogForwarderFactory, so the
// container never constructs a forwarder and the root logger / audit repos are
// left untouched (zero overhead — today's CE behaviour). EE injects a real
// adapter over *logship.Router via service.ProviderSet.LogForwarderFactory.
//
// Nil-receiver safety: every CE call site holds the interface value and only
// calls into it after a non-nil check, so a Community (nil) forwarder is inert.
type LogForwarder interface {
	// Start launches the forwarder's async ship loop. Called once at boot
	// after the sinks are built.
	Start()
	// Drain flushes pending events and stops the ship loop on shutdown,
	// bounded by ctx. Returns an error when the drain budget elapses.
	Drain(ctx context.Context) error
	// SetScopes hot-swaps the live scope allowlist (logging.forward.scopes)
	// without a restart. Called from applyHotConfig on a config reload.
	SetScopes(scopes []string)
	// AppWriter returns the writer the root logger should be re-pointed at so
	// every JSON log line is also offered to the forwarder. The returned
	// writer tees to the forwarder; callers typically wrap it in
	// io.MultiWriter(os.Stdout, fwd.AppWriter(os.Stdout)).
	AppWriter(out io.Writer) io.Writer
	// DecorateAuditRepos wraps the Admin/Tool audit repositories so a
	// successful audit write also ships an audit Event. Idempotent: safe to
	// re-apply after a c.repos rebuild (it guards against double-wrapping).
	DecorateAuditRepos(repos *storage.Repositories)
}

// WebhookRelayClient is the CE-visible handle to the EE DMZ→job-tier mTLS relay
// client (internal/enterprise/clustering/webhookrelay). It is the seam through
// which the CE service container injects the relay forwarder into the API server
// and registers the relay-upstream readiness probe without naming any
// webhookrelay type.
//
// Nil in Community: a Community build wires no WebhookRelayClientFactory (it is
// folded behind the ClusteringProvider, which is nil in Community), so a CE
// build constructs no relay client — today's CE behaviour. The concrete
// *webhookrelay.Client satisfies this interface; the EE clustering provider
// type-asserts it back to the concrete type when wiring the webhook-heartbeat
// subsystem.
//
// The Forward method matches api.WebhookForwarder structurally, so the same
// value flows into api.WithWebhookRelay without an adapter.
type WebhookRelayClient interface {
	// Forward relays a verified webhook to the job tier. Mirrors
	// api.WebhookForwarder.Forward.
	Forward(ctx context.Context, projectID, source, deliveryID string, body []byte) (int, error)
	// Ping checks reachability of the relay upstream for the readiness probe.
	Ping(ctx context.Context) error
}

// LearnedTierResult is the result of a successful LearnedTier lookup.
// Tier is the resolved toolbudget tier string (e.g. "standard", "open_ended");
// InstinctID is the source instinct row ID for application recording.
type LearnedTierResult struct {
	Tier       string
	InstinctID string
}

// InstinctBudgetResolver resolves a learned tool-budget tier for a given
// project/role. The learning algorithm is IP; the interface is CE.
type InstinctBudgetResolver interface {
	// LearnedTier looks up the highest-confidence active/promoted budget-domain
	// instinct for (projectID, role) and resolves it to a tier.
	// Returns (result, true) when a qualifying instinct was found;
	// (zero, false) when the resolver is nil, no qualifying instinct exists,
	// confidence is below minConf, or both signals tie exactly.
	// called from executor/container.go:527
	LearnedTier(ctx context.Context, projectID, role string, minConf float64) (LearnedTierResult, bool)
}

// HealingApplier is the seam between the CE-side trial orchestration
// (workflowhealing) and the EE replay engine. The plan/trace are plain DTOs;
// the engine (IP) converts them to/from its internal representation.
type HealingApplier interface {
	// ApplyPlan spawns a counterfactual replay task for the given plan and
	// returns an ExecutionTrace whose TaskID is set to the newly-created
	// replay task. The caller (CE replay adapter) then polls for settlement
	// and calls BaselineTrace(replayTaskID) to obtain the full settled trace.
	// The engine internals are EE; the caller sees only the DTO shape.
	// called from workflowhealing/replay_adapter.go (Task 5 inversion)
	ApplyPlan(ctx context.Context, plan CounterfactualPlan) (ExecutionTrace, error)

	// BaselineTrace assembles the recorded (or cached) execution trace for
	// the given task id. Used by the CE replay adapter to obtain both the
	// original evidence trace (baseline arm) and the settled replay trace
	// (candidate arm) after waiting for settlement.
	// called from workflowhealing/replay_adapter.go (Task 5 inversion)
	BaselineTrace(ctx context.Context, taskID string) (ExecutionTrace, error)
}

// --- Plain DTOs that cross the seam by value (NO behaviour) ---
// These are derived from blackbox.Plan / blackbox.Trace fields.

// CounterfactualVariable names the one variable to mutate in a counterfactual
// plan (e.g. "model", "prompt", "workflow", "budget", "tool_result",
// "memory_chunk_excluded").
type CounterfactualVariable = string

// CounterfactualPlan describes a single counterfactual to run. It is the
// CE-side equivalent of blackbox.Plan — plain data that crosses the seam by
// value. EE converts to/from its internal Plan at the boundary.
type CounterfactualPlan struct {
	// OriginalTaskID is the task whose payload + workflow the counterfactual
	// mirrors.
	OriginalTaskID string
	// Variable names the one variable to mutate (CounterfactualVariable).
	Variable CounterfactualVariable
	// Value is the new value for the variable.
	Value string
	// Role is the workflow role the variable applies to (empty = all roles,
	// only meaningful for model/prompt variables).
	Role string
	// Label is operator-supplied free text recorded on the new execution row's
	// counterfactual_label column.
	Label string
}

// ExecutionEvent is one event in an execution trace, crossing the seam as
// plain data. Fields derived from blackbox.Event (types.go). EE converts its
// internal Event → ExecutionEvent at the boundary.
type ExecutionEvent struct {
	// Kind discriminates the event (e.g. "step", "llm_call", "tool_call").
	Kind string
	// Role is the workflow role that produced the event.
	Role string
	// Model is the LLM model used (set for LLM calls and judge verdicts).
	Model string
	// Detail is a one-line human-readable summary of the event. For
	// operator_op events the EE adapter sets this to the op value (e.g.
	// "cancel", "retry", "fork", "hint") so the CE isIntervention check can
	// inspect it without accessing Details.
	Detail string
	// CostUSD is the marginal cost contributed by this event (0 for non-LLM events).
	CostUSD float64
	// Verdict is the judge verdict string (set for judge_verdict events).
	// Vocabulary: "pass" / "fail" / "abstain" (persistence.TaskJudgeVerdict).
	Verdict string
	// Hallucination is set to true by the EE adapter when the judge verdict
	// indicates an ungrounded/unsupported result (verdict == "fail" or the
	// legacy boolean hallucination flag from Details["hallucination"]).
	Hallucination bool
	// Outcome is the step terminal outcome (set for step events).
	// Vocabulary: "ok", "parse_error", "schema_violation", "refused", …
	// (stepoutcome vocabulary). Any non-"ok" non-empty outcome is a failure.
	Outcome string
}

// TraceCounts pre-rolls common aggregates for an execution trace.
// Fields derived from blackbox.TraceCounts (types.go).
type TraceCounts struct {
	// Messages is the count of message-kind events.
	Messages int
	// ToolCalls is the count of tool_call-kind events.
	ToolCalls int
	// LLMCalls is the count of llm_call-kind events.
	LLMCalls int
	// TotalCostUSD is the sum of all event CostUSD values. Mirrors
	// blackbox.TraceCounts.TotalCostUSD so the CE aggregator can compute
	// AvgCostUSD without walking the event list.
	TotalCostUSD float64
}

// HealingObserver is the combined metrics observer for the workflowhealing layer.
// EE wires a *blackbox.Metrics (which satisfies both RecordHealingTrial and
// RecordPromotion). CE code passes it as workflowhealing.TrialMetrics /
// workflowhealing.Metrics — both are subsets of this interface.
// Nil in Community (callers pass nil to the workflowhealing constructors, which
// are nil-safe).
type HealingObserver interface {
	RecordHealingTrial(mode, verdict string, durationSeconds float64)
	RecordPromotion()
}

// ExecutionTrace is the CE-side view of a completed execution trace.
// EE converts blackbox.Trace → ExecutionTrace at the boundary.
type ExecutionTrace struct {
	// TaskID is the task this trace belongs to.
	TaskID string
	// Events is the chronologically-ordered list of execution events.
	Events []ExecutionEvent
	// Digest is the canonical sha256 of the event sequence (matches
	// blackbox.Trace.Header.TraceDigest).
	Digest string
	// Counts pre-rolls common aggregates.
	Counts TraceCounts
	// Status is the terminal execution status (mirrors
	// blackbox.TraceHeader.Status). E.g. "COMPLETED", "FAILED",
	// "CANCELLED". Used by the CE aggregator to count successes/failures
	// without inspecting the raw persistence types.
	Status string
	// StartedAt is when the execution started (mirrors
	// blackbox.TraceHeader.StartedAt). Zero when not recorded.
	StartedAt time.Time
	// CompletedAt is when the execution reached a terminal state (mirrors
	// blackbox.TraceHeader.CompletedAt). Zero when not recorded.
	CompletedAt time.Time
	// Inconclusive is set to true by the EE adapter when the replay trace
	// was heavily stubbed (one or more side-effecting tools were blocked by
	// the counterfactual MCP gate). The CE aggregator uses this flag to
	// surface low-fidelity replays as inconclusive rather than counting them
	// as confident deltas. Mirrors the Inconclusive field of
	// blackbox.Scorecard (computed at trace assembly time by the EE adapter).
	Inconclusive bool
}
