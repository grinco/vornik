// Package service: tests for the admin-page data adapters that
// wrap raw SQL + the registry into the ui-facing shapes. Each
// adapter is small but lives below an HTTP handler that the rest
// of the test infra can't reach without a full container, so the
// unit tests here are how the SQL surface is pinned.
package service

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestNewAdminReadinessFromAPI_NilSafe — passing a nil api.Server
// must return a nil ReadinessProvider so the UI's render path
// (which already nil-checks the provider) renders an empty state
// instead of panicking.
func TestNewAdminReadinessFromAPI_NilSafe(t *testing.T) {
	got := newAdminReadinessFromAPI(nil)
	if got != nil {
		t.Errorf("expected nil ReadinessProvider, got %T", got)
	}
}

// TestAdminReadinessFromAPI_NilReceiverReturnsNil — the
// ReadinessChecks method is called by the UI's render path. If the
// adapter is wrapped in a typed nil (it can happen via the
// interface-of-nil pattern), it must still return nil rather than
// dereferencing the embedded api server.
func TestAdminReadinessFromAPI_NilReceiverReturnsNil(t *testing.T) {
	var a *adminReadinessFromAPI
	if got := a.ReadinessChecks(context.Background()); got != nil {
		t.Errorf("nil receiver: expected nil, got %v", got)
	}
	// Receiver with nil .s should also be safe.
	a = &adminReadinessFromAPI{s: nil}
	if got := a.ReadinessChecks(context.Background()); got != nil {
		t.Errorf("nil .s: expected nil, got %v", got)
	}
}

// TestNewAdminLeaseAudit_NilDB — nil DB means "this surface isn't
// wired"; the constructor must return nil so the UI hides the page
// instead of panicking on first query.
func TestNewAdminLeaseAudit_NilDB(t *testing.T) {
	if got := newAdminLeaseAudit(nil); got != nil {
		t.Errorf("nil DB should yield nil source, got %T", got)
	}
}

// TestAdminLeaseAudit_CountByStatus_GroupsRows — the SQL must return
// a map[status]count keyed on the new_status column. Pins the query
// shape (GROUP BY new_status) and result decode.
func TestAdminLeaseAudit_CountByStatus_GroupsRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	a := &adminLeaseAudit{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks_lease_audit GROUP BY new_status")).
		WillReturnRows(sqlmock.NewRows([]string{"new_status", "n"}).
			AddRow("LEASED", int64(100)).
			AddRow("FAILED", int64(5)))

	got, err := a.CountByStatus(context.Background())
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if got["LEASED"] != 100 || got["FAILED"] != 5 {
		t.Errorf("counts mismatch: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestAdminLeaseAudit_CountByStatus_PropagatesDBError — a transient
// DB failure must surface to the caller rather than render an
// empty map (the UI's caller distinguishes "no rows" from "DB
// down").
func TestAdminLeaseAudit_CountByStatus_PropagatesDBError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	a := &adminLeaseAudit{db: db}

	mock.ExpectQuery("tasks_lease_audit").
		WillReturnError(errors.New("simulated connection reset"))

	_, err := a.CountByStatus(context.Background())
	if err == nil {
		t.Error("expected DB error to propagate, got nil")
	}
}

// TestAdminLeaseAudit_Recent_DefaultsAndDecodes — limit ≤ 0 must
// snap to 50 per the comment ("most recent N"). Rows decode into
// the ui-side struct verbatim.
func TestAdminLeaseAudit_Recent_DefaultsAndDecodes(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	a := &adminLeaseAudit{db: db}

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM tasks_lease_audit")).
		WithArgs(50). // default snap
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "changed_at", "old_status", "new_status",
			"old_lease_id", "new_lease_id", "sql_snippet",
		}).AddRow(1, "t1", now, "QUEUED", "LEASED", "", "lease_abc", "UPDATE …"))

	rows, err := a.Recent(context.Background(), 0)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].TaskID != "t1" || rows[0].NewStatus != "LEASED" {
		t.Errorf("row decode mismatch: %+v", rows[0])
	}
}

// TestNewAdminStuckExecs_NilDB — same nil-safe contract as the
// other admin adapters.
func TestNewAdminStuckExecs_NilDB(t *testing.T) {
	if got := newAdminStuckExecs(nil); got != nil {
		t.Errorf("nil DB should yield nil source, got %T", got)
	}
}

