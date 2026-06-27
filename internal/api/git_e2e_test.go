package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workspacelock"
)

// Task 2.5 — end-to-end git-over-HTTPS suite (File A: push path + lock
// contention through the REAL registered router).
//
// These tests drive a real `git` CLI against `t.TempDir()` repos behind an
// httptest.NewServer wired through the production registerGitRoutes dispatch
// (via gitRegisteredRouter / registerGitRoutes). They reuse the helpers from
// git_http_test.go (mustGit, gitWriteFile, gitRegisteredRouter, fakeAdminAuditRepo)
// and git_http_push_test.go (newPushRepo, pushRouter, fakeCountTaskRepo,
// spyEnsurer, realGuardsEnsurer).
//
// TEST-ONLY: no production code is modified. The goal is to PROVE the behavior.

// pushRouterWithLock is pushRouter + an injected shared *workspacelock.Locker so
// a test goroutine can simulate a concurrent executor mutation by taking the
// SAME lock the receive-pack handler takes (scenario 6).
func pushRouterWithLock(t *testing.T, root string, taskRepo persistence.TaskRepository, ensurer func(context.Context, string) error, lock *workspacelock.Locker) (*Server, *fakeAdminAuditRepo) {
	t.Helper()
	opts := []ServerOption{
		WithConfig(minimalConfigWithWorkspace(root)),
		WithLogger(zerolog.Nop()),
		WithWorkspaceLock(lock),
	}
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

// emptyCounts is the "no active task" stub for the push gate.
func emptyCounts() *fakeCountTaskRepo {
	return &fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{}}
}

// Scenario 1 — Clone (read): a real `git clone` retrieves committed content
// through the registered router (upload-pack / RLock path).
func TestGitE2E_S1_Clone(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	proj := "proj_s1"
	repo := newPushRepo(t, root, proj)
	gitWriteFile(t, filepath.Join(repo, "hello.txt"), "world\n")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "add", "-A")
	mustGit(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "-q", "-m", "hello")

	srv, _ := pushRouter(t, root, emptyCounts(), (&spyEnsurer{}).fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	dst := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", dst).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dst, "hello.txt"))
	if err != nil {
		t.Fatalf("cloned content missing: %v", err)
	}
	if string(got) != "world\n" {
		t.Fatalf("cloned content = %q, want %q", got, "world\n")
	}
}

