package executor

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"vornik.io/vornik/internal/pricing"
)

const (
	executorNamespace = "vornik"
	executorSubsystem = "executor"

	// modelLabelOther is the catch-all bucket for an unknown `model` string
	// in metric labels. Models are validated against the pricing-table
	// catalog before becoming a label so a typo'd or provider-echoed model
	// can't mint a new permanent Prometheus series (cardinality guard).
	modelLabelOther = "other"
)

// Metrics holds the Prometheus metrics for the executor package.
type Metrics struct {
	// Active is a gauge tracking the number of currently executing tasks.
	Active *prometheus.GaugeVec

	// StartedTotal is a counter tracking the total number of executions started.
	StartedTotal *prometheus.CounterVec

	// CompletedTotal is a counter tracking the total number of executions completed successfully.
	CompletedTotal *prometheus.CounterVec

	// FailedTotal is a counter tracking the total number of executions failed.
	FailedTotal *prometheus.CounterVec

	// CancelledTotal is a counter tracking the total number of executions cancelled.
	CancelledTotal *prometheus.CounterVec

	// RetriedTotal is a counter tracking the total number of execution retries.
	RetriedTotal *prometheus.CounterVec

	// ToolCallsTotal is a counter tracking the total number of tool invocations
	// reported by agent containers.
	ToolCallsTotal *prometheus.CounterVec

	// LLMTokensTotal is a counter of prompt/completion tokens consumed by
	// agent-container LLM calls, reported via result.json.usage.
	LLMTokensTotal *prometheus.CounterVec

	// LLMIterationsTotal is a counter of tool-calling iterations used per
	// agent step. Useful for spotting runaway loops and tuning caps.
	LLMIterationsTotal *prometheus.CounterVec

	// LLMCostUSDTotal is the dollar cost attributed to each project, computed
	// from tokens × pricing table. This is the "real spend" view — what the
	// deployment is actually paying for each project.
	LLMCostUSDTotal *prometheus.CounterVec

	// ModelCostUSDTotal mirrors LLMCostUSDTotal but drops the project dimension
	// so it can be combined with AgentStepOutcomesTotal for effective-cost
	// analysis across projects.
	ModelCostUSDTotal *prometheus.CounterVec

	// LLMCacheSavingsUSDTotal is the cumulative dollar value of prompt-cache
	// reads (what the call WOULD have cost if cache_read tokens had been
	// billed at the full input rate). Per (role, model), no project label
	// — the spend dashboard's "$ saved by cache today" tile sums this across
	// projects to mirror how ModelCostUSDTotal aggregates.
	LLMCacheSavingsUSDTotal *prometheus.CounterVec

	// AgentStepOutcomesTotal counts per-step completions by outcome, indexed
	// by role+model. Combined with ModelCostUSDTotal this lets Grafana show
	// "effective cost" = spend × attempts / successes — so a cheap but flaky
	// model gets penalised against a pricier reliable one. Deliberately has
	// no project label; this is a model-effectiveness view, not a billing
	// view.
	AgentStepOutcomesTotal *prometheus.CounterVec

	// ModelSuccessRate is success / (success+failed+timeout) per (role, model).
	// Cancelled outcomes are excluded: a user-initiated cancel should not
	// penalise the model. Exposed as a native gauge so dashboards can read
	// the ratio without a PromQL join; vornik keeps the mirror state below
	// in sync with the counter increments.
	ModelSuccessRate *prometheus.GaugeVec

	// ModelEffectiveCostUSD is lifetime_cost_usd / successful_step_count per
	// (role, model). Captures the "real" cost-per-useful-outcome — a cheap
	// model that fails 60% of the time is 2.5× more expensive per success
	// than its list price suggests. Reset only on process restart.
	ModelEffectiveCostUSD *prometheus.GaugeVec

	// ModelParseFailureRate is parse_error / total_terminal per (role, model).
	// Surfaces models that produce frequently-unparseable JSON — the quality
	// signal that the legacy success/failed split missed. Cancelled is
	// excluded from the denominator.
	ModelParseFailureRate *prometheus.GaugeVec

	// ModelRefusalRate is refused / total_terminal per (role, model). A model
	// that declines to act often may need a different system prompt or a
	// different role assignment rather than a retry.
	ModelRefusalRate *prometheus.GaugeVec

	// ModelLoopRate is degenerate_loop / total_terminal per (role, model).
	// High values mean a model is burning tokens revisiting the same tool
	// call — typically a sign of context exhaustion or poor tool hints.
	ModelLoopRate *prometheus.GaugeVec

	// ModelSchemaViolationRate is schema_violation / total_terminal per
	// (role, model). Distinct from ModelParseFailureRate: parse failures
	// are JSON-malformed output; schema violations are valid JSON missing
	// the role's requiredOutputKeys (e.g. lead emitting result.json
	// without "plan"). Surfaces models that confidently produce
	// well-formed but contractually-wrong output — a class that GLM-5,
	// Kimi K2.5, and other open-weight models exhibit at meaningful
	// rates against strict schemas.
	ModelSchemaViolationRate *prometheus.GaugeVec

	// ShapeRetryTotal counts shape-retry attempts per (role, model,
	// kind), where kind is the classifier output (schema_violation /
	// parse_error / shape_failure). One increment per shape retry
	// the executor decides to fire — the recovery counter below
	// pairs with this to give per-model "shape retry recovered on
	// 2nd attempt" rate.
	ShapeRetryTotal *prometheus.CounterVec

	// ShapeRetryRecoveredTotal counts shape retries that succeeded
	// on the second attempt (same model, corrective prompt). Per
	// (role, model, kind). A model with a high
	// ShapeRetryRecoveredTotal/ShapeRetryTotal ratio is salvageable
	// by prompt nudging; a low ratio means the model is structurally
	// unable to follow the schema and a model swap is the right fix.
	ShapeRetryRecoveredTotal *prometheus.CounterVec

	// ShapeRetryByOutcomeTotal is the operator-facing per-role
	// rescue-rate view requested by item 10 of
	// https://docs.vornik.io Labels:
	//   - role: the role whose output failed validation
	//   - outcome: "attempted" (one shape retry fired), "recovered"
	//     (the retry produced valid output), "failed" (the retry's
	//     own output also failed validation).
	// Distinct from ShapeRetryTotal (which is role+model+kind for
	// per-model salvage analysis) — this counter trades model/kind
	// labels for a clean outcome timeline. Operators read
	// recovered/attempted as the rescue rate per role.
	ShapeRetryByOutcomeTotal *prometheus.CounterVec

	// ModelFallbackTotal counts model-fallback escalations per
	// (role, primary_model, fallback_model). Increments when the
	// executor gives up on the primary model after shape-retry
	// also failed. The counter rate by (primary_model) tells
	// operators which models need replacing in role configs.
	ModelFallbackTotal *prometheus.CounterVec

	// ToolBudgetResolvedTotal counts dynamic tool-budget resolutions
	// per (role, tier) — incremented at every worker spawn where the
	// tool_budget feature injected a scaled VORNIK_MAX_TOOL_ITERATIONS.
	// Cross-referenced with actual iteration use (tool_audit_log) it
	// surfaces systematic over- or under-provisioning per role+tier and
	// feeds the future instinct layer. See
	// https://docs.vornik.io §9.
	ToolBudgetResolvedTotal *prometheus.CounterVec

	// DurationSeconds is a histogram tracking the execution duration by status.
	DurationSeconds *prometheus.HistogramVec

	// --- Inter-project orchestration Phase C metrics (LLD §9.3) ---

	// CrossProjectCallsTotal counts cross_project_calls rows by
	// outcome status. Labels: caller (project), callee (project),
	// status (pending → … → completed/failed/timed_out/rejected).
	// Dashboard usage: spot projects that get rejected often
	// (acceptCallsFrom misconfiguration) vs ones whose callees
	// fail.
	CrossProjectCallsTotal *prometheus.CounterVec

	// CrossProjectCallDurationSeconds is a histogram of the
	// wall-clock latency from CPC create → terminal resolve.
	// Labels: caller, callee. The buckets default to the
	// Prometheus histogram defaults (sufficient for 1s-10min
	// range typical of agent workflows).
	CrossProjectCallDurationSeconds *prometheus.HistogramVec

	// ProjectSpawnsTotal counts materialised spawn_project rows
	// by template + caller. Idempotent re-runs (LLD §6.2 skip
	// path) are NOT counted — only fresh spawns.
	ProjectSpawnsTotal *prometheus.CounterVec

	// --- Canonical-context pre-load (context-discovery LLD Phase B) ---

	// CanonicalContextLoadedTotal fires once per task whose
	// workspace prep populated PROJECT_CONTEXT.md / USER_GUIDANCE.md
	// into task.json. Labels: project (the project ID), source
	// (one of "dot_autonomy" / "plain_autonomy" / "mixed"). Used
	// to track adoption of the convention + spot projects that
	// still rely on the legacy plain-autonomy layout.
	CanonicalContextLoadedTotal *prometheus.CounterVec

	// CanonicalContextTruncatedTotal fires once per pre-loaded
	// file that exceeded the 16 KiB cap and was truncated.
	// Labels: project, file (one of "project" / "user").
	// Non-zero rates here suggest the cap should be raised — or
	// that the file's growing without bound.
	CanonicalContextTruncatedTotal *prometheus.CounterVec

	// RetryFromStepTotal counts operator-initiated retry-from-step
	// attempts by result. Labels:
	//   - result: "succeeded" (state reset + relaunch ok),
	//     "refused_bad_state" (execution not terminal, or already
	//     executing), "refused_unknown_step" (step not in the run's
	//     completed_steps), "error" (load/persist/relaunch failure).
	// LLD: https://docs.vornik.io §3.1.2.
	RetryFromStepTotal *prometheus.CounterVec

	// RetryFromStepSideEffectingUpstreamTotal counts retry-from-step
	// attempts where at least one PRESERVED upstream step had external
	// side effects the retry does NOT replay (workflow step type
	// "system" — forge writes, RAG indexing — or "call_project"). The
	// re-run sees those effects as already-done, so they may be stale or
	// have happened twice from the operator's mental model. Unlabelled:
	// an operator-awareness / alerting signal, not a per-project SLO.
	// LLD: https://docs.vornik.io §3.1.2.
	RetryFromStepSideEffectingUpstreamTotal prometheus.Counter

	// DelegationGuardRejectionsTotal counts intra-task delegation batches
	// refused by an N4 safety guard, by reason. Labels:
	//   - reason: "fanout" (cumulative per-parent child cap exceeded),
	//     "depth" (delegation nesting limit reached), "cycle" (looping
	//     lineage detected).
	// LLD: https://docs.vornik.io §3.
	DelegationGuardRejectionsTotal *prometheus.CounterVec

	// registry is the Prometheus registerer used for registering metrics.
	registry prometheus.Registerer

	// modelStats mirrors counter state so the two derived gauges can be
	// recomputed in O(1) on each record. Map key is "<role>\x00<model>".
	// The counter vectors are the source of truth for raw values; this
	// mirror exists only to avoid the Prometheus-client "can't read your
	// own counters" limitation. Guarded by statsMu.
	statsMu    sync.Mutex
	modelStats map[string]*modelStatEntry

	// modelCatalog is the pricing table used to validate a `model` string
	// before it becomes a metric label (cardinality guard — see
	// modelLabel). Stored atomically because SetModelCatalog can swap it on
	// config-reload while record paths read it concurrently. Nil → no
	// catalog yet, so labels pass through unbucketed (current behavior).
	modelCatalog atomic.Pointer[pricing.Table]
}

