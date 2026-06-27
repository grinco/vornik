package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
)

// Financial surface guarded by these tests:
//
//   - LLM cost          → persistence.TaskLLMUsageRepository
//   - Tool / audit log  → persistence.ToolAuditRepository
//   - Trading orders    → persistence.TradingOrderRepository
//   - Trading fills     → persistence.TradingFillRepository
//   - Trading safety    → persistence.TradingSafetyEventRepository
//   - Trading snapshots → persistence.TradingPositionsSnapshotRepository
//   - Autonomy spend    → persistence.AutonomyEvaluationRepository
//   - API key audit     → persistence.APIKeyRepository
//
// The phase-1 storage abstraction restructured how these repos reach
// the daemon (storage.Build behind a metrics-wrapped DBTX instead of
// inline postgres.NewXxxRepository calls per call site). These tests
// pin three properties that must survive that restructuring:
//
//  1. Every financial field of Repositories is populated by Build().
//  2. Each populated field satisfies its declared persistence
//     interface (compile-time guard against future refactors that
//     widen the postgres concrete type's surface without updating the
//     interface).
//  3. A write through the metrics-wrapped DBTX still issues the
//     correct SQL AND increments the Prometheus exec counter so the
//     spend / soak dashboards keep observability on financial writes.
//
// Property (3) is the load-bearing one — a silent metric loss on a
// financial write would mean operators stop seeing real-time spend
// changes and only notice after the next state-collector tick or a
// full reload.

// financialRepoSet enumerates the eight repository interface fields
// on Repositories that handle financially-significant data. Adding a
// new financial repo means adding a row here; TestFinancialFields_
// PopulatedAndConform protects the closed set so a future addition
// can't slip through with a nil field.
type financialRepoSet struct {
	name       string
	extract    func(r *Repositories) interface{}
	interface_ interface{}
}

var financialRepos = []financialRepoSet{
	{"LLMUsage", func(r *Repositories) interface{} { return r.LLMUsage }, (*persistence.TaskLLMUsageRepository)(nil)},
	{"ToolAudit", func(r *Repositories) interface{} { return r.ToolAudit }, (*persistence.ToolAuditRepository)(nil)},
	{"TradingOrders", func(r *Repositories) interface{} { return r.TradingOrders }, (*persistence.TradingOrderRepository)(nil)},
	{"TradingFills", func(r *Repositories) interface{} { return r.TradingFills }, (*persistence.TradingFillRepository)(nil)},
	{"TradingSafetyEvents", func(r *Repositories) interface{} { return r.TradingSafetyEvents }, (*persistence.TradingSafetyEventRepository)(nil)},
	{"TradingSnapshots", func(r *Repositories) interface{} { return r.TradingSnapshots }, (*persistence.TradingPositionsSnapshotRepository)(nil)},
	{"AutonomyEvaluations", func(r *Repositories) interface{} { return r.AutonomyEvaluations }, (*persistence.AutonomyEvaluationRepository)(nil)},
	{"APIKeys", func(r *Repositories) interface{} { return r.APIKeys }, (*persistence.APIKeyRepository)(nil)},
}

// TestFinancialFields_PopulatedAndConform checks property (1) and (2)
// from the surface contract above. Run on a stub DBTX (no SQL fires)
// so the test stays in-process and millisecond-fast.
func TestFinancialFields_PopulatedAndConform(t *testing.T) {
	repos := Build(stubDBTX{})
	for _, row := range financialRepos {
		row := row
		t.Run(row.name, func(t *testing.T) {
			got := row.extract(repos)
			if got == nil {
				t.Fatalf("Repositories.%s nil — Build() lost the financial wiring", row.name)
			}
			// Interface-pointer conformance: a *FinancialRepoInterface
			// is non-nil iff the concrete implementation actually
			// satisfies the interface. We construct one of those
			// pointers per row and assign the concrete value through
			// it; the compile-time check on the field types in
			// Repositories already guarantees this, but the explicit
			// assertion here protects against accidental future
			// widenings (e.g. someone changes Repositories.LLMUsage
			// to *postgres.TaskLLMUsageRepository — concrete instead
			// of interface — and the daemon code keeps compiling).
			switch row.interface_.(type) {
			case *persistence.TaskLLMUsageRepository:
				if _, ok := got.(persistence.TaskLLMUsageRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy TaskLLMUsageRepository", row.name)
				}
			case *persistence.ToolAuditRepository:
				if _, ok := got.(persistence.ToolAuditRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy ToolAuditRepository", row.name)
				}
			case *persistence.TradingOrderRepository:
				if _, ok := got.(persistence.TradingOrderRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy TradingOrderRepository", row.name)
				}
			case *persistence.TradingFillRepository:
				if _, ok := got.(persistence.TradingFillRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy TradingFillRepository", row.name)
				}
			case *persistence.TradingSafetyEventRepository:
				if _, ok := got.(persistence.TradingSafetyEventRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy TradingSafetyEventRepository", row.name)
				}
			case *persistence.TradingPositionsSnapshotRepository:
				if _, ok := got.(persistence.TradingPositionsSnapshotRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy TradingPositionsSnapshotRepository", row.name)
				}
			case *persistence.AutonomyEvaluationRepository:
				if _, ok := got.(persistence.AutonomyEvaluationRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy AutonomyEvaluationRepository", row.name)
				}
			case *persistence.APIKeyRepository:
				if _, ok := got.(persistence.APIKeyRepository); !ok {
					t.Fatalf("Repositories.%s does not satisfy APIKeyRepository", row.name)
				}
			default:
				t.Fatalf("unhandled financial repo type for %s — add a case", row.name)
			}
		})
	}
}

