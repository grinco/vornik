package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/registry"
)

// reloadingReg is a tiny ConfigReloader-shaped wrapper so the
// archive handler test can confirm Reload() is invoked and that
// it picks up the YAML change.
type reloadingReg struct {
	reg *registry.Registry
	dir string
}

func (r *reloadingReg) Reload() error {
	return r.reg.Load(r.dir)
}

// stubArchiveSweeper counts SweepNow invocations so the
// delete-now test can confirm the UI kicks the sweeper. SweepNow
// runs on a goroutine spawned by ProjectDeleteNow, so the counter
// is atomic to keep the test's wait-then-read race-free.
type stubArchiveSweeper struct {
	swept atomic.Int32
}

func (s *stubArchiveSweeper) SweepNow(_ context.Context) {
	s.swept.Add(1)
}

// seedProjectFixture writes a minimal project YAML (and the
// matching swarm/workflow .md frontmatter the loader requires) so
// the archive handler can read+rewrite the file and the registry
// can reload without complaining about missing references.
func seedProjectFixture(t *testing.T, dir, projectID string) {
	t.Helper()
	must := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	must(filepath.Join(dir, "projects", projectID+".yaml"), `projectId: `+projectID+`
displayName: `+projectID+`
swarmId: test-swarm
defaultWorkflowId: test-wf
`)
	must(filepath.Join(dir, "swarms", "test-swarm.md"), `---
swarmId: test-swarm
displayName: Test
leadRole: lead
roles:
  - name: lead
    description: lead
    count: 1
    model: ollama/test:1
    runtime:
      image: vornik-agent:latest
---

# test-swarm

You are the lead.
`)
	must(filepath.Join(dir, "workflows", "test-wf.md"), `---
workflowId: test-wf
displayName: Test
entrypoint: do
steps:
  do:
    type: agent
    role: lead
    prompt: do the thing
---

# test-wf

Test workflow fixture.
`)
}

