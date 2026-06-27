package api

// Coverage tests for the doctor self-check functions that only ran
// their query-error branch before: checkStaleLeases, checkOrphanedWatchers,
// checkStuckExecutions, checkTaskStateAudit. Drives the full
// scan → WARNING → fix → OK ladder plus the row-iteration / query-error
// failure shapes using DATA-DOG/go-sqlmock so no real Postgres is needed.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func doctorCovHandlers(t *testing.T) (*DoctorHandlers, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := NewDoctorHandlers(db)
	// Stuck-execution check publishes a gauge; give it a private
	// registry so the metric is wired and the SetExecutionsStuck call
	// doesn't nil-panic.
	h.SetAPIMetrics(NewAPIMetrics(prometheus.NewRegistry()))
	return h, mock
}

// --- checkStaleLeases ----------------------------------------------

func TestDoctorCov_StaleLeases_NoneFound(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM tasks").
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "lease_expires_at"}))
	check := h.checkStaleLeases(context.Background(), false)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, "stale_leases", check.Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDoctorCov_StaleLeases_QueryError(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM tasks").WillReturnError(errors.New("conn refused"))
	check := h.checkStaleLeases(context.Background(), false)
	assert.Equal(t, "ERROR", check.Status)
	assert.Contains(t, check.Message, "conn refused")
}

func TestDoctorCov_StaleLeases_WarnNoFix(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	rows := sqlmock.NewRows([]string{"id", "project_id", "lease_expires_at"}).
		AddRow("t1", "p1", time.Now().Add(-2*time.Hour)).
		AddRow("t2", "p1", time.Now().Add(-3*time.Hour))
	mock.ExpectQuery("FROM tasks").WillReturnRows(rows)
	check := h.checkStaleLeases(context.Background(), false)
	assert.Equal(t, "WARNING", check.Status)
	assert.Len(t, check.Items, 2)
	assert.Equal(t, 0, check.Fixed)
}

func TestDoctorCov_StaleLeases_FixReleasesAll(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	rows := sqlmock.NewRows([]string{"id", "project_id", "lease_expires_at"}).
		AddRow("t1", "p1", time.Now().Add(-2*time.Hour))
	mock.ExpectQuery("FROM tasks").WillReturnRows(rows)
	mock.ExpectExec("UPDATE tasks").WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	check := h.checkStaleLeases(context.Background(), true)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, 1, check.Fixed)
	assert.Contains(t, check.Message, "released")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- checkOrphanedWatchers -----------------------------------------

func TestDoctorCov_OrphanedWatchers_NoneFound(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM task_watchers").
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "chat_id", "status"}))
	check := h.checkOrphanedWatchers(context.Background(), false)
	assert.Equal(t, "OK", check.Status)
}

func TestDoctorCov_OrphanedWatchers_QueryError(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM task_watchers").WillReturnError(errors.New("boom"))
	check := h.checkOrphanedWatchers(context.Background(), false)
	assert.Equal(t, "ERROR", check.Status)
}

func TestDoctorCov_OrphanedWatchers_FixDeletes(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	rows := sqlmock.NewRows([]string{"task_id", "chat_id", "status"}).
		AddRow("t1", int64(42), "COMPLETED").
		AddRow("t1", int64(43), "COMPLETED") // dup task → deduped to one DELETE
	mock.ExpectQuery("FROM task_watchers").WillReturnRows(rows)
	mock.ExpectExec("DELETE FROM task_watchers").WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	check := h.checkOrphanedWatchers(context.Background(), true)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, 1, check.Fixed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- checkStuckExecutions ------------------------------------------

func TestDoctorCov_StuckExecutions_NoneFound(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM executions").
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "project_id", "status", "created_at"}))
	check := h.checkStuckExecutions(context.Background(), false)
	assert.Equal(t, "OK", check.Status)
}

func TestDoctorCov_StuckExecutions_QueryError(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("FROM executions").WillReturnError(errors.New("down"))
	check := h.checkStuckExecutions(context.Background(), false)
	assert.Equal(t, "ERROR", check.Status)
}

func TestDoctorCov_StuckExecutions_FixMarksFailed(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	rows := sqlmock.NewRows([]string{"id", "task_id", "project_id", "status", "created_at"}).
		AddRow("e1", "t1", "p1", "RUNNING", time.Now().Add(-2*time.Hour)).
		AddRow("e2", "t2", "p1", "PENDING", time.Now().Add(-3*time.Hour))
	mock.ExpectQuery("FROM executions").WillReturnRows(rows)
	mock.ExpectExec("UPDATE executions").WithArgs("e1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE executions").WithArgs("e2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	check := h.checkStuckExecutions(context.Background(), true)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, 2, check.Fixed)
	assert.Contains(t, check.Message, "marked")
	require.NoError(t, mock.ExpectationsWereMet())
}

// --- checkTaskStateAudit -------------------------------------------

func TestDoctorCov_TaskStateAudit_Clean(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	check := h.checkTaskStateAudit(context.Background(), false)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, "no state leaks", check.Message)
}

func TestDoctorCov_TaskStateAudit_QueryError(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("SELECT COUNT").WillReturnError(errors.New("oops"))
	check := h.checkTaskStateAudit(context.Background(), false)
	assert.Equal(t, "ERROR", check.Status)
}

func TestDoctorCov_TaskStateAudit_WarnNoFix(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	check := h.checkTaskStateAudit(context.Background(), false)
	assert.Equal(t, "WARNING", check.Status)
	assert.Contains(t, check.Message, "data leak")
}

func TestDoctorCov_TaskStateAudit_FixCleans(t *testing.T) {
	h, mock := doctorCovHandlers(t)
	mock.ExpectQuery("SELECT COUNT").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(2)))
	mock.ExpectExec("UPDATE tasks").
		WillReturnResult(sqlmock.NewResult(0, 2))
	check := h.checkTaskStateAudit(context.Background(), true)
	assert.Equal(t, "OK", check.Status)
	assert.Equal(t, 2, check.Fixed)
	assert.Contains(t, check.Message, "cleaned")
	require.NoError(t, mock.ExpectationsWereMet())
}