// SetModelCatalog points the metrics layer at the pricing table whose model
// IDs define the allowed metric-label set. Called at executor construction
// and on config-reload (SetPricing). A nil table clears the catalog (labels
// pass through unbucketed).
func (m *Metrics) SetModelCatalog(t *pricing.Table) {
	if m == nil {
		return
	}
	m.modelCatalog.Store(t)
}

// modelLabel buckets a raw model string for use as a metric label: a model
// the catalog knows passes through; anything else collapses to
// modelLabelOther so unbounded/typo'd model strings can't grow the series
// set. With no catalog set, the raw model passes through (cost math always
// uses the raw model regardless — only the label is bucketed).
func (m *Metrics) modelLabel(model string) string {
	if m == nil {
		return model
	}
	t := m.modelCatalog.Load()
	if t == nil {
		return model
	}
	if _, known := t.Lookup(model); !known {
		return modelLabelOther
	}
	return model
}

// modelStatEntry accumulates per-(role, model) state used to derive the
// ModelSuccessRate, ModelEffectiveCostUSD, and richer quality gauges
// (ModelParseFailureRate, ModelRefusalRate, ModelLoopRate).
//
// The taxonomy here mirrors internal/stepoutcome: one counter per
// terminal outcome. The gauges are computed from these; raw Prometheus
// counters are updated in parallel so dashboards that bypass the mirror
// stay consistent.
type modelStatEntry struct {
	role  string
	model string

	// Legacy 4-label outcomes — preserved for the old success-rate
	// gauge semantics during the transition. Incremented when outcomes
	// land in their "legacy equivalent" bucket: ok → success,
	// failed/parse_error/schema_violation/refused/downstream_rejected/
	// gate_failed → failed. timeout/cancelled stay themselves.
	success   int64
	failed    int64
	timeout   int64
	cancelled int64

	// Richer outcome counters — one per internal/stepoutcome.Outcome.
	// Populated by RecordFinalOutcome; used by the new quality gauges
	// (parse-failure rate, refusal rate, loop rate).
	ok                 int64
	parseError         int64
	schemaViolation    int64
	refused            int64
	iterationExhausted int64
	degenerateLoop     int64
	downstreamRejected int64
	gateFailed         int64

	costUSD float64
}

