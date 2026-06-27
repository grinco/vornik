package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

// mockSpawnRepo is the thread-safe in-memory
// ProjectSpawnRepository used by the spawn handler tests.
type mockSpawnRepo struct {
	mu        sync.Mutex
	rows      map[string]*persistence.ProjectSpawn
	bySpawned map[string]*persistence.ProjectSpawn
	createErr error
}

func newMockSpawnRepo() *mockSpawnRepo {
	return &mockSpawnRepo{
		rows:      make(map[string]*persistence.ProjectSpawn),
		bySpawned: make(map[string]*persistence.ProjectSpawn),
	}
}

func (m *mockSpawnRepo) Create(_ context.Context, s *persistence.ProjectSpawn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	if _, ok := m.bySpawned[s.SpawnedProject]; ok {
		return persistence.ErrDuplicateKey
	}
	if s.ID == "" {
		s.ID = persistence.GenerateID("ps")
	}
	cp := *s
	m.rows[s.ID] = &cp
	m.bySpawned[s.SpawnedProject] = &cp
	return nil
}

func (m *mockSpawnRepo) GetBySpawnedProject(_ context.Context, spawnedProjectID string) (*persistence.ProjectSpawn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.bySpawned[spawnedProjectID]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (m *mockSpawnRepo) CountForProjectSince(_ context.Context, parentProjectID string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, r := range m.rows {
		if r.ParentProject == parentProjectID && !r.CreatedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// mockTemplateCatalog is the narrow fake for the spawn
// handler's spawnTemplateCatalog dependency. Mirrors the
// production *templates.Catalog API surface — Get + Materialise
// — without requiring on-disk template files.
type mockTemplateCatalog struct {
	manifests map[string]templates.Manifest
	rendered  map[string]map[string]string // slug → target → body
	renderErr error
}

func (m *mockTemplateCatalog) Get(slug string) (templates.Manifest, bool) {
	mf, ok := m.manifests[slug]
	return mf, ok
}

func (m *mockTemplateCatalog) MaterialiseFiles(mf templates.Manifest, params map[string]string) (map[string]string, error) {
	if m.renderErr != nil {
		return nil, m.renderErr
	}
	out, ok := m.rendered[mf.Slug]
	if !ok {
		return map[string]string{}, nil
	}
	return out, nil
}

// stubReloader tracks Reload() call count so tests can assert
// the spawn handler hits the registry-reload path.
type stubReloader struct {
	mu     sync.Mutex
	calls  int
	failOn error
}

func (s *stubReloader) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failOn
}

// newSpawnExecutor stamps a minimal executor with the spawn-
// related deps wired. Caller can override fields after the
// constructor returns.
func newSpawnExecutor(
	workflows WorkflowResolver,
	cat spawnTemplateCatalog,
	spawn persistence.ProjectSpawnRepository,
	configsDir string,
	reloader registryReloader,
) (*Executor, *MockTaskRepo) {
	e, _, _, _, tr := setup()
	e.workflows = workflows
	e.templateCatalog = cat
	e.spawnRepo = spawn
	e.configsDir = configsDir
	e.registryReloader = reloader
	return e, tr
}

// TestSpawnProject_FeatureFlagOff_ReturnsDisabled — secure
// default. With the flag unset, the handler refuses without
// touching any dependencies.
func TestSpawnProject_FeatureFlagOff_ReturnsDisabled(t *testing.T) {
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
	}
	spawn := newMockSpawnRepo()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"marketing": marketingProjectWithSpawn(),
		},
	}, cat, spawn, t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if !errors.Is(err, errSpawnDisabled) {
		t.Fatalf("expected errSpawnDisabled, got %v", err)
	}
}

