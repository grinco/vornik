package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/fixitdoctor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

// --- config_apply_gate (CE) ------------------------------------------------

func TestFindGateFeature(t *testing.T) {
	f, ok := findGateFeature("instinct.enabled")
	if !ok || f.ID != "instinct" {
		t.Fatalf("expected the instinct feature, got %+v ok=%v", f, ok)
	}
	if _, ok := findGateFeature("not.a.real.gate"); ok {
		t.Fatal("expected an unregistered key to resolve to nothing")
	}
}

func TestRenderGateDiff(t *testing.T) {
	if got := renderGateDiff(nil); got == "" {
		t.Fatal("expected a non-empty no-op message")
	}
	got := renderGateDiff([]featuredoctor.GateChange{{Key: "k", From: false, To: true}})
	if !strings.Contains(got, "k") || !strings.Contains(got, "false") || !strings.Contains(got, "true") {
		t.Fatalf("expected the diff to mention key/from/to, got %q", got)
	}
}

func TestFixitGatePipeline_UnregisteredKey_ActionConflict(t *testing.T) {
	p := &fixitGatePipeline{}
	if _, err := p.Plan(context.Background(), "bogus.key"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("Plan err = %v, want ErrActionConflict", err)
	}
	if _, err := p.Apply(context.Background(), "bogus.key"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("Apply err = %v, want ErrActionConflict", err)
	}
}

func TestFixitGatePipeline_Apply_NoConfigPath(t *testing.T) {
	p := &fixitGatePipeline{deps: featuredoctor.Deps{Config: fixitConfigReader{cfg: &config.Config{}}}}
	if _, err := p.Apply(context.Background(), "instinct.enabled"); err == nil {
		t.Fatal("expected an error with no config path wired")
	}
}

func TestFixitGatePipeline_Apply_HappyPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	seed := "api:\n  auth_enabled: false\ninstinct:\n  enabled: false\n"
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	p := &fixitGatePipeline{
		deps:       featuredoctor.Deps{Config: fixitConfigReader{cfg: &config.Config{}}},
		configPath: configPath,
		reloader:   fixitConfigReloaderAdapter{},
	}
	diff, err := p.Plan(context.Background(), "instinct.enabled")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if diff == "" {
		t.Fatal("expected a non-empty diff for a real gate flip")
	}
	detail, err := p.Apply(context.Background(), "instinct.enabled")
	// instinct is RestartRequired: Apply writes+validates but does not
	// reload, so a nil reloader is fine here.
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if detail == "" {
		t.Fatal("expected a non-empty apply detail")
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	if !strings.Contains(string(written), "enabled: true") {
		t.Fatalf("expected the gate to be flipped on disk, got:\n%s", written)
	}
}

// --- fixitTaskLister --------------------------------------------------------

type fixitCountingTaskRepo struct {
	persistence.TaskRepository
	counts map[persistence.TaskStatus]int64
	err    error
}

func (c fixitCountingTaskRepo) Count(_ context.Context, f persistence.TaskFilter) (int64, error) {
	if c.err != nil {
		return 0, c.err
	}
	if f.Status == nil {
		return 0, nil
	}
	return c.counts[*f.Status], nil
}

func TestFixitTaskLister_HasActiveTasks(t *testing.T) {
	if active, err := (fixitTaskLister{}).HasActiveTasks(context.Background()); active || err != nil {
		t.Fatalf("nil repo should report inactive/no-error, got %v %v", active, err)
	}
	repo := fixitCountingTaskRepo{counts: map[persistence.TaskStatus]int64{persistence.TaskStatusRunning: 1}}
	if active, err := (fixitTaskLister{repo: repo}).HasActiveTasks(context.Background()); err != nil || !active {
		t.Fatalf("expected active=true, got %v %v", active, err)
	}
	repo2 := fixitCountingTaskRepo{counts: map[persistence.TaskStatus]int64{persistence.TaskStatusLeased: 1}}
	if active, err := (fixitTaskLister{repo: repo2}).HasActiveTasks(context.Background()); err != nil || !active {
		t.Fatalf("expected active=true (leased), got %v %v", active, err)
	}
	repo3 := fixitCountingTaskRepo{err: errors.New("db down")}
	if _, err := (fixitTaskLister{repo: repo3}).HasActiveTasks(context.Background()); err == nil {
		t.Fatal("expected the Count error to propagate")
	}
}