// denominator is the total "billable attempts" count used for rate
// metrics — everything except cancelled (user-initiated cancels don't
// penalise the model). Uses the richer-taxonomy counters so the rate
// reflects the full outcome space, not just success/failed.
func (e *modelStatEntry) denominator() int64 {
	return e.ok + e.failed + e.timeout +
		e.parseError + e.schemaViolation + e.refused +
		e.iterationExhausted + e.degenerateLoop +
		e.downstreamRejected + e.gateFailed
}

// NewMetrics creates a new Metrics instance with the given Prometheus registerer.
// If registerer is nil, prometheus.DefaultRegisterer is used.
func NewMetrics(registerer prometheus.Registerer) *Metrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	m := &Metrics{
		registry: registerer,
		Active: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "active",
				Help:      "Number of currently executing tasks.",
			},
			[]string{"project_id"},
		),
		StartedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "started_total",
				Help:      "Total number of executions started.",
			},
			[]string{"project_id"},
		),
		CompletedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "completed_total",
				Help:      "Total number of executions completed successfully.",
			},
			[]string{"project_id"},
		),
		FailedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "failed_total",
				Help:      "Total number of executions failed.",
			},
			[]string{"project_id"},
		),
		CancelledTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "cancelled_total",
				Help:      "Total number of executions cancelled.",
			},
			[]string{"project_id"},
		),
		RetriedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "retried_total",
				Help:      "Total number of execution retries.",
			},
			[]string{"project_id"},
		),
		ToolCallsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "tool_calls_total",
				Help:      "Total tool invocations reported by agent containers.",
			},
			[]string{"project_id", "tool"},
		),
		LLMTokensTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "llm_tokens_total",
				Help:      "LLM tokens consumed by agent containers, split by direction.",
			},
			[]string{"project_id", "role", "model", "direction"},
		),
		LLMIterationsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "llm_iterations_total",
				Help:      "Tool-calling iterations used per agent step.",
			},
			[]string{"project_id", "role", "model"},
		),
		LLMCostUSDTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "llm_cost_usd_total",
				Help:      "LLM dollar cost by project, derived from tokens × pricing table.",
			},
			[]string{"project_id", "role", "model"},
		),
		ModelCostUSDTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_cost_usd_total",
				Help:      "LLM dollar cost by role+model (no project dim), used for effective-cost analysis alongside agent_step_outcomes_total.",
			},
			[]string{"role", "model"},
		),
		LLMCacheSavingsUSDTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "llm_cache_savings_usd_total",
				Help:      "Dollar value of prompt-cache reads — what the call would have cost at the full input rate minus what it actually cost at the cache_read rate. Aggregated by (role, model).",
			},
			[]string{"role", "model"},
		),
		AgentStepOutcomesTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "agent_step_outcomes_total",
				Help:      "Agent-step outcomes by role+model, for model-effectiveness analysis. Successful usable output is outcome=\"ok\".",
			},
			[]string{"role", "model", "outcome"},
		),
		ModelSuccessRate: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_success_rate",
				Help:      "Lifetime success ratio per (role, model): success / (success+failed+timeout). Cancelled excluded — user-initiated cancels don't penalise the model. Process-lifetime; resets on restart.",
			},
			[]string{"role", "model"},
		),
		ModelEffectiveCostUSD: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_effective_cost_usd",
				Help:      "Lifetime cost_usd / successful_step_count per (role, model). The true $/useful-outcome — a cheap model that fails a lot looks cheap in model_cost_usd_total but expensive here.",
			},
			[]string{"role", "model"},
		),
		ModelParseFailureRate: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_parse_failure_rate",
				Help:      "Lifetime parse_error ratio per (role, model): parse_error / non-cancelled terminal outcomes. Fires when a model produces JSON that downstream consumers can't read.",
			},
			[]string{"role", "model"},
		),
		ModelRefusalRate: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_refusal_rate",
				Help:      "Lifetime refused ratio per (role, model): refused / non-cancelled terminal outcomes. Different from failure — a model that declined deliberately, not one that broke.",
			},
			[]string{"role", "model"},
		),
		ModelLoopRate: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_loop_rate",
				Help:      "Lifetime degenerate-loop ratio per (role, model): degenerate_loop / non-cancelled terminal outcomes. A stuck-in-a-loop signal that a model is burning tokens revisiting the same tool call.",
			},
			[]string{"role", "model"},
		),
		ModelSchemaViolationRate: promauto.With(registerer).NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_schema_violation_rate",
				Help:      "Lifetime schema-violation ratio per (role, model): schema_violation / non-cancelled terminal outcomes. Distinct from parse_failure — output was valid JSON but missing the role's requiredOutputKeys (e.g. lead emitting result.json without 'plan').",
			},
			[]string{"role", "model"},
		),
		ShapeRetryTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "shape_retry_total",
				Help:      "Total shape-retry attempts per (role, model, kind). 'kind' is the classifier output: schema_violation, parse_error, shape_failure. Operators read this with shape_retry_recovered_total to compute per-model salvage rate.",
			},
			[]string{"role", "model", "kind"},
		),
		ShapeRetryRecoveredTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "shape_retry_recovered_total",
				Help:      "Shape retries that succeeded on the second attempt (same model, corrective prompt). Per (role, model, kind). High recovered/total ratio means the model is salvageable by prompt nudging; low ratio means the model is structurally unable to follow the schema.",
			},
			[]string{"role", "model", "kind"},
		),
		ShapeRetryByOutcomeTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "shape_retry_by_outcome_total",
				Help:      "Per-role shape-retry outcomes (item 10 visibility). outcome ∈ {attempted, recovered, failed}. Operators read recovered/attempted as the per-role rescue rate; failed/attempted is the residual hard-fail rate that a model fallback (or schema rework) would address.",
			},
			[]string{"role", "outcome"},
		),
		ModelFallbackTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "model_fallback_total",
				Help:      "Model-fallback escalations per (role, primary_model, fallback_model). Increments when shape-retry on the primary model also failed and the executor escalated to the configured fallback. High rates per primary_model indicate the role config should be updated.",
			},
			[]string{"role", "primary_model", "fallback_model"},
		),
		ToolBudgetResolvedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "tool_budget_resolved_total",
				Help:      "Dynamic tool-budget resolutions per (role, tier). Increments at each worker spawn where tool_budget injected a scaled VORNIK_MAX_TOOL_ITERATIONS. Compare against actual iteration use (tool_audit_log) to spot over/under-provisioning per role+tier.",
			},
			[]string{"role", "tier"},
		),
		modelStats: make(map[string]*modelStatEntry),
		DurationSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: executorNamespace,
				Subsystem: executorSubsystem,
				Name:      "duration_seconds",
				Help:      "Duration of task executions by status.",
				// Buckets optimized for execution duration: 100ms to 30 minutes
				Buckets: []float64{
					0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800,
				},
			},
			[]string{"project_id", "status"},
		),
		CrossProjectCallsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "cross_project_calls_total",
				Help:      "Cross-project calls grouped by terminal status. status takes the persistence.CrossProjectCallStatus string values; 'started' fires once on CPC create.",
			},
			[]string{"caller", "callee", "status"},
		),
		CrossProjectCallDurationSeconds: promauto.With(registerer).NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "vornik",
				Name:      "cross_project_call_duration_seconds",
				Help:      "Wall-clock latency from cross_project_calls.created_at to resolved_at.",
				Buckets:   []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800},
			},
			[]string{"caller", "callee"},
		),
		ProjectSpawnsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "project_spawns_total",
				Help:      "Materialised spawn_project rows by caller project + template slug. Idempotent skip-on-collision is NOT counted.",
			},
			[]string{"caller", "template"},
		),
		CanonicalContextLoadedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "canonical_context_loaded_total",
				Help:      "Tasks where workspace prep pre-loaded PROJECT_CONTEXT.md / USER_GUIDANCE.md into task.json. source labels: dot_autonomy / plain_autonomy / mixed.",
			},
			[]string{"project", "source"},
		),
		CanonicalContextTruncatedTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "canonical_context_truncated_total",
				Help:      "Canonical-context files that exceeded the 16 KiB cap during pre-load and were truncated. file labels: project / user.",
			},
			[]string{"project", "file"},
		),
		RetryFromStepTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "retry_from_step_total",
				Help:      "Operator-initiated retry-from-step attempts by result: succeeded / refused_bad_state / refused_unknown_step / error.",
			},
			[]string{"result"},
		),
		RetryFromStepSideEffectingUpstreamTotal: promauto.With(registerer).NewCounter(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "retry_from_step_side_effecting_upstream_total",
				Help:      "Retry-from-step attempts that preserved >=1 upstream step with non-replayed external side effects (system / call_project).",
			},
		),
		DelegationGuardRejectionsTotal: promauto.With(registerer).NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "vornik",
				Name:      "delegation_guard_rejections_total",
				Help:      "Intra-task delegation batches refused by an N4 safety guard, by reason: fanout / depth / cycle.",
			},
			[]string{"reason"},
		),
	}

	return m
}

