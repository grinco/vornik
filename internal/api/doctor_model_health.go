package api

// Doctor check: model_health.
//
// For every model a swarm role pins (`model` + `modelFallback`), compute a
// recent runtime health signal and flag models that are failing or producing
// degenerate output. This is the check that would have caught the dead
// `z-ai/glm-4.5-air:free` (100% step failure) and the local `qwen3.6:35b`
// (timeouts + empty output) before they silently degraded every task routed
// through the affected role.
//
// Data source — execution_step_outcomes + task_llm_usage:
//
//   - execution_step_outcomes is the daemon's purpose-built per-(role,model)
//     output-quality table (its own column comment: "model-effectiveness
//     metrics reflect real output quality per (role, model)"). The `outcome`
//     taxonomy distinguishes 'ok' from parse_error / schema_violation /
//     refused / degenerate_loop / timeout / failed — exactly the failure
//     shapes a bad model produces. This is the failure-rate signal.
//   - task_llm_usage carries completion_tokens per call; a model that times
//     out or returns empty output shows a degenerate (near-zero) median
//     completion-token count even when the step is later salvaged. This is
//     the degenerate-output signal that catches a model which "fails quietly"
//     rather than erroring outright.
//
// We deliberately do NOT use the Prometheus registry: it's process-lifetime
// and resets on restart, whereas the DB tables give a stable recent window
// that survives a daemon bounce — the right horizon for an operator running
// `vornikctl doctor` after noticing trouble.
//
// RECOMMEND, never auto-switch: a flagged model's finding names the role's
// configured modelFallback (or notes none is set). There is no --fix mutation
// — swapping a model under a live trading swarm is too risky to automate.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

const (
	// modelHealthWindow bounds how far back the recent-health query looks.
	modelHealthWindow = 24 * time.Hour
	// modelHealthRowCap bounds the per-model aggregation scan so a busy
	// deployment can't turn the check into a table sweep.
	modelHealthRowCap = 50000
	// modelHealthMinSamples is the smallest sample count we'll judge a model
	// on — below this, one or two bad calls shouldn't trip an alarm.
	modelHealthMinSamples = 5
	// modelHealthFailureRate is the recent step-failure fraction at/above
	// which a model is flagged.
	modelHealthFailureRate = 0.5
	// modelHealthDegenerateTokens is the median completion-token count below
	// which a model's output is considered degenerate (empty/near-empty).
	modelHealthDegenerateTokens = 10
)

// modelHealthStat is one model's recent aggregate runtime health.
type modelHealthStat struct {
	model                  string
	samples                int   // total recent steps observed for this model
	failures               int   // steps whose outcome was not 'ok'/'pending_validation'
	medianCompletionTokens int64 // median completion_tokens across recent calls
}

// modelHealthFinding is one flagged model with its severity + recommendation.
type modelHealthFinding struct {
	model   string
	status  string // WARNING | ERROR
	message string
}

// SetModelHealthSource overrides the recent-health data source. Optional —
// when unset, checkModelHealth uses the DB-backed query (or skips if there's
// no DB). Tests inject a fake to exercise the check without a database.
func (h *DoctorHandlers) SetModelHealthSource(src func(ctx context.Context) ([]modelHealthStat, error)) {
	if h == nil {
		return
	}
	h.modelHealthSource = src
}

// checkModelHealth flags swarm-role models with a poor recent health signal
// and RECOMMENDS each role's configured fallback. Read-only — no --fix.
func (h *DoctorHandlers) checkModelHealth(ctx context.Context) DoctorCheck {
	name := "model_health"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config dir; skipping"}
	}

	source := h.modelHealthSource
	if source == nil {
		if h.db == nil {
			return DoctorCheck{Name: name, Status: "OK", Message: "no health data source wired; skipping"}
		}
		source = h.queryModelHealthStats
	}

	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v", err)}
	}

	referenced, fallbacks := collectModelFallbacks(reg.ListSwarms())
	if len(referenced) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no role-pinned models to evaluate"}
	}

	stats, err := source(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("health query failed: %v", err)}
	}

	// Restrict to referenced models only — we don't alarm on models no role
	// uses (e.g. historical rows from a since-removed config).
	scoped := stats[:0:0]
	for _, s := range stats {
		if referenced[s.model] {
			scoped = append(scoped, s)
		}
	}

	findings := evalModelHealth(scoped, fallbacks)
	if len(findings) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("all %d role-pinned model(s) healthy over last %s", len(referenced), modelHealthWindow)}
	}

	worst := "WARNING"
	items := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.status == "ERROR" {
			worst = "ERROR"
		}
		items = append(items, fmt.Sprintf("[%s] %s", f.status, f.message))
	}
	return DoctorCheck{
		Name:    name,
		Status:  worst,
		Message: fmt.Sprintf("%d role-pinned model(s) degraded over last %s — review and consider the recommended fallback", len(findings), modelHealthWindow),
		Items:   items,
	}
}

