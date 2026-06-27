package sqlite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// Round-2 financial repo smoke tests. Each exercises the write
// path plus the most-used aggregator the dashboards depend on.

func TestTaskLLMUsageRepository_RoundtripAndAggregators(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)

	taskA, taskB := "t-a", "t-b"
	rows := []persistence.TaskLLMUsage{
		{ID: "u1", ProjectID: "p", TaskID: &taskA, StepID: "s1", Role: "worker", Model: "m", PromptTokens: 100, CompletionTokens: 50, Iterations: 1, CostUSD: 0.01},
		{ID: "u2", ProjectID: "p", TaskID: &taskA, StepID: "s2", Role: "worker", Model: "m", PromptTokens: 200, CompletionTokens: 80, Iterations: 1, CostUSD: 0.02},
		{ID: "u3", ProjectID: "p", TaskID: &taskB, StepID: "s1", Role: "judge", Model: "m2", PromptTokens: 50, CompletionTokens: 20, Iterations: 1, CostUSD: 0.005},
	}
	for i := range rows {
		if err := repo.Record(ctx, &rows[i]); err != nil {
			t.Fatalf("Record %s: %v", rows[i].ID, err)
		}
	}
	total, err := repo.SumCost(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCost: %v", err)
	}
	if total != 0.035 {
		t.Errorf("SumCost = %v, want 0.035", total)
	}
	byProject, err := repo.SumCostByProject(ctx, "p", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumCostByProject: %v", err)
	}
	if byProject != 0.035 {
		t.Errorf("SumCostByProject = %v, want 0.035", byProject)
	}
	rolemodel, err := repo.AggregateByRoleModel(ctx, time.Time{}, time.Time{}, 10, "p")
	if err != nil {
		t.Fatalf("AggregateByRoleModel: %v", err)
	}
	if len(rolemodel) != 2 {
		t.Fatalf("expected 2 (role, model) groups, got %d", len(rolemodel))
	}
}

func TestTaskLLMUsageRepository_UpsertReplacesByID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTaskLLMUsageRepository(db.DB)
	row := &persistence.TaskLLMUsage{
		ID:        "u-stream",
		ProjectID: "p",
		StepID:    "s",
		Role:      "worker",
		Model:     "m",
		CostUSD:   0.001,
	}
	if err := repo.Upsert(ctx, row); err != nil {
		t.Fatalf("Upsert#1: %v", err)
	}
	row.CostUSD = 0.05
	row.PromptTokens = 999
	if err := repo.Upsert(ctx, row); err != nil {
		t.Fatalf("Upsert#2: %v", err)
	}
	total, _ := repo.SumCostByProject(ctx, "p", time.Time{}, time.Time{})
	if total != 0.05 {
		t.Errorf("total after upsert = %v, want 0.05 (single row replaced)", total)
	}
}

func TestTradingOrderRepository_RecordIdempotentAndIdentityCheck(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTradingOrderRepository(db.DB)
	limit := 178.50
	order := &persistence.TradingOrder{
		ID:             "o-1",
		ProjectID:      "p",
		IdempotencyKey: "idem-1",
		Mode:           "paper",
		Symbol:         "AAPL",
		Action:         "buy",
		OrderType:      "LMT",
		Qty:            6.0,
		LimitPrice:     &limit,
		TimeInForce:    "DAY",
		Status:         "submitted",
	}
	if err := repo.Record(ctx, order); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Same idempotency key + same identity = merge (status update).
	order.Status = "filled"
	if err := repo.Record(ctx, order); err != nil {
		t.Fatalf("Record (same idem): %v", err)
	}
	got, err := repo.List(ctx, persistence.TradingOrderFilter{ProjectID: strPtr("p")})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected merged single row, got %d", len(got))
	}
	if got[0].Status != "filled" {
		t.Errorf("status = %s, want filled", got[0].Status)
	}

	// Different identity (symbol) with same idem key = mismatch.
	bad := &persistence.TradingOrder{
		ID: "o-bad", ProjectID: "p", IdempotencyKey: "idem-1",
		Mode: "paper", Symbol: "MSFT", Action: "buy", OrderType: "LMT",
		Qty: 6.0, LimitPrice: &limit, Status: "submitted",
	}
	err = repo.Record(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "differs in symbol") {
		t.Fatalf("expected identity-mismatch error, got %v", err)
	}
}