// RecordRetryFromStep counts one operator-initiated retry-from-step
// attempt. result ∈ {succeeded, refused_bad_state, refused_unknown_step,
// error}. Nil-safe so executors constructed without metrics (tests) are
// a no-op.
func (m *Metrics) RecordRetryFromStep(result string) {
	if m == nil {
		return
	}
	m.RetryFromStepTotal.WithLabelValues(result).Inc()
}

// RecordRetryFromStepSideEffectingUpstream counts one retry-from-step that
// preserved at least one upstream step with non-replayed external side
// effects. Nil-safe.
func (m *Metrics) RecordRetryFromStepSideEffectingUpstream() {
	if m == nil || m.RetryFromStepSideEffectingUpstreamTotal == nil {
		return
	}
	m.RetryFromStepSideEffectingUpstreamTotal.Inc()
}

// RecordDelegationGuardRejection counts one intra-task delegation batch
// refused by an N4 guard. reason ∈ {fanout, depth, cycle}. Nil-safe so
// executors constructed without metrics (tests) are a no-op.
func (m *Metrics) RecordDelegationGuardRejection(reason string) {
	if m == nil {
		return
	}
	m.DelegationGuardRejectionsTotal.WithLabelValues(reason).Inc()
}

// RecordCrossProjectCallStarted fires once at CPC creation
// time. Status="started" — distinct from the terminal statuses
// so dashboards can compute "created today" separately from
// "resolved today".
func (m *Metrics) RecordCrossProjectCallStarted(caller, callee string) {
	if m == nil {
		return
	}
	m.CrossProjectCallsTotal.WithLabelValues(caller, callee, "started").Inc()
}