// collectModelFallbacks returns the set of models referenced by any swarm role
// and a model→recommended-fallback map. A model can appear in several roles;
// the first non-empty fallback seen wins for the recommendation text. A model
// that appears only as a fallback is referenced (so it's evaluated) but has no
// fallback of its own modeled.
func collectModelFallbacks(swarms []*registry.Swarm) (referenced map[string]bool, fallbacks map[string]string) {
	referenced = map[string]bool{}
	fallbacks = map[string]string{}
	for _, s := range swarms {
		if s == nil {
			continue
		}
		for _, role := range s.Roles {
			if m := strings.TrimSpace(role.Model); m != "" {
				referenced[m] = true
				if _, ok := fallbacks[m]; !ok {
					fallbacks[m] = strings.TrimSpace(role.ModelFallback)
				}
			}
			if fb := strings.TrimSpace(role.ModelFallback); fb != "" {
				referenced[fb] = true
				if _, ok := fallbacks[fb]; !ok {
					fallbacks[fb] = ""
				}
			}
		}
	}
	return referenced, fallbacks
}

// evalModelHealth scores each stat and returns a finding per unhealthy model.
// Pure — directly unit-testable. fallbacks maps model → its recommended
// fallback ("" = none configured).
func evalModelHealth(stats []modelHealthStat, fallbacks map[string]string) []modelHealthFinding {
	var findings []modelHealthFinding
	for _, s := range stats {
		if s.samples < modelHealthMinSamples {
			continue
		}
		failRate := float64(s.failures) / float64(s.samples)
		degenerate := s.medianCompletionTokens < modelHealthDegenerateTokens
		highFail := failRate >= modelHealthFailureRate
		if !highFail && !degenerate {
			continue
		}

		var reasons []string
		status := "WARNING"
		if highFail {
			reasons = append(reasons, fmt.Sprintf("%.0f%% step-failure rate (%d/%d)", failRate*100, s.failures, s.samples))
			if failRate >= 0.9 {
				status = "ERROR"
			}
		}
		if degenerate {
			reasons = append(reasons, fmt.Sprintf("degenerate output (median %d completion tokens)", s.medianCompletionTokens))
		}

		rec := "no fallback configured — set modelFallback on the affected role(s)"
		if fb := fallbacks[s.model]; fb != "" {
			rec = fmt.Sprintf("recommend switching to configured modelFallback %q", fb)
		}
		findings = append(findings, modelHealthFinding{
			model:   s.model,
			status:  status,
			message: fmt.Sprintf("%s: %s; %s", s.model, strings.Join(reasons, ", "), rec),
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].model < findings[j].model })
	return findings
}

// queryModelHealthStats is the default DB-backed source: per-model recent
// failure counts (execution_step_outcomes) joined with median completion
// tokens (task_llm_usage), bounded by window + row cap. Postgres-only SQL
// (production); tests inject a fake so SQLite need not parse it.
func (h *DoctorHandlers) queryModelHealthStats(ctx context.Context) ([]modelHealthStat, error) {
	since := time.Now().Add(-modelHealthWindow)

	// Failure aggregation from the purpose-built outcomes table. 'ok' and
	// 'pending_validation' (not yet finalized) are NOT failures; everything
	// else in the outcome taxonomy is. Row-capped via a bounded subquery.
	outcomeRows, err := h.db.QueryContext(ctx, `
		SELECT model,
		       COUNT(*) AS samples,
		       COUNT(*) FILTER (WHERE outcome NOT IN ('ok', 'pending_validation')) AS failures
		FROM (
		    SELECT model, outcome
		    FROM execution_step_outcomes
		    WHERE recorded_at >= $1 AND model <> ''
		    ORDER BY recorded_at DESC
		    LIMIT $2
		) recent
		GROUP BY model
	`, since, modelHealthRowCap)
	if err != nil {
		return nil, fmt.Errorf("query step outcomes: %w", err)
	}
	defer func() { _ = outcomeRows.Close() }()

	statByModel := map[string]*modelHealthStat{}
	for outcomeRows.Next() {
		var model string
		var samples, failures int
		if err := outcomeRows.Scan(&model, &samples, &failures); err != nil {
			continue
		}
		statByModel[model] = &modelHealthStat{model: model, samples: samples, failures: failures}
	}
	if err := outcomeRows.Err(); err != nil {
		return nil, fmt.Errorf("scan step outcomes: %w", err)
	}

	// Median completion tokens per model from the usage table.
	tokenRows, err := h.db.QueryContext(ctx, `
		SELECT model,
		       PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY completion_tokens) AS median_completion
		FROM (
		    SELECT model, completion_tokens
		    FROM task_llm_usage
		    WHERE recorded_at >= $1 AND model <> ''
		    ORDER BY recorded_at DESC
		    LIMIT $2
		) recent
		GROUP BY model
	`, since, modelHealthRowCap)
	if err != nil {
		return nil, fmt.Errorf("query usage tokens: %w", err)
	}
	defer func() { _ = tokenRows.Close() }()

	for tokenRows.Next() {
		var model string
		var median sql.NullFloat64
		if err := tokenRows.Scan(&model, &median); err != nil {
			continue
		}
		st, ok := statByModel[model]
		if !ok {
			// A model with usage rows but no outcome rows: still track it so a
			// degenerate-token model that never reached the outcome finalizer
			// is visible. samples stays 0 → it's skipped by the min-sample
			// guard unless outcomes exist, which is the conservative default.
			st = &modelHealthStat{model: model}
			statByModel[model] = st
		}
		if median.Valid {
			st.medianCompletionTokens = int64(median.Float64)
		}
	}
	if err := tokenRows.Err(); err != nil {
		return nil, fmt.Errorf("scan usage tokens: %w", err)
	}

	out := make([]modelHealthStat, 0, len(statByModel))
	for _, st := range statByModel {
		out = append(out, *st)
	}
	return out, nil
}
