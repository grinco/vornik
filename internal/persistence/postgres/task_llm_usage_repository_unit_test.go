package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// newUsageRepo wires a TaskLLMUsageRepository against a sqlmock-backed
// *sql.DB. Returns the repo, the mock controller, and a cleanup func.
func newUsageRepo(t *testing.T) (*TaskLLMUsageRepository, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewTaskLLMUsageRepository(db), mock, func() { _ = db.Close() }
}

func strPtr(s string) *string { return &s }

// recentTimeMatcher accepts any time.Time arg that is not before
// notBefore. Lets tests assert "the repo defaulted to ~now" without
// snapshotting an exact wall-clock value.
type recentTimeMatcher struct{ notBefore time.Time }

func (m recentTimeMatcher) Match(v driver.Value) bool {
	t, ok := v.(time.Time)
	if !ok {
		return false
	}
	return !t.Before(m.notBefore)
}

func TestUsageRecord_NilRow(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	if err := repo.Record(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "nil usage row") {
		t.Fatalf("expected nil usage row error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("did not expect any DB call: %v", err)
	}
}

// TestUsageRecord_HappyPath_ArgsAndSQL drives the canonical INSERT
// statement and asserts that every column lands in the right
// placeholder slot. Argument transposition is the kind of bug that
// type-checks cleanly but corrupts cost data in production, so the
// guard sits at the repo boundary.
func TestUsageRecord_HappyPath_ArgsAndSQL(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 10, 30, 0, 0, time.UTC)
	u := &persistence.TaskLLMUsage{
		ID:               "u-1",
		ProjectID:        "p-1",
		TaskID:           strPtr("t-1"),
		ExecutionID:      strPtr("e-1"),
		StepID:           "s-1",
		Role:             "worker",
		Model:            "claude-opus-4-7",
		PromptTokens:     100,
		CompletionTokens: 50,
		Iterations:       2,
		CostUSD:          1.23,
		Source:           persistence.TaskLLMUsageSourceWorkflowStep,
		SessionID:        strPtr("sess-1"),
		RecordedAt:       recorded,
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_llm_usage")).
		WithArgs(
			"u-1", "p-1", "t-1", "e-1", "s-1",
			"worker", "claude-opus-4-7", int64(100), int64(50), 2,
			1.23, persistence.TaskLLMUsageSourceWorkflowStep, "sess-1", recorded,
			int64(0), int64(0), // cache_creation_tokens, cache_read_tokens (phase A)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), u); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUsageRecord_DefaultsSourceAndRecordedAt verifies that the
// repo fills in workflow_step + a non-zero recorded_at when the
// caller leaves them blank (executor/artifacts.go relies on this).
// The defaulting writes a local variable, not the caller's struct,
// so verification has to ride along on the SQL arg list.
func TestUsageRecord_DefaultsSourceAndRecordedAt(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{
		ID:        "u-2",
		ProjectID: "p-2",
		StepID:    "s-2",
		Role:      "worker",
		Model:     "claude",
		// Source and RecordedAt deliberately omitted.
	}

	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_llm_usage")).
		WithArgs(
			"u-2", "p-2", nil, nil, "s-2",
			"worker", "claude", int64(0), int64(0), 0,
			0.0, persistence.TaskLLMUsageSourceWorkflowStep, nil,
			recentTimeMatcher{notBefore: before},
			int64(0), int64(0), // cache_creation_tokens, cache_read_tokens (phase A)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), u); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUsageRecord_NullableStringEmptyAndNil locks in the
// contract that an empty string and a nil pointer both bind
// to SQL NULL. Dispatcher rows (no task_id / no execution_id)
// depend on this so their columns can stay nullable.
func TestUsageRecord_NullableStringEmptyAndNil(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	empty := ""
	u := &persistence.TaskLLMUsage{
		ID:         "u-3",
		ProjectID:  "p-3",
		TaskID:     &empty, // empty string -> NULL
		StepID:     "s-3",
		Role:       "dispatcher",
		Model:      "claude",
		Source:     persistence.TaskLLMUsageSourceDispatcher,
		RecordedAt: time.Now().UTC(),
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_llm_usage")).
		WithArgs(
			"u-3", "p-3",
			nil, // task_id (empty string -> NULL)
			nil, // execution_id (nil pointer -> NULL)
			"s-3", "dispatcher", "claude",
			int64(0), int64(0), 0,
			0.0, persistence.TaskLLMUsageSourceDispatcher,
			nil, // session_id (nil pointer -> NULL)
			sqlmock.AnyArg(),
			int64(0), int64(0), // cache_creation_tokens, cache_read_tokens (phase A)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Record(context.Background(), u); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUsageRecord_MapsDuplicateKey confirms that a pq.Error with
// code 23505 surfaces as persistence.ErrDuplicateKey rather than
// a raw driver error. Callers depend on the sentinel to dedupe
// retries from genuine failures.
func TestUsageRecord_MapsDuplicateKey(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{
		ID:         "u-dup",
		ProjectID:  "p-1",
		StepID:     "s-1",
		Role:       "worker",
		Model:      "claude",
		RecordedAt: time.Now().UTC(),
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_llm_usage")).
		WillReturnError(&pq.Error{Code: "23505"})

	err := repo.Record(context.Background(), u)
	if !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
}

// TestUsageRecord_PropagatesGenericError covers the non-pq error
// path through mapDBError (no special translation, returns as-is).
func TestUsageRecord_PropagatesGenericError(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{
		ID:         "u-err",
		ProjectID:  "p-1",
		StepID:     "s-1",
		Role:       "worker",
		Model:      "claude",
		RecordedAt: time.Now().UTC(),
	}

	dbErr := errors.New("connection reset")
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO task_llm_usage")).
		WillReturnError(dbErr)

	err := repo.Record(context.Background(), u)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected raw DB error, got %v", err)
	}
}

func TestUsageUpsert_NilRow(t *testing.T) {
	repo, _, cleanup := newUsageRepo(t)
	defer cleanup()

	if err := repo.Upsert(context.Background(), nil); err == nil ||
		!strings.Contains(err.Error(), "nil usage row") {
		t.Fatalf("expected nil usage row error, got %v", err)
	}
}

func TestUsageUpsert_RequiresID(t *testing.T) {
	repo, _, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{ProjectID: "p-1", StepID: "s-1"}
	if err := repo.Upsert(context.Background(), u); err == nil ||
		!strings.Contains(err.Error(), "id is required") {
		t.Fatalf("expected id-required error, got %v", err)
	}
}

// TestUsageUpsert_HappyPath_ArgsAndSQL pins both the ON CONFLICT
// clause (otherwise streaming usage rows would explode the unique
// index on id) and the column-by-column arg binding.
func TestUsageUpsert_HappyPath_ArgsAndSQL(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	recorded := time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC)
	u := &persistence.TaskLLMUsage{
		ID:               "tu_task-1_step-1_worker",
		ProjectID:        "p-1",
		TaskID:           strPtr("task-1"),
		ExecutionID:      strPtr("exec-1"),
		StepID:           "step-1",
		Role:             "worker",
		Model:            "claude-opus",
		PromptTokens:     200,
		CompletionTokens: 75,
		Iterations:       3,
		CostUSD:          2.5,
		Source:           persistence.TaskLLMUsageSourceWorkflowStep,
		SessionID:        nil,
		RecordedAt:       recorded,
	}

	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (id) DO UPDATE SET")).
		WithArgs(
			"tu_task-1_step-1_worker", "p-1", "task-1", "exec-1", "step-1",
			"worker", "claude-opus", int64(200), int64(75), 3,
			2.5, persistence.TaskLLMUsageSourceWorkflowStep, nil, recorded,
			int64(0), int64(0),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), u); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestUsageUpsert_DefaultsSourceAndRecordedAt mirrors the Record
// counterpart — defaulting logic lives on the same code path and
// would silently regress otherwise.
func TestUsageUpsert_DefaultsSourceAndRecordedAt(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{
		ID:        "u-stream",
		ProjectID: "p-1",
		StepID:    "s-1",
		Role:      "worker",
		Model:     "claude",
	}

	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (id) DO UPDATE")).
		WithArgs(
			"u-stream", "p-1", nil, nil, "s-1",
			"worker", "claude", int64(0), int64(0), 0,
			0.0, persistence.TaskLLMUsageSourceWorkflowStep, nil,
			recentTimeMatcher{notBefore: before},
			int64(0), int64(0),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), u); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUsageUpsert_PropagatesError(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	u := &persistence.TaskLLMUsage{
		ID:         "u-err",
		ProjectID:  "p-1",
		StepID:     "s-1",
		Role:       "worker",
		Model:      "claude",
		RecordedAt: time.Now().UTC(),
	}

	mock.ExpectExec(regexp.QuoteMeta("ON CONFLICT (id) DO UPDATE")).
		WillReturnError(&pq.Error{Code: "23505"})

	if err := repo.Upsert(context.Background(), u); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("expected ErrDuplicateKey, got %v", err)
	}
}

// TestNullableStringHelpers tests the small conversion helpers
// directly — they're used by Record/Upsert above but also by List
// (scanning), so a unit-level pin is cheap insurance.
func TestNullableStringHelpers(t *testing.T) {
	// nullableString: nil pointer and "" both yield NULL.
	if ns := nullableString(nil); ns.Valid {
		t.Errorf("nil pointer should yield invalid NullString, got %+v", ns)
	}
	empty := ""
	if ns := nullableString(&empty); ns.Valid {
		t.Errorf("empty string should yield invalid NullString, got %+v", ns)
	}
	val := "x"
	if ns := nullableString(&val); !ns.Valid || ns.String != "x" {
		t.Errorf("non-empty string should yield valid NullString, got %+v", ns)
	}

	// stringPtrOrNil: roundtrip.
	if p := stringPtrOrNil(sql.NullString{Valid: false}); p != nil {
		t.Errorf("invalid NullString should yield nil pointer, got %v", *p)
	}
	if p := stringPtrOrNil(sql.NullString{Valid: true, String: ""}); p != nil {
		t.Errorf("empty Valid NullString should yield nil pointer, got %v", *p)
	}
	if p := stringPtrOrNil(sql.NullString{Valid: true, String: "v"}); p == nil || *p != "v" {
		t.Errorf("valid NullString should yield matching pointer, got %v", p)
	}
}

// TestSumCostByAPIKey_JoinsTasksAndSums pins the budget-cap query
// (finding #2 / mitigation §7.2): it joins task_llm_usage → tasks on
// the companion api_key_id expression and sums cost_usd. The window
// args are appended when non-zero.
func TestSumCostByAPIKey_JoinsTasksAndSums(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(u.cost_usd), 0)")).
		WithArgs("akey-1", since).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(7.5))

	got, err := repo.SumCostByAPIKey(context.Background(), "akey-1", since, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByAPIKey: %v", err)
	}
	if got != 7.5 {
		t.Fatalf("sum = %v, want 7.5", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestSumCostByAPIKey_UnboundedWindowOmitsTimeArgs confirms zero
// since/until produce a single-arg query (just the key).
func TestSumCostByAPIKey_UnboundedWindowOmitsTimeArgs(t *testing.T) {
	repo, mock, cleanup := newUsageRepo(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta("WHERE t.payload->'companion'->>'api_key_id' = $1")).
		WithArgs("akey-2").
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0))

	if _, err := repo.SumCostByAPIKey(context.Background(), "akey-2", time.Time{}, time.Time{}); err != nil {
		t.Fatalf("SumCostByAPIKey: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