// RecordCrossProjectCallResolved fires at CPC terminal-state
// transition. Status is the persistence.CrossProjectCallStatus
// string value (completed / failed / rejected / timed_out).
// Duration is the wall-clock from create to resolve.
func (m *Metrics) RecordCrossProjectCallResolved(caller, callee, status string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.CrossProjectCallsTotal.WithLabelValues(caller, callee, status).Inc()
	if durationSeconds > 0 {
		m.CrossProjectCallDurationSeconds.WithLabelValues(caller, callee).Observe(durationSeconds)
	}
}

// RecordCanonicalContextLoaded fires once per task whose
// workspace prep populated PROJECT_CONTEXT.md / USER_GUIDANCE.md
// into task.json. source must be one of the canonical strings
// from CanonicalContext.Source.
func (m *Metrics) RecordCanonicalContextLoaded(projectID, source string) {
	if m == nil || source == "" {
		return
	}
	m.CanonicalContextLoadedTotal.WithLabelValues(projectID, source).Inc()
}

// RecordCanonicalContextTruncated fires once per pre-loaded file
// that exceeded the 16 KiB cap. file is one of "project" or
// "user".
func (m *Metrics) RecordCanonicalContextTruncated(projectID, file string) {
	if m == nil || file == "" {
		return
	}
	m.CanonicalContextTruncatedTotal.WithLabelValues(projectID, file).Inc()
}

