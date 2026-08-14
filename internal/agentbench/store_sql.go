package agentbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// SQL trace assembly (§3.2).
//
// Every table read here already exists and is already written on the executor
// path. That was the load-bearing finding of the design: the benchmark needs no
// new instrumentation, only a join.
//
//	execution_tool_grants   requested / accepted / refused / is_escalation, append-only
//	tool_audit_log          one row per invocation, with its output
//	execution_step_outcomes outcome taxonomy, attempts, tool budget and calls used
//	task_llm_usage          cost_usd, prompt/completion tokens
//
// tool_audit_log keeps 30 days. That is why the runner scores in-run and the
// journal holds verdicts: a store that could be re-read later would make
// historical re-scoring look available when it is not.

// DBTX is the read surface this store needs. Narrow on purpose — the benchmark
// only ever reads, and a store holding something that could write would be one
// refactor away from a harness that mutates the deployment it is measuring.
type DBTX interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Dialect selects the placeholder syntax. ONE store serves both backends
// rather than two implementations that can drift — the same reasoning §6.1
// applies to the guard, applied to a much smaller thing: a benchmark that reads
// Postgres in production and SQLite in its tests must exercise the SAME query
// text, or the tests prove nothing about the query that runs.
type Dialect int

const (
	// Postgres is the production deployment's backend.
	Postgres Dialect = iota
	// SQLite is what the tests run against.
	SQLite
)

func (d Dialect) arg(n int) string {
	if d == SQLite {
		return "?"
	}
	return fmt.Sprintf("$%d", n)
}

// SQLTraceStore assembles traces from the ledger.
type SQLTraceStore struct {
	DB      DBTX
	Dialect Dialect
}

// NewSQLTraceStore constructs the store for the production backend.
func NewSQLTraceStore(db DBTX) *SQLTraceStore { return &SQLTraceStore{DB: db} }

