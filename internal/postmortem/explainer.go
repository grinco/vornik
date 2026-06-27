// Package postmortem turns a failed task into a one-paragraph
// operator-friendly explainer by joining step outcomes + tool
// audit + container-log tail and asking a small LLM to
// summarise. Idempotent per task — Generate caches the result
// in task_post_mortems so a re-render of the failed-task UI
// returns the cached row instead of burning another LLM call.
package postmortem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
)

// LogTailFetcher is the narrow interface the explainer needs
// for container-log access. The daemon's executor already
// satisfies it via its TaskLogs method (api.TaskLogSource);
// this re-typing keeps the postmortem package out of the api
// import cycle.
type LogTailFetcher interface {
	TaskLogs(ctx context.Context, taskID string, tail int) (string, error)
}

// Explainer composes the LLM provider + repos + log source
// into one Generate(taskID) call. Construction goes through
// New so a future per-project model override (mirroring the
// judge's LazyProjectJudge) can land without touching every
// caller.
type Explainer struct {
	Tasks       persistence.TaskRepository
	Executions  persistence.ExecutionRepository
	Outcomes    persistence.ExecutionStepOutcomeRepository
	Audits      persistence.ToolAuditRepository
	PostMortems persistence.TaskPostMortemRepository
	LLMUsage    UsageRecorder
	Logs        LogTailFetcher
	Chat        chat.Provider
	Model       string
	Pricing     *pricing.Table
	Logger      zerolog.Logger
	// MaxEvidenceBytes caps the user-message length the LLM
	// sees. Default 12 KiB — large enough for a typical 5-
	// step trace + 100 lines of container log, small enough
	// to keep the cost predictable on a small judge-tier
	// model. Configurable so an operator running on a richer
	// model can lift the cap.
	MaxEvidenceBytes int
	// LogTailLines bounds the container-log tail pulled into
	// the prompt. 100 lines is the BACKLOG-stipulated value;
	// most stack traces fit in 30, but giving the model the
	// retry's preamble too usually helps it spot the
	// proximate cause.
	LogTailLines int
}

// UsageRecorder is the narrow subset of
// persistence.TaskLLMUsageRepository the explainer needs for
// cost accounting. Same shape as the judge runner's
// UsageRecorder so the two share the muscle.
type UsageRecorder interface {
	Record(ctx context.Context, u *persistence.TaskLLMUsage) error
}

// Result is what Generate returns: the cached/just-written
// post-mortem and a flag for whether it came from cache. The
// HTTP handler uses Cached to decide whether to respond 200
// (fresh) or 304-equivalent (cached).
type Result struct {
	Cached     bool
	PostMortem *persistence.TaskPostMortem
}