// RecordProjectSpawn fires when a spawn_project step
// materialises a new project (not on the idempotent skip
// path).
func (m *Metrics) RecordProjectSpawn(caller, template string) {
	if m == nil {
		return
	}
	m.ProjectSpawnsTotal.WithLabelValues(caller, template).Inc()
}

// RecordStarted increments the started counter.
// The active gauge is updated separately via SetActiveGauge.
func (m *Metrics) RecordStarted(projectID string) {
	if m == nil {
		return
	}
	m.StartedTotal.WithLabelValues(projectID).Inc()
}

// RecordCompleted records a successful execution.
func (m *Metrics) RecordCompleted(projectID string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.CompletedTotal.WithLabelValues(projectID).Inc()
	m.DurationSeconds.WithLabelValues(projectID, "completed").Observe(durationSeconds)
}

// RecordFailed records a failed execution.
func (m *Metrics) RecordFailed(projectID string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.FailedTotal.WithLabelValues(projectID).Inc()
	m.DurationSeconds.WithLabelValues(projectID, "failed").Observe(durationSeconds)
}

// RecordCancelled records a cancelled execution.
func (m *Metrics) RecordCancelled(projectID string, durationSeconds float64) {
	if m == nil {
		return
	}
	m.CancelledTotal.WithLabelValues(projectID).Inc()
	m.DurationSeconds.WithLabelValues(projectID, "cancelled").Observe(durationSeconds)
}

// SetActiveGauge sets the active execution gauge from actual executor state.
// This is called after each start/completion to keep the gauge accurate
// across process restarts (avoids negative values from orphaned Dec calls).
func (m *Metrics) SetActiveGauge(counts map[string]int) {
	if m == nil {
		return
	}
	m.Active.Reset()
	for projectID, count := range counts {
		m.Active.WithLabelValues(projectID).Set(float64(count))
	}
}

// RecordToolCalls increments the tool call counter for each tool invocation.
func (m *Metrics) RecordToolCalls(projectID string, toolCounts map[string]int) {
	if m == nil {
		return
	}
	for tool, count := range toolCounts {
		m.ToolCallsTotal.WithLabelValues(projectID, tool).Add(float64(count))
	}
}

// RecordLLMUsage is the backwards-compatible entry point — records
// prompt/completion tokens with no cache fields. Prefer
// RecordLLMUsageWithCache for new call sites so cache observability
// stays accurate.
func (m *Metrics) RecordLLMUsage(projectID, role, model string, promptTokens, completionTokens, iterations int, pricingTable *pricing.Table) {
	m.RecordLLMUsageWithCache(projectID, role, model, promptTokens, completionTokens, iterations, 0, 0, pricingTable)
}