// Executions lists the executions a task produced, newest last.
//
// The LEDGER is authoritative here, not the daemon. The companion status
// surface is deliberately narrow and does not report execution ids — an earlier
// version of this harness read a field that does not exist, assembled nothing,
// and reported a clean run with every figure zero. A benchmark that silently
// measures nothing is worse than one that fails.
func (s *SQLTraceStore) Executions(ctx context.Context, taskID string) ([]string, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("trace store has no database")
	}
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`
		SELECT id FROM executions WHERE task_id = %s ORDER BY created_at ASC`,
		s.Dialect.arg(1)), taskID)
	if err != nil {
		return nil, fmt.Errorf("list executions for %s: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan execution id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ObservedModels reports the role -> model map a run ACTUALLY used.
//
// OBSERVED, NOT DECLARED, and that distinction is the point. An arm names its
// models in configuration, but a router fallback can silently serve a different
// model — on a different provider — mid-run. Two runs would then key identically
// and be incomparable. Measured on this deployment: `glm-5.2` fell back to
// `zai.glm-5` on Bedrock for 473k tokens without anything recording that the arm
// had changed.
//
// membench applies the same rule to its embedder (ObservedEmbedder) for the same
// reason: a titan-versus-cohere comparison once matched clean because the key
// recorded what an operator declared rather than what ran.
func (s *SQLTraceStore) ObservedModels(ctx context.Context, executionIDs []string) (map[string]string, error) {
	if s == nil || s.DB == nil {
		return nil, fmt.Errorf("trace store has no database")
	}
	out := map[string]string{}
	for _, execID := range executionIDs {
		rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`
			SELECT role, model FROM task_llm_usage
			 WHERE execution_id = %s AND role <> '' AND model <> ''`,
			s.Dialect.arg(1)), execID)
		if err != nil {
			return nil, fmt.Errorf("read models for %s: %w", execID, err)
		}
		for rows.Next() {
			var role, model string
			if err := rows.Scan(&role, &model); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan model: %w", err)
			}
			// A role that served two models in one run is itself a finding: the
			// key records both, so such a run cannot silently match a clean one.
			prev, seen := out[role]
			switch {
			case !seen:
				out[role] = model
			case prev != model && !strings.Contains(prev, model):
				out[role] = prev + "+" + model
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// Assemble returns the execution's record and one trace per step.
//
// Per STEP, not per execution: grants are scoped to (execution_id, step_id), and
// collapsing them would average a lead's per-step decisions into a number no
// decision corresponds to.
func (s *SQLTraceStore) Assemble(ctx context.Context, taskID, executionID string) (ExecutionRecord, []Trace, error) {
	if s == nil || s.DB == nil {
		return ExecutionRecord{}, nil, fmt.Errorf("trace store has no database")
	}

	rec := ExecutionRecord{TaskID: taskID, ExecutionID: executionID}
	if err := s.loadUsage(ctx, &rec, taskID, executionID); err != nil {
		return ExecutionRecord{}, nil, err
	}

	byStep := map[string]*Trace{}
	step := func(stepID, role string) *Trace {
		t, ok := byStep[stepID]
		if !ok {
			t = &Trace{ExecutionID: executionID, StepID: stepID, Role: role}
			byStep[stepID] = t
		}
		if t.Role == "" {
			t.Role = role
		}
		return t
	}

	if err := s.loadGrants(ctx, executionID, step); err != nil {
		return ExecutionRecord{}, nil, err
	}
	if err := s.loadCalls(ctx, executionID, step, &rec); err != nil {
		return ExecutionRecord{}, nil, err
	}
	if err := s.loadOutcomes(ctx, executionID, step); err != nil {
		return ExecutionRecord{}, nil, err
	}

	traces := make([]Trace, 0, len(byStep))
	for _, t := range byStep {
		traces = append(traces, *t)
	}
	return rec, traces, nil
}

// loadUsage sums the execution's spend. Cost is per (task, execution, step,
// role, model), so an execution's figure is the sum across its steps.
func (s *SQLTraceStore) loadUsage(ctx context.Context, rec *ExecutionRecord, taskID, executionID string) error {
	row := s.DB.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(cost_usd), 0), COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0)
		  FROM task_llm_usage
		 WHERE task_id = %s AND execution_id = %s`, s.Dialect.arg(1), s.Dialect.arg(2)),
		taskID, executionID)
	if err := row.Scan(&rec.CostUSD, &rec.PromptTokens, &rec.CompletionTokens); err != nil {
		return fmt.Errorf("read usage for %s: %w", executionID, err)
	}
	return nil
}

// loadGrants reads the append-only grant trail.
//
// The CURRENT grant for a step is the newest accepted row; everything older is
// audit. Escalations are counted across all rows, because an escalation is a
// decision that happened whether or not it was the last one.
func (s *SQLTraceStore) loadGrants(ctx context.Context, executionID string, step func(string, string) *Trace) error {
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`
		SELECT step_id, role, requested_tools, accepted, refused_tools, is_escalation
		  FROM execution_tool_grants
		 WHERE execution_id = %s
		 ORDER BY created_at ASC`, s.Dialect.arg(1)), executionID)
	if err != nil {
		return fmt.Errorf("read grants for %s: %w", executionID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var stepID, role string
		var requestedJSON, refusedJSON []byte
		var accepted, escalation bool
		if err := rows.Scan(&stepID, &role, &requestedJSON, &accepted, &refusedJSON, &escalation); err != nil {
			return fmt.Errorf("scan grant: %w", err)
		}
		requested, err := decodeStrings(requestedJSON)
		if err != nil {
			return fmt.Errorf("decode requested_tools: %w", err)
		}
		refused, err := decodeStrings(refusedJSON)
		if err != nil {
			return fmt.Errorf("decode refused_tools: %w", err)
		}

		t := step(stepID, role)
		t.Requested = append(t.Requested, requested...)
		t.Refused = append(t.Refused, refused...)
		if accepted {
			// Accepted rows carry the tools that were granted: everything
			// requested and not refused.
			t.Accepted = append(t.Accepted, subtract(requested, refused)...)
		}
		if escalation {
			t.Escalations++
		}
	}
	return rows.Err()
}

// loadCalls reads the invocation log.
func (s *SQLTraceStore) loadCalls(ctx context.Context, executionID string, step func(string, string) *Trace, rec *ExecutionRecord) error {
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`
		SELECT step_id, tool_name, tool_output
		  FROM tool_audit_log
		 WHERE execution_id = %s
		 ORDER BY created_at ASC`, s.Dialect.arg(1)), executionID)
	if err != nil {
		return fmt.Errorf("read tool calls for %s: %w", executionID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var stepID, toolName, output string
		if err := rows.Scan(&stepID, &toolName, &output); err != nil {
			return fmt.Errorf("scan tool call: %w", err)
		}
		t := step(stepID, "")
		failed, errText := callFailure(output)
		t.Calls = append(t.Calls, ToolCall{
			Name: toolName, Role: t.Role, Failed: failed, ErrorText: errText,
		})
		if !failed {
			t.Invoked = append(t.Invoked, toolName)
		}
		rec.ToolCalls++
	}
	return rows.Err()
}

// loadOutcomes reads the per-step outcome taxonomy plus the tool budget.
func (s *SQLTraceStore) loadOutcomes(ctx context.Context, executionID string, step func(string, string) *Trace) error {
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`
		SELECT step_id, role, model, outcome,
		       COALESCE(error_class, ''), COALESCE(effective_tool_budget, 0),
		       COALESCE(tool_calls_used, 0)
		  FROM execution_step_outcomes
		 WHERE execution_id = %s
		 ORDER BY recorded_at ASC`, s.Dialect.arg(1)), executionID)
	if err != nil {
		return fmt.Errorf("read step outcomes for %s: %w", executionID, err)
	}
	defer func() { _ = rows.Close() }()

	// The table records one row per outcome, not an attempt number. Attempts are
	// therefore derived from arrival order within a step — the second recorded
	// outcome for a step IS its second attempt.
	attempts := map[string]int{}
	for rows.Next() {
		var stepID, role, model, outcome, errorClass string
		var budget, used int
		if err := rows.Scan(&stepID, &role, &model, &outcome, &errorClass, &budget, &used); err != nil {
			return fmt.Errorf("scan step outcome: %w", err)
		}
		attempts[stepID]++
		t := step(stepID, role)
		t.Outcomes = append(t.Outcomes, StepOutcome{
			StepID:     stepID,
			Role:       role,
			Model:      model,
			Outcome:    outcome,
			Attempt:    attempts[stepID],
			ErrorClass: errorClass,
		})
		if budget > 0 {
			t.ToolBudget = budget
		}
		if used > 0 {
			t.ToolCallsUsed = used
		}
		if outcome == OutcomeIterationExhausted {
			t.Stalled = true
		}
	}
	return rows.Err()
}

// callFailure reads a recorded tool output for evidence the call failed.
//
// tool_audit_log stores the output verbatim rather than a status, so failure is
// inferred from the text. That is lossy, and deliberately biased toward NOT
// blaming the agent: an output that merely mentions an error is not scored as a
// failed call, because inflating the invalid-call rate would make the tool-use
// probe report a problem that is really our parsing.
func callFailure(output string) (bool, string) {
	if output == "" {
		return false, ""
	}
	var probe struct {
		Error   string `json:"error"`
		IsError *bool  `json:"isError"`
	}
	if err := json.Unmarshal([]byte(output), &probe); err == nil {
		if probe.IsError != nil && *probe.IsError {
			return true, probe.Error
		}
		if probe.Error != "" {
			return true, probe.Error
		}
	}
	return false, ""
}

// subtract returns the elements of a that are not in b.
func subtract(a, b []string) []string {
	drop := set(b)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

// decodeStrings reads a JSONB string array, tolerating SQL NULL as empty.
func decodeStrings(blob []byte) ([]string, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, err
	}
	return out, nil
}