func TestFixitConfigReloaderAdapter_NilSafe(t *testing.T) {
	if err := (fixitConfigReloaderAdapter{}).Reload(context.Background()); err != nil {
		t.Fatalf("nil reloader should no-op, got %v", err)
	}
}

// --- retry_task -------------------------------------------------------------

type retryTaskRepo struct {
	persistence.TaskRepository
	task           *persistence.Task
	getErr         error
	transitioned   bool
	requeueErr     error
	requeueAttempt int
	requeueMaxAtt  int
}

func (r *retryTaskRepo) Get(_ context.Context, _ string) (*persistence.Task, error) {
	return r.task, r.getErr
}

func (r *retryTaskRepo) RequeueTerminalTask(_ context.Context, _ string, attempt, maxAttempts int) (bool, error) {
	r.requeueAttempt, r.requeueMaxAtt = attempt, maxAttempts
	return r.transitioned, r.requeueErr
}
func (r *retryTaskRepo) ListRetryInFlight(context.Context, []string, time.Time) ([]*persistence.Task, error) {
	return nil, nil
}

func TestFixitTaskRetrier_NotFound(t *testing.T) {
	repo := &retryTaskRepo{getErr: persistence.ErrNotFound}
	if _, err := (fixitTaskRetrier{tasks: repo}).Retry(context.Background(), "p1", "t1"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict", err)
	}
}

func TestFixitTaskRetrier_WrongProject(t *testing.T) {
	repo := &retryTaskRepo{task: &persistence.Task{ID: "t1", ProjectID: "other", Status: persistence.TaskStatusFailed}}
	if _, err := (fixitTaskRetrier{tasks: repo}).Retry(context.Background(), "p1", "t1"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict", err)
	}
}

func TestFixitTaskRetrier_NotTerminal(t *testing.T) {
	repo := &retryTaskRepo{task: &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning}}
	if _, err := (fixitTaskRetrier{tasks: repo}).Retry(context.Background(), "p1", "t1"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict", err)
	}
}

func TestFixitTaskRetrier_AlreadyRequeued_Conflict(t *testing.T) {
	repo := &retryTaskRepo{
		task:         &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, Attempt: 1, MaxAttempts: 3},
		transitioned: false,
	}
	if _, err := (fixitTaskRetrier{tasks: repo}).Retry(context.Background(), "p1", "t1"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict (first-wins/409)", err)
	}
}

func TestFixitTaskRetrier_HappyPath(t *testing.T) {
	repo := &retryTaskRepo{
		task:         &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed, Attempt: 1, MaxAttempts: 1},
		transitioned: true,
	}
	detail, err := (fixitTaskRetrier{tasks: repo}).Retry(context.Background(), "p1", "t1")
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if detail == "" {
		t.Fatal("expected a non-empty detail")
	}
	if repo.requeueAttempt != 2 || repo.requeueMaxAtt != 2 {
		t.Fatalf("expected attempt bumped to 2 (and max bumped alongside), got attempt=%d max=%d", repo.requeueAttempt, repo.requeueMaxAtt)
	}
}

// --- set_secret --------------------------------------------------------

type fakeProjectResolver struct {
	proj *registry.Project
}

func (f fakeProjectResolver) ResolveProjectConfig(_ string) (*registry.Project, *registry.Swarm, *registry.Workflow, error) {
	return f.proj, nil, nil, nil
}

type memorySecretWriter struct{ set map[string]string }

func (m *memorySecretWriter) Set(name, value string) error {
	if m.set == nil {
		m.set = map[string]string{}
	}
	m.set[name] = value
	return nil
}

func TestFixitSecretSetter_NilDoctor(t *testing.T) {
	if err := (fixitSecretSetter{}).Set(context.Background(), "p1", "X", "v"); err == nil {
		t.Fatal("expected an error with no doctor wired")
	}
}

