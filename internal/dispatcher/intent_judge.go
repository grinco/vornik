package dispatcher

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/intentjudge"
	"vornik.io/vornik/internal/persistence"
)

// intentVerdictRepo is the narrow surface the dispatcher needs
// from persistence.IntentVerdictRepository. Decoupled so tests
// can supply a stub without the full repo surface.
type intentVerdictRepo interface {
	Insert(ctx context.Context, v *persistence.IntentVerdict) error
	UpdateLLMRefinement(ctx context.Context, v *persistence.IntentVerdict) error
}

// intentJudgeConfig holds the dispatcher's two-tier judge wiring.
// The heuristic tier runs sync before every tool call; the LLM
// refiner (when wired) runs async in a goroutine. Both verdicts
// persist to intent_verdicts for calibration analyses.
type intentJudgeConfig struct {
	// Repo persists verdicts. Required — without a repo the
	// verdicts have no home and the judge is useless.
	Repo intentVerdictRepo

	// Refiner is the async LLM tier. Optional — without it the
	// heuristic verdict is the only one persisted (Tier=heuristic,
	// LLM columns NULL).
	Refiner *intentjudge.LLMRefiner

	// RefineMinRisk gates which heuristic verdicts trigger LLM
	// refinement. "medium" (the default) skips low-risk calls
	// where the LLM cost wouldn't pay back. "critical" / "high"
	// further tighten; "low" refines every call (most expensive
	// but yields the best calibration dataset).
	RefineMinRisk intentjudge.Risk
}

// WithIntentJudge enables the two-tier intent judge. `repo` is
// required (verdicts persist here); `refiner` is optional (when
// nil, only the heuristic tier records).
//
// refineMinRisk gates which heuristic verdicts trigger async LLM
// refinement: only verdicts at this risk level or higher fire
// the LLM call. Default "medium" — skip low-risk no-op calls so
// the LLM cost only pays for ambiguous decisions.
func WithIntentJudge(repo intentVerdictRepo, refiner *intentjudge.LLMRefiner, refineMinRisk intentjudge.Risk) AgentOption {
	return func(a *Agent) {
		if refineMinRisk == "" {
			refineMinRisk = intentjudge.RiskMedium
		}
		a.intentJudge = &intentJudgeConfig{
			Repo:          repo,
			Refiner:       refiner,
			RefineMinRisk: refineMinRisk,
		}
	}
}

// evaluate fires the heuristic tier, inserts the row, and
// optionally spawns the async LLM refiner. Returns the heuristic
// Verdict so the caller (dispatcher tool loop) can log it. The
// return value is the verdict the dispatcher ACTS on — the LLM
// refinement lands after the fact.
//
// All persistence is best-effort: a DB hiccup logs a warning and
// returns the verdict anyway. The tool call still fires; we
// never block the dispatcher on the judge.
//
// `spawnAsync` is the goroutine launcher. Production passes nil
// (uses `go fn()`); tests pass a synchronous runner so the
// refinement assertion lands deterministically.
func (c *intentJudgeConfig) evaluate(
	ctx context.Context,
	projectID string,
	taskID, executionID *string,
	chatID *int64,
	toolName, argsJSON string,
	spawnAsync func(fn func()),
	logger zerolog.Logger,
) intentjudge.Verdict {
	if c == nil || c.Repo == nil {
		return intentjudge.Verdict{}
	}
	heuristic := intentjudge.EvaluateHeuristic(toolName, argsJSON)

	// Global / cross-project tool calls (e.g. list_projects) carry no
	// project context. The intent_verdict table is project-scoped
	// (project_id is NOT NULL — the repo rejects an empty id), so there
	// is nothing to attribute the row to. Skip persistence (and the
	// async refiner, which keys off the persisted row id) and return the
	// heuristic verdict — the tool call proceeds either way. Previously
	// this fell through to Insert and logged "intent judge: insert
	// failed: missing project id" on every such call (log noise + a
	// guaranteed-failing round-trip), not a real failure.
	if strings.TrimSpace(projectID) == "" {
		logger.Debug().Str("tool", toolName).
			Msg("intent judge: no project context; skipping verdict persistence")
		return heuristic
	}

	row := &persistence.IntentVerdict{
		ID:                      persistence.GenerateID("iv"),
		ProjectID:               projectID,
		TaskID:                  taskID,
		ExecutionID:             executionID,
		ChatID:                  chatID,
		ToolName:                toolName,
		ToolArgs:                argsJSON,
		HeuristicRisk:           string(heuristic.Risk),
		HeuristicConfidence:     heuristic.Confidence,
		HeuristicRecommendation: string(heuristic.Recommendation),
		HeuristicReasoning:      heuristic.Reasoning,
		HeuristicLatencyMs:      heuristic.LatencyMs,
		FinalRisk:               string(heuristic.Risk),
		FinalRecommendation:     string(heuristic.Recommendation),
		CreatedAt:               time.Now().UTC(),
	}
	// Persistence is best-effort — the tool call already needs
	// to fire whether the row lands or not. Insert runs sync
	// here because we need the ID to be visible to the async
	// refiner; the call is one round-trip.
	if err := c.Repo.Insert(ctx, row); err != nil {
		logger.Warn().Err(err).Str("tool", toolName).
			Msg("intent judge: insert failed")
		return heuristic
	}

	// Async LLM refinement, gated by RefineMinRisk. The
	// dispatcher's tool call has already fired by the time the
	// refinement lands; this is purely calibration data.
	if c.Refiner != nil && shouldRefine(heuristic.Risk, c.RefineMinRisk) {
		row := row // capture for the goroutine
		heuristic := heuristic
		runner := spawnAsync
		if runner == nil {
			runner = func(fn func()) { go fn() }
		}
		runner(func() {
			// Detach from the request context — refinement
			// outlives the tool call. 30s ceiling so a wedged
			// LLM call can't pile up goroutines forever.
			refineCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			refined, err := c.Refiner.Refine(refineCtx, toolName, argsJSON, heuristic)
			if err != nil {
				logger.Warn().Err(err).Str("tool", toolName).
					Msg("intent judge: LLM refinement failed")
				return
			}
			risk := string(refined.Risk)
			conf := refined.Confidence
			rec := string(refined.Recommendation)
			reason := refined.Reasoning
			lat := refined.LatencyMs
			model := c.Refiner.Model
			row.LLMRisk = &risk
			row.LLMConfidence = &conf
			row.LLMRecommendation = &rec
			row.LLMReasoning = &reason
			row.LLMLatencyMs = &lat
			row.LLMModel = &model
			if err := c.Repo.UpdateLLMRefinement(refineCtx, row); err != nil {
				logger.Warn().Err(err).Str("tool", toolName).
					Msg("intent judge: refinement upsert failed")
			}
		})
	}

	return heuristic
}

// shouldRefine reports whether a verdict at risk `r` should
// trigger async LLM refinement given the configured floor `min`.
func shouldRefine(r, min intentjudge.Risk) bool {
	rank := func(x intentjudge.Risk) int {
		switch x {
		case intentjudge.RiskCritical:
			return 4
		case intentjudge.RiskHigh:
			return 3
		case intentjudge.RiskMedium:
			return 2
		case intentjudge.RiskLow:
			return 1
		}
		return 0
	}
	return rank(r) >= rank(min)
}
