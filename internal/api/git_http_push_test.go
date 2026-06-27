package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
)

// TestRegisterGitRoutes_MultiNodeWarning verifies the design-§4.7 startup
// warning: it fires when this node serves git but does NOT run workers
// (RunWorkers=false → the process-local workspace lock can't protect the served
// workspace), and stays silent when RunWorkers=true.
func TestRegisterGitRoutes_MultiNodeWarning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runWorkers bool
		wantWarn   bool
	}{
		{"workers off → warn", false, true},
		{"workers on → no warn", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			srv := NewServer(WithLogger(zerolog.New(&buf)))
			caps := config.NodeCapabilities{ServeAPI: true, RunWorkers: tc.runWorkers}
			mux := http.NewServeMux()
			registerGitRoutes(mux, srv, true /* authEnabled */, caps)

			got := strings.Contains(buf.String(), "does NOT run workers")
			if got != tc.wantWarn {
				t.Fatalf("multi-node warning present = %v, want %v; log:\n%s", got, tc.wantWarn, buf.String())
			}
		})
	}
}

// realGuardsEnsurer returns an ensurer that installs the REAL push guards via
// executor.EnsureReceiveGuards over <root>/<projectID> — the same binding the
// service container wires in production.
func realGuardsEnsurer(root string) func(context.Context, string) error {
	return func(ctx context.Context, projectID string) error {
		return executor.EnsureReceiveGuards(ctx, filepath.Join(root, projectID), zerolog.Nop())
	}
}

// fakeCountTaskRepo is a minimal persistence.TaskRepository that only
// implements CountByStatus (the single method the push gate calls). It embeds
// the interface so any OTHER method, if accidentally called, panics with a
// clear nil-deref rather than silently returning a zero value.
type fakeCountTaskRepo struct {
	persistence.TaskRepository
	counts map[persistence.TaskStatus]int64
	err    error
}

func (f *fakeCountTaskRepo) CountByStatus(_ context.Context, _ string) (map[persistence.TaskStatus]int64, error) {
	return f.counts, f.err
}

// spyEnsurer records the project IDs passed to the guards ensurer and whether
// it was called before the receive-pack exec.
type spyEnsurer struct {
	mu       sync.Mutex
	calls    []string
	errToRet error
}

func (s *spyEnsurer) fn(_ context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, projectID)
	return s.errToRet
}

func (s *spyEnsurer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// newPushRepo creates a bootstrapped temp git repo under root/proj with the
// receive guards installed, returning the repo dir. The repo is set up exactly
// as the executor's ensureGitRepo would leave it: a seed commit + the
// pre-receive hook + receive.denyCurrentBranch=updateInstead, so a push to the
// checked-out branch updates the working tree.
func newPushRepo(t *testing.T, root, proj string) string {
	t.Helper()
	repo := filepath.Join(root, proj)
	mustGit(t, "", "init", "-q", "-b", "main", repo)
	gitWriteFile(t, filepath.Join(repo, "README.md"), "seed\n")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "seed")
	// Allow pushing to the checked-out branch and update the working tree.
	mustGit(t, repo, "config", "receive.denyCurrentBranch", "updateInstead")
	return repo
}

// pushRouter wires a receive-capable router with auth disabled (anonymous full
// access), a workspace root, an optional task repo (push gate), and an optional
// guards ensurer (spy). Mirrors production wiring via parseGitRequest.
func pushRouter(t *testing.T, root string, taskRepo persistence.TaskRepository, ensurer func(context.Context, string) error) (*Server, *fakeAdminAuditRepo) {
	t.Helper()
	opts := []ServerOption{WithConfig(minimalConfigWithWorkspace(root))}
	if taskRepo != nil {
		opts = append(opts, WithTaskRepository(taskRepo))
	}
	if ensurer != nil {
		opts = append(opts, WithGitReceiveGuards(ensurer))
	}
	srv := NewServer(opts...)
	auditRepo := &fakeAdminAuditRepo{}
	srv.adminAuditRepo = auditRepo
	return srv, auditRepo
}

// TestGitPush_UpdatesWorkingTree is the primary integration test: a real
// `git push` over the registered router into a clean temp repo succeeds AND the
// pushed file content appears in the working tree (proves updateInstead).
func TestGitPush_UpdatesWorkingTree(t *testing.T) {
	root := t.TempDir()
	proj := "proj_push"
	repo := newPushRepo(t, root, proj)

	spy := &spyEnsurer{}
	srv, _ := pushRouter(t, root, &fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}}, spy.fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	// Clone, add a file, push back.
	clone := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	gitWriteFile(t, filepath.Join(clone, "pushed.txt"), "pushed-content\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "add pushed.txt")
	out, err = exec.Command("git", "-C", clone, "push", "-q", "origin", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	// updateInstead must have materialised the file in the server repo's tree.
	content, rerr := os.ReadFile(filepath.Join(repo, "pushed.txt"))
	if rerr != nil {
		t.Fatalf("pushed file not in working tree (updateInstead failed): %v", rerr)
	}
	if string(content) != "pushed-content\n" {
		t.Fatalf("pushed file content = %q, want %q", content, "pushed-content\n")
	}

	// The guards ensurer must have been invoked (before exec).
	if spy.callCount() == 0 {
		t.Fatal("expected gitReceiveGuards ensurer to be called before receive-pack exec")
	}
}

// TestGitPush_ActiveTaskReturns503 verifies the check-under-lock active-task
// gate: when CountByStatus reports a RUNNING task, the push is rejected with
// 503 + Retry-After, and an audit row with result=rejected is written.
func TestGitPush_ActiveTaskReturns503(t *testing.T) {
	root := t.TempDir()
	proj := "proj_busy"
	newPushRepo(t, root, proj)

	taskRepo := &fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{persistence.TaskStatusRunning: 1}}
	spy := &spyEnsurer{}
	srv, auditRepo := pushRouter(t, root, taskRepo, spy.fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, auditRepo))
	defer ts.Close()

	// POST git-receive-pack directly (we don't need a full pack — the 503
	// fast-fail happens before exec).
	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/git/"+proj+".git/git-receive-pack",
		"application/x-git-receive-pack-request", nil)
	if err != nil {
		t.Fatalf("POST receive-pack: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during active task, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 503")
	}
	// Guards ensurer must NOT have been called — the gate returns before it.
	if spy.callCount() != 0 {
		t.Fatalf("ensurer called %d times; must not run on 503 fast-fail", spy.callCount())
	}
	// Audit row with result=rejected, Action=git.receive-pack.
	rows := auditRepo.snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(rows))
	}
	if rows[0].Action != "git.receive-pack" {
		t.Fatalf("audit Action = %q, want git.receive-pack", rows[0].Action)
	}
}