// Generate produces (or returns the cached) post-mortem for
// taskID. forceRefresh skips the cache and re-runs the LLM —
// useful when the operator just kicked the task and wants the
// summary regenerated with the new evidence.
//
// Returns persistence.ErrNotFound when the task itself
// doesn't exist; ChatProvider errors propagate (the handler
// surfaces them as 502 to the operator).
func (e *Explainer) Generate(ctx context.Context, taskID string, forceRefresh bool) (*Result, error) {
	if e == nil || e.Tasks == nil || e.PostMortems == nil {
		return nil, fmt.Errorf("postmortem: explainer not configured")
	}
	if !forceRefresh {
		if existing, err := e.PostMortems.Get(ctx, taskID); err == nil && existing != nil {
			return &Result{Cached: true, PostMortem: existing}, nil
		}
	}
	if e.Chat == nil || e.Model == "" {
		return nil, fmt.Errorf("postmortem: chat provider or model not configured")
	}

	task, err := e.Tasks.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	if task == nil {
		return nil, persistence.ErrNotFound
	}

	evidence, executionID := e.gatherEvidence(ctx, task)
	prompt := postMortemPrompt
	user := evidence

	client := e.Chat
	if mo, ok := client.(chat.ModelOverridable); ok && e.Model != "" {
		client = mo.WithModel(e.Model)
	}
	resp, err := client.Complete(ctx, []chat.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: user},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 {
		return nil, fmt.Errorf("empty LLM response")
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return nil, fmt.Errorf("LLM returned empty summary")
	}

	model := e.Model
	if resp.Model != "" {
		// Prefer the response's reported model — when a
		// fallback fired or the router rerouted, the billed
		// model differs from what we configured.
		model = resp.Model
	}
	cost := 0.0
	if e.Pricing != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0) {
		cost = e.Pricing.CostUSD(model, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}

	pm := &persistence.TaskPostMortem{
		TaskID:           task.ID,
		ProjectID:        task.ProjectID,
		Summary:          summary,
		Model:            model,
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostUSD:          cost,
		RecordedAt:       time.Now().UTC(),
	}
	if err := e.PostMortems.Record(ctx, pm); err != nil {
		return nil, fmt.Errorf("persist post-mortem: %w", err)
	}

	if e.LLMUsage != nil && (resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0) {
		taskID := task.ID
		var execID *string
		if executionID != "" {
			execID = &executionID
		}
		usage := &persistence.TaskLLMUsage{
			ID:               persistence.GenerateID("llm"),
			ProjectID:        task.ProjectID,
			TaskID:           &taskID,
			ExecutionID:      execID,
			StepID:           "post_mortem",
			Role:             "post_mortem",
			Model:            model,
			PromptTokens:     int64(resp.Usage.PromptTokens),
			CompletionTokens: int64(resp.Usage.CompletionTokens),
			Iterations:       1,
			CostUSD:          cost,
			Source:           persistence.TaskLLMUsageSourcePostMortem,
			RecordedAt:       time.Now().UTC(),
		}
		if err := e.LLMUsage.Record(ctx, usage); err != nil {
			e.Logger.Warn().Err(err).Str("task_id", task.ID).Msg("postmortem: usage persist failed (summary already recorded)")
		}
	}

	e.Logger.Info().
		Str("task_id", task.ID).
		Str("model", model).
		Int("prompt_tokens", resp.Usage.PromptTokens).
		Int("completion_tokens", resp.Usage.CompletionTokens).
		Float64("cost_usd", cost).
		Msg("postmortem: explainer recorded")
	return &Result{Cached: false, PostMortem: pm}, nil
}

