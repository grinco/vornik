package projectarchive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubDeleter captures DeleteProjectData calls so tests can pin the
// ordering between DB wipe / file unlink / audit row write.
type stubDeleter struct {
	mu     sync.Mutex
	called []string
	err    error
	// What the deleter reports about the derived-cache eviction. Default zero
	// value is the "not wired" state, which is what the audit row must be able
	// to distinguish from "nothing to evict".
	cacheEvictionRan        bool
	cachedEmbeddingsEvicted int
}

func (s *stubDeleter) DeleteProjectData(_ context.Context, projectID string) (persistence.ProjectDataStats, error) {
	s.mu.Lock()
	s.called = append(s.called, projectID)
	s.mu.Unlock()
	if s.err != nil {
		return persistence.ProjectDataStats{}, s.err
	}
	return persistence.ProjectDataStats{
		TablesCleared:           3,
		RowsDeleted:             42,
		CacheEvictionRan:        s.cacheEvictionRan,
		CachedEmbeddingsEvicted: s.cachedEmbeddingsEvicted,
	}, nil
}

// stubAudit collects rows so tests can assert the
// project.deleted audit was emitted.
type stubAudit struct {
	mu   sync.Mutex
	rows []*persistence.AdminAuditEntry
}

func (s *stubAudit) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubAudit) List(_ context.Context, _ persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, errors.New("not used")
}

// newTestRegistry builds an in-memory registry with the given
// projects keyed by ID. Bypasses the YAML loader so tests don't
// have to maintain swarm fixtures.
func newTestRegistry(projects map[string]*registry.Project) *registry.Registry {
	r := registry.New()
	// The registry's exported surface doesn't let tests inject
	// projects directly; round-trip through Load against a
	// temporary config dir would need swarm + workflow fixtures.
	// Use the package-internal hook via a helper we just added —
	// see registry/test_seed.go.
	registry.SeedForTest(r, projects)
	return r
}

// stubWiper records DeleteProjectArtifacts calls + the return
// values. Used by TestSweeper_UsesArtifactWiper to confirm the
// preferred path lands.
type stubWiper struct {
	mu    sync.Mutex
	calls []string
	count int
	err   error
}

func (s *stubWiper) DeleteProjectArtifacts(_ context.Context, projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, projectID)
	return s.count, s.err
}

// TestSweeper_UsesArtifactWiper confirms the backend-aware wiper
// is preferred over the fs-path fallback when both are set. Audit
// row carries the deletion count for ops observability.
func TestSweeper_UsesArtifactWiper(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(configDir, "projects", "doomed.yaml")
	if err := os.WriteFile(yamlPath, []byte("projectId: doomed\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"doomed": {ID: "doomed", Lifecycle: registry.ProjectLifecycle{
			Status: "archived", ScheduledDeleteAt: past,
		}},
	})

	wiper := &stubWiper{count: 7}
	audit := &stubAudit{}
	sw := NewSweeper(Config{
		Registry:      reg,
		DataDeleter:   &stubDeleter{},
		ConfigDir:     configDir,
		ArtifactWiper: wiper,
		// Also set ArtifactBasePath to confirm the wiper wins
		// over the fs fallback when both are present.
		ArtifactBasePath: tmp,
		AuditRepo:        audit,
		Reload:           func(_ context.Context) error { return nil },
		Logger:           zerolog.Nop(),
	})

	sw.sweepOnce(context.Background())

	wiper.mu.Lock()
	if len(wiper.calls) != 1 || wiper.calls[0] != "doomed" {
		t.Errorf("wiper.DeleteProjectArtifacts: want [doomed], got %v", wiper.calls)
	}
	wiper.mu.Unlock()

	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.rows) != 1 {
		t.Fatalf("expected 1 audit row; got %d", len(audit.rows))
	}
	if !strings.Contains(audit.rows[0].After, `"artifacts_count":7`) {
		t.Errorf("audit row should carry the wiped artifact count; got %q", audit.rows[0].After)
	}
}