func TestFixitSecretSetter_NonDeclaredField_ActionConflict(t *testing.T) {
	proj := &registry.Project{ID: "p1"}
	proj.Permissions.Secrets = []string{"DECLARED_ONE"}
	writer := &memorySecretWriter{}
	doctor := projectdoctor.New(projectdoctor.Deps{
		Registry:     fakeProjectResolver{proj: proj},
		SecretWriter: writer,
	})
	err := fixitSecretSetter{doctor: doctor}.Set(context.Background(), "p1", "NOT_DECLARED", "v")
	if !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict", err)
	}
	if len(writer.set) != 0 {
		t.Fatalf("SecretWriter must not be called for a non-declared field, got %v", writer.set)
	}
}

func TestFixitSecretSetter_Declared_HappyPath(t *testing.T) {
	proj := &registry.Project{ID: "p1"}
	proj.Permissions.Secrets = []string{"DECLARED_ONE"}
	writer := &memorySecretWriter{}
	doctor := projectdoctor.New(projectdoctor.Deps{
		Registry:     fakeProjectResolver{proj: proj},
		SecretWriter: writer,
	})
	err := fixitSecretSetter{doctor: doctor}.Set(context.Background(), "p1", "DECLARED_ONE", "the-value")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if writer.set["DECLARED_ONE"] != "the-value" {
		t.Fatalf("expected the declared secret written, got %v", writer.set)
	}
}

// --- config_apply (EE) — real DB + real ApplyEngine ------------------------

func newTestProposalRepo(t *testing.T) persistence.ProposalRepository {
	t.Helper()
	db, err := sqlite.Connect(context.Background(), sqlite.DefaultConfig())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return sqlite.NewProposalRepository(db.DB)
}

