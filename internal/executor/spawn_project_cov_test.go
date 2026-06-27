package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

// spawnProjectCov_countErrRepo embeds mockSpawnRepo but fails
// CountForProjectSince so the rate-limit branch surfaces the error.
type spawnProjectCov_countErrRepo struct {
	*mockSpawnRepo
}

func (r *spawnProjectCov_countErrRepo) CountForProjectSince(_ context.Context, _ string, _ time.Time) (int64, error) {
	return 0, errors.New("count query failed")
}

// spawnProjectCov_dupCreateRepo returns not-found from
// GetBySpawnedProject (so the idempotence pre-check passes) but
// ErrDuplicateKey from Create — modelling a concurrent-spawn race
// where another worker inserted the row between the check and the
// write.
type spawnProjectCov_dupCreateRepo struct {
	*mockSpawnRepo
}

func (r *spawnProjectCov_dupCreateRepo) Create(_ context.Context, _ *persistence.ProjectSpawn) error {
	return persistence.ErrDuplicateKey
}

func spawnProjectCov_cat() *mockTemplateCatalog {
	return &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		rendered: map[string]map[string]string{
			"sales-campaign": {"configs/projects/sales-q3.yaml": "projectId: sales-q3\n"},
		},
	}
}

// TestSpawnProjectCov_ParentProjectNotFound covers the
// PROJECT_NOT_FOUND branch when the parent project isn't in the
// registry.
func TestSpawnProjectCov_ParentProjectNotFound(t *testing.T) {
	withInterProjectEnabled(t)
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{}, // marketing absent
	}, spawnProjectCov_cat(), newMockSpawnRepo(), t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "PROJECT_NOT_FOUND") {
		t.Fatalf("expected PROJECT_NOT_FOUND, got %v", err)
	}
}

// TestSpawnProjectCov_CountError covers the rate-limit count-error
// branch.
func TestSpawnProjectCov_CountError(t *testing.T) {
	withInterProjectEnabled(t)
	proj := marketingProjectWithSpawn()
	proj.AllowSpawn.MaxSpawnsPerDay = 5 // >0 so the count path runs
	repo := &spawnProjectCov_countErrRepo{mockSpawnRepo: newMockSpawnRepo()}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": proj},
	}, spawnProjectCov_cat(), repo, t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "count recent spawns") {
		t.Fatalf("expected count error, got %v", err)
	}
}

// TestSpawnProjectCov_TemplateNotFound covers the TEMPLATE_NOT_FOUND
// branch: the template passes the allowlist (project allows it) but
// the catalog has no manifest. Uses a wildcard-allow project so the
// allowlist gate passes for an arbitrary slug.
func TestSpawnProjectCov_TemplateNotFound(t *testing.T) {
	withInterProjectEnabled(t)
	proj := &registry.Project{
		ID: "marketing",
		AllowSpawn: registry.ProjectAllowSpawn{
			Templates: []string{"ghost-*"},
		},
	}
	cat := &mockTemplateCatalog{manifests: map[string]templates.Manifest{}} // empty catalog
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": proj},
	}, cat, newMockSpawnRepo(), t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "ghost-template", Params: map[string]any{"projectId": "ghost-1"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "TEMPLATE_NOT_FOUND") {
		t.Fatalf("expected TEMPLATE_NOT_FOUND, got %v", err)
	}
}

// TestSpawnProjectCov_MaterialiseError covers the render-error
// branch.
func TestSpawnProjectCov_MaterialiseError(t *testing.T) {
	withInterProjectEnabled(t)
	cat := &mockTemplateCatalog{
		manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
		renderErr: errors.New("bad template syntax"),
	}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, cat, newMockSpawnRepo(), t.TempDir(), &stubReloader{})

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	_, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err == nil || !contains(err.Error(), "render template") {
		t.Fatalf("expected render-template error, got %v", err)
	}
}

// TestSpawnProjectCov_DuplicateCreateIsIdempotent covers the
// ErrDuplicateKey-on-Create branch (concurrent-spawn race): the
// files are written, but the lineage insert collides, which the
// handler treats as an idempotence win (Skipped=true).
func TestSpawnProjectCov_DuplicateCreateIsIdempotent(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	repo := &spawnProjectCov_dupCreateRepo{mockSpawnRepo: newMockSpawnRepo()}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, spawnProjectCov_cat(), repo, cfgDir, &stubReloader{})

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	result, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err != nil {
		t.Fatalf("duplicate-key create should be treated as idempotent success: %v", err)
	}
	if !result.Skipped {
		t.Error("expected Skipped=true on the concurrent-spawn duplicate-key path")
	}
}

// TestSpawnProjectCov_ReloaderFailureIsBestEffort covers the
// reloader-error branch: a failed registry reload is logged but the
// spawn still succeeds (the file-watcher poll catches the new YAML).
func TestSpawnProjectCov_ReloaderFailureIsBestEffort(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	reloader := &stubReloader{failOn: errors.New("reload boom")}
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, spawnProjectCov_cat(), newMockSpawnRepo(), cfgDir, reloader)

	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	result, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err != nil {
		t.Fatalf("reloader failure must not fail the spawn: %v", err)
	}
	if result.Skipped {
		t.Error("happy spawn should not be skipped despite reloader failure")
	}
	if reloader.calls != 1 {
		t.Errorf("reloader should have been attempted once, got %d", reloader.calls)
	}
}