// TestStore_DeleteProjectArtifacts_HappyPath covers the new
// store helper against a LocalBackend fixture. Writes 3 keys
// across two projects; DeleteProjectArtifacts on one leaves the
// other intact.
//
// (Lives in this package because the artifacts package is what's
// under test; importing artifacts here is cycle-safe — projectarchive
// already depends on persistence not artifacts.)

// fixedLeader is a tiny LeaderGate stub. Returns whatever
// IsLeader was set to at construction.
type fixedLeader struct{ leader bool }

func (f *fixedLeader) IsLeader() bool { return f.leader }

// TestSweeper_LeaderGateSkipsTick: a sweeper whose LeaderGate
// reports false skips the wipe entirely. Even past-due projects
// stay on disk. Pairs with the leaderelection package — only the
// elected leader runs the wipe in a multi-replica deployment.
func TestSweeper_LeaderGateSkipsTick(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(configDir, "projects", "doomed.yaml")
	if err := os.WriteFile(yamlPath, []byte("projectId: doomed\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"doomed": {ID: "doomed", Lifecycle: registry.ProjectLifecycle{
			Status: "archived", ScheduledDeleteAt: past,
		}},
	})
	deleter := &stubDeleter{}
	sw := NewSweeper(Config{
		Registry:    reg,
		DataDeleter: deleter,
		ConfigDir:   configDir,
		Logger:      zerolog.Nop(),
		LeaderGate:  &fixedLeader{leader: false},
	})
	sw.sweepOnce(context.Background())

	// Past-due project must STILL be on disk + no DB wipe.
	if _, err := os.Stat(yamlPath); err != nil {
		t.Errorf("yaml should be untouched when non-leader; got stat err=%v", err)
	}
	if len(deleter.called) != 0 {
		t.Errorf("DataDeleter should not be called for non-leader; got %v", deleter.called)
	}
}

// TestSweeper_LeaderGateAllowsTick: positive sanity — when
// IsLeader=true the sweeper runs normally.
func TestSweeper_LeaderGateAllowsTick(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(configDir, "projects", "doomed.yaml")
	if err := os.WriteFile(yamlPath, []byte("projectId: doomed\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"doomed": {ID: "doomed", Lifecycle: registry.ProjectLifecycle{
			Status: "archived", ScheduledDeleteAt: past,
		}},
	})
	deleter := &stubDeleter{}
	sw := NewSweeper(Config{
		Registry:    reg,
		DataDeleter: deleter,
		ConfigDir:   configDir,
		Logger:      zerolog.Nop(),
		LeaderGate:  &fixedLeader{leader: true},
		Reload:      func(_ context.Context) error { return nil },
	})
	sw.sweepOnce(context.Background())

	if len(deleter.called) != 1 || deleter.called[0] != "doomed" {
		t.Errorf("DataDeleter should be called for leader; got %v", deleter.called)
	}
}