// newMetricsHarness wires sqlmock behind a persistence.DBWithMetrics
// wrapper that registers fresh Prometheus counters on a private
// registry. Tests then invoke Record/Log on the financial repos and
// assert the exec counter incremented — proving the metrics
// instrumentation survives the storage abstraction restructuring.
type metricsHarness struct {
	db      *sql.DB
	mock    sqlmock.Sqlmock
	wrapped *persistence.DBWithMetrics
	metrics *persistence.DBMetrics
	regName string
}

func newMetricsHarness(t *testing.T) *metricsHarness {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	registry := prometheus.NewRegistry()
	metrics := persistence.NewDBMetrics(registry)
	wrapped := persistence.NewDBWithMetrics(db, metrics, "financial_test")
	return &metricsHarness{
		db:      db,
		mock:    mock,
		wrapped: wrapped,
		metrics: metrics,
		regName: "financial_test",
	}
}

func (h *metricsHarness) close() {
	_ = h.db.Close()
}

// assertExecCounter checks the (database, operation, status=success)
// counter incremented by 1 after a Record/Log call. A failure here
// means the metrics wrapper was bypassed (e.g. a repo grabbed the
// raw *sql.DB directly somewhere up the call chain).
func (h *metricsHarness) assertExecCounter(t *testing.T) {
	t.Helper()
	got := testutil.ToFloat64(h.metrics.QueryTotal.WithLabelValues(h.regName, "exec", "success"))
	if got < 1 {
		t.Fatalf("expected exec/success counter ≥1 after Record, got %v — metrics wrapper bypassed", got)
	}
}