// TestSpawnProject_TemplateNotAllowed asserts the per-project
// AllowSpawn.Templates gate fires before any disk write. A
// project that omits a template from its allowlist can't
// spawn it even if the template exists in the catalog.
func TestSpawnProject_TemplateNotAllowed(t *testing.T) {
	withInterProjectEnabled(t)
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
	}
	spawn := newMockSpawnRepo()
	// Project has no AllowSpawn block — closed by default.
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"marketing": {ID: "marketing"},
		},
	}, cat, spawn, t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "TEMPLATE_NOT_ALLOWED") {
		t.Fatalf("expected TEMPLATE_NOT_ALLOWED, got %v", err)
	}
}

// TestSpawnProject_MaxSpawnsPerDay enforces the rate limit.
// The repo stub reports 5 spawns in the past 24h; the
// project's cap is 5; the handler must refuse.
func TestSpawnProject_MaxSpawnsPerDay(t *testing.T) {
	withInterProjectEnabled(t)
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
	}
	spawn := newMockSpawnRepo()
	now := time.Now()
	for i := 0; i < 5; i++ {
		row := &persistence.ProjectSpawn{
			ID: persistence.GenerateID("ps"), ParentProject: "marketing",
			SpawnedProject: "prev-" + string(rune('a'+i)), CreatedAt: now,
		}
		spawn.rows[row.ID] = row
		spawn.bySpawned[row.SpawnedProject] = row
	}
	proj := marketingProjectWithSpawn()
	proj.AllowSpawn.MaxSpawnsPerDay = 5
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": proj},
	}, cat, spawn, t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "SPAWN_LIMIT_EXCEEDED") {
		t.Fatalf("expected SPAWN_LIMIT_EXCEEDED, got %v", err)
	}
}

// TestSpawnProject_HappyPath writes the rendered template to a
// real tmp dir, asserts the project_spawns row landed, the
// reloader was called, and the configsDir/projects directory
// holds the rendered YAML.
// TestSpawnProject_ConsentBypassRejected: a rendered child that would
// accept calls from its spawner (here marketing) at spawn time is a
// consent-bypass and must be refused before any file or lineage row is
// written. (Inter-project review batch 4.)
func TestSpawnProject_ConsentBypassRejected(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	yamlBody := "projectId: sales-q3-2026\ndisplayName: X\nswarmId: sales\n" +
		"defaultWorkflowId: campaign-runner\nacceptCallsFrom:\n  - marketing\n"
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		rendered: map[string]map[string]string{
			"sales-campaign": {"configs/projects/sales-q3-2026.yaml": yamlBody},
		},
	}
	spawn := newMockSpawnRepo()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, spawn, cfgDir, &stubReloader{})

	step := &registry.WorkflowStep{
		Type: "spawn_project", Template: "sales-campaign",
		Params: map[string]any{"projectId": "sales-q3-2026"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}

	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{ID: "e1"}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "SPAWN_CONSENT_BYPASS") {
		t.Fatalf("expected SPAWN_CONSENT_BYPASS, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(cfgDir, "configs/projects/sales-q3-2026.yaml")); !os.IsNotExist(statErr) {
		t.Error("consent-bypass spawn must not write the project file")
	}
	if len(spawn.rows) != 0 {
		t.Errorf("no lineage row should be persisted on consent-bypass, got %d", len(spawn.rows))
	}
}