// RecordLLMUsageWithCache records prompt/completion/cache tokens and
// iteration count for a single agent step, and (if a pricing table is
// supplied) the dollar cost along both the project-attributed and
// model-attributed dimensions, plus the dollar value saved by cache reads.
//
// pricingTable may be nil; in that case no cost-derived metric is emitted.
// cacheCreationTokens / cacheReadTokens may be 0 — they're populated only
// by Bedrock + Anthropic providers, so non-cache-capable providers report
// zeros and the metric series stays clean.
func (m *Metrics) RecordLLMUsageWithCache(projectID, role, model string, promptTokens, completionTokens, iterations, cacheCreationTokens, cacheReadTokens int, pricingTable *pricing.Table) {
	if m == nil {
		return
	}
	// Keep the label catalog in sync with whatever table the caller is using
	// (covers SetPricing-at-runtime without separate wiring), then bucket the
	// model for label use. Cost math below still uses the RAW model so an
	// unknown-but-priced model is costed correctly — only the label collapses.
	if pricingTable != nil {
		m.SetModelCatalog(pricingTable)
	}
	modelLbl := m.modelLabel(model)
	if promptTokens > 0 {
		m.LLMTokensTotal.WithLabelValues(projectID, role, modelLbl, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.LLMTokensTotal.WithLabelValues(projectID, role, modelLbl, "completion").Add(float64(completionTokens))
	}
	if cacheCreationTokens > 0 {
		m.LLMTokensTotal.WithLabelValues(projectID, role, modelLbl, "cache_creation").Add(float64(cacheCreationTokens))
	}
	if cacheReadTokens > 0 {
		m.LLMTokensTotal.WithLabelValues(projectID, role, modelLbl, "cache_read").Add(float64(cacheReadTokens))
	}
	if iterations > 0 {
		m.LLMIterationsTotal.WithLabelValues(projectID, role, modelLbl).Add(float64(iterations))
	}

	if pricingTable != nil {
		cost := pricingTable.CostUSDWithCache(model, promptTokens, completionTokens, cacheCreationTokens, cacheReadTokens)
		if cost > 0 {
			m.LLMCostUSDTotal.WithLabelValues(projectID, role, modelLbl).Add(cost)
			m.ModelCostUSDTotal.WithLabelValues(role, modelLbl).Add(cost)
			m.statsMu.Lock()
			entry := m.entryLocked(role, modelLbl)
			entry.costUSD += cost
			m.updateModelGaugesLocked(entry)
			m.statsMu.Unlock()
		}
		if saved := pricingTable.CacheSavingsUSD(model, cacheReadTokens); saved > 0 {
			m.LLMCacheSavingsUSDTotal.WithLabelValues(role, modelLbl).Add(saved)
		}
	}
}

// RecordAgentStepOutcome attributes one agent-step completion to a role+model
// pair using the legacy 4-label taxonomy ("success", "failed", "timeout",
// "cancelled"). Kept for the container.go defer which only has err-based
// classification at its disposal. RecordFinalOutcome is the source of
// truth for the richer taxonomy.
func (m *Metrics) RecordAgentStepOutcome(role, model, outcome string) {
	if m == nil || role == "" || model == "" {
		return
	}
	m.AgentStepOutcomesTotal.WithLabelValues(role, m.modelLabel(model), outcome).Inc()
	// Don't mirror here — the richer stats live on the RecordFinalOutcome
	// path. This legacy method just keeps the original counter label
	// space populated for any existing dashboards.
}

// RecordFinalOutcome records a step's terminal outcome in the richer
// taxonomy. Unlike RecordAgentStepOutcome, this is the source of truth
// for the quality-rate gauges — every finalize, sweep, and direct
// terminal write goes through here. The Prometheus counter
// AgentStepOutcomesTotal gets the same label value so a single counter
// carries both legacy and richer outcomes.
//
// Called from:
//   - finalizePendingOutcome (consumer finalized a pending row)
//   - sweepPendingOutcomes (terminal sweep finalized leftover pending)
//   - recordStepOutcome when outcome != pending_validation (direct
//     terminal write, e.g. container-level failure)
func (m *Metrics) RecordFinalOutcome(role, model, outcome string) {
	if m == nil || role == "" || model == "" || outcome == "" {
		return
	}
	model = m.modelLabel(model)
	m.AgentStepOutcomesTotal.WithLabelValues(role, model, outcome).Inc()

	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	entry := m.entryLocked(role, model)
	switch outcome {
	case "ok":
		entry.ok++
		entry.success++ // legacy mirror: ok is the new name for success
	case "failed":
		entry.failed++
	case "timeout":
		entry.timeout++
	case "cancelled":
		entry.cancelled++
	case "parse_error":
		entry.parseError++
	case "schema_violation":
		entry.schemaViolation++
	case "refused":
		entry.refused++
	case "iteration_exhausted":
		entry.iterationExhausted++
	case "degenerate_loop":
		entry.degenerateLoop++
	case "downstream_rejected":
		entry.downstreamRejected++
	case "gate_failed":
		entry.gateFailed++
	}
	m.updateModelGaugesLocked(entry)
}

// entryLocked fetches or creates the mirror entry. Caller must hold statsMu.
func (m *Metrics) entryLocked(role, model string) *modelStatEntry {
	key := role + "\x00" + model
	e, ok := m.modelStats[key]
	if !ok {
		e = &modelStatEntry{role: role, model: model}
		m.modelStats[key] = e
	}
	return e
}

// updateModelGaugesLocked recomputes and sets all derived gauges for one
// entry. Caller must hold statsMu. Gauges are only set when the
// denominator is meaningful — no terminal outcomes yet means the rates
// are undefined and a zero would mislead dashboards ("model has a 0%
// parse failure rate — it must be perfect" when really it's never run).
//
// The denominator is non-cancelled terminal outcomes (ok + all
// failure-like outcomes). Cancelled is excluded because user-initiated
// cancels don't reflect model quality.
func (m *Metrics) updateModelGaugesLocked(e *modelStatEntry) {
	denom := e.denominator()
	if denom == 0 {
		return
	}
	denomF := float64(denom)
	// Success = ok. Redefined from the legacy "not failed/timeout"
	// semantics to the richer "output was actually usable" semantics.
	m.ModelSuccessRate.WithLabelValues(e.role, e.model).Set(float64(e.ok) / denomF)
	m.ModelParseFailureRate.WithLabelValues(e.role, e.model).Set(float64(e.parseError) / denomF)
	m.ModelRefusalRate.WithLabelValues(e.role, e.model).Set(float64(e.refused) / denomF)
	m.ModelLoopRate.WithLabelValues(e.role, e.model).Set(float64(e.degenerateLoop) / denomF)
	m.ModelSchemaViolationRate.WithLabelValues(e.role, e.model).Set(float64(e.schemaViolation) / denomF)

	if e.ok > 0 {
		m.ModelEffectiveCostUSD.WithLabelValues(e.role, e.model).Set(e.costUSD / float64(e.ok))
	}
}

// RecordRetried increments the retried counter.
func (m *Metrics) RecordRetried(projectID string) {
	if m == nil {
		return
	}
	m.RetriedTotal.WithLabelValues(projectID).Inc()
}

// RecordShapeRetry counts a shape-retry attempt for the given role/model/kind.
// Called by retry.go when the executor decides to fire a shape-retry pass with
// the corrective prompt. kind is the classifier output ("schema_violation",
// "parse_error", or the lower-cardinality "shape_failure" fallback).
func (m *Metrics) RecordShapeRetry(role, model, kind string) {
	if m == nil || role == "" || model == "" {
		return
	}
	if kind == "" {
		kind = "shape_failure"
	}
	m.ShapeRetryTotal.WithLabelValues(role, model, kind).Inc()
}

// RecordShapeRetryRecovered counts a shape-retry that succeeded on the second
// attempt. Paired with RecordShapeRetry above so the dashboard can compute the
// per-model salvage ratio: a model with low recovered/total is one that
// can't be coaxed into the right shape and should be replaced rather than
// retried.
func (m *Metrics) RecordShapeRetryRecovered(role, model, kind string) {
	if m == nil || role == "" || model == "" {
		return
	}
	if kind == "" {
		kind = "shape_failure"
	}
	m.ShapeRetryRecoveredTotal.WithLabelValues(role, model, kind).Inc()
}

// RecordShapeRetryOutcome counts a per-role shape-retry outcome event.
// outcome ∈ {"attempted","recovered","failed"} — anything else is treated
// as a no-op so a future caller-side typo doesn't pollute the cardinality.
// Item 10 of https://docs.vornik.io: operators read
// recovered/attempted as the rescue rate without joining two counters.
func (m *Metrics) RecordShapeRetryOutcome(role, outcome string) {
	if m == nil || role == "" || outcome == "" {
		return
	}
	switch outcome {
	case "attempted", "recovered", "failed":
		// allowed
	default:
		return
	}
	m.ShapeRetryByOutcomeTotal.WithLabelValues(role, outcome).Inc()
}

// RecordModelFallback counts a model-fallback escalation. Called when the
// executor gives up on the primary model after shape-retry also failed and
// switches to the role's configured fallback model.
func (m *Metrics) RecordModelFallback(role, primaryModel, fallbackModel string) {
	if m == nil || role == "" || primaryModel == "" || fallbackModel == "" {
		return
	}
	m.ModelFallbackTotal.WithLabelValues(role, primaryModel, fallbackModel).Inc()
}

// RecordToolBudgetResolved counts a dynamic tool-budget resolution at worker
// spawn, labelled by role and the planner's complexity tier. An empty tier is
// recorded as "standard" so the series is always populated when the feature
// is on.
func (m *Metrics) RecordToolBudgetResolved(role, tier string) {
	if m == nil || role == "" {
		return
	}
	if tier == "" {
		tier = "standard"
	}
	m.ToolBudgetResolvedTotal.WithLabelValues(role, tier).Inc()
}