// spawnProjectCov_initTaskErrRepo embeds MockTaskRepo and fails
// Create so seedInitialTask returns an error (logged, non-fatal).
type spawnProjectCov_initTaskErrRepo struct {
	*MockTaskRepo
}

func (r *spawnProjectCov_initTaskErrRepo) Create(_ context.Context, _ *persistence.Task) error {
	return errors.New("seed task insert failed")
}

// TestSpawnProjectCov_InitialTaskFailureIsBestEffort covers the
// branch where seedInitialTask fails — the project is still
// materialised; the initial-task ID is just left empty.
func TestSpawnProjectCov_InitialTaskFailureIsBestEffort(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"marketing": marketingProjectWithSpawn()},
	}, spawnProjectCov_cat(), newMockSpawnRepo(), cfgDir, &stubReloader{})
	e.taskRepo = &spawnProjectCov_initTaskErrRepo{MockTaskRepo: NewMockTaskRepo()}

	step := &registry.WorkflowStep{
		Type: "spawn_project", Template: "sales-campaign",
		Params:      map[string]any{"projectId": "sales-q3"},
		InitialTask: &registry.WorkflowInitialTask{Workflow: "kickoff", Payload: map[string]any{"brief": "x"}},
	}
	task := &persistence.Task{ID: "t1", ProjectID: "marketing"}
	result, err := e.handleSpawnProjectStep(context.Background(), task, &persistence.Execution{}, "s1", step, nil)
	if err != nil {
		t.Fatalf("initial-task failure must not fail the spawn: %v", err)
	}
	if result.InitialTaskID != "" {
		t.Errorf("expected empty InitialTaskID when seed task creation failed, got %q", result.InitialTaskID)
	}
	if result.SpawnedProject != "sales-q3" {
		t.Error("project should still be materialised")
	}
}

// TestSpawnProjectCov_SeedInitialTaskNil covers the early return in
// seedInitialTask when the initial-task pointer is nil.
func TestSpawnProjectCov_SeedInitialTaskNil(t *testing.T) {
	e, _, _, _, _ := setup()
	id, err := e.seedInitialTask(context.Background(), &persistence.Task{ID: "p"}, "child", nil)
	if err != nil || id != "" {
		t.Errorf("nil initial task → ('', nil), got (%q, %v)", id, err)
	}
}

// TestSpawnProjectCov_RenderedProjectYAMLFallback exercises the
// fallback path of renderedProjectYAML: no exact projects/<slug>.yaml
// match, but a different projects/*.yaml file is present.
func TestSpawnProjectCov_RenderedProjectYAMLFallback(t *testing.T) {
	rendered := map[string]string{
		"configs/projects/other-name.yaml": "projectId: other\n",
		"configs/workflows/wf.yaml":        "id: wf\n",
	}
	body, ok := renderedProjectYAML(rendered, "wanted-slug")
	if !ok {
		t.Fatal("expected fallback to find a projects/*.yaml file")
	}
	if !contains(body, "projectId: other") {
		t.Errorf("fallback returned wrong body: %q", body)
	}

	// No projects/*.yaml at all → ("", false).
	if _, ok := renderedProjectYAML(map[string]string{"configs/workflows/wf.yaml": "x"}, "slug"); ok {
		t.Error("expected (\"\", false) when no project file present")
	}
}

// TestSpawnProjectCov_StringifyParamsMarshalError covers the
// marshal-error branch via an unmarshalable value (a channel).
func TestSpawnProjectCov_StringifyParamsMarshalError(t *testing.T) {
	_, err := stringifyParams(map[string]any{"ch": make(chan int)})
	if err == nil {
		t.Fatal("expected marshal error for an unmarshalable param value")
	}
}

// TestSpawnProjectCov_NoSpawnFileWritten is a guard alongside the
// PROJECT_NOT_FOUND/etc paths — confirms early refusals leave the
// configsDir untouched.
func TestSpawnProjectCov_NoFileOnParentMissing(t *testing.T) {
	withInterProjectEnabled(t)
	cfgDir := t.TempDir()
	e, _ := newSpawnExecutor(&MockWorkflowResolver{projects: map[string]*registry.Project{}}, spawnProjectCov_cat(), newMockSpawnRepo(), cfgDir, &stubReloader{})
	step := &registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign", Params: map[string]any{"projectId": "sales-q3"}}
	_, _ = e.handleSpawnProjectStep(context.Background(), &persistence.Task{ID: "t1", ProjectID: "marketing"}, &persistence.Execution{}, "s1", step, nil)
	if _, statErr := os.Stat(filepath.Join(cfgDir, "configs/projects/sales-q3.yaml")); !os.IsNotExist(statErr) {
		t.Error("no file should be written when the parent project is missing")
	}
}
