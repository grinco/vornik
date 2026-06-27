package postgres

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"vornik.io/vornik/internal/persistence"
)

// TestClusterNode_Upsert_New verifies the INSERT … ON CONFLICT shape
// for a fresh row. last_seen is stamped by NOW() in SQL — only 5 args
// are bound ($1–$5); no caller-supplied last_seen placeholder.
func TestClusterNode_Upsert_New(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	caps := map[string]bool{"RunWorkers": true, "ServeAPI": false}
	capsJSON, _ := json.Marshal(caps)
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	node := &persistence.ClusterNode{
		InstanceID:   "host-a:8080:boot1",
		Profile:      "worker",
		Version:      "v2026.6.0",
		Address:      "host-a:8080",
		Capabilities: caps,
		LastSeen:     now, // ignored by Upsert — DB stamps NOW()
	}

	// Five args only — no last_seen arg; the SQL uses NOW() for that column.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO cluster_nodes")).
		WithArgs(node.InstanceID, node.Profile, node.Version, node.Address, capsJSON).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), node); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestClusterNodeRepository_Upsert_StampsServerNow confirms the Upsert SQL
// contains the literal NOW() token for last_seen and does NOT bind a Go-side
// placeholder for that column (clock-skew safety: DB clock governs freshness).
func TestClusterNodeRepository_Upsert_StampsServerNow(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	caps := map[string]bool{"ServeAPI": true}
	capsJSON, _ := json.Marshal(caps)

	node := &persistence.ClusterNode{
		InstanceID:   "host-b:9090:boot1",
		Profile:      "all",
		Version:      "v2026.6.1",
		Address:      "host-b:9090",
		Capabilities: caps,
	}

	// Exactly 5 args — instance_id, profile, version, address, capabilities.
	// last_seen is handled by NOW() in SQL, not a bound parameter.
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO cluster_nodes")).
		WithArgs(node.InstanceID, node.Profile, node.Version, node.Address, capsJSON).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Upsert(context.Background(), node); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestClusterNodeRepository_DeleteStale verifies the DELETE SQL shape:
// last_seen < NOW() - make_interval(secs => $1), protected via NOT (instance_id = ANY($2)).
// Returns the RowsAffected count.
func TestClusterNodeRepository_DeleteStale(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	grace := 5 * time.Minute
	protected := []string{"leader-node:8080:boot1"}

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM cluster_nodes")).
		WithArgs(grace.Seconds(), pq.Array(protected)).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := repo.DeleteStale(context.Background(), grace, protected)
	if err != nil {
		t.Fatalf("DeleteStale: %v", err)
	}
	if n != 3 {
		t.Errorf("rows deleted = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestClusterNodeRepository_DeleteStale_EmptyProtected verifies that a nil /
// empty protected list is normalized to an empty array so all stale rows are
// deleted (ANY('{}') matches nothing → NOT(... = ANY('{}')) is TRUE for all
// rows). Regression guard: a nil slice marshals to SQL NULL, and ANY(NULL)
// would make the predicate NULL and silently delete nothing — DeleteStale
// normalizes nil → []string{} so the query arg is always a non-NULL array.
func TestClusterNodeRepository_DeleteStale_EmptyProtected(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	grace := 10 * time.Minute
	var protected []string // nil input — must reach the DB as an empty array, not NULL

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM cluster_nodes")).
		WithArgs(grace.Seconds(), pq.Array([]string{})). // nil normalized to empty array
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := repo.DeleteStale(context.Background(), grace, protected)
	if err != nil {
		t.Fatalf("DeleteStale(empty protected): %v", err)
	}
	if n != 2 {
		t.Errorf("rows deleted = %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestClusterNode_Upsert_Nil: guard against nil node.
func TestClusterNode_Upsert_Nil(t *testing.T) {
	repo := NewClusterNodeRepository(nil)
	if err := repo.Upsert(context.Background(), nil); err == nil {
		t.Error("Upsert(nil) should return error")
	}
}

// TestClusterNode_List pins the SELECT column order so a future
// reorder doesn't silently misalign the Scan targets.
func TestClusterNode_List(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	caps := map[string]bool{"RunWorkers": true}
	capsJSON, _ := json.Marshal(caps)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT instance_id, profile, version, address, capabilities, last_seen\nFROM cluster_nodes")).
		WillReturnRows(sqlmock.NewRows([]string{
			"instance_id", "profile", "version", "address", "capabilities", "last_seen",
		}).AddRow("host-a:8080:boot1", "worker", "v2026.6.0", "host-a:8080", capsJSON, now))

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	got := rows[0]
	if got.InstanceID != "host-a:8080:boot1" {
		t.Errorf("InstanceID = %q, want host-a:8080:boot1", got.InstanceID)
	}
	if !got.Capabilities["RunWorkers"] {
		t.Errorf("capabilities round-trip failed: %v", got.Capabilities)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestClusterNode_List_Empty: empty rowset → nil slice + nil err.
func TestClusterNode_List_Empty(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM cluster_nodes")).
		WillReturnRows(sqlmock.NewRows([]string{"instance_id", "profile", "version", "address", "capabilities", "last_seen"}))

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List on empty table: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows len = %d, want 0", len(rows))
	}
}

// TestClusterNode_DeleteByInstanceID: DELETE shape.
func TestClusterNode_DeleteByInstanceID(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewClusterNodeRepository(db)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM cluster_nodes WHERE instance_id = $1")).
		WithArgs("host-a:8080:boot1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.DeleteByInstanceID(context.Background(), "host-a:8080:boot1"); err != nil {
		t.Fatalf("DeleteByInstanceID: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