// TestAdminStuckExecs_RecentWatchdogFailures_Default — limit ≤ 0
// snaps to 20 per the docstring. The SQL must filter on the
// `watchdog%` prefix (the watchdog package's error-code convention)
// — a regression that drops the prefix filter would surface every
// FAILED execution as a "stuck" entry.
func TestAdminStuckExecs_RecentWatchdogFailures_Default(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	a := &adminStuckExecs{db: db}

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("error_code LIKE 'watchdog%'")).
		WithArgs(20).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "project_id", "workflow_id",
			"started_at", "updated_at", "error_code", "error_message",
		}).AddRow("exec_1", "task_1", "p1", "w1", now, now, "watchdog/stuck", "lease lost"))

	rows, err := a.RecentWatchdogFailures(context.Background(), 0)
	if err != nil {
		t.Fatalf("RecentWatchdogFailures: %v", err)
	}
	if len(rows) != 1 || rows[0].ErrorCode != "watchdog/stuck" {
		t.Errorf("row decode mismatch: %+v", rows)
	}
}

// TestAdminStuckExecs_RecentWatchdogFailures_HonoursExplicitLimit —
// explicit positive limit threads through to the query argument.
func TestAdminStuckExecs_RecentWatchdogFailures_HonoursExplicitLimit(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	a := &adminStuckExecs{db: db}

	mock.ExpectQuery(regexp.QuoteMeta("error_code LIKE 'watchdog%'")).
		WithArgs(5).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "task_id", "project_id", "workflow_id",
			"started_at", "updated_at", "error_code", "error_message",
		}))

	if _, err := a.RecentWatchdogFailures(context.Background(), 5); err != nil {
		t.Errorf("explicit limit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestNewAdminMCPInventory_NilManager — nil mcp.Manager yields nil
// source so the UI hides the integrations page rather than rendering
// empty stat tiles that look like a broken backend.
func TestNewAdminMCPInventory_NilManager(t *testing.T) {
	if got := newAdminMCPInventory(nil); got != nil {
		t.Errorf("nil manager should yield nil source, got %T", got)
	}
}

// TestAdminMCPInventory_Snapshot_NilReceiver — typed-nil safety.
// The Snapshot method must not dereference a nil manager.
func TestAdminMCPInventory_Snapshot_NilReceiver(t *testing.T) {
	var a *adminMCPInventory
	got := a.Snapshot()
	if got.ProjectCount != 0 || got.ServerCount != 0 {
		t.Errorf("nil receiver snapshot should be zero-valued, got %+v", got)
	}
	a = &adminMCPInventory{m: nil}
	got = a.Snapshot()
	if got.ProjectCount != 0 || got.ServerCount != 0 {
		t.Errorf("nil manager snapshot: got %+v", got)
	}
}

// TestNewAdminMCPRefresher_NilArgs — RefreshAll needs both a manager
// AND a registry; either being nil must yield a nil refresher.
func TestNewAdminMCPRefresher_NilArgs(t *testing.T) {
	if got := newAdminMCPRefresher(nil, nil); got != nil {
		t.Errorf("nil/nil: expected nil refresher, got %T", got)
	}
}

// TestAdminMCPRefresher_RefreshAll_NoConfigPanicsCleanly — refreshing
// with no manager/registry returns an error rather than panicking.
// The UI's button handler relies on the error to surface "MCP not
// configured" rather than 500.
func TestAdminMCPRefresher_RefreshAll_NoConfig(t *testing.T) {
	a := &adminMCPRefresher{} // no m, no r
	if err := a.RefreshAll(context.Background()); err == nil {
		t.Error("expected error from RefreshAll with no manager/registry")
	}
}

// TestNewAdminMCPConfig_NilRegistry — same nil-safe shape: no
// registry means no config to list.
func TestNewAdminMCPConfig_NilRegistry(t *testing.T) {
	if got := newAdminMCPConfig(nil); got != nil {
		t.Errorf("nil registry should yield nil source, got %T", got)
	}
}

// TestAdminMCPConfig_ConfiguredMCPServers_NilReceiver — typed-nil
// safety on the read method.
func TestAdminMCPConfig_ConfiguredMCPServers_NilReceiver(t *testing.T) {
	var a *adminMCPConfig
	if got := a.ConfiguredMCPServers(); got != nil {
		t.Errorf("nil receiver: expected nil rows, got %v", got)
	}
}