// TestSweeper_DeleteFiresOnDueProject covers the happy path:
//   - Registry has one archived project past its scheduledDeleteAt.
//   - Files exist on disk under configDir/projects/.
//   - Sweeper deletes the YAML + companion .md + the artifact dir
//   - DB rows.
//   - Audit row emitted with the wipe scope.
func TestSweeper_DeleteFiresOnDueProject(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	artifactsDir := filepath.Join(tmp, "artifacts")

	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(artifactsDir, "doomed"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yamlPath := filepath.Join(configDir, "projects", "doomed.yaml")
	mdPath := filepath.Join(configDir, "projects", "doomed.md")
	if err := os.WriteFile(yamlPath, []byte("projectId: doomed\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(mdPath, []byte("# doomed\n"), 0o644); err != nil {
		t.Fatalf("write md: %v", err)
	}
	blobPath := filepath.Join(artifactsDir, "doomed", "report.json")
	if err := os.WriteFile(blobPath, []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"doomed": {ID: "doomed", Lifecycle: registry.ProjectLifecycle{
			Status:            "archived",
			ArchivedAt:        past,
			ScheduledDeleteAt: past,
		}},
		"safe": {ID: "safe"},
	})

	deleter := &stubDeleter{}
	audit := &stubAudit{}
	reloadCalled := 0
	sw := NewSweeper(Config{
		Registry:         reg,
		DataDeleter:      deleter,
		ConfigDir:        configDir,
		ArtifactBasePath: artifactsDir,
		AuditRepo:        audit,
		Reload: func(_ context.Context) error {
			reloadCalled++
			return nil
		},
		Logger: zerolog.Nop(),
	})

	sw.sweepOnce(context.Background())

	// File-level effects.
	if _, err := os.Stat(yamlPath); !os.IsNotExist(err) {
		t.Errorf("yaml should be removed; stat err=%v", err)
	}
	if _, err := os.Stat(mdPath); !os.IsNotExist(err) {
		t.Errorf("md should be removed; stat err=%v", err)
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Errorf("artifact blob should be removed; stat err=%v", err)
	}

	// Deleter called with the doomed project, not the safe one.
	deleter.mu.Lock()
	if len(deleter.called) != 1 || deleter.called[0] != "doomed" {
		t.Errorf("deleter calls: want [doomed], got %v", deleter.called)
	}
	deleter.mu.Unlock()

	// Audit row written.
	audit.mu.Lock()
	if len(audit.rows) != 1 {
		t.Errorf("audit rows: want 1, got %d", len(audit.rows))
	} else if audit.rows[0].Action != "project.deleted" {
		t.Errorf("audit action: want project.deleted, got %q", audit.rows[0].Action)
	}
	audit.mu.Unlock()

	if reloadCalled != 1 {
		t.Errorf("reload should be called once; got %d", reloadCalled)
	}
}

// TestSweeper_SkipsActiveAndFuture confirms the sweeper leaves
// non-due rows alone.
func TestSweeper_SkipsActiveAndFuture(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "projects", "fresh.yaml"), []byte("projectId: fresh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"fresh": {ID: "fresh", Lifecycle: registry.ProjectLifecycle{
			Status:            "archived",
			ScheduledDeleteAt: future,
		}},
		"active": {ID: "active"},
	})
	deleter := &stubDeleter{}
	sw := NewSweeper(Config{
		Registry:    reg,
		DataDeleter: deleter,
		ConfigDir:   configDir,
		Logger:      zerolog.Nop(),
	})

	sw.sweepOnce(context.Background())

	if len(deleter.called) != 0 {
		t.Errorf("deleter should not be called for non-due projects; got %v", deleter.called)
	}
	if _, err := os.Stat(filepath.Join(configDir, "projects", "fresh.yaml")); err != nil {
		t.Errorf("fresh.yaml should still exist: %v", err)
	}
}

