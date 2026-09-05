package api

// Doctor check: fallback_rungs.
//
// A model fallback that dies before it can attempt inference is not a fallback.
//
// MEASURED 2026-08-26, outreach-discover over 30 days — every
// `discover_model_fallback` hop on `gemma4:26b`:
//
//	discover_model_fall…  scout  gemma4:26b  failed=1  0.9s  container_non_zero_exit
//	discover_model_fall…  scout  gemma4:26b  failed=1  0.8s  container_non_zero_exit
//	discover_model_fall…  scout  gemma4:26b  failed=1  1.0s  container_non_zero_exit
//	discover_model_fall…  scout  gemma4:26b  failed=1  0.0s  container_non_zero_exit
//
// 4 of 4, all sub-second. Re-measured 2026-09-03: same rung, now classified
// `model_unhealthy`, and the reason is plain — `MODEL_UNHEALTHY: model
// "gemma4:26b" on route "agent" circuit open (open since 2026-08-23T01:54:15)`.
// The sub-second exits were the breaker refusing INSTANTLY, which is why they
// never looked like inference. So the `scout` role has had a fallback rung that
// has not worked once in eleven days, and the ladder silently walks past it to
// the next hop.
//
// WHY THE EXISTING CHECK CANNOT SEE THIS. `agent_model_circuits` reads the LIVE
// in-memory breaker, so it answers "is this circuit open right now". After a
// daemon restart every breaker starts closed and only reopens on the next
// attempt, so a rung that has failed every attempt for eleven days can read
// perfectly healthy between the restart and the next call. Live state is not
// history.
//
// So this check reads the LEDGER instead: a fallback step with attempts and no
// successes over the window. That is the same principle `connector_auth` states
// for credentials — a configured value is a statement of intent, never evidence
// of behaviour. A configured fallback rung is intent; `execution_step_outcomes`
// is behaviour.
//
// Backlog: "P2 — The gemma4 model fallback fails 4 of 4 in under a second, so
// that rung never runs (2026-08-26)".

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	// fallbackRungWindow is how far back rung outcomes are counted. 30 days
	// because a fallback rung is by definition rare — it runs only when the
	// primary already failed — so a short window would report "no data" for a
	// rung that is genuinely dead and simply not reached often.
	fallbackRungWindow = 30 * 24 * time.Hour
	// fallbackRungMinAttempts is how many attempts a rung needs before a
	// zero-success record means anything. Two: one failure is a bad day, and
	// waiting for more than two on a path this rare would mean never reporting.
	fallbackRungMinAttempts = 2
)

// unreachedInferenceClasses are the failure classes that mean the rung never
// got to make its model call — image, env, entrypoint, a missing mount, or a
// breaker refusing before the request.
//
// THE DISTINCTION IS THE WHOLE CHECK, and getting it wrong would rebuild the
// defect this doctor exists to report. A rung that RAN and was then judged —
// hallucinated_claim, verifier_warn, prompt_token_budget, context_timeout,
// plausibility_violation — is a rung that works and produced bad output. That is
// a quality problem for a different surface. Counting it here would report
// thirteen "dead" rungs on a deployment that has one or two, and a check that
// cries wolf is one that gets muted — which is how the original 4-of-4 went
// unnoticed for eleven days in the first place.
//
// Anything unrecognised is EXCLUDED rather than included: a new class this list
// has not learned about is more likely to be a new way of running badly than a
// new way of not running at all, and a false ERROR costs more than a missed one
// on a check whose whole value is that it is quiet until it is not.
var unreachedInferenceClasses = []string{
	"model_unhealthy",
	"container_non_zero_exit",
	"container_start_failed",
	"container_killed",
	"container_wait_failed",
	"llm_call_failed",
	"missing_prerequisite",
}

// deadFallbackRung is one fallback step that has never succeeded in the window.
type deadFallbackRung struct {
	stepID     string
	role       string
	model      string
	attempts   int
	lastClass  string
	lastFailed time.Time
}

// checkFallbackRungs reports configured fallback rungs that have attempted and
// never once succeeded.
func (h *DoctorHandlers) checkFallbackRungs(ctx context.Context) DoctorCheck {
	const name = "fallback_rungs"
	if h == nil || h.db == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no database; skipping"}
	}

	rungs, err := h.queryDeadFallbackRungs(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf("fallback rung query failed: %v", err)}
	}
	items, status, message := evaluateFallbackRungs(rungs)
	return DoctorCheck{Name: name, Status: status, Message: message, Items: items}
}

// evaluateFallbackRungs turns the query result into the check's verdict.
//
// Split from the query so the DECISION is testable without a database — the
// same split connector_auth uses, and for the same reason: the interesting part
// of a doctor check is what it concludes, not how it reads rows.
func evaluateFallbackRungs(rungs []deadFallbackRung) (items []string, status, message string) {
	if len(rungs) == 0 {
		return nil, "OK", "no fallback rung has failed before inference on every attempt in the last 30 days"
	}
	items = make([]string, 0, len(rungs))
	models := map[string]int{}
	for _, r := range rungs {
		class := r.lastClass
		if class == "" {
			class = "unclassified"
		}
		role := r.role
		if role == "" {
			role = "—"
		}
		items = append(items, fmt.Sprintf(
			"[ERROR] %s (role %s, model %s) — %d attempts, 0 reached inference in 30d; last failed %s as %s",
			r.stepID, role, r.model, r.attempts,
			r.lastFailed.Format(time.RFC3339), class))
		if r.model != "" {
			models[r.model]++
		}
	}
	message = fmt.Sprintf(
		"%d fallback rung(s) never reached inference — the ladder is walking past them, "+
			"so the retry budget is spent on hops that cannot work", len(rungs))
	// Name the model when one accounts for every dead rung. That is the actual
	// fault — several rungs failing is usually ONE model being unavailable, and
	// pointing at the model is what turns a list into an action.
	if len(models) == 1 {
		for m := range models {
			message += fmt.Sprintf("; all of them on %q, so the model is the fault rather than the rungs", m)
		}
	}
	return items, "ERROR", message
}