// Scenario 2 — Clean-window push (updateInstead): a real `git push` lands the
// pushed file in the server working tree.
func TestGitE2E_S2_CleanWindowPush(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	proj := "proj_s2"
	repo := newPushRepo(t, root, proj)

	spy := &spyEnsurer{}
	srv, _ := pushRouter(t, root, emptyCounts(), spy.fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	gitWriteFile(t, filepath.Join(clone, "pushed.txt"), "pushed-content\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "add pushed.txt")
	if out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	content, err := os.ReadFile(filepath.Join(repo, "pushed.txt"))
	if err != nil {
		t.Fatalf("updateInstead failed — pushed file not in working tree: %v", err)
	}
	if string(content) != "pushed-content\n" {
		t.Fatalf("pushed content = %q, want %q", content, "pushed-content\n")
	}
	if spy.callCount() == 0 {
		t.Fatal("expected receive-guards ensurer to run before exec")
	}
}

// Scenario 3 — 503 during active task: CountByStatus → {RUNNING:1} makes the
// push fast-fail with 503 + Retry-After before the receive-pack exec.
func TestGitE2E_S3_ActiveTask503(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	proj := "proj_s3"
	newPushRepo(t, root, proj)

	taskRepo := &fakeCountTaskRepo{counts: map[persistence.TaskStatus]int64{persistence.TaskStatusRunning: 1}}
	spy := &spyEnsurer{}
	srv, _ := pushRouter(t, root, taskRepo, spy.fn)
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	resp, err := ts.Client().Post(
		ts.URL+"/api/v1/git/"+proj+".git/git-receive-pack",
		"application/x-git-receive-pack-request", nil)
	if err != nil {
		t.Fatalf("POST receive-pack: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 503")
	}
	if spy.callCount() != 0 {
		t.Fatalf("ensurer must not run on 503 fast-fail (ran %d times)", spy.callCount())
	}
}

// Scenario 4 — Reserved-ref reject: a push to refs/heads/worktree/* is rejected
// by the real pre-receive hook (client sees a failure).
func TestGitE2E_S4_ReservedRefReject(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	proj := "proj_s4"
	newPushRepo(t, root, proj)

	srv, _ := pushRouter(t, root, emptyCounts(), realGuardsEnsurer(root))
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	mustGit(t, clone, "checkout", "-q", "-b", "worktree/evil")
	gitWriteFile(t, filepath.Join(clone, "x.txt"), "x\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "evil")

	out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", "worktree/evil").CombinedOutput()
	if err == nil {
		t.Fatalf("expected push to refs/heads/worktree/evil to be REJECTED; got success\n%s", out)
	}
	if !strings.Contains(string(out), "reserved worktree branch") {
		t.Logf("note: rejection message did not mention reserved worktree branch:\n%s", out)
	}
}

// Scenario 5 — NON-FF reject (closes Task 2.4 review Q1): force-rewrite the
// default branch to a divergent history and `git push --force`. The pre-receive
// hook rejects it AND the server's default-branch ref is UNCHANGED.
func TestGitE2E_S5_NonFastForwardReject(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	proj := "proj_s5"
	repo := newPushRepo(t, root, proj)

	srv, _ := pushRouter(t, root, emptyCounts(), realGuardsEnsurer(root))
	ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
	defer ts.Close()

	// Record the server's default-branch ref before the attempted force-push.
	serverRefBefore := gitRevParse(t, repo, "refs/heads/main")

	clone := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	// Rewrite history divergently: reset to root and create an unrelated commit
	// so the new tip is NOT a descendant of the server's current tip.
	mustGit(t, clone, "checkout", "-q", "--orphan", "divergent")
	mustGit(t, clone, "rm", "-rf", "--cached", ".")
	gitWriteFile(t, filepath.Join(clone, "rewritten.txt"), "divergent history\n")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
	mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "divergent")

	// Force-push the divergent branch onto the server's default branch (main).
	out, err := exec.Command("git", "-C", clone, "push", "-q", "--force", "origin", "divergent:main").CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-fast-forward force-push to main to be REJECTED; got success\n%s", out)
	}
	if !strings.Contains(string(out), "non-fast-forward") {
		t.Logf("note: rejection message did not mention non-fast-forward:\n%s", out)
	}

	// Server default-branch ref must be UNCHANGED (Task 2.4 review Q1).
	serverRefAfter := gitRevParse(t, repo, "refs/heads/main")
	if serverRefBefore != serverRefAfter {
		t.Fatalf("server default-branch ref CHANGED despite rejection: before=%s after=%s", serverRefBefore, serverRefAfter)
	}
}

// Scenario 6 — Concurrent job vs push, no corruption. A goroutine simulates an
// executor mutation by acquiring the SAME injected workspacelock.Locker the
// receive-pack handler takes, mutating the server repo on a side branch, while a
// real `git push` to the default branch runs concurrently. The exclusive lock
// serializes them; afterward `git fsck --full` is clean, HEAD/refs resolve, and
// both the push commit and the job commit are reachable. Run under -race; both
// orderings are exercised.
func TestGitE2E_S6_ConcurrentJobVsPush(t *testing.T) {
	requireGit(t)
	for _, ordering := range []string{"push-first", "job-first"} {
		t.Run(ordering, func(t *testing.T) {
			root := t.TempDir()
			proj := "proj_s6"
			repo := newPushRepo(t, root, proj)

			lock := workspacelock.New() // shared between handler and test goroutine
			srv, _ := pushRouterWithLock(t, root, emptyCounts(), realGuardsEnsurer(root), lock)
			ts := httptest.NewServer(gitRegisteredRouter(srv, srv.adminAuditRepo.(*fakeAdminAuditRepo)))
			defer ts.Close()

			clone := filepath.Join(t.TempDir(), "clone")
			if out, err := exec.Command("git", "clone", "-q", ts.URL+"/api/v1/git/"+proj+".git", clone).CombinedOutput(); err != nil {
				t.Fatalf("clone: %v\n%s", err, out)
			}
			gitWriteFile(t, filepath.Join(clone, "from_push.txt"), "push\n")
			mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "add", "-A")
			mustGit(t, clone, "-c", "user.email=p@p.c", "-c", "user.name=p", "commit", "-q", "-m", "push commit")

			// The simulated executor: take the SAME per-project lock and create a
			// commit on a side branch of the server repo (a raw workspace mutation),
			// mirroring how the executor serializes against the push handler.
			runJob := func() {
				unlock := lock.Lock(proj)
				defer unlock()
				mustGit(t, repo, "checkout", "-q", "-b", "job-branch")
				gitWriteFile(t, filepath.Join(repo, "from_job.txt"), "job\n")
				mustGit(t, repo, "-c", "user.email=j@j.c", "-c", "user.name=j", "add", "-A")
				mustGit(t, repo, "-c", "user.email=j@j.c", "-c", "user.name=j", "commit", "-q", "-m", "job commit")
				// Return to the default branch so updateInstead on the incoming push
				// applies to main (a checked-out side branch would block the push tree
				// update — irrelevant to corruption, which is what we assert).
				mustGit(t, repo, "checkout", "-q", "main")
			}
			runPush := func() error {
				out, err := exec.Command("git", "-C", clone, "push", "-q", "origin", "main").CombinedOutput()
				if err != nil {
					return fmt.Errorf("push: %v\n%s", err, out)
				}
				return nil
			}

			var wg sync.WaitGroup
			var pushErr error
			wg.Add(2)
			if ordering == "job-first" {
				// Hold the lock from the job side first so the push must wait.
				started := make(chan struct{})
				go func() {
					defer wg.Done()
					unlock := lock.Lock(proj)
					close(started)
					// Do the mutation while holding the lock, then release.
					mustGit(t, repo, "checkout", "-q", "-b", "job-branch")
					gitWriteFile(t, filepath.Join(repo, "from_job.txt"), "job\n")
					mustGit(t, repo, "-c", "user.email=j@j.c", "-c", "user.name=j", "add", "-A")
					mustGit(t, repo, "-c", "user.email=j@j.c", "-c", "user.name=j", "commit", "-q", "-m", "job commit")
					mustGit(t, repo, "checkout", "-q", "main")
					unlock()
				}()
				go func() {
					defer wg.Done()
					<-started // ensure the job has the lock before the push contends
					pushErr = runPush()
				}()
			} else { // push-first
				go func() {
					defer wg.Done()
					pushErr = runPush()
				}()
				go func() {
					defer wg.Done()
					runJob()
				}()
			}
			wg.Wait()
			if pushErr != nil {
				t.Fatalf("concurrent push failed: %v", pushErr)
			}

			// fsck the server repo: no corruption, no errors.
			out, err := exec.Command("git", "-C", repo, "fsck", "--full").CombinedOutput()
			if err != nil {
				t.Fatalf("git fsck --full reported errors after concurrent ops: %v\n%s", err, out)
			}
			// HEAD + refs resolve.
			if h := gitRevParse(t, repo, "HEAD"); h == "" {
				t.Fatal("HEAD does not resolve after concurrent ops")
			}
			// Both commits reachable: the push landed from_push.txt on main; the job
			// commit lives on job-branch. Both objects exist in the repo.
			if _, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/main").CombinedOutput(); err != nil {
				t.Fatalf("main ref unresolvable: %v", err)
			}
			if _, err := exec.Command("git", "-C", repo, "rev-parse", "refs/heads/job-branch").CombinedOutput(); err != nil {
				t.Fatalf("job-branch ref unresolvable: %v", err)
			}
			// The push's blob must be reachable from main.
			if out, err := exec.Command("git", "-C", repo, "cat-file", "-e", "main:from_push.txt").CombinedOutput(); err != nil {
				t.Fatalf("push commit content not reachable from main: %v\n%s", err, out)
			}
			// The job's blob must be reachable from job-branch.
			if out, err := exec.Command("git", "-C", repo, "cat-file", "-e", "job-branch:from_job.txt").CombinedOutput(); err != nil {
				t.Fatalf("job commit content not reachable from job-branch: %v\n%s", err, out)
			}
		})
	}
}

// gitRevParse returns the resolved object id for ref in repo (fatal on error).
func gitRevParse(t *testing.T, repo, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("git rev-parse %s in %s: %v", ref, repo, err)
	}
	return strings.TrimSpace(string(out))
}

// requireGit skips the test when the git binary is unavailable.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}
