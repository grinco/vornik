package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// auditCov_insertErrRepo records Insert calls and returns an error
// so the writeAuditRow insert-error branch runs.
type auditCov_insertErrRepo struct {
	calls int
}

func (r *auditCov_insertErrRepo) Insert(_ context.Context, _ *persistence.AdminAuditEntry) error {
	r.calls++
	return errors.New("audit insert failed")
}

func (r *auditCov_insertErrRepo) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// TestAuditCov_NilRepoGuards covers the nil-adminAuditRepo guards on
// every writer — none should panic and none should attempt a write.
func TestAuditCov_NilRepoGuards(t *testing.T) {
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	e.adminAuditRepo = nil
	ctx := context.Background()
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	cpc := &persistence.CrossProjectCall{ID: "c", CalleeProject: "callee"}
	spawn := &persistence.ProjectSpawn{ID: "s", SpawnedProject: "child"}

	// All of these must no-op cleanly with a nil repo.
	e.recordCPCAuditCreate(ctx, task, "step", cpc, "callee-task")
	e.recordCPCAuditResolve(ctx, cpc, "completed", "")
	e.recordSpawnAudit(ctx, task, "step", spawn, "")
	e.recordCPCRefusal(ctx, task, "step", "callee", auditActionCPCDepthExceeded, "too deep", 9, []string{"p"})
	e.writeAuditRow(ctx, "action", "target", map[string]any{"k": "v"})
}

// TestAuditCov_SpawnAuditWithInitialTask covers the initialTaskID
// branch of recordSpawnAudit (the field gets added to after-state).
func TestAuditCov_SpawnAuditWithInitialTask(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	e.adminAuditRepo = audit
	spawn := &persistence.ProjectSpawn{
		ID: "sp1", SpawnedProject: "child", ParentProject: "parent",
		ParentTaskID: "pt", ParentStepID: "ps", TemplateSlug: "tmpl",
	}
	e.recordSpawnAudit(context.Background(), &persistence.Task{ID: "pt"}, "ps", spawn, "init-task-99")
	rows := audit.byAction(auditActionProjectSpawn)
	if len(rows) != 1 {
		t.Fatalf("expected one spawn audit row, got %d", len(rows))
	}
	if !contains(rows[0].After, "init-task-99") {
		t.Errorf("after-state should carry the initial_task_id, got %q", rows[0].After)
	}
}

// TestAuditCov_RefusalWritesRow covers recordCPCRefusal's happy path
// (audit repo wired) so the row lands with the refusal action.
func TestAuditCov_RefusalWritesRow(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	e.adminAuditRepo = audit
	e.recordCPCRefusal(context.Background(), &persistence.Task{ID: "t", ProjectID: "marketing"},
		"step", "architect", auditActionCPCCycleDetected, "cycle", 3, []string{"marketing", "architect"})
	if rows := audit.byAction(auditActionCPCCycleDetected); len(rows) != 1 {
		t.Errorf("expected one cycle-detected audit row, got %d", len(rows))
	}
}

// TestAuditCov_WriteAuditRowMarshalError covers the marshal-error
// branch of writeAuditRow: an unmarshalable after-state value (a
// channel) is logged + the row skipped, never panicking.
func TestAuditCov_WriteAuditRowMarshalError(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	e.adminAuditRepo = audit
	e.writeAuditRow(context.Background(), "action", "target", map[string]any{"bad": make(chan int)})
	if len(audit.rows) != 0 {
		t.Errorf("marshal failure should skip the insert, got %d rows", len(audit.rows))
	}
}

// TestAuditCov_WriteAuditRowInsertError covers the insert-error
// branch: a failing repo is logged + swallowed (best-effort).
func TestAuditCov_WriteAuditRowInsertError(t *testing.T) {
	repo := &auditCov_insertErrRepo{}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, newMockCPCRepo())
	e.adminAuditRepo = repo
	e.writeAuditRow(context.Background(), "action", "target", map[string]any{"k": "v"})
	if repo.calls != 1 {
		t.Errorf("expected one insert attempt, got %d", repo.calls)
	}
}
