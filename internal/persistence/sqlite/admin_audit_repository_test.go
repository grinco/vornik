package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
)

// TestAdminAuditRepository_RoundTrip — insert a row, list it back,
// confirm every field round-trips.
func TestAdminAuditRepository_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAdminAuditRepository(db.DB)
	ctx := context.Background()

	entry := &persistence.AdminAuditEntry{
		Principal: "sk-admin-1",
		Source:    "ui",
		Action:    "mcp.refresh",
		Target:    "proj-alpha",
		After:     `{"result":"ok"}`,
		IP:        "10.0.0.1",
		UserAgent: "vornikctl/test",
	}
	if err := repo.Insert(ctx, entry); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if entry.ID == "" {
		t.Fatal("Insert should populate the auto-generated ID")
	}

	rows, err := repo.List(ctx, persistence.AdminAuditFilter{PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List: want 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.ID != entry.ID {
		t.Errorf("ID: want %q, got %q", entry.ID, got.ID)
	}
	if got.Action != "mcp.refresh" {
		t.Errorf("Action: got %q", got.Action)
	}
	if got.Target != "proj-alpha" {
		t.Errorf("Target: got %q", got.Target)
	}
	if got.After != `{"result":"ok"}` {
		t.Errorf("After: got %q", got.After)
	}
	if got.IP != "10.0.0.1" {
		t.Errorf("IP: got %q", got.IP)
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp: should be auto-populated to NOW() on Insert")
	}
}

// TestAdminAuditRepository_Filter — confirm the Action / Principal /
// TargetPrefix / Since filters compose with AND.
func TestAdminAuditRepository_Filter(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAdminAuditRepository(db.DB)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	mustInsert := func(p, action, target string, when time.Time) {
		t.Helper()
		if err := repo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: p, Source: "ui", Action: action, Target: target, Timestamp: when,
		}); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	mustInsert("sk-a", "mcp.refresh", "proj-alpha", base.Add(-3*time.Hour))
	mustInsert("sk-a", "config.reload", "proj-alpha", base.Add(-2*time.Hour))
	mustInsert("sk-b", "mcp.refresh", "proj-beta", base.Add(-time.Hour))
	mustInsert("sk-b", "key.revoke", "key-abc", base.Add(-30*time.Minute))

	// Action filter narrows to two rows.
	rows, err := repo.List(ctx, persistence.AdminAuditFilter{Action: "mcp.refresh", PageSize: 10})
	if err != nil {
		t.Fatalf("filter action: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("action=mcp.refresh: want 2 rows, got %d", len(rows))
	}

	// Principal+action narrows to one row.
	rows, err = repo.List(ctx, persistence.AdminAuditFilter{Action: "mcp.refresh", Principal: "sk-a", PageSize: 10})
	if err != nil {
		t.Fatalf("filter principal+action: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("principal=sk-a + action=mcp.refresh: want 1 row, got %d", len(rows))
	}

	// Target prefix matches proj-alpha rows only.
	rows, err = repo.List(ctx, persistence.AdminAuditFilter{TargetPrefix: "proj-alpha", PageSize: 10})
	if err != nil {
		t.Fatalf("filter target: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("target=proj-alpha: want 2 rows, got %d", len(rows))
	}

	// Since filter excludes the oldest row.
	rows, err = repo.List(ctx, persistence.AdminAuditFilter{Since: base.Add(-90 * time.Minute), PageSize: 10})
	if err != nil {
		t.Fatalf("filter since: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("since=-90m: want 2 rows, got %d", len(rows))
	}
}

// TestAdminAuditRepository_PageSizeRequired — repo rejects an
// unbounded scan (PageSize <= 0).
func TestAdminAuditRepository_PageSizeRequired(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAdminAuditRepository(db.DB)
	ctx := context.Background()
	_, err := repo.List(ctx, persistence.AdminAuditFilter{})
	if err == nil {
		t.Fatal("List with PageSize=0 should return an error")
	}
}

// TestAdminAuditRepository_NilEntry — defensive shape; the repo
// rejects nil rather than panicking.
func TestAdminAuditRepository_NilEntry(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAdminAuditRepository(db.DB)
	if err := repo.Insert(context.Background(), nil); err == nil {
		t.Fatal("Insert(nil) should return an error")
	}
}

// TestAdminAuditRepository_TargetWildcardEscaped — operator-supplied
// target prefix with a literal `%` shouldn't widen the filter.
func TestAdminAuditRepository_TargetWildcardEscaped(t *testing.T) {
	db := newTestDB(t)
	repo := sqlite.NewAdminAuditRepository(db.DB)
	ctx := context.Background()

	mustInsert := func(target string) {
		t.Helper()
		if err := repo.Insert(ctx, &persistence.AdminAuditEntry{
			Principal: "p", Source: "ui", Action: "a", Target: target,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustInsert("100%verified")
	mustInsert("regular-target")

	rows, err := repo.List(ctx, persistence.AdminAuditFilter{TargetPrefix: "100%", PageSize: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("target=100%%: want 1 row (escaped literal), got %d", len(rows))
	}
	if rows[0].Target != "100%verified" {
		t.Errorf("target match: got %q", rows[0].Target)
	}
}
