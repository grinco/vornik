package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunLLMExchangeSuite pins persistence.LLMExchangeRepository on both backends
// (llm-exchange record/replay design §3, §9): seq is assigned per
// (execution, step) and monotonic; both bodies round-trip byte-exact,
// non-ASCII and embedded newlines included; a nil iteration stays nil; an
// unrecorded step is an empty, non-nil list. Executions are seeded through
// their repositories because the row references them on Postgres.
func RunLLMExchangeSuite(t *testing.T, repo persistence.LLMExchangeRepository, execs persistence.ExecutionRepository, tasks persistence.TaskRepository) {
	t.Helper()
	ctx := context.Background()
	project := uniqueID("proj")
	exec := seedTerminalExecution(ctx, t, execs, tasks, project, persistence.ExecutionStatusRunning)
	other := seedTerminalExecution(ctx, t, execs, tasks, project, persistence.ExecutionStatusRunning)

	t.Run("Record_validation_and_defaults", func(t *testing.T) { llmExchangeRecordDefaults(ctx, t, repo, exec) })
	t.Run("Seq_is_per_step_and_bodies_round_trip", func(t *testing.T) { llmExchangeSeqAndRoundTrip(ctx, t, repo, exec, other) })
	t.Run("Unrecorded_step_is_empty_not_nil_and_CountByExecution", func(t *testing.T) { llmExchangeEmptyAndCount(ctx, t, repo, exec) })
}

func llmExchangeRecordDefaults(ctx context.Context, t *testing.T, repo persistence.LLMExchangeRepository, exec *persistence.Execution) {
	t.Helper()
	{
		if err := repo.Record(ctx, nil); err == nil {
			t.Error("Record(nil) must be refused")
		}
		if err := repo.Record(ctx, &persistence.LLMExchange{StepID: "plan"}); err == nil {
			t.Error("an exchange needs an execution id")
		}
		x := &persistence.LLMExchange{ExecutionID: exec.ID, StepID: "plan", RequestHash: "h", RequestJSON: "{}", ResponseJSON: "{}"}
		if err := repo.Record(ctx, x); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if x.ID == "" || x.RecordedAt.IsZero() || x.Seq != 1 {
			t.Errorf("Record must fill ID, RecordedAt and Seq=1 on a fresh step: %+v", x)
		}
	}
}

func llmExchangeSeqAndRoundTrip(ctx context.Context, t *testing.T, repo persistence.LLMExchangeRepository, exec, other *persistence.Execution) {
	t.Helper()
	{
		iter := 3
		at := time.Date(2026, 9, 5, 9, 0, 0, 0, time.UTC)
		body := "{\"messages\":[{\"role\":\"user\",\"content\":\"héllo\\nworld — “quoted”\"}],\"tools\":[]}"
		x := &persistence.LLMExchange{ExecutionID: exec.ID, StepID: "plan", Iteration: &iter, RequestHash: "sha-2",
			RequestJSON: body, ResponseJSON: `{"choices":[{"message":{"content":"ok\n"}}]}`,
			Model: "m", PromptTokens: 12, CompletionTokens: 3, DurationMs: 250, Redactions: 1, RecordedAt: at}
		if err := repo.Record(ctx, x); err != nil {
			t.Fatalf("Record: %v", err)
		}
		if x.Seq != 2 {
			t.Errorf("second exchange of the step gets seq 2, got %d", x.Seq)
		}
		y := &persistence.LLMExchange{ExecutionID: exec.ID, StepID: "review", RequestHash: "h3", RequestJSON: "{}", ResponseJSON: "{}"}
		if err := repo.Record(ctx, y); err != nil || y.Seq != 1 {
			t.Errorf("another step of the same execution starts at seq 1: %d %v", y.Seq, err)
		}
		z := &persistence.LLMExchange{ExecutionID: other.ID, StepID: "plan", RequestHash: "h4", RequestJSON: "{}", ResponseJSON: "{}"}
		if err := repo.Record(ctx, z); err != nil || z.Seq != 1 {
			t.Errorf("another execution's same step starts at seq 1: %d %v", z.Seq, err)
		}

		rows, err := repo.ListByStep(ctx, exec.ID, "plan")
		if err != nil {
			t.Fatalf("ListByStep: %v", err)
		}
		if len(rows) != 2 || rows[0].Seq != 1 || rows[1].Seq != 2 {
			t.Fatalf("ListByStep must return the step's rows in seq order: %+v", rows)
		}
		got := rows[1]
		if got.RequestJSON != body || got.ResponseJSON != x.ResponseJSON {
			t.Errorf("bodies must round-trip byte-exact: %q / %q", got.RequestJSON, got.ResponseJSON)
		}
		if got.Iteration == nil || *got.Iteration != 3 || got.RequestHash != "sha-2" || got.Model != "m" ||
			got.PromptTokens != 12 || got.CompletionTokens != 3 || got.DurationMs != 250 || got.Redactions != 1 || !got.RecordedAt.Equal(at) {
			t.Errorf("round trip lost a field: %+v", got)
		}
		if rows[0].Iteration != nil {
			t.Errorf("an absent iteration header stays nil: %+v", rows[0])
		}
	}
}

func llmExchangeEmptyAndCount(ctx context.Context, t *testing.T, repo persistence.LLMExchangeRepository, exec *persistence.Execution) {
	t.Helper()
	{
		rows, err := repo.ListByStep(ctx, exec.ID, uniqueID("ghost-step"))
		if err != nil || rows == nil || len(rows) != 0 {
			t.Errorf("unrecorded step: %v %v", rows, err)
		}
		if n, err := repo.CountByExecution(ctx, exec.ID); err != nil || n != 3 {
			t.Errorf("CountByExecution = %d %v, want 3", n, err)
		}
		if n, err := repo.CountByExecution(ctx, uniqueID("ghost-exec")); err != nil || n != 0 {
			t.Errorf("unknown execution counts 0: %d %v", n, err)
		}
	}
}