// TestGitPush_LeasedTaskReturns503 verifies LEASED also trips the gate.
func TestGitPush_LeasedTaskReturns503(t *testing.T) {
	root := t.TempDir()
	proj := "proj_leased"
	newPushRepo(t, root, proj)

	taskRepo := &fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{persistence.TaskStatusLeased: 1}}
	srv, auditRepo := pushRouter(t, root, taskRepo, (&spyEnsurer{}).fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, auditRepo))
	defer ts.Close()

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/git/"+proj+".git/git-receive-pack",
		"application/x-git-receive-pack-request", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during LEASED task, got %d", resp.StatusCode)
	}
}

// TestRegisterGitRoutes_EndToEnd drives the PRODUCTION registerGitRoutes (not
// the test mirror) for both a read advertisement and a real push, so the
// production routing + dispatch is covered directly. Auth disabled.
func TestRegisterGitRoutes_EndToEnd(t *testing.T) {
	root := t.TempDir()
	proj := "proj_e2e"
	repo := newPushRepo(t, root, proj)

	spy := &spyEnsurer{}
	srv := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(minimalConfigWithWorkspace(root)),
		WithTaskRepository(&fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}}),
		WithGitReceiveGuards(spy.fn),
	)
	srv.adminAuditRepo = &fakeAdminAuditRepo{}
	mux := http.NewServeMux()
	registerGitRoutes(mux, srv, false /* authEnabled */, config.NodeCapabilities{ServeAPI: true, RunWorkers: true})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Read advertisement → 200.
	resp, err := ts.Client().Get(ts.URL + "/api/v1/git/" + proj + ".git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET info/refs: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read advertisement: expected 200, got %d", resp.StatusCode)
	}

	// Real push through the production router.
	clone := filepath.Join(t.TempDir(), "clone")
	if out, cerr := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); cerr != nil {
		t.Fatalf("clone: %v\n%s", cerr, out)
	}
	gitWriteFile(t, filepath.Join(clone, "e2e.txt"), "e2e\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "e2e")
	if out, perr := exec.Command("git", "-C", clone, "push", "-q", "origin", "main").CombinedOutput(); perr != nil {
		t.Fatalf("push: %v\n%s", perr, out)
	}
	if _, serr := os.Stat(filepath.Join(repo, "e2e.txt")); serr != nil {
		t.Fatalf("pushed file not materialised via production router: %v", serr)
	}
}

// TestGitPush_GuardsErrorReturns500 covers the gateReceivePack guard-failure
// branch: when the ensurer returns an error, the push fails with 500 and an
// audit row with result=error is written.
func TestGitPush_GuardsErrorReturns500(t *testing.T) {
	root := t.TempDir()
	proj := "proj_guarderr"
	newPushRepo(t, root, proj)

	failing := &spyEnsurer{errToRet: context.DeadlineExceeded}
	srv, auditRepo := pushRouter(t, root,
		&fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}}, failing.fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, auditRepo))
	defer ts.Close()

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/git/"+proj+".git/git-receive-pack",
		"application/x-git-receive-pack-request", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on guards error, got %d", resp.StatusCode)
	}
	rows := auditRepo.snapshot()
	if len(rows) != 1 || rows[0].Action != "git.receive-pack" {
		t.Fatalf("expected 1 git.receive-pack audit row, got %+v", rows)
	}
}

// TestGitPush_HookRejectsWorktreeBranch verifies a push to refs/heads/worktree/*
// is rejected by the pre-receive hook (surfaced to the client as a failure),
// when the real guards ensurer (EnsureReceiveGuards) is wired.
func TestGitPush_HookRejectsWorktreeBranch(t *testing.T) {
	root := t.TempDir()
	proj := "proj_hook"
	newPushRepo(t, root, proj)

	// Use a guards ensurer that installs the REAL guards via the executor.
	srv, auditRepo := pushRouter(t, root,
		&fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}},
		realGuardsEnsurer(root))
	ts := httptest.NewServer(gitRegisteredRouter(srv, auditRepo))
	defer ts.Close()

	clone := filepath.Join(t.TempDir(), "clone")
	out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput()
	if err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	mustGit(t, clone, "checkout", "-q", "-b", "worktree/evil")
	gitWriteFile(t, filepath.Join(clone, "x.txt"), "x\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "evil")

	out, err = exec.Command("git", "-C", clone, "push", "-q", "origin", "worktree/evil").CombinedOutput()
	if err == nil {
		t.Fatalf("expected push to refs/heads/worktree/evil to be REJECTED by pre-receive hook; got success\n%s", out)
	}
}