// TestSweeper_IdempotentWhenFilesGone covers the retry path:
// re-running over a partially-deleted project (DB wipe succeeded
// but a previous sweep crashed before file unlink) shouldn't
// error.
func TestSweeper_IdempotentWhenFilesGone(t *testing.T) {
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "configs")
	if err := os.MkdirAll(filepath.Join(configDir, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No yaml file written — simulating a previously-partial wipe.

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	reg := newTestRegistry(map[string]*registry.Project{
		"gone": {ID: "gone", Lifecycle: registry.ProjectLifecycle{
			Status:            "archived",
			ScheduledDeleteAt: past,
		}},
	})
	sw := NewSweeper(Config{
		Registry:    reg,
		DataDeleter: &stubDeleter{},
		ConfigDir:   configDir,
		Logger:      zerolog.Nop(),
		Reload:      func(_ context.Context) error { return nil },
	})
	// Should NOT panic / fail despite the missing file.
	sw.sweepOnce(context.Background())
}

// TestAssertWithinRoot pins the path-traversal guard the file
// remover relies on. ConfigDir / ArtifactBasePath could in
// theory contain a misconfigured "/" — without the guard a
// sweep would wipe / and break the host.
func TestAssertWithinRoot(t *testing.T) {
	tmp := t.TempDir()
	cases := []struct {
		path string
		root string
		ok   bool
	}{
		{filepath.Join(tmp, "projects", "foo.yaml"), tmp, true},
		{filepath.Join(tmp, "..", "elsewhere"), tmp, false},
		{"/etc/passwd", tmp, false},
		// path == root is now rejected (safepath.AssertUnder treats rel=="." as
		// outside): real callers only ever pass <root>/projects/<id>… sub-paths.
		{tmp, tmp, false},
	}
	for _, tc := range cases {
		err := assertWithinRoot(tc.path, tc.root)
		if (err == nil) != tc.ok {
			t.Errorf("assertWithinRoot(%q, %q) err=%v, ok=%v", tc.path, tc.root, err, tc.ok)
		}
	}
}

// TestSweeper_SweepNowKick covers the on-demand kick path. The
// "Delete now" UI button drops a token on kickCh; the Run loop
// picks it up out of the ticker schedule.
func TestSweeper_SweepNowKick(t *testing.T) {
	sw := NewSweeper(Config{
		Registry: registry.New(),
		Logger:   zerolog.Nop(),
	})
	// SweepNow x2 should collapse into a single pending kick.
	sw.SweepNow(context.Background())
	sw.SweepNow(context.Background())
	if len(sw.kickCh) != 1 {
		t.Errorf("kick channel len: want 1, got %d", len(sw.kickCh))
	}
}

// The wipe now evicts embedding_cache rows a project-scoped DELETE cannot
// reach, and the audit row is where an operator looks for what a deletion
// actually did. Reporting tables and rows while staying silent about the
// derived vectors would make the row look complete when it is not — the same
// defect one layer up from the leak itself.
func TestDeleteProject_AuditRowRecordsTheCacheEviction(t *testing.T) {
	del := &stubDeleter{cacheEvictionRan: true, cachedEmbeddingsEvicted: 9}
	audit := &stubAudit{}
	s := newSweeperForDeleteTest(t, del, audit)

	if err := s.deleteProject(context.Background(), testDeletableProject()); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}
	if len(audit.rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit.rows))
	}
	got := auditPayload(t, audit.rows[0])
	if got["cached_embeddings_evicted"] != float64(9) {
		t.Errorf("cached_embeddings_evicted = %v, want 9", got["cached_embeddings_evicted"])
	}
	if got["cache_eviction_ran"] != true {
		t.Errorf("cache_eviction_ran = %v, want true", got["cache_eviction_ran"])
	}
}

// A deployment whose deleter has no evictor wired reports zero evicted. The row
// must say the eviction did not RUN, or that zero reads as "there was nothing
// derived from this project", which is a claim nobody checked.
func TestDeleteProject_AuditRowSaysWhenEvictionNeverRan(t *testing.T) {
	del := &stubDeleter{} // cacheEvictionRan false — the unwired state
	audit := &stubAudit{}
	s := newSweeperForDeleteTest(t, del, audit)

	if err := s.deleteProject(context.Background(), testDeletableProject()); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}
	got := auditPayload(t, audit.rows[0])
	ran, present := got["cache_eviction_ran"]
	if !present {
		t.Fatal("the audit row does not mention the cache eviction at all")
	}
	if ran != false {
		t.Errorf("cache_eviction_ran = %v, want false", ran)
	}
	if got["cached_embeddings_evicted"] != float64(0) {
		t.Errorf("cached_embeddings_evicted = %v, want 0", got["cached_embeddings_evicted"])
	}
}