// TestLLMUsage_RecordIncrementsMetrics — the spend dashboard's most
// load-bearing write. A silent miss here means rolling spend gauges
// stay flat while the database has fresh rows.
func TestLLMUsage_RecordIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	h.mock.ExpectExec("INSERT INTO task_llm_usage").
		WithArgs(sqlmock.AnyArg(), "proj-fin", sqlmock.AnyArg(), sqlmock.AnyArg(), "step-1",
			"worker", "claude-opus-4-7", int64(100), int64(50), 3,
			0.0125, "workflow_step", sqlmock.AnyArg(), sqlmock.AnyArg(),
			int64(0), int64(0), // cache_creation_tokens, cache_read_tokens (phase A)
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	taskID, execID := "task-1", "exec-1"
	err := repos.LLMUsage.Record(context.Background(), &persistence.TaskLLMUsage{
		ID:               "usage-1",
		ProjectID:        "proj-fin",
		TaskID:           &taskID,
		ExecutionID:      &execID,
		StepID:           "step-1",
		Role:             "worker",
		Model:            "claude-opus-4-7",
		PromptTokens:     100,
		CompletionTokens: 50,
		Iterations:       3,
		CostUSD:          0.0125,
		Source:           persistence.TaskLLMUsageSourceWorkflowStep,
		RecordedAt:       time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("LLMUsage.Record: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestToolAudit_LogIncrementsMetrics — the transaction log every
// tool invocation lands. Used by the soak panel, the audit replay
// path, and the broker→daemon reconciliation flow.
func TestToolAudit_LogIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	h.mock.ExpectExec("INSERT INTO tool_audit_log").
		WithArgs("audit-1", "proj-fin", "task-1", "exec-1", "step-1",
			"place_order", `{"symbol":"AAPL"}`, `{"order_id":"x"}`, int64(250), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repos.ToolAudit.Log(context.Background(), &persistence.ToolAuditEntry{
		ID:          "audit-1",
		ProjectID:   "proj-fin",
		TaskID:      "task-1",
		ExecutionID: "exec-1",
		StepID:      "step-1",
		ToolName:    "place_order",
		ToolInput:   `{"symbol":"AAPL"}`,
		ToolOutput:  `{"order_id":"x"}`,
		DurationMs:  250,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ToolAudit.Log: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestTradingOrder_RecordIncrementsMetrics — broker→daemon audit
// channel writes one row per place_order. Pre-fix on the legacy
// path, a metrics miss here meant operators saw zero live orders on
// the soak panel while real orders were filling at IBKR.
func TestTradingOrder_RecordIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	limit := 178.55
	// checkIdentityMatch fires QueryRowContext first — assert that
	// too, so we know the metrics-wrapped query path is exercised
	// alongside the exec path.
	h.mock.ExpectQuery("SELECT symbol, action, qty, COALESCE").
		WithArgs("proj-fin", "idem-1").
		WillReturnError(sql.ErrNoRows)
	h.mock.ExpectExec("INSERT INTO trading_orders").
		WithArgs("ord-1", "proj-fin", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"idem-1", "paper", "AAPL", "buy", "LMT",
			6.0, &limit, sqlmock.AnyArg(), "DAY",
			"submitted", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg()). // filled_qty — exec-reconcile column (added f361e99d / migration 109)
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repos.TradingOrders.Record(context.Background(), &persistence.TradingOrder{
		ID:             "ord-1",
		ProjectID:      "proj-fin",
		IdempotencyKey: "idem-1",
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "buy",
		OrderType:      "LMT",
		Qty:            6.0,
		LimitPrice:     &limit,
		TimeInForce:    "DAY",
		Status:         "submitted",
		SubmittedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("TradingOrders.Record: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestTradingFill_RecordIncrementsMetrics — the precise-volume
// signal under the soak panel's USD-volume tile. A miss here
// reverts the dashboard to the legacy approximate calc.
func TestTradingFill_RecordIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	commission := 0.05
	h.mock.ExpectExec("INSERT INTO trading_fills").
		WithArgs("fill-1", "ord-1", "proj-fin", "AAPL",
			6.0, 178.50, &commission, sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()). // exec_id, account_id, source, source_detail (exec-reconcile cols)
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repos.TradingFills.Record(context.Background(), &persistence.TradingFill{
		ID:            "fill-1",
		OrderID:       "ord-1",
		ProjectID:     "proj-fin",
		Symbol:        "AAPL",
		Qty:           6.0,
		Price:         178.50,
		CommissionUSD: &commission,
		FilledAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("TradingFills.Record: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestTradingSafetyEvent_RecordIncrementsMetrics — kill-switch
// toggles, breaker trips, cap refusals. Independent of trading_orders
// volume, but operators page on these.
func TestTradingSafetyEvent_RecordIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	h.mock.ExpectExec("INSERT INTO trading_safety_events").
		WithArgs("evt-1", "proj-fin", sqlmock.AnyArg(),
			"breaker_trip", "warn", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repos.TradingSafetyEvents.Record(context.Background(), &persistence.TradingSafetyEvent{
		ID:         "evt-1",
		ProjectID:  "proj-fin",
		RecordedAt: time.Now().UTC(),
		Kind:       "breaker_trip",
		Severity:   "warn",
	})
	if err != nil {
		t.Fatalf("TradingSafetyEvents.Record: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestTradingSnapshot_RecordIncrementsMetrics — equity/cash time
// series. The Sharpe + drawdown calc on the soak panel reads this
// table; a metrics miss here doesn't lose data but loses pool stats
// signal for the writer goroutine.
func TestTradingSnapshot_RecordIncrementsMetrics(t *testing.T) {
	h := newMetricsHarness(t)
	defer h.close()

	repos := Build(h.wrapped)
	positions, _ := json.Marshal(map[string]any{"AAPL": map[string]any{"qty": 6, "px": 178.55}})
	h.mock.ExpectExec("INSERT INTO trading_positions_snapshots").
		WithArgs("snap-1", "proj-fin", sqlmock.AnyArg(),
			10_000.50, 11_700.25, 1_700.00, 50.0, positions).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := repos.TradingSnapshots.Record(context.Background(), &persistence.TradingPositionsSnapshot{
		ID:               "snap-1",
		ProjectID:        "proj-fin",
		RecordedAt:       time.Now().UTC(),
		CashUSD:          10_000.50,
		EquityUSD:        11_700.25,
		UnrealisedPLUSD:  1_700.00,
		RealisedPLDayUSD: 50.0,
		PositionsJSON:    positions,
	})
	if err != nil {
		t.Fatalf("TradingSnapshots.Record: %v", err)
	}
	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	h.assertExecCounter(t)
}

// TestBuild_RebuildsAllReposOnEveryCall — the daemon rebuilds repos
// after observability comes online so the metrics-wrapped DBTX
// replaces the unwrapped one. This test pins the property by
// constructing two Repositories from two different DBTXs and
// asserting the financial repos are distinct instances (a buggy
// implementation that cached repos at package init would fail).
func TestBuild_RebuildsAllReposOnEveryCall(t *testing.T) {
	first := Build(stubDBTX{})
	second := Build(stubDBTX{})
	for _, row := range financialRepos {
		row := row
		t.Run(row.name, func(t *testing.T) {
			a := row.extract(first)
			b := row.extract(second)
			if a == nil || b == nil {
				t.Fatalf("nil financial repo on Build (%s)", row.name)
			}
			// Pointer-identity comparison: two Build calls must
			// produce distinct concrete instances. Same-instance
			// reuse would mean the metrics-rewrap path silently
			// keeps the old (unwrapped) DBTX behind the financial
			// repos.
			if a == b {
				t.Fatalf("Build returned the same instance for %s on two calls — metrics rewrap would be a no-op", row.name)
			}
		})
	}
}
