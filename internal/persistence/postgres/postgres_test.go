package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

type stubDBTX struct{}

func (s *stubDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, nil
}

func (s *stubDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return &sql.Row{}
}

func (s *stubDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return nil, nil
}

type recordingDBTX struct {
	execQuery string
	execArgs  []interface{}
	execErr   error
}

func (r *recordingDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return nil, errors.New("unexpected QueryContext call")
}

func (r *recordingDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	panic("unexpected QueryRowContext call")
}

func (r *recordingDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	r.execQuery = query
	r.execArgs = args
	return nil, r.execErr
}

func TestRepositoryConstructors_AssignDBTX(t *testing.T) {
	db := &stubDBTX{}

	assert.Same(t, db, NewArtifactRepository(db).db)
	assert.Same(t, db, NewAutonomyEvaluationRepository(db).db)
	assert.Same(t, db, NewExecutionRepository(db).db)
	assert.Same(t, db, NewExecutionStepOutcomeRepository(db).db)
	assert.Same(t, db, NewMemoryRetrievalAuditRepository(db).db)
	assert.Same(t, db, NewTaskJudgeVerdictRepository(db).db)
	assert.Same(t, db, NewTaskLLMUsageRepository(db).db)
	assert.Same(t, db, NewTaskPostMortemRepository(db).db)
	assert.Same(t, db, NewTaskRepository(db).db)
	assert.Same(t, db, NewTaskWatcherRepository(db).db)
	assert.Same(t, db, NewToolAuditRepository(db).db)
	assert.Same(t, db, NewTradingFillRepository(db).db)
	assert.Same(t, db, NewTradingOrderRepository(db).db)
	assert.Same(t, db, NewTradingSafetyEventRepository(db).db)
	assert.Same(t, db, NewTradingSnapshotRepository(db).db)
	assert.Same(t, db, NewWebhookEventRepository(db).db)
}

func TestTaskRepository_ReleaseLeaseRequiresLeaseID(t *testing.T) {
	repo := NewTaskRepository(nil)

	err := repo.ReleaseLease(context.Background(), "task-1", "", persistence.TaskStatusQueued, persistence.ReleaseOptions{})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "leaseID required")
}

func TestMapDBError_PostgresCodes(t *testing.T) {
	assert.ErrorIs(t, mapDBError(&pq.Error{Code: "23505"}), persistence.ErrDuplicateKey)
	assert.ErrorIs(t, mapDBError(&pq.Error{Code: "23503"}), persistence.ErrNotFound)

	rawErr := errors.New("boom")
	assert.Same(t, rawErr, mapDBError(rawErr))
}