func TestTradingFillRepository_RoundtripAndSumVolume(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTradingFillRepository(db.DB)
	commission := 0.02
	fills := []persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "p", Symbol: "AAPL", Qty: 6, Price: 178.50, CommissionUSD: &commission},
		{ID: "f2", OrderID: "o1", ProjectID: "p", Symbol: "AAPL", Qty: 4, Price: 179.00, CommissionUSD: &commission},
		{ID: "f3", OrderID: "o2", ProjectID: "p", Symbol: "MSFT", Qty: 10, Price: 400, CommissionUSD: nil},
	}
	for i := range fills {
		if err := repo.Record(ctx, &fills[i]); err != nil {
			t.Fatalf("Record %s: %v", fills[i].ID, err)
		}
	}
	// Idempotent retry (same ID) — no extra row, no error.
	if err := repo.Record(ctx, &fills[0]); err != nil {
		t.Fatalf("Record retry: %v", err)
	}
	project := "p"
	got, err := repo.List(ctx, persistence.TradingFillFilter{ProjectID: &project})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List returned %d, want 3", len(got))
	}
	volume, err := repo.SumVolume(ctx, persistence.TradingFillFilter{ProjectID: &project})
	if err != nil {
		t.Fatalf("SumVolume: %v", err)
	}
	want := 6*178.50 + 4*179.00 + 10*400.0
	if volume != want {
		t.Errorf("SumVolume = %v, want %v", volume, want)
	}
}

// TestTradingFillRepository_RecordShadow is the regression for the
// 2026-06-26 final-review finding: trading_fills_shadow.recorded_at is
// NOT NULL with no column default on sqlite (Postgres uses DEFAULT NOW()),
// so the sqlite RecordShadow INSERT must bind recorded_at explicitly or it
// hits a NOT NULL violation at runtime. This exercises the real in-memory
// sqlite DB: a RecordShadow insert must succeed AND recorded_at must be
// populated (non-NULL, non-empty).
func TestTradingFillRepository_RecordShadow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTradingFillRepository(db.DB)

	execID := "0001.01"
	acct := "DUH1"
	commission := 0.02
	fill := &persistence.TradingFill{
		ID:            "exec-0001.01",
		OrderID:       "tag-1",
		ProjectID:     "p",
		Symbol:        "AAPL",
		Qty:           8,
		Price:         286.0,
		CommissionUSD: &commission,
		ExecID:        &execID,
		AccountID:     &acct,
		Source:        "reconcile",
		FilledAt:      time.Date(2026, 6, 25, 13, 32, 0, 0, time.UTC),
	}
	if err := repo.RecordShadow(ctx, fill); err != nil {
		t.Fatalf("RecordShadow: %v", err)
	}
	// Idempotent retry on the same id — no error, no second row.
	if err := repo.RecordShadow(ctx, fill); err != nil {
		t.Fatalf("RecordShadow retry: %v", err)
	}

	var (
		count      int
		recordedAt string
	)
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(recorded_at), '') FROM trading_fills_shadow WHERE id = ?`,
		fill.ID).Scan(&count, &recordedAt); err != nil {
		t.Fatalf("query shadow row: %v", err)
	}
	if count != 1 {
		t.Fatalf("shadow row count = %d, want 1 (idempotent on id)", count)
	}
	if recordedAt == "" {
		t.Fatalf("recorded_at must be populated (NOT NULL with no default on sqlite), got empty")
	}
}

func TestTradingSafetyEventRepository_RecordAndList(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTradingSafetyEventRepository(db.DB)
	if err := repo.Record(ctx, &persistence.TradingSafetyEvent{
		ID: "evt-1", ProjectID: "p", Kind: "breaker_trip",
		Severity: "alert", Detail: []byte(`{"window_pct":-0.05}`),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Idempotent retry.
	if err := repo.Record(ctx, &persistence.TradingSafetyEvent{
		ID: "evt-1", ProjectID: "p", Kind: "breaker_trip", Severity: "alert",
	}); err != nil {
		t.Fatalf("Record retry: %v", err)
	}
	project := "p"
	rows, err := repo.List(ctx, persistence.TradingSafetyEventFilter{ProjectID: &project})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List = %d rows, want 1", len(rows))
	}
	if string(rows[0].Detail) != `{"window_pct":-0.05}` {
		t.Errorf("Detail mismatch: %s", rows[0].Detail)
	}
	n, err := repo.Count(ctx, persistence.TradingSafetyEventFilter{ProjectID: &project})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
}

func TestTradingSnapshotRepository_RecordAndListSince(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	repo := sqlite.NewTradingSnapshotRepository(db.DB)
	base := time.Now().UTC().Add(-1 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := repo.Record(ctx, &persistence.TradingPositionsSnapshot{
			ID:              "snap-" + string(rune('a'+i)),
			ProjectID:       "p",
			RecordedAt:      base.Add(time.Duration(i) * time.Minute),
			CashUSD:         10000 + float64(i),
			EquityUSD:       11000 + float64(i),
			UnrealisedPLUSD: 100 * float64(i),
			PositionsJSON:   []byte(`{"AAPL":10}`),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	since := base.Add(30 * time.Second) // catches indexes 1 + 2
	got, err := repo.ListSince(ctx, "p", since, 0)
	if err != nil {
		t.Fatalf("ListSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSince = %d snapshots, want 2", len(got))
	}
	// Oldest-first: first row's UnrealisedPLUSD must be 100 (i=1).
	if got[0].UnrealisedPLUSD != 100 {
		t.Errorf("oldest-first violated: %v", got[0].UnrealisedPLUSD)
	}
}

func strPtr(s string) *string { return &s }
