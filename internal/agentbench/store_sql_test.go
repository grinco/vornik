package agentbench

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// Assembly is exercised against a real database rather than a fake, because the
// thing most likely to be wrong here is the SQL — a hand-rolled fake would
// happily agree with a query that no backend accepts.

func newLedgerDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema := []string{
		`CREATE TABLE task_llm_usage (
			task_id TEXT, execution_id TEXT, step_id TEXT, role TEXT, model TEXT,
			prompt_tokens INTEGER, completion_tokens INTEGER, cost_usd REAL)`,
		`CREATE TABLE execution_tool_grants (
			execution_id TEXT, step_id TEXT, role TEXT, requested_tools TEXT,
			accepted INTEGER, refused_tools TEXT, is_escalation INTEGER, created_at TEXT)`,
		`CREATE TABLE tool_audit_log (
			execution_id TEXT, step_id TEXT, tool_name TEXT, tool_output TEXT, created_at TEXT)`,
		`CREATE TABLE execution_step_outcomes (
			execution_id TEXT, step_id TEXT, role TEXT, model TEXT, outcome TEXT,
			error_class TEXT, effective_tool_budget INTEGER, tool_calls_used INTEGER,
			duration_ms INTEGER, recorded_at TEXT, agent_image_id TEXT)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db
}

func seedLedger(t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	exec(`INSERT INTO task_llm_usage VALUES ('t1','e1','s1','lead','m',1000,200,0.30)`)
	exec(`INSERT INTO task_llm_usage VALUES ('t1','e1','s2','worker','m',500,100,0.20)`)

	// The lead asked for four tools on s1 and the ceiling refused two.
	exec(`INSERT INTO execution_tool_grants VALUES
		('e1','s1','lead','["a","b","c","d"]',1,'["c","d"]',0,'2026-08-13T10:00:00Z')`)
	// Then escalated for one more, which was granted.
	exec(`INSERT INTO execution_tool_grants VALUES
		('e1','s1','lead','["c"]',1,'[]',1,'2026-08-13T10:01:00Z')`)

	exec(`INSERT INTO tool_audit_log VALUES ('e1','s1','a','{"result":"ok"}','2026-08-13T10:02:00Z')`)
	exec(`INSERT INTO tool_audit_log VALUES
		('e1','s1','made_up','{"isError":true,"error":"unknown tool: made_up"}','2026-08-13T10:03:00Z')`)

	exec(`INSERT INTO execution_step_outcomes VALUES
		('e1','s1','lead','m','schema_violation','shape',20,7,1500,'2026-08-13T10:04:00Z','sha256:aaa')`)
	exec(`INSERT INTO execution_step_outcomes VALUES
		('e1','s1','lead','m','ok','',20,7,2500,'2026-08-13T10:05:00Z','sha256:aaa')`)
	exec(`INSERT INTO execution_step_outcomes VALUES
		('e1','s2','worker','m','ok','',10,2,4000,'2026-08-13T10:06:00Z','sha256:bbb')`)
}

func assembleFixture(t *testing.T) (ExecutionRecord, []Trace) {
	t.Helper()
	db := newLedgerDB(t)
	seedLedger(t, db)

	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	rec, traces, err := store.Assemble(context.Background(), "t1", "e1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	sort.Slice(traces, func(i, j int) bool { return traces[i].StepID < traces[j].StepID })
	return rec, traces
}

func TestSQLTraceStore_SumsSpendAcrossAnExecutionsSteps(t *testing.T) {
	rec, _ := assembleFixture(t)

	if rec.CostUSD != 0.50 {
		t.Errorf("cost = %v, want 0.50 summed across both steps", rec.CostUSD)
	}
	if rec.PromptTokens != 1500 || rec.CompletionTokens != 300 {
		t.Errorf("tokens = %d/%d, want 1500/300", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.ToolCalls != 2 {
		t.Errorf("tool calls = %d, want 2", rec.ToolCalls)
	}
}

// Grants are scoped to (execution, step). Collapsing them would average a lead's
// per-step decisions into a number no decision corresponds to.
func TestSQLTraceStore_ReturnsOneTracePerStep(t *testing.T) {
	_, traces := assembleFixture(t)

	if len(traces) != 2 {
		t.Fatalf("traces = %d, want one per step", len(traces))
	}
	if traces[0].StepID != "s1" || traces[1].StepID != "s2" {
		t.Errorf("steps = %s,%s", traces[0].StepID, traces[1].StepID)
	}
	if traces[0].Role != "lead" || traces[1].Role != "worker" {
		t.Errorf("roles = %s,%s", traces[0].Role, traces[1].Role)
	}
}

// Accepted means "requested and not refused" — reading the requested list as
// granted would credit the lead with tools the ceiling denied.
func TestSQLTraceStore_AcceptedExcludesRefusedTools(t *testing.T) {
	_, traces := assembleFixture(t)
	s1 := traces[0]

	accepted := set(s1.Accepted)
	if !accepted["a"] || !accepted["b"] {
		t.Errorf("accepted = %v, want a and b", s1.Accepted)
	}
	if !accepted["c"] {
		t.Error("the escalated grant of c was not recorded as accepted")
	}
	if accepted["d"] {
		t.Error("a refused tool was recorded as granted")
	}
	if len(s1.Requested) != 5 {
		t.Errorf("requested = %v, want all five requests across both rows", s1.Requested)
	}
}

func TestSQLTraceStore_CountsEscalations(t *testing.T) {
	_, traces := assembleFixture(t)
	if traces[0].Escalations != 1 {
		t.Errorf("escalations = %d, want 1", traces[0].Escalations)
	}
}

func TestSQLTraceStore_ReadsFailedCallsAndKeepsThemOutOfInvoked(t *testing.T) {
	_, traces := assembleFixture(t)
	s1 := traces[0]

	if len(s1.Calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(s1.Calls))
	}
	var failed *ToolCall
	for i := range s1.Calls {
		if s1.Calls[i].Failed {
			failed = &s1.Calls[i]
		}
	}
	if failed == nil {
		t.Fatal("the failing call was recorded as successful")
	}
	if failed.ErrorText == "" {
		t.Error("the failure text was dropped, so the tool-use probe cannot classify it")
	}
	// A call that failed was not an invocation of that tool for grant purposes.
	if contains(s1.Invoked, "made_up") {
		t.Error("a failed call counted as an invocation, which would inflate grant precision")
	}
}

// The table records one row per outcome, not an attempt number: the second
// recorded outcome for a step IS its second attempt.
func TestSQLTraceStore_DerivesAttemptNumbersFromArrivalOrder(t *testing.T) {
	_, traces := assembleFixture(t)
	s1 := traces[0]

	if len(s1.Outcomes) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(s1.Outcomes))
	}
	if s1.Outcomes[0].Attempt != 1 || s1.Outcomes[1].Attempt != 2 {
		t.Errorf("attempts = %d,%d, want 1,2", s1.Outcomes[0].Attempt, s1.Outcomes[1].Attempt)
	}
	// And the schema probe reads that as "conformed, at the cost of one retry".
	v := SchemaProbe{}.ScoreSchema(s1, TaskRef{ID: "t1"})
	if v.SchemaConformance != 1.0 {
		t.Errorf("conformance = %v, want 1.0 — the step did produce usable output", v.SchemaConformance)
	}
	if v.RetriesToValid != 1 {
		t.Errorf("retries = %d, want 1", v.RetriesToValid)
	}
}

func TestSQLTraceStore_ReadsToolBudget(t *testing.T) {
	_, traces := assembleFixture(t)
	if traces[0].ToolBudget != 20 || traces[0].ToolCallsUsed != 7 {
		t.Errorf("budget = %d/%d, want 20/7", traces[0].ToolCallsUsed, traces[0].ToolBudget)
	}
}

func TestSQLTraceStore_ObservedAgentImagesUsesImmutableLedgerIDsAndRetainsDrift(t *testing.T) {
	db := newLedgerDB(t)
	seedLedger(t, db)
	store := &SQLTraceStore{DB: db, Dialect: SQLite}

	got, err := store.ObservedAgentImages(context.Background(), []string{"e1"})
	if err != nil {
		t.Fatalf("observed agent images: %v", err)
	}
	if got["lead"] != "sha256:aaa" || got["worker"] != "sha256:bbb" {
		t.Fatalf("observed images = %+v", got)
	}
	if _, err := db.Exec(`INSERT INTO execution_step_outcomes
		(execution_id,step_id,role,model,outcome,error_class,recorded_at,agent_image_id)
		VALUES ('e1','s3','lead','m','ok','','2026-08-13T10:07:00Z','sha256:ccc')`); err != nil {
		t.Fatal(err)
	}
	got, err = store.ObservedAgentImages(context.Background(), []string{"e1"})
	if err != nil {
		t.Fatal(err)
	}
	if got["lead"] != "sha256:aaa+sha256:ccc" {
		t.Fatalf("mid-run image drift was hidden: %+v", got)
	}
}

func TestSQLTraceStore_ObservedAgentImagesRefusesMissingProvenance(t *testing.T) {
	db := newLedgerDB(t)
	if _, err := db.Exec(`INSERT INTO execution_step_outcomes
		(execution_id,step_id,role,model,outcome,error_class,recorded_at)
		VALUES ('e1','s1','worker','m','ok','','2026-08-13T10:07:00Z')`); err != nil {
		t.Fatal(err)
	}
	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	if _, err := store.ObservedAgentImages(context.Background(), []string{"e1"}); err == nil ||
		!strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing immutable image was presented as a complete arm: %v", err)
	}
}

func TestSQLTraceStore_RefusesWithoutADatabase(t *testing.T) {
	var s *SQLTraceStore
	if _, _, err := s.Assemble(context.Background(), "t1", "e1"); err == nil {
		t.Fatal("assembled from a nil store")
	}
}

// An execution with no rows is an empty trace, not an error: a task that did
// nothing is a finding, and erroring would file it as a harness failure.
func TestSQLTraceStore_UnknownExecutionYieldsAnEmptyTrace(t *testing.T) {
	db := newLedgerDB(t)
	store := &SQLTraceStore{DB: db, Dialect: SQLite}

	rec, traces, err := store.Assemble(context.Background(), "nope", "nope")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(traces) != 0 || rec.CostUSD != 0 {
		t.Errorf("expected an empty assembly, got %d traces and $%v", len(traces), rec.CostUSD)
	}
}

// Biased toward NOT blaming the agent: an output merely mentioning an error is
// not a failed call, because inflating the invalid-call rate would report our
// parsing as the agent's problem.
func TestCallFailure_OnlyTreatsExplicitErrorsAsFailures(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"explicit isError", `{"isError":true,"error":"unknown tool"}`, true},
		{"error field", `{"error":"invalid arguments"}`, true},
		{"successful result", `{"result":"ok"}`, false},
		{"prose mentioning an error", `the build failed with an error`, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := callFailure(c.output)
			if got != c.want {
				t.Errorf("callFailure(%q) = %v, want %v", c.output, got, c.want)
			}
		})
	}
}

// The ledger, not the daemon, knows which executions a task produced. An earlier
// version read an execution_ids field that does not exist in the companion
// status payload, so every run assembled nothing and reported zeroes while
// claiming success — a benchmark that silently measures nothing.
func TestSQLTraceStore_ListsExecutionsForATask(t *testing.T) {
	db := newLedgerDB(t)
	if _, err := db.Exec(`CREATE TABLE executions (id TEXT, task_id TEXT, created_at TEXT)`); err != nil {
		t.Fatalf("create executions: %v", err)
	}
	for _, row := range []string{
		`INSERT INTO executions VALUES ('e1','t1','2026-08-13T10:00:00Z')`,
		`INSERT INTO executions VALUES ('e2','t1','2026-08-13T10:05:00Z')`,
		`INSERT INTO executions VALUES ('e9','other','2026-08-13T10:06:00Z')`,
	} {
		if _, err := db.Exec(row); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	store := &SQLTraceStore{DB: db, Dialect: SQLite}
	got, err := store.Executions(context.Background(), "t1")
	if err != nil {
		t.Fatalf("executions: %v", err)
	}
	if len(got) != 2 || got[0] != "e1" || got[1] != "e2" {
		t.Errorf("executions = %v, want [e1 e2] in creation order", got)
	}
}

func TestSQLTraceStore_ReadsThePinnedExecutionStateSnapshot(t *testing.T) {
	db := newLedgerDB(t)
	if _, err := db.Exec(`CREATE TABLE executions (id TEXT, state_snapshot TEXT)`); err != nil {
		t.Fatalf("create executions: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO executions VALUES ('exec-1', '{"stepResults":{}}')`); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	store := &SQLTraceStore{DB: db, Dialect: SQLite}

	got, err := store.StateSnapshot(context.Background(), "exec-1")
	if err != nil {
		t.Fatalf("StateSnapshot: %v", err)
	}
	if !strings.Contains(string(got), "stepResults") {
		t.Fatalf("snapshot = %s", got)
	}
}