// gatherEvidence assembles the user-message body the LLM sees.
// Returns the evidence string and the execution_id that we
// pulled it from (for the LLMUsage row). Best-effort —
// missing repos / errors degrade to empty sections so the
// LLM still produces something useful even on a thin trace.
func (e *Explainer) gatherEvidence(ctx context.Context, task *persistence.Task) (string, string) {
	cap := e.MaxEvidenceBytes
	if cap <= 0 {
		cap = 12 * 1024
	}
	tail := e.LogTailLines
	if tail <= 0 {
		tail = 100
	}
	var b strings.Builder

	fmt.Fprintf(&b, "Task ID: %s\nProject: %s\nStatus: %s\nAttempt: %d/%d\n",
		task.ID, task.ProjectID, task.Status, task.Attempt, task.MaxAttempts,
	)
	if task.LastError != nil && *task.LastError != "" {
		fmt.Fprintf(&b, "Last error: %s\n", truncate(*task.LastError, 1000))
	}
	if task.LastErrorClass != nil && *task.LastErrorClass != "" {
		fmt.Fprintf(&b, "Error class: %s\n", *task.LastErrorClass)
	}
	if len(task.Payload) > 0 {
		var p map[string]any
		if json.Unmarshal(task.Payload, &p) == nil {
			if t, ok := p["taskType"].(string); ok {
				fmt.Fprintf(&b, "Type: %s\n", t)
			}
			if c, ok := p["context"].(map[string]any); ok {
				if pr, ok := c["prompt"].(string); ok {
					fmt.Fprintf(&b, "Original prompt: %s\n", truncate(pr, 800))
				}
			}
		}
	}

	executionID := ""
	if e.Executions != nil {
		taskID := task.ID
		execs, err := e.Executions.List(ctx, persistence.ExecutionFilter{
			TaskID:   &taskID,
			PageSize: 1,
		})
		if err == nil && len(execs) > 0 {
			ex := execs[0]
			executionID = ex.ID
			fmt.Fprintf(&b, "\nExecution: %s status=%s\n", ex.ID, ex.Status)
			if ex.ErrorMessage != nil && *ex.ErrorMessage != "" {
				fmt.Fprintf(&b, "Execution error: %s\n", truncate(*ex.ErrorMessage, 600))
			}
			if len(ex.CompletedSteps) > 0 {
				fmt.Fprintf(&b, "Completed steps: %s\n", strings.Join(ex.CompletedSteps, ", "))
			}
		}
	}

	if e.Outcomes != nil && executionID != "" {
		execID := executionID
		rows, err := e.Outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{
			ExecutionID: &execID,
			PageSize:    50,
		})
		if err == nil && len(rows) > 0 {
			// Verifier violations get a dedicated top-level section so
			// the LLM doesn't conflate the *required* numbers (from the
			// verifier's Violation.Detail) with the *aspirational*
			// numbers from the task's original prompt. Without this
			// split, "find the top 10 listings" in the prompt and
			// "≥5 list items" in the verifier compete; the LLM has
			// historically picked the prompt's wording and produced
			// operator-facing messages like "minimum 10 results" that
			// don't match what was actually checked.
			//
			// The detection heuristic matches the prefix Violation.Error()
			// emits in the executor — see internal/verifier/verifier.go.
			verifierLines := collectVerifierViolations(rows)
			if len(verifierLines) > 0 {
				b.WriteString("\nVERIFIER VIOLATIONS (use these — not the task prompt — for required thresholds):\n")
				for _, vl := range verifierLines {
					fmt.Fprintf(&b, "  - %s\n", truncate(vl, 400))
				}
			}

			b.WriteString("\nStep outcomes (most recent first):\n")
			for i := len(rows) - 1; i >= 0 && len(rows)-i <= 20; i-- {
				r := rows[i]
				if r == nil {
					continue
				}
				detail := strings.TrimSpace(r.ErrorDetail)
				if detail == "" {
					detail = r.Outcome
				}
				fmt.Fprintf(&b, "  - step=%s outcome=%s detail=%s\n",
					r.StepID, r.Outcome, truncate(detail, 200),
				)
			}
		}
	}

	if e.Audits != nil && executionID != "" {
		execID := executionID
		entries, err := e.Audits.List(ctx, persistence.ToolAuditFilter{
			ExecutionID: &execID,
			PageSize:    20,
		})
		if err == nil && len(entries) > 0 {
			b.WriteString("\nTool audit (last 20):\n")
			for i := len(entries) - 1; i >= 0; i-- {
				ent := entries[i]
				if ent == nil {
					continue
				}
				fmt.Fprintf(&b, "  [%s] in=%s out=%s\n",
					ent.ToolName,
					truncate(strings.TrimSpace(ent.ToolInput), 160),
					truncate(strings.TrimSpace(ent.ToolOutput), 160),
				)
			}
		}
	}

	if e.Logs != nil {
		log, err := e.Logs.TaskLogs(ctx, task.ID, tail)
		if err == nil && strings.TrimSpace(log) != "" {
			fmt.Fprintf(&b, "\nContainer log tail (last %d lines):\n", tail)
			b.WriteString(truncate(log, 4000))
			b.WriteString("\n")
		}
	}

	out := b.String()
	if len(out) > cap {
		out = out[:cap] + "\n…(evidence truncated)"
	}
	return out, executionID
}