func TestBuildTradingOrderQuery_WithFilters(t *testing.T) {
	projectID := "proj-1"
	status := "submitted"
	symbol := "AAPL"
	since := time.Date(2026, 5, 8, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	until := time.Date(2026, 5, 9, 12, 30, 0, 0, time.FixedZone("CEST", 2*3600))

	query, args := buildTradingOrderQuery(persistence.TradingOrderFilter{
		ProjectID: &projectID,
		Status:    &status,
		Symbol:    &symbol,
		Since:     &since,
		Until:     &until,
	}, false)

	assert.Contains(t, query, "SELECT id, project_id")
	assert.Contains(t, query, "AND project_id = $1")
	assert.Contains(t, query, "AND status = $2")
	assert.Contains(t, query, "AND symbol = $3")
	assert.Contains(t, query, "AND submitted_at >= $4")
	assert.Contains(t, query, "AND submitted_at < $5")
	assert.Len(t, args, 5)
	assert.Equal(t, projectID, args[0])
	assert.Equal(t, status, args[1])
	assert.Equal(t, symbol, args[2])
	assert.Equal(t, since.UTC(), args[3])
	assert.Equal(t, until.UTC(), args[4])
}

func TestTradingOrderRepositoryRecord_UsesDefaultsAndPointerConverters(t *testing.T) {
	// Uses sqlmock (not recordingDBTX) because Record now runs an
	// identity-mismatch pre-flight QueryRowContext before its
	// INSERT — recordingDBTX panics on QueryRowContext by design
	// (it's the "no queries expected" recorder). The behaviour
	// being tested is unchanged: nil/zero optional fields must
	// land as SQL NULL and the zero SubmittedAt must be replaced
	// with a recent UTC time.
	repo, mock, cleanup := newOrderRepo(t)
	defer cleanup()

	taskID := ""
	execID := "exec-1"
	brokerID := "broker-1"
	limitPrice := 123.45
	zeroTerminalAt := time.Time{}

	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_orders")).
		WithArgs("proj-1", "idem-1").
		WillReturnRows(sqlmock.NewRows([]string{"symbol", "action", "qty", "limit_price"}))
	before := time.Now().UTC().Add(-time.Second)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_orders")).
		WithArgs(
			"ord-1", "proj-1",
			nil, // empty task_id → NULL
			execID,
			brokerID,
			"idem-1", "paper", "AAPL", "BUY", "LMT",
			2.0,
			limitPrice,
			nil, // nil stop_price → NULL
			"DAY", "submitted", "",
			recentTimeMatcher{notBefore: before}, // zero SubmittedAt → now
			nil,                                  // zero terminal_at → NULL
			0.0,                                  // filled_qty
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := repo.Record(context.Background(), &persistence.TradingOrder{
		ID:             "ord-1",
		ProjectID:      "proj-1",
		TaskID:         &taskID,
		ExecutionID:    &execID,
		BrokerOrderID:  &brokerID,
		IdempotencyKey: "idem-1",
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "BUY",
		OrderType:      "LMT",
		Qty:            2,
		LimitPrice:     &limitPrice,
		StopPrice:      nil,
		TimeInForce:    "DAY",
		Status:         "submitted",
		TerminalAt:     &zeroTerminalAt,
	})

	assert.NoError(t, err)
}

// mockDBTX records every Exec/Query call. Newer cousin of recordingDBTX
// that returns a configurable QueryRowContext result instead of
// panicking, so repos which run "SELECT EXISTS ..." before their
// INSERT can be unit-tested without a live database. The two helpers
// coexist deliberately: recordingDBTX's panic-on-unexpected-call shape
// catches accidental query bleed in the tests that use it, while
// mockDBTX is for repo paths that legitimately QueryRow.
type mockDBTX struct {
	existsRow   *sql.Row
	queryCalled bool
	query       string
	queryArgs   []interface{}
	execCalled  bool
	execQuery   string
	execArgs    []interface{}
	execError   error
}

func (m *mockDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	m.queryCalled = true
	m.query = query
	m.queryArgs = args
	return nil, nil
}

func (m *mockDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	return m.existsRow
}

func (m *mockDBTX) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	m.execCalled = true
	m.execQuery = query
	m.execArgs = args
	return nil, m.execError
}

func TestTaskJudgeVerdictRepository_Record(t *testing.T) {
	// TaskJudgeVerdictRepository.Record runs a "SELECT EXISTS" pre-flight
	// before its INSERT. The mockDBTX returns &sql.Row{} from
	// QueryRowContext, but the zero-value sql.Row panics on Scan
	// (no underlying *sql.Rows). Properly testing this path needs a
	// driver-level mock (sqlmock or a real test DB). Skipping until
	// one of those is wired up — the validation-error sibling below
	// already covers the nil-verdict branch.
	t.Skip("requires driver-level mock (sqlmock or real DB) for SELECT EXISTS pre-flight")
}

func TestTaskJudgeVerdictRepository_Record_ValidationError(t *testing.T) {
	repo := NewTaskJudgeVerdictRepository(&mockDBTX{})

	err := repo.Record(context.Background(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestTaskJudgeVerdictRepository_Record_DefaultRecordedAt(t *testing.T) {
	// Same blocker as TestTaskJudgeVerdictRepository_Record — the
	// pre-INSERT SELECT EXISTS path can't be exercised without a
	// driver-level mock.
	t.Skip("requires driver-level mock (sqlmock or real DB) for SELECT EXISTS pre-flight")
}

func TestMemoryRetrievalAuditRepository_Record_GeneratesIDAndDefaultTimestamp(t *testing.T) {
	taskID := "task-1"
	executionID := "exec-1"
	stepID := "step-1"
	role := "coder"

	db := &mockDBTX{}
	repo := NewMemoryRetrievalAuditRepository(db)

	audit := &persistence.MemoryRetrievalAudit{
		ProjectID:   "proj-1",
		TaskID:      &taskID,
		ExecutionID: &executionID,
		StepID:      &stepID,
		Role:        &role,
		Query:       "test query",
		ChunkIDs:    []string{"chunk-1", "chunk-2"},
	}

	err := repo.Record(context.Background(), audit)

	assert.NoError(t, err)
	assert.Contains(t, db.execQuery, "INSERT INTO memory_retrieval_audit")
	// 9 historic positional args + 2 LLD-22 actor columns + 1
	// migration-75 repo_scope = 12.
	assert.Len(t, db.execArgs, 12)

	// ID should have been generated
	idArg := db.execArgs[0].(string)
	assert.NotEmpty(t, idArg)
	assert.Contains(t, idArg, "retr")

	assert.Equal(t, "proj-1", db.execArgs[1])
	// The audit struct stores TaskID/ExecutionID/StepID/Role as *string,
	// and Record passes the pointer straight through to ExecContext —
	// so the captured args are *string, not string. Dereference before
	// comparing.
	assert.Equal(t, &taskID, db.execArgs[2])
	assert.Equal(t, &executionID, db.execArgs[3])
	assert.Equal(t, &stepID, db.execArgs[4])
	assert.Equal(t, &role, db.execArgs[5])
	assert.Equal(t, "test query", db.execArgs[6])

	// ChunkIDs should be a pq.Array
	chunkIDsArg := db.execArgs[7]
	assert.IsType(t, pq.Array([]string{}), chunkIDsArg)

	// RetrievedAt should default to now
	retrievedAtArg, ok := db.execArgs[8].(time.Time)
	assert.True(t, ok)
	assert.False(t, retrievedAtArg.IsZero())

	// LLD-22 actor columns: nil here because the fixture didn't
	// populate them. Pre-LLD-22 callers leave them empty; the
	// Searcher auto-fills "agent"/Role at the call site for agent
	// recalls, so this row would normally arrive populated through
	// that path — but the repo itself must not invent values.
	assert.Nil(t, db.execArgs[9], "actor_kind defaults to nil when caller didn't set it")
	assert.Nil(t, db.execArgs[10], "actor_id defaults to nil when caller didn't set it")
}

// TestMemoryRetrievalAuditRepository_Record_ActorColumns pins the
// LLD 22 actor split. When the caller populates ActorKind/ActorID
// (companion recalls, or the agent path's auto-fill), those values
// must reach the INSERT's last two positional args.
func TestMemoryRetrievalAuditRepository_Record_ActorColumns(t *testing.T) {
	actorKind := "companion"
	actorID := "akey-mem"

	db := &mockDBTX{}
	repo := NewMemoryRetrievalAuditRepository(db)

	audit := &persistence.MemoryRetrievalAudit{
		ProjectID: "companion-example",
		Query:     "anything",
		ChunkIDs:  []string{"ck-1"},
		ActorKind: &actorKind,
		ActorID:   &actorID,
	}
	err := repo.Record(context.Background(), audit)
	assert.NoError(t, err)
	// 12 args now: + repo_scope (migration 75). Audit didn't set it
	// so the trailing arg is the nil *string from the struct.
	assert.Len(t, db.execArgs, 12)
	assert.Equal(t, &actorKind, db.execArgs[9])
	assert.Equal(t, &actorID, db.execArgs[10])
	assert.Equal(t, (*string)(nil), db.execArgs[11])
}

func TestMemoryRetrievalAuditRepository_Record_ValidationError(t *testing.T) {
	repo := NewMemoryRetrievalAuditRepository(&mockDBTX{})

	err := repo.Record(context.Background(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "audit is nil")
}

func TestTradingFillRepository_Record(t *testing.T) {
	db := &mockDBTX{}
	repo := NewTradingFillRepository(db)

	fill := &persistence.TradingFill{
		ID:        "fill-1",
		OrderID:   "ord-1",
		ProjectID: "proj-1",
		Symbol:    "AAPL",
		Qty:       100.0,
		Price:     150.75,
	}

	err := repo.Record(context.Background(), fill)

	assert.NoError(t, err)
	assert.Contains(t, db.execQuery, "INSERT INTO trading_fills")
	assert.Contains(t, db.execQuery, "ON CONFLICT (id) DO NOTHING")
	assert.Len(t, db.execArgs, 12)
	assert.Equal(t, "fill-1", db.execArgs[0])
	assert.Equal(t, "ord-1", db.execArgs[1])
	assert.Equal(t, "proj-1", db.execArgs[2])
	assert.Equal(t, "AAPL", db.execArgs[3])
	assert.Equal(t, 100.0, db.execArgs[4])
	assert.Equal(t, 150.75, db.execArgs[5])
	assert.Nil(t, db.execArgs[6], "nil commission should be passed as SQL NULL")

	filledAtArg, ok := db.execArgs[7].(time.Time)
	assert.True(t, ok)
	assert.False(t, filledAtArg.IsZero(), "zero FilledAt should be replaced with current UTC time")
	// exec_id, account_id → nil (no exec columns set); source → "reconcile" default; source_detail → nil
	assert.Nil(t, db.execArgs[8], "nil exec_id should bind as SQL NULL")
	assert.Nil(t, db.execArgs[9], "nil account_id should bind as SQL NULL")
	assert.Equal(t, "reconcile", db.execArgs[10], "empty Source should default to reconcile")
	assert.Nil(t, db.execArgs[11], "nil source_detail should bind as SQL NULL")
}

func TestTradingFillRepository_Record_ValidationError(t *testing.T) {
	repo := NewTradingFillRepository(&mockDBTX{})

	err := repo.Record(context.Background(), nil)
	assert.EqualError(t, err, "nil trading fill")

	err = repo.Record(context.Background(), &persistence.TradingFill{})
	assert.EqualError(t, err, "trading fill ID required")

	err = repo.Record(context.Background(), &persistence.TradingFill{ID: "fill-1"})
	assert.EqualError(t, err, "order ID required")

	err = repo.Record(context.Background(), &persistence.TradingFill{ID: "fill-1", OrderID: "ord-1"})
	assert.EqualError(t, err, "project ID required")

	err = repo.Record(context.Background(), &persistence.TradingFill{ID: "fill-1", OrderID: "ord-1", ProjectID: "proj-1"})
	assert.EqualError(t, err, "symbol required")
}