// newSweeperForDeleteTest builds a sweeper wired only with what deleteProject
// touches: the deleter, the audit sink, and a config dir with the project's
// files in place so removeProjectFiles has something to unlink.
//
// The stubDeleter is the only field these tests actually vary — everything else
// exists so deleteProject reaches the audit call without erroring on a missing
// dependency. ArtifactBasePath gets an empty temp dir on purpose: the legacy
// direct-disk wipe branch then succeeds against nothing, which keeps artifact
// handling out of assertions about the eviction fields.
func newSweeperForDeleteTest(t *testing.T, deleter *stubDeleter, audit *stubAudit) *Sweeper {
	t.Helper()
	configDir := t.TempDir()
	projectsDir := filepath.Join(configDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectsDir, "doomed.yaml"), []byte("id: doomed\n"), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	return NewSweeper(Config{
		Registry:         newTestRegistry(map[string]*registry.Project{"doomed": testDeletableProject()}),
		DataDeleter:      deleter,
		ConfigDir:        configDir,
		ArtifactBasePath: t.TempDir(),
		AuditRepo:        audit,
		Logger:           zerolog.Nop(),
	})
}

func testDeletableProject() *registry.Project {
	return &registry.Project{ID: "doomed", Lifecycle: registry.ProjectLifecycle{
		Status: "archived", ScheduledDeleteAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}}
}

// auditPayload decodes the sweeper's JSON-marshalled extras.
func auditPayload(t *testing.T, e *persistence.AdminAuditEntry) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(e.After), &out); err != nil {
		t.Fatalf("decode audit payload %q: %v", e.After, err)
	}
	return out
}

// A deployment with no memory data still has the evictor WIRED — it runs, finds
// nothing, and evicts zero. The warning must not fire there, or it becomes
// noise on every deletion and trains operators to ignore it.
//
// Raised as a spuriosity risk by the 2026-09-07 follow-up review, which read the
// warning as keyed on "nothing was evicted". It is keyed on whether the eviction
// RAN: runCacheEviction sets CacheEvictionRan after a successful call regardless
// of the count, so the only way to reach the warning is a nil evictor — which,
// since the constructor split, a caller has to ask for by name. This pins that
// distinction so the next reader does not "fix" it into counting rows.
func TestDeleteProject_NoWarningWhenTheEvictionRanAndFoundNothing(t *testing.T) {
	del := &stubDeleter{cacheEvictionRan: true, cachedEmbeddingsEvicted: 0}
	audit := &stubAudit{}
	var logs strings.Builder
	s := newSweeperForDeleteTest(t, del, audit)
	s.cfg.Logger = zerolog.New(&logs)

	if err := s.deleteProject(context.Background(), testDeletableProject()); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}
	if strings.Contains(logs.String(), "NO embedding-cache eviction") {
		t.Errorf("warned about a missing eviction that actually ran and found nothing:\n%s", logs.String())
	}
	got := auditPayload(t, audit.rows[0])
	if got["cache_eviction_ran"] != true || got["cached_embeddings_evicted"] != float64(0) {
		t.Errorf("audit row = %v, want ran=true evicted=0", got)
	}
}

// The converse: a wipe with NO evictor wired must warn. That is the state the
// warning exists for, and since the constructor split it is only reachable by
// asking for it by name.
func TestDeleteProject_WarnsWhenTheEvictionNeverRan(t *testing.T) {
	del := &stubDeleter{} // cacheEvictionRan false
	audit := &stubAudit{}
	var logs strings.Builder
	s := newSweeperForDeleteTest(t, del, audit)
	s.cfg.Logger = zerolog.New(&logs)

	if err := s.deleteProject(context.Background(), testDeletableProject()); err != nil {
		t.Fatalf("deleteProject: %v", err)
	}
	if !strings.Contains(logs.String(), "NO embedding-cache eviction") {
		t.Errorf("a wipe with no eviction was silent in the log:\n%s", logs.String())
	}
}