func TestSpawnProject_HappyPath(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	// Phase C observability fixtures — assertions at the end
	// of the happy path verify live event + audit row fired.
	pub := &stubLivePub{}
	audit := &stubAdminAuditRepo{}

	yamlBody := "projectId: sales-q3-2026\ndisplayName: Sales Q3 2026\nswarmId: sales\ndefaultWorkflowId: campaign-runner\n"
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{
			"sales-campaign": {Slug: "sales-campaign"},
		},
		rendered: map[string]map[string]string{
			"sales-campaign": {
				"configs/projects/sales-q3-2026.yaml": yamlBody,
			},
		},
	}
	spawn := newMockSpawnRepo()
	reloader := &stubReloader{}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"marketing": marketingProjectWithSpawn(),
		},
	}, cat, spawn, cfgDir, reloader)
	e.livePub = pub
	e.adminAuditRepo = audit

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3-2026", "extra": "data"},
	}
	task := &persistence.Task{ID: "task-marketing", ProjectID: "marketing"}

	result, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{ID: "exec-marketing"}, "step-spawn", step, nil)
	if err != nil {
		t.Fatalf("handleSpawnProjectStep: %v", err)
	}
	if result.SpawnedProject != "sales-q3-2026" {
		t.Errorf("SpawnedProject = %q, want sales-q3-2026", result.SpawnedProject)
	}
	if result.Skipped {
		t.Error("happy-path result should not be marked Skipped")
	}

	// File on disk.
	body, err := os.ReadFile(filepath.Join(cfgDir, "configs/projects/sales-q3-2026.yaml"))
	if err != nil {
		t.Fatalf("rendered YAML not on disk: %v", err)
	}
	if !contains(string(body), "sales-q3-2026") {
		t.Errorf("rendered YAML missing slug: %s", body)
	}

	// File mode tightened by WriteRenderedFilesExclusive.
	if info, _ := os.Stat(filepath.Join(cfgDir, "configs/projects/sales-q3-2026.yaml")); info != nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("rendered file mode = %o, want 0o600 (project YAML carries credentials)", perm)
		}
	}

	// Lineage row.
	row, _ := spawn.GetBySpawnedProject(context.Background(), "sales-q3-2026")
	if row == nil {
		t.Fatal("lineage row not stored")
	}
	if row.ParentTaskID != "task-marketing" || row.ParentProject != "marketing" || row.ParentStepID != "step-spawn" {
		t.Errorf("lineage row wrong: %+v", row)
	}
	if row.TemplateSlug != "sales-campaign" {
		t.Errorf("template_slug = %q", row.TemplateSlug)
	}

	// Reloader called.
	if reloader.calls != 1 {
		t.Errorf("reloader called %d times, want exactly 1", reloader.calls)
	}

	// Phase C: live event + audit row.
	if events := pub.byKind(livepubsub.KindProjectSpawned); len(events) != 1 {
		t.Errorf("expected one project_spawned event, got %d", len(events))
	}
	if rows := audit.byAction(auditActionProjectSpawn); len(rows) != 1 {
		t.Errorf("expected one project.spawn audit row, got %d", len(rows))
	}
}

// TestSpawnProject_Idempotent asserts a second execution of
// the same step for the same slug is a no-op (LLD §10): no
// duplicate row, no second materialise. Returns
// Skipped=true so the caller's workflow can branch on it.
func TestSpawnProject_Idempotent(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		rendered:  map[string]map[string]string{"sales-campaign": {"configs/projects/sales-q3.yaml": "projectId: sales-q3\n"}},
	}
	spawn := newMockSpawnRepo()
	reloader := &stubReloader{}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, spawn, cfgDir, reloader)

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}

	// First run — materialises.
	first, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s", step, nil)
	if err != nil || first.Skipped {
		t.Fatalf("first run should materialise cleanly, got skipped=%v err=%v", first.Skipped, err)
	}

	// Second run — must short-circuit. NOT increment row count,
	// NOT call the reloader again (no new YAML to load).
	second, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s", step, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !second.Skipped {
		t.Error("second run should be Skipped=true (idempotent)")
	}
	if reloader.calls != 1 {
		t.Errorf("reloader should not fire on idempotent skip, got %d calls", reloader.calls)
	}
}