func TestSQLTraceStore_ExecutionsRefusesWithoutADatabase(t *testing.T) {
	var s *SQLTraceStore
	if _, err := s.Executions(context.Background(), "t1"); err == nil {
		t.Fatal("listed executions from a nil store")
	}
}

// Regression, 2026-08-16. ExecutionRecord.DurationMS was declared, JSON-tagged
// and written by NOTHING: loadUsage read cost and tokens from task_llm_usage,
// which has no duration column, and loadOutcomes read execution_step_outcomes
// without selecting duration_ms. Every journal recorded 0, and the rollup
// reported a confident 0 ms/task that nobody could tell from a measurement.
//
// It surfaced only because the qwen-local-fixed arm's journal had durationMs=0
// on all 46 records — a benchmark that silently measures nothing is worse than
// one that fails, which is the same lesson the Executions docstring records.
func TestSQLTraceStore_SumsStepDurationsIntoTheExecutionRecord(t *testing.T) {
	db := newLedgerDB(t)
	seedLedger(t, db)
	store := &SQLTraceStore{DB: db, Dialect: SQLite}

	rec, traces, err := store.Assemble(context.Background(), "t1", "e1")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	// 1500 + 2500 + 4000. A SUM, not a max or a last-write-wins: the fixture
	// values are distinct so any of those would produce a different number.
	if rec.DurationMS != 8000 {
		t.Errorf("DurationMS = %d, want 8000 (the sum of every step's duration)", rec.DurationMS)
	}

	// Per-step durations survive too — the speed-aware timeout fit regresses
	// duration on completion tokens and tool calls PER STEP, and an
	// execution-level total cannot separate a slow model from a slow tool.
	var seen int
	for _, tr := range traces {
		for _, o := range tr.Outcomes {
			if o.DurationMS > 0 {
				seen++
			}
		}
	}
	if seen != 3 {
		t.Errorf("step outcomes carrying a duration = %d, want 3", seen)
	}
}
