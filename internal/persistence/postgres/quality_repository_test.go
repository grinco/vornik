package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// Pins the A1 role-aggregate query's arg (window) + column order (project_id,
// role, total, passing, passing_prompt_tokens) → quality.RoleAggregate mapping.
// A schema/column rename surfaces here.
func TestQualityRepository_RoleAggregates(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewQualityRepository(db)

	since := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	// Pin the fan-out fix by SEMANTICS, not just presence: the query must
	// canonicalise (DISTINCT ON), exclude superseded rows, AND order by
	// recorded_at DESC (latest wins — a flip to ASC or a dropped filter fails).
	canonRe := "(?s)" + regexp.QuoteMeta("DISTINCT ON (o.execution_id, o.step_id)") +
		".*" + regexp.QuoteMeta("outcome NOT IN ('superseded', 'orphaned')") +
		".*" + regexp.QuoteMeta("ORDER BY o.execution_id, o.step_id, o.recorded_at DESC")
	mock.ExpectQuery(canonRe).
		WithArgs(since).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "total", "passing", "passing_prompt_tokens"}).
			AddRow("assistant", "researcher", int64(100), int64(80), int64(800000)).
			AddRow("janka", "researcher", int64(40), int64(30), int64(400000)))

	got, err := repo.RoleQualityAggregates(context.Background(), since)
	if err != nil {
		t.Fatalf("RoleQualityAggregates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d aggregates, want 2", len(got))
	}
	if got[0].ProjectID != "assistant" || got[0].Role != "researcher" ||
		got[0].Total != 100 || got[0].Passing != 80 || got[0].PromptTokens != 800000 {
		t.Errorf("row0 = %+v, want assistant/researcher 100/80/800000", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Pins the A2 task-aggregate query: the CTE canonicalises steps, the outer
// select maps (project_id, workflow_id, total, passing, passing_prompt_tokens)
// → quality.TaskAggregate. A2 is the load-bearing bar (COMPLETED-and-no-hard-fail)
// and was previously untested (review-20260721-78d1 #7).
func TestQualityRepository_TaskAggregates(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewQualityRepository(db)

	since := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	// Pin both the canonicalisation AND the A2 hard-fail bar.
	a2Re := "(?s)" + regexp.QuoteMeta("DISTINCT ON (o.execution_id, o.step_id)") +
		".*" + regexp.QuoteMeta("outcome NOT IN ('superseded', 'orphaned')") +
		".*" + regexp.QuoteMeta("bool_or(c.outcome IN ('schema_violation','failed','refused'))")
	mock.ExpectQuery(a2Re).
		WithArgs(since).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "workflow_id", "total", "passing", "passing_prompt_tokens"}).
			AddRow("assistant", "research", int64(203), int64(155), int64(65861755)).
			AddRow("assistant", "adaptive", int64(131), int64(112), int64(2293744)).
			// NULL workflow_id (task created without a workflow) must not crash
			// the scan — live bug 2026-07-21.
			AddRow("assistant", nil, int64(7), int64(5), int64(1000)))

	got, err := repo.TaskQualityAggregates(context.Background(), since)
	if err != nil {
		t.Fatalf("TaskQualityAggregates: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d aggregates, want 3", len(got))
	}
	if got[2].WorkflowID != "" || got[2].Total != 7 {
		t.Errorf("NULL workflow_id row = %+v, want WorkflowID='' Total=7", got[2])
	}
	if got[0].ProjectID != "assistant" || got[0].WorkflowID != "research" ||
		got[0].Total != 203 || got[0].Passing != 155 || got[0].PromptTokens != 65861755 {
		t.Errorf("row0 = %+v, want assistant/research 203/155/65861755", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Pins the (swarm,role) percentile query: project→swarm map passed as arrays
// ($2/$3), percentile_disc over the per-step token distribution, canonicalised.
func TestQualityRepository_RolePercentiles(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewQualityRepository(db)

	since := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	re := "(?s)" + regexp.QuoteMeta("unnest($2::text[])") +
		".*" + regexp.QuoteMeta("DISTINCT ON (o.execution_id, o.step_id)") +
		".*" + regexp.QuoteMeta("percentile_disc(0.95) WITHIN GROUP (ORDER BY pt)")
	mock.ExpectQuery(re).
		WithArgs(since, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"swarm_id", "role", "n", "p95", "p99"}).
			AddRow("assistant-swarm", "researcher", int64(835), int64(776253), int64(1857554)))

	got, err := repo.RolePercentiles(context.Background(), since,
		[]string{"assistant", "janka"}, []string{"assistant-swarm", "assistant-swarm"})
	if err != nil {
		t.Fatalf("RolePercentiles: %v", err)
	}
	if len(got) != 1 || got[0].Swarm != "assistant-swarm" || got[0].Role != "researcher" ||
		got[0].N != 835 || got[0].P95 != 776253 || got[0].P99 != 1857554 {
		t.Fatalf("got %+v, want assistant-swarm/researcher 835/776253/1857554", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Pins the bounded A1 twin (design §4.1): recorded_at is BOUNDED both sides
// (>= $1 AND < $2), two time args are passed in order, and the fan-out
// canonicalisation is preserved.
func TestQualityRepository_RoleAggregatesBetween(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewQualityRepository(db)

	from := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	re := "(?s)" + regexp.QuoteMeta("DISTINCT ON (o.execution_id, o.step_id)") +
		".*" + regexp.QuoteMeta("o.recorded_at >= $1 AND o.recorded_at < $2") +
		".*" + regexp.QuoteMeta("outcome NOT IN ('superseded', 'orphaned')")
	mock.ExpectQuery(re).
		WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "role", "total", "passing", "passing_prompt_tokens"}).
			AddRow("assistant", "researcher", int64(50), int64(40), int64(400000)))

	got, err := repo.RoleQualityAggregatesBetween(context.Background(), from, to)
	if err != nil {
		t.Fatalf("RoleQualityAggregatesBetween: %v", err)
	}
	if len(got) != 1 || got[0].ProjectID != "assistant" || got[0].Total != 50 || got[0].Passing != 40 {
		t.Fatalf("got %+v, want assistant/researcher 50/40", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// Pins the bounded A2 twin (design §4.1): recorded_at bounded both sides, two
// time args in order, terminal-task rollup preserved.
func TestQualityRepository_TaskAggregatesBetween(t *testing.T) {
	db, mock, cleanup := newMockDBTX(t)
	defer cleanup()
	repo := NewQualityRepository(db)

	from := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 28, 0, 0, 0, 0, time.UTC)
	re := "(?s)" + regexp.QuoteMeta("o.recorded_at >= $1 AND o.recorded_at < $2") +
		".*" + regexp.QuoteMeta("t.status IN ('COMPLETED','FAILED','CANCELLED')")
	mock.ExpectQuery(re).
		WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{"project_id", "workflow_id", "total", "passing", "passing_prompt_tokens"}).
			AddRow("assistant", "research", int64(20), int64(16), int64(1600000)))

	got, err := repo.TaskQualityAggregatesBetween(context.Background(), from, to)
	if err != nil {
		t.Fatalf("TaskQualityAggregatesBetween: %v", err)
	}
	if len(got) != 1 || got[0].WorkflowID != "research" || got[0].Total != 20 || got[0].Passing != 16 {
		t.Fatalf("got %+v, want assistant/research 20/16", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