func TestFixitConfigProposalPipeline_FilesAsFixItDoctor_AppliesAsHumanUser_Real(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("chat:\n  timeout: 10s\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	repo := newTestProposalRepo(t)
	engine := &controlplane.ApplyEngine{
		Proposals: repo,
		ConfigDir: dir,
		Reload:    func() error { return nil },
		Logger:    zerolog.Nop(),
	}
	pipe := &fixitConfigProposalPipeline{proposals: repo, applier: engine, configPath: configPath}

	proposalID, diff, err := pipe.File(context.Background(), "proj-1", "chat.timeout", "30s")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if diff == "" || proposalID == "" {
		t.Fatalf("expected a non-empty proposal id/diff, got id=%q diff=%q", proposalID, diff)
	}

	stored, err := repo.GetByID(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if stored.ProposedBy != "fix_it_doctor" {
		t.Fatalf("ProposedBy = %q, want fix_it_doctor", stored.ProposedBy)
	}
	if stored.Status != persistence.ProposalStatusDraft {
		t.Fatalf("Status = %q, want DRAFT before Apply", stored.Status)
	}

	// Apply as the HUMAN operator — proposer (fix_it_doctor) != approver
	// (op-1) clears ErrProposalSelfApprove BY CONSTRUCTION, proven here
	// against the real DB-backed SetStatus guard, not a mock.
	if err := pipe.Apply(context.Background(), proposalID, "op-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	applied, err := repo.GetByID(context.Background(), proposalID)
	if err != nil {
		t.Fatalf("GetByID after apply: %v", err)
	}
	if applied.Status != persistence.ProposalStatusApplied {
		t.Fatalf("Status = %q, want APPLIED", applied.Status)
	}
	if applied.Approver != "op-1" {
		t.Fatalf("Approver = %q, want op-1 (the human)", applied.Approver)
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(written), "30s") {
		t.Fatalf("expected the config change on disk, got:\n%s", written)
	}

	// Rollback restores the pre-apply snapshot.
	if err := pipe.Rollback(context.Background(), proposalID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	restored, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read after rollback: %v", err)
	}
	if !strings.Contains(string(restored), "10s") {
		t.Fatalf("expected the pre-apply content restored, got:\n%s", restored)
	}
}

func TestFixitConfigProposalPipeline_NotWired(t *testing.T) {
	pipe := &fixitConfigProposalPipeline{}
	if _, _, err := pipe.File(context.Background(), "p1", "k", "v"); err == nil {
		t.Fatal("expected an error with no proposals repo/config path wired")
	}
	if err := pipe.Apply(context.Background(), "id", "actor"); err == nil {
		t.Fatal("expected an error with no proposals/applier wired")
	}
	if err := pipe.Rollback(context.Background(), "id"); err == nil {
		t.Fatal("expected an error with no applier wired")
	}
}

func TestFixitConfigProposalPipeline_ApplyBusy_ActionConflict(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("chat:\n  timeout: 10s\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	repo := newTestProposalRepo(t)
	engine := &controlplane.ApplyEngine{
		Proposals: repo,
		ConfigDir: dir,
		Reload:    func() error { return nil },
		Logger:    zerolog.Nop(),
		HasActiveTasks: func(context.Context, string) (bool, error) {
			return true, nil // simulate a busy daemon/project
		},
	}
	pipe := &fixitConfigProposalPipeline{proposals: repo, applier: engine, configPath: configPath}

	proposalID, _, err := pipe.File(context.Background(), "proj-1", "chat.timeout", "30s")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if err := pipe.Apply(context.Background(), proposalID, "op-1"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict (busy)", err)
	}
}

func TestFixitConfigProposalPipeline_ApplySelfApprove_ActionConflict(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("chat:\n  timeout: 10s\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	repo := newTestProposalRepo(t)
	engine := &controlplane.ApplyEngine{Proposals: repo, ConfigDir: dir, Reload: func() error { return nil }, Logger: zerolog.Nop()}
	pipe := &fixitConfigProposalPipeline{proposals: repo, applier: engine, configPath: configPath}

	proposalID, _, err := pipe.File(context.Background(), "proj-1", "chat.timeout", "30s")
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	// Approving as "fix_it_doctor" itself (the proposer) must be refused —
	// this is the exact self-approval scenario the design forbids; the
	// dispatcher always passes the HUMAN operator here in production, but
	// this proves the guard actually fires if it somehow didn't.
	if err := pipe.Apply(context.Background(), proposalID, "fix_it_doctor"); !errors.Is(err, fixitdoctor.ErrActionConflict) {
		t.Fatalf("err = %v, want ErrActionConflict (self-approve)", err)
	}
}

func TestBlastRadiusForProject(t *testing.T) {
	if got := blastRadiusForProject(""); got != persistence.ProposalScopeDaemon {
		t.Errorf("got %q, want daemon scope for an empty project", got)
	}
	if got := blastRadiusForProject("p1"); got != persistence.ProposalScopeProject {
		t.Errorf("got %q, want project scope", got)
	}
}

// --- wireFixItDispatcher -----------------------------------------------

func TestWireFixItDispatcher_NilSafe(t *testing.T) {
	wireFixItDispatcher(nil, nil, nil) // must not panic
	svc := &fixitdoctor.Service{}
	wireFixItDispatcher(svc, nil, nil) // must not panic; leaves everything nil
	if svc.GatePipeline != nil {
		t.Fatal("expected GatePipeline nil with a nil Container")
	}
}

func TestWireFixItDispatcher_WiresEverythingAvailable(t *testing.T) {
	proj := &registry.Project{ID: "p1"}
	writer := &memorySecretWriter{}
	doctor := projectdoctor.New(projectdoctor.Deps{Registry: fakeProjectResolver{proj: proj}, SecretWriter: writer})
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
		repos: &storage.Repositories{
			Tasks:      &retryTaskRepo{},
			Proposals:  newTestProposalRepo(t),
			AdminAudit: nil,
		},
	}
	svc := &fixitdoctor.Service{}
	wireFixItDispatcher(svc, c, doctor)
	if svc.GatePipeline == nil {
		t.Error("expected GatePipeline wired")
	}
	if svc.ConfigProposals == nil {
		t.Error("expected ConfigProposals wired (repos.Proposals present)")
	}
	if svc.ActionTaskRetrier == nil {
		t.Error("expected ActionTaskRetrier wired (repos.Tasks present)")
	}
	if svc.SecretSetter == nil {
		t.Error("expected SecretSetter wired (projectDoctor present)")
	}
	// Task 3.4: IntegrationReprober is now ALWAYS wired (against
	// *Container.uiServer, read lazily) — a nil c.uiServer degrades the
	// adapter itself to fail-closed at call time (see
	// fixit_ui_bridge_adapter_test.go), it no longer leaves the seam
	// unwired at construction time the way task 3.3 deferred it.
	if svc.IntegrationReprober == nil {
		t.Error("expected IntegrationReprober wired (task 3.4)")
	}
}