// TestParseGraceDuration pins the duration parser: days, hours,
// minutes, default fallback, defensive rejection of negatives.
func TestParseGraceDuration(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", defaultArchiveGraceDuration, false},
		{"default", defaultArchiveGraceDuration, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"-1d", 0, true},
		{"-1h", 0, true},
		{"foo", 0, true},
		{"3y", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseGraceDuration(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProjectArchive_HappyPath drives the archive POST end-to-end:
//
//  1. Seed an active project YAML.
//  2. POST /projects/{id}/archive with grace=7d + reason.
//  3. Confirm the YAML now carries lifecycle.status=archived,
//     archivedAt, scheduledDeleteAt ≈ now+7d, reason.
//  4. Confirm Project.IsArchived() returns true after reload.
//  5. Confirm an audit row was written.
func TestProjectArchive_HappyPath(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "doomed")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(&reloadingReg{reg: reg, dir: dir}),
		WithAdminAuditRepository(audit),
	)

	form := strings.NewReader("grace=7d&reason=end+of+project")
	req := httptest.NewRequest(http.MethodPost, "/projects/doomed/archive", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	p := reg.GetProject("doomed")
	if p == nil {
		t.Fatalf("project disappeared after archive")
	}
	if !p.IsArchived() {
		t.Errorf("IsArchived after archive=false; lifecycle=%+v", p.Lifecycle)
	}
	if p.Lifecycle.Reason != "end of project" {
		t.Errorf("Reason=%q", p.Lifecycle.Reason)
	}
	dt, ok := p.ScheduledDeletion()
	if !ok {
		t.Fatalf("ScheduledDeletion not set after archive")
	}
	want := time.Now().Add(7 * 24 * time.Hour)
	if diff := dt.Sub(want); diff > 5*time.Minute || diff < -5*time.Minute {
		t.Errorf("ScheduledDeleteAt off by %v (want ~7d from now)", diff)
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "project.archive" {
		t.Errorf("audit rows: %+v", audit.rows)
	}
}

// TestProjectUnarchive_RestoresActive drives the unarchive POST
// and checks the YAML no longer reports as archived.
func TestProjectUnarchive_RestoresActive(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "comeback")
	// Pre-archive the project so we have something to unarchive.
	if err := os.WriteFile(filepath.Join(dir, "projects", "comeback.yaml"), []byte(`projectId: comeback
displayName: comeback
swarmId: test-swarm
defaultWorkflowId: test-wf
lifecycle:
  status: archived
  archivedAt: 2026-05-23T12:00:00Z
  scheduledDeleteAt: 2026-05-30T12:00:00Z
  reason: temporary
`), 0o644); err != nil {
		t.Fatalf("write archived yaml: %v", err)
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reg.GetProject("comeback").IsArchived() {
		t.Fatalf("fixture not archived")
	}
	audit := &stubAdminAuditRepo{}
	s := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(&reloadingReg{reg: reg, dir: dir}),
		WithAdminAuditRepository(audit),
	)

	req := httptest.NewRequest(http.MethodPost, "/projects/comeback/unarchive", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if reg.GetProject("comeback").IsArchived() {
		t.Errorf("project still archived after unarchive")
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "project.unarchive" {
		t.Errorf("audit rows: %+v", audit.rows)
	}
}

// TestProjectDeleteNow_RejectsActiveProject covers the
// "delete-now only works on archived projects" guard.
func TestProjectDeleteNow_RejectsActiveProject(t *testing.T) {
	dir := t.TempDir()
	seedProjectFixture(t, dir, "active")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodPost, "/projects/active/delete-now", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: want 409, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestProjectDeleteNow_KicksSweeper covers the happy path: an
// archived project's POST shortens the grace window AND kicks the
// sweeper synchronously so the operator doesn't wait a full tick.
func TestProjectDeleteNow_KicksSweeper(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedProjectFixture(t, dir, "doomed")
	// Re-write the project YAML as already-archived (the helper
	// writes an active one).
	if err := os.WriteFile(filepath.Join(dir, "projects", "doomed.yaml"), []byte(`projectId: doomed
displayName: doomed
swarmId: test-swarm
defaultWorkflowId: test-wf
lifecycle:
  status: archived
  archivedAt: 2026-05-23T12:00:00Z
  scheduledDeleteAt: 2026-06-23T12:00:00Z
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	sweeper := &stubArchiveSweeper{}
	s := NewServer(
		WithProjectRegistry(reg),
		WithConfigReloader(&reloadingReg{reg: reg, dir: dir}),
		WithArchiveSweeper(sweeper),
	)
	req := httptest.NewRequest(http.MethodPost, "/projects/doomed/delete-now", strings.NewReader(""))
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	// SweepNow is dispatched on a goroutine; wait briefly to
	// let it land. Tight loop with a short sleep is fine here —
	// the test is otherwise CPU-bound.
	for i := 0; i < 50 && sweeper.swept.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sweeper.swept.Load(); got != 1 {
		t.Errorf("sweeper kick count = %d, want 1", got)
	}
	// Confirm the scheduledDeleteAt was rewound to ~now.
	dt, ok := reg.GetProject("doomed").ScheduledDeletion()
	if !ok {
		t.Fatalf("ScheduledDeletion not set after delete-now")
	}
	if time.Since(dt) > 1*time.Minute {
		t.Errorf("scheduledDeleteAt should be ~now after delete-now; got %v ago", time.Since(dt))
	}
}

// TestProjectCreateTaskSubmit_BlockedOnArchived confirms an
// archived project's task-create POST is rejected with 409 so
// queue work stops landing the moment the project is archived.
func TestProjectCreateTaskSubmit_BlockedOnArchived(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedProjectFixture(t, dir, "doomed")
	if err := os.WriteFile(filepath.Join(dir, "projects", "doomed.yaml"), []byte(`projectId: doomed
displayName: doomed
swarmId: test-swarm
defaultWorkflowId: test-wf
lifecycle:
  status: archived
  archivedAt: 2026-05-23T12:00:00Z
  scheduledDeleteAt: 2026-05-30T12:00:00Z
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	s := NewServer(WithProjectRegistry(reg))
	form := strings.NewReader("prompt=hello&workflowId=test-wf&taskType=task")
	req := httptest.NewRequest(http.MethodPost, "/projects/doomed/tasks/new", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.projectRouter(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: want 409, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "archived") {
		t.Errorf("body should explain why it was blocked; got: %s", rec.Body.String())
	}
}