// postMortemPrompt is the framing instruction the LLM gets.
// Asks for a single short paragraph so the operator gets a
// scannable explainer rather than a multi-page essay; the
// detail is in the per-step audit they can drill into via the
// rest of the UI. Explicitly forbids speculation so the model
// doesn't invent a root cause it has no evidence for.
const postMortemPrompt = `You are a post-mortem assistant for a multi-agent task system.

You will be given evidence about a task that failed:
  - The task's metadata (status, error message, original prompt).
  - Step outcomes — the per-step quality signal showing where
    things went wrong.
  - A truncated tool-call audit (tool names + truncated inputs/
    outputs for the most recent calls).
  - The last lines of the container's stderr/stdout log.

Write a SINGLE PARAGRAPH (3-5 sentences) explaining why the
task failed in operator-friendly language:
  1. What was the immediate symptom (the failure mode the
     operator sees: timeout, schema violation, gate failed,
     LLM error, hallucination block, etc).
  2. The proximate cause if visible from the evidence (which
     step, which tool, which assertion).
  3. A pointer for the operator's next action when one is
     obvious (e.g. "retry with iteration cap raised", "fix
     project prompt to require X", "the broker MCP was
     down — restart and retry").

GROUNDING RULES — must be followed:
  - When a VERIFIER VIOLATIONS section is present, any numeric
    threshold, minimum count, file pattern, or required field
    you mention MUST come from there, not from the task's
    original prompt. The prompt describes what the agent was
    asked to aim for; the verifier section describes what was
    actually checked. These often disagree (e.g. prompt says
    "find top 10", verifier requires "≥5") and operators need
    to know what tripped the gate, not what was aspirational.
  - Quote the verifier's failure detail verbatim or paraphrase
    closely. Do not invent thresholds that don't appear there.

Do NOT speculate beyond what the evidence shows. If the
evidence is too thin to identify a cause, say "the evidence
trail does not show why this failed; check the container log
in full for context" and stop. Do NOT include preamble like
"Here is the post-mortem:" — go straight to the explanation.`

// truncate clips a long string with an ellipsis so a single
// runaway field doesn't dominate the prompt budget.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// collectVerifierViolations scans step outcomes for entries whose
// detail looks like a verifier violation (the prefix the executor
// emits when joining failMsgs — see internal/verifier/verifier.go's
// Violation.Error()). Returns the deduplicated, newest-first list so
// the explainer can surface them in a dedicated section.
//
// We dedupe because the same verifier often fires across multiple
// adaptive-routing iterations — three rows with identical "verifier
// %q (%s): %s" detail would just bloat the prompt without adding
// signal.
func collectVerifierViolations(rows []*persistence.ExecutionStepOutcome) []string {
	seen := make(map[string]struct{})
	var out []string
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if r == nil {
			continue
		}
		detail := strings.TrimSpace(r.ErrorDetail)
		if detail == "" {
			continue
		}
		// One outcome row can carry multiple semicolon-joined
		// violations (RunAll joins them). Split so each lands on its
		// own bullet in the prompt.
		for _, line := range strings.Split(detail, ";") {
			line = strings.TrimSpace(line)
			if !looksLikeVerifierViolation(line) {
				continue
			}
			if _, dup := seen[line]; dup {
				continue
			}
			seen[line] = struct{}{}
			out = append(out, line)
		}
	}
	return out
}

// looksLikeVerifierViolation matches Violation.Error()'s output shape:
// `verifier "name" (type): detail` or `[warn] verifier "name" (type):
// detail`. Strict-enough that the postmortem doesn't accidentally
// route random ErrorDetail lines into the VERIFIER VIOLATIONS section.
func looksLikeVerifierViolation(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[warn] ")
	if !strings.HasPrefix(s, "verifier ") {
		return false
	}
	// Must have the (type) marker — distinguishes from any other line
	// that happens to start with "verifier ".
	if !strings.Contains(s, "(") || !strings.Contains(s, "):") {
		return false
	}
	return true
}