// queryDeadFallbackRungs finds fallback steps with attempts and no successes.
//
// Matched on the step id rather than a step TYPE because a fallback rung is a
// naming convention in the workflow, not a schema concept: the executor writes
// `<step>_model_fallback` and `<step>_infra_retryN` when the ladder synthesises
// a hop. Reading the convention here is honest about what it is — a heuristic
// over the ledger — and it is the only thing that distinguishes a rung from an
// ordinary step in this table.
func (h *DoctorHandlers) queryDeadFallbackRungs(ctx context.Context) ([]deadFallbackRung, error) {
	// PORTABLE BY CONSTRUCTION (doctor design, Extension 2026-09-04 E2/E2.1).
	// This query had four Postgres-only constructs and shipped with no
	// database-backed test, so on every SQLite deployment the check reported a
	// driver error as a WARNING — except for the LIKE, which reported nothing
	// at all, which was worse. Each rewrite is equivalent, not merely similar:
	//
	//   COUNT(*) FILTER (WHERE p)  →  SUM(CASE WHEN p THEN 1 ELSE 0 END)
	//       exact for integer counts.
	//   = ANY($3) + pq.Array       →  IN (…) built from the slice
	//       same set membership, no array type needed.
	//   (ARRAY_AGG(x ORDER BY t DESC))[1]  →  correlated SELECT … LIMIT 1
	//       equivalent HERE because the aggregate sits behind
	//       HAVING COUNT(*) >= $2 with $2 >= 1, so no empty group reaches the
	//       output and [1] always indexes a real element. The correlation
	//       compares COALESCE(role,'') rather than role so it reproduces
	//       GROUP BY's NULL grouping (role = NULL never matches). Ties on
	//       recorded_at are arbitrary in both forms — unchanged, not improved.
	//   LIKE '%\_model\_fallback%'  →  the same pattern + ESCAPE '\'
	//       Postgres treats \ as LIKE's default escape; SQLite has NO default
	//       escape character, so there the pattern asked for a literal
	//       backslash and matched nothing. Measured 2026-09-04: 0 rows without
	//       the clause, the right row with it. Both drivers accept ESCAPE.
	classPlaceholders := make([]string, 0, len(unreachedInferenceClasses))
	args := []any{time.Now().Add(-fallbackRungWindow), fallbackRungMinAttempts}
	for _, c := range unreachedInferenceClasses {
		args = append(args, c)
		classPlaceholders = append(classPlaceholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT o.step_id,
		       COALESCE(o.role, ''),
		       COALESCE(o.model, ''),
		       COUNT(*)                                              AS attempts,
		       SUM(CASE WHEN o.outcome = 'ok' THEN 1 ELSE 0 END)     AS successes,
		       MAX(o.recorded_at)                                    AS last_at,
		       (SELECT COALESCE(i.error_class, '')
		          FROM execution_step_outcomes i
		         WHERE i.step_id = o.step_id
		           AND COALESCE(i.role, '') = COALESCE(o.role, '')
		           AND COALESCE(i.model, '') = COALESCE(o.model, '')
		           AND i.recorded_at >= $1
		         ORDER BY i.recorded_at DESC
		         LIMIT 1)                                            AS last_class
		  FROM execution_step_outcomes o
		 WHERE o.recorded_at >= $1
		   AND o.step_id LIKE '%\_model\_fallback%' ESCAPE '\'
		 GROUP BY o.step_id, o.role, o.model
		HAVING COUNT(*) >= $2
		   AND SUM(CASE WHEN o.outcome = 'ok' THEN 1 ELSE 0 END) = 0
		   -- EVERY attempt must have failed before inference. A rung with even
		   -- one judged failure demonstrably reaches the model, so it is not
		   -- dead; it is producing bad output, which is a different surface.
		   AND COUNT(*) = SUM(CASE WHEN o.error_class IN (`+strings.Join(classPlaceholders, ", ")+`) THEN 1 ELSE 0 END)
		 ORDER BY COUNT(*) DESC, o.step_id`,
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []deadFallbackRung
	for rows.Next() {
		var r deadFallbackRung
		var successes int
		// MAX(recorded_at) comes back as a time.Time on Postgres and as a
		// STRING on SQLite (the sqlite layer stores RFC3339Nano text), so a
		// sql.NullTime destination fails to scan there — a portability break
		// in the RESULT rather than the SQL, and the one the driver-portability
		// test caught after the query itself was fixed. `any` accepts both and
		// timestampValue normalises.
		var last any
		var lastClass sql.NullString
		if err := rows.Scan(&r.stepID, &r.role, &r.model, &r.attempts, &successes, &last, &lastClass); err != nil {
			return nil, err
		}
		r.lastFailed = timestampValue(last)
		r.lastClass = lastClass.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// timestampValue normalises a scanned timestamp across drivers: Postgres hands
// back a time.Time, SQLite the RFC3339Nano text its repositories write. A value
// it cannot read yields the zero time, which every caller here renders as "no
// timestamp" rather than failing the check over a display field.
func timestampValue(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
	case []byte:
		return timestampValue(string(t))
	}
	return time.Time{}
}
