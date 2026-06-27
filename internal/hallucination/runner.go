package hallucination

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// JudgeRunner orchestrates Phase 3 evaluation: when a task
// reaches a terminal status, the runner gathers the evidence
// (artifacts, audit, last result text), invokes the configured
// Judge, and persists the verdict. Decoupled from the executor
// so the executor stays focused on workflow control flow.
//
// The runner is fire-and-forget: callers spawn it in a
// goroutine on task completion and don't wait. Failures get
// logged; nothing about the task's terminal status changes
// based on the verdict (Phase 3 is purely informational —
// judge as a quality signal, not a gate).
type JudgeRunner struct {
	Judge        Judge
	Verdicts     persistence.TaskJudgeVerdictRepository
	Audits       AuditLister
	Artifacts    ArtifactLister
	Executions   ExecutionGetter
	Logger       zerolog.Logger
	JudgeRoleTag string // Defaults to "judge"; allows operators to label per-project judges differently.
	// Pricing + LLMUsage wire the judge call's token cost
	// into the same dashboards as worker + dispatcher cost.
	// Nil for either disables that side of accounting; the
	// verdict still lands but cost_usd stays 0 and no
	// task_llm_usage row gets written. Missing pricing means
	// "we have tokens but can't price them"; missing LLMUsage
	// means "we don't write rollup-able usage rows from the
	// judge" — both useful in test/minimal deployments.
	Pricing  *pricing.Table
	LLMUsage UsageRecorder
	// Metrics is the Prometheus sink for verdict + cost counters.
	// Nil-safe: callers can leave it unset in tests / minimal
	// deployments and the runner still records verdicts to the DB,
	// it just doesn't bump the counters.
	Metrics *Metrics
}

// UsageRecorder is the narrow subset of
// persistence.TaskLLMUsageRepository the runner needs. The full
// repo carries a dozen aggregation queries the runner doesn't
// touch; defining the narrow shape here lets test stubs
// implement just Record without padding the file with
// no-op group-by methods. The production repo satisfies it
// trivially.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// ExecutionGetter is the narrow subset of ExecutionRepository
// the runner needs — pull the most recent execution for a task
// to find the last result text. Defined locally so the runner
// doesn't pull in the broader interface.
type ExecutionGetter interface {
	List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)
}

