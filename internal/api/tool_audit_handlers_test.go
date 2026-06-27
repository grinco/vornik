package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// capturingLiveSub records Publish calls so tests can assert the live events
// emitted from IngestToolAudit (C6).
type capturingLiveSub struct {
	published []struct{ execID, kind string }
}

func (c *capturingLiveSub) Subscribe(string, int64) (<-chan livepubsub.LiveEvent, func(), error) {
	return nil, func() {}, nil
}

func (c *capturingLiveSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return nil, func() {}, nil
}

func (c *capturingLiveSub) Publish(_ context.Context, executionID, kind string, _ any) int64 {
	c.published = append(c.published, struct{ execID, kind string }{executionID, kind})
	return 0
}

// TestIngestToolAudit_PublishesLiveToolEvents — C6: an in-container agent's
// per-call tool-audit report must surface on the execution's /live stream
// (the daemon's chat tap can't see in-container tools). The endpoint publishes
// tool_call_started + tool_call_finished keyed by the entry's execution.
func TestIngestToolAudit_PublishesLiveToolEvents(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	sub := &capturingLiveSub{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithLiveSubscriber(sub))

	body := `{"audit_id":"a1","project_id":"proj-a","execution_id":"exec-1","tool_name":"run_shell","tool_input":"git status","tool_output":"nothing to commit"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	var started, finished bool
	for _, p := range sub.published {
		if p.execID != "exec-1" {
			t.Errorf("live event published for wrong execution %q", p.execID)
		}
		switch p.kind {
		case livepubsub.KindToolCallStarted:
			started = true
		case livepubsub.KindToolCallFinished:
			finished = true
		}
	}
	if !started || !finished {
		t.Fatalf("want tool_call_started + tool_call_finished published, got %+v", sub.published)
	}
}

// TestIngestToolAudit_NoExecutionIDNoPublish — without an execution_id there's
// no live stream to target; the row still persists (204) but nothing publishes.
func TestIngestToolAudit_NoExecutionIDNoPublish(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	sub := &capturingLiveSub{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithLiveSubscriber(sub))

	body := `{"audit_id":"a2","project_id":"proj-a","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(sub.published) != 0 {
		t.Fatalf("no execution_id should publish nothing, got %+v", sub.published)
	}
}

// taskScopedKeyCtx returns a context carrying a task-scoped DB key
// Identity bound to boundTaskID (name "agent:task_<id>"), plus the
// project scope. Mirrors what AuthMiddleware stamps for a per-task agent
// key. Used by the B3 task-binding tests.
func taskScopedKeyCtx(parent context.Context, boundTaskID, projectID string) context.Context {
	id := &auth.Identity{
		Extra: map[string]any{
			auth.ExtraDBKeyRow: &persistence.APIKey{
				Name:      persistence.TaskKeyNamePrefix + boundTaskID,
				ProjectID: projectID,
			},
		},
	}
	ctx := context.WithValue(parent, identityKey, id)
	ctx = context.WithValue(ctx, projectIDKey, []string{projectID})
	return ctx
}

// TestIngestToolAudit_RejectsTaskKeyWritingOtherTask — Finding B3. A
// per-task key for task X (same project) must not write an audit row for
// task Y. Pre-fix the handler checked task→project but not key→task, so
// any task's key could forge rows for sibling tasks in the same project.
func TestIngestToolAudit_RejectsTaskKeyWritingOtherTask(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithTaskRepository(taskRepo))

	body := `{"audit_id":"a1","project_id":"proj-a","task_id":"task-Y","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.logged != nil {
		t.Fatalf("Log called despite task-key/task_id mismatch: %#v", repo.logged)
	}
}

// TestIngestToolAudit_AcceptsTaskKeyWritingOwnTask — the same per-task
// key writing its OWN task's row succeeds (B3).
func TestIngestToolAudit_AcceptsTaskKeyWritingOwnTask(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithTaskRepository(taskRepo))

	body := `{"audit_id":"a1","project_id":"proj-a","task_id":"task-X","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	req = req.WithContext(taskScopedKeyCtx(req.Context(), "task-X", "proj-a"))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.logged == nil || repo.logged.TaskID != "task-X" {
		t.Fatalf("own-task row not persisted: %#v", repo.logged)
	}
}

// TestIngestToolAudit_NonTaskScopedKeyKeepsProjectBehavior — an
// admin/operator key (not task-scoped) keeps project-level behavior:
// it may write any task's row within its project scope (B3 must not
// regress the non-task-scoped path).
func TestIngestToolAudit_NonTaskScopedKeyKeepsProjectBehavior(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-a"}, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo), WithTaskRepository(taskRepo))

	// Admin-style key: a DB key whose name is NOT task-scoped.
	id := &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: &persistence.APIKey{Name: "operator-key", ProjectID: "proj-a"}}}
	body := `{"audit_id":"a1","project_id":"proj-a","task_id":"task-any","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), identityKey, id)
	ctx = context.WithValue(ctx, projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req.WithContext(ctx))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
}

// capturingToolAuditRepo records what reaches the persistence layer
// so the assertions can confirm both that legitimate calls succeed
// AND that authorisation failures never write a row.
type capturingToolAuditRepo struct {
	logged *persistence.ToolAuditEntry
}

func (m *capturingToolAuditRepo) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	m.logged = e
	return nil
}
func (m *capturingToolAuditRepo) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}
func (m *capturingToolAuditRepo) CountByTool(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

// TestIngestToolAudit_AcceptsValidRequest — happy path. Confirms the
// row reaches the repo when scope and IDs line up.
func TestIngestToolAudit_AcceptsValidRequest(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo))

	body := `{"audit_id":"a1","project_id":"proj-a","task_id":"task1","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s, want 204", rec.Code, rec.Body.String())
	}
	if repo.logged == nil || repo.logged.ID != "a1" || repo.logged.ProjectID != "proj-a" {
		t.Fatalf("repo did not capture row: %#v", repo.logged)
	}
}

func TestIngestToolAudit_RejectsOversizedBody(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	if repo.logged != nil {
		t.Fatalf("Log called for oversized body: %#v", repo.logged)
	}
}

// TestIngestToolAudit_RejectsProjectScopeMismatch is the audit fix:
// a key with scope ["proj-a"] cannot post audit rows claiming
// project_id="proj-b". Pre-fix the row landed under proj-b, polluting
// that project's audit ledger.
func TestIngestToolAudit_RejectsProjectScopeMismatch(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithToolAuditRepository(repo))

	body := `{"audit_id":"a1","project_id":"proj-b","task_id":"task1","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), projectIDKey, []string{"proj-a"})
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.logged != nil {
		t.Fatalf("Log called despite scope mismatch: %#v", repo.logged)
	}
}

// TestIngestToolAudit_RejectsTaskFromOtherProject — body's project_id
// matches the API key's scope but task_id resolves to a different
// project's row. Either tampering or a bug — refuse.
func TestIngestToolAudit_RejectsTaskFromOtherProject(t *testing.T) {
	repo := &capturingToolAuditRepo{}
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "proj-b"}, nil
		},
	}
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithToolAuditRepository(repo),
		WithTaskRepository(taskRepo),
	)

	body := `{"audit_id":"a1","project_id":"proj-a","task_id":"task-stolen","tool_name":"file_read"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/tool-audit", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	server.IngestToolAudit(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if repo.logged != nil {
		t.Fatalf("Log called despite task-project mismatch: %#v", repo.logged)
	}
}