// TestSpawnProject_InitialTaskCreated asserts the seed task
// lands in the spawned project's queue when initial_task is
// declared on the step.
func TestSpawnProject_InitialTaskCreated(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		rendered:  map[string]map[string]string{"sales-campaign": {"configs/projects/sales-x.yaml": "projectId: sales-x\n"}},
	}
	spawn := newMockSpawnRepo()
	e, tr := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, spawn, cfgDir, &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-x"},
		InitialTask: &registry.WorkflowInitialTask{
			Workflow: "kickoff",
			Payload:  map[string]any{"brief": "launch Q3"},
		},
	}
	task := &persistence.Task{ID: "task-parent", ProjectID: "marketing"}

	result, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s", step, nil)
	if err != nil {
		t.Fatalf("handleSpawnProjectStep: %v", err)
	}
	if result.InitialTaskID == "" {
		t.Fatal("initial task ID should be populated")
	}
	stored, _ := tr.Get(context.Background(), result.InitialTaskID)
	if stored == nil {
		t.Fatal("initial task not stored")
	}
	if stored.ProjectID != "sales-x" {
		t.Errorf("initial task project = %q, want sales-x", stored.ProjectID)
	}
	if stored.WorkflowID == nil || *stored.WorkflowID != "kickoff" {
		t.Errorf("initial task workflow = %v, want kickoff", stored.WorkflowID)
	}
	if stored.ParentTaskID == nil || *stored.ParentTaskID != "task-parent" {
		t.Errorf("initial task parent = %v, want task-parent", stored.ParentTaskID)
	}
	if stored.Status != persistence.TaskStatusQueued {
		t.Errorf("initial task status = %q, want QUEUED", stored.Status)
	}
}

// TestSpawnProject_PROJECT_EXISTS — WriteRenderedFilesExclusive
// returns ExistingTargetError on collision. Handler must
// surface the dedicated error code so the workflow's on_fail
// branch can route accordingly.
func TestSpawnProject_ProjectExists(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	// Pre-create the file so the writer returns ExistingTargetError.
	if err := os.MkdirAll(filepath.Join(cfgDir, "configs/projects"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "configs/projects/sales-q3.yaml"), []byte("existing"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		rendered:  map[string]map[string]string{"sales-campaign": {"configs/projects/sales-q3.yaml": "projectId: sales-q3\n"}},
	}
	spawn := newMockSpawnRepo()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, spawn, cfgDir, &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"projectId": "sales-q3"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s", step, nil)
	if err == nil || !contains(err.Error(), "PROJECT_EXISTS") {
		t.Fatalf("expected PROJECT_EXISTS, got %v", err)
	}
}

// TestSpawnProject_MissingSlugRejected asserts the handler
// refuses when neither projectId nor name is in params — the
// spawned project's slug must be deterministic + operator-
// declared.
func TestSpawnProject_MissingSlugRejected(t *testing.T) {
	withInterProjectEnabled(t)
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
	}
	spawn := newMockSpawnRepo()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, spawn, t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{
		Type:     "spawn_project",
		Template: "sales-campaign",
		Params:   map[string]any{"other": "value"},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s", step, nil)
	if err == nil || !contains(err.Error(), "projectId") {
		t.Fatalf("expected slug-missing error, got %v", err)
	}
}

func marketingProjectWithSpawn() *registry.Project {
	return &registry.Project{
		ID: "marketing",
		AllowSpawn: registry.ProjectAllowSpawn{
			Templates:       []string{"sales-campaign", "partner-*"},
			MaxSpawnsPerDay: 10,
		},
	}
}

// TestStringifyParams covers the v1 param conversion table.
// Workflows pass arbitrary YAML values; the template renderer
// wants strings. The handler converts predictably so workflow
// authors know what they're getting.
func TestStringifyParams(t *testing.T) {
	got, err := stringifyParams(map[string]any{
		"name":      "sales-q3",
		"count":     5,
		"price":     9.99,
		"enabled":   true,
		"watchlist": []string{"AAPL", "MSFT"},
		"meta":      map[string]any{"region": "EU"},
		"empty":     nil,
	})
	if err != nil {
		t.Fatalf("stringifyParams: %v", err)
	}
	checks := map[string]string{
		"name":      "sales-q3",
		"count":     "5",
		"price":     "9.99",
		"enabled":   "true",
		"empty":     "",
		"watchlist": `["AAPL","MSFT"]`,
		"meta":      `{"region":"EU"}`,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("param %q = %q, want %q", k, got[k], want)
		}
	}
}