// Run evaluates a single completed task and persists the
// verdict. Idempotent: a verdict already on file for this task
// short-circuits with a debug log, no second LLM call.
//
// The "fire and forget" semantics live at the caller — Run
// itself returns an error so tests can assert on outcomes; the
// production caller wraps this in a goroutine and discards the
// error after logging.
func (r *JudgeRunner) Run(ctx context.Context, task *persistence.Task) error {
	if r == nil || r.Judge == nil || task == nil {
		return nil
	}
	if r.Verdicts == nil {
		return nil
	}
	if existing, err := r.Verdicts.GetByTask(ctx, task.ID); err == nil && existing != nil {
		r.Logger.Debug().Str("task_id", task.ID).Msg("judge: verdict already recorded, skipping")
		if r.Metrics != nil {
			r.Metrics.JudgeEvaluationsTotal.WithLabelValues(task.ProjectID, "skipped_existing").Inc()
		}
		return nil
	} else if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		// Real DB error — proceed anyway so a transient outage
		// doesn't permanently skip the judge for this task.
		r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: verdict lookup failed, proceeding")
	}

	in := JudgeInput{Task: task}

	if r.Executions != nil {
		taskID := task.ID
		execs, err := r.Executions.List(ctx, persistence.ExecutionFilter{
			TaskID:   &taskID,
			PageSize: 1,
		})
		if err != nil {
			r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: execution lookup failed")
		} else if len(execs) > 0 {
			in.Execution = execs[0]
			if len(execs[0].Result) > 0 {
				// Last result text is the writer's summary in
				// most workflows. Pulled from the execution row's
				// Result column, which carries the most-recent
				// step's result.json.
				in.LastResultText = string(execs[0].Result)
			}
		}
	}

	if r.Audits != nil && in.Execution != nil {
		execID := in.Execution.ID
		entries, err := r.Audits.List(ctx, persistence.ToolAuditFilter{
			ExecutionID: &execID,
			PageSize:    100,
		})
		if err != nil {
			r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: audit fetch failed; proceeding with empty audit")
		} else {
			in.AuditEntries = entries
		}
	}
	if r.Artifacts != nil {
		taskID := task.ID
		arts, err := r.Artifacts.List(ctx, persistence.ArtifactFilter{
			TaskID:   &taskID,
			PageSize: 50,
		})
		if err != nil {
			r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: artifact fetch failed; proceeding with empty list")
		} else {
			in.Artifacts = arts
		}
	}

	verdict, metrics, err := r.Judge.Evaluate(ctx, in)
	if err != nil {
		r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: evaluation failed")
		if r.Metrics != nil {
			r.Metrics.JudgeEvaluationsTotal.WithLabelValues(task.ProjectID, "error").Inc()
		}
		return err
	}
	if verdict == nil {
		if r.Metrics != nil {
			r.Metrics.JudgeEvaluationsTotal.WithLabelValues(task.ProjectID, "abstain_no_config").Inc()
		}
		return nil
	}

	signalsBlob, _ := json.Marshal(verdict.Signals)
	if len(verdict.Signals) == 0 {
		signalsBlob = nil
	}
	roleTag := r.JudgeRoleTag
	if roleTag == "" {
		roleTag = "judge"
	}
	// Resolve the model label from the metrics first (the
	// response's reported model is what actually billed); fall
	// back to the LLMJudge's configured model when metrics
	// don't carry it (StubJudge, abstain-on-no-config).
	model := ""
	if metrics != nil && metrics.Model != "" {
		model = metrics.Model
	} else if lj, ok := r.Judge.(*LLMJudge); ok {
		model = lj.Model
	}
	// Compute the judge call's own cost. Skip when the pricing
	// table isn't wired or the metrics carry zero tokens (any
	// pre-LLM abstain path returns zero metrics). Going through
	// the pricing table — same one that powers worker/dispatcher
	// cost — keeps judge spend on the same axis as everything else
	// for the spend dashboard.
	costUSD := 0.0
	if metrics != nil && r.Pricing != nil && (metrics.PromptTokens > 0 || metrics.CompletionTokens > 0) {
		costUSD = r.Pricing.CostUSD(model, metrics.PromptTokens, metrics.CompletionTokens)
	}
	row := &persistence.TaskJudgeVerdict{
		ID:         persistence.GenerateID("judge"),
		ProjectID:  task.ProjectID,
		TaskID:     task.ID,
		Role:       roleTag,
		Model:      model,
		Verdict:    verdict.Decision,
		Confidence: verdict.Confidence,
		Signals:    signalsBlob,
		Summary:    verdict.Summary,
		CostUSD:    costUSD,
		RecordedAt: time.Now().UTC(),
	}
	if err := r.Verdicts.Record(ctx, row); err != nil {
		// ErrDuplicateKey is benign — the runner is idempotent
		// per task and a race that lands two writers on the
		// same task is a rare edge.
		if errors.Is(err, persistence.ErrDuplicateKey) {
			return nil
		}
		r.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("judge: verdict persist failed")
		return err
	}
	r.Logger.Info().
		Str("task_id", task.ID).
		Str("verdict", verdict.Decision).
		Float64("confidence", verdict.Confidence).
		Int("signals", len(verdict.Signals)).
		Float64("cost_usd", costUSD).
		Int("prompt_tokens", promptToksFromMetrics(metrics)).
		Int("completion_tokens", completionToksFromMetrics(metrics)).
		Msg("judge: verdict recorded")

	if r.Metrics != nil {
		r.Metrics.JudgeEvaluationsTotal.WithLabelValues(task.ProjectID, "ok").Inc()
		r.Metrics.JudgeVerdictsTotal.WithLabelValues(task.ProjectID, roleTag, verdict.Decision).Inc()
		r.Metrics.JudgeConfidence.WithLabelValues(task.ProjectID, roleTag, verdict.Decision).Observe(verdict.Confidence)
		if costUSD > 0 {
			r.Metrics.JudgeCostUSDTotal.WithLabelValues(task.ProjectID, roleTag, model).Add(costUSD)
		}
	}

	// Record the judge call against the same task_llm_usage
	// table the worker + dispatcher write to, with
	// source="judge". Skip when no LLM was actually called
	// (zero tokens) — there's nothing to bill, and a zero-token
	// row would clutter spend dashboards with "successful"
	// abstain-on-no-config rows that didn't cost anything.
	if r.LLMUsage != nil && metrics != nil && (metrics.PromptTokens > 0 || metrics.CompletionTokens > 0) {
		taskID := task.ID
		var execID *string
		if in.Execution != nil && in.Execution.ID != "" {
			id := in.Execution.ID
			execID = &id
		}
		usage := &persistence.TaskLLMUsage{
			ID:                  persistence.GenerateID("llm"),
			ProjectID:           task.ProjectID,
			TaskID:              &taskID,
			ExecutionID:         execID,
			StepID:              "judge",
			Role:                roleTag,
			Model:               model,
			PromptTokens:        int64(metrics.PromptTokens),
			CompletionTokens:    int64(metrics.CompletionTokens),
			Iterations:          1,
			CostUSD:             costUSD,
			Source:              persistence.TaskLLMUsageSourceJudge,
			RecordedAt:          time.Now().UTC(),
			CacheCreationTokens: int64(metrics.CacheCreationTokens),
			CacheReadTokens:     int64(metrics.CacheReadTokens),
		}
		if err := r.LLMUsage.Record(ctx, usage); err != nil {
			r.Logger.Warn().Err(err).Str("task_id", task.ID).
				Msg("judge: usage persist failed (verdict already recorded)")
		}
	}
	return nil
}

// promptToksFromMetrics / completionToksFromMetrics — small
// helpers so the verdict-logged log line stays readable when
// metrics is nil. Used only for log-line readability; runtime
// behaviour doesn't depend on them.
func promptToksFromMetrics(m *JudgeMetrics) int {
	if m == nil {
		return 0
	}
	return m.PromptTokens
}

func completionToksFromMetrics(m *JudgeMetrics) int {
	if m == nil {
		return 0
	}
	return m.CompletionTokens
}
