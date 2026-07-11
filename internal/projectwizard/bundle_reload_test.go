package projectwizard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeReloader is a test double for the Reloader seam. It never
// actually waits — d is recorded but ignored — so these tests never
// take a real 30 s (or any) wall-clock hit even though
// commitBundleSession passes the production 30 s default.
type fakeReloader struct {
	ok      bool
	err     error
	calls   int
	lastDur time.Duration
}

func (f *fakeReloader) TryReload(d time.Duration) (bool, error) {
	f.calls++
	f.lastDur = d
	return f.ok, f.err
}

// TestCommit_Bundle_ReloadSuccess: design §5.6 step 5 happy path — the
// reloader reports ok, so the commit proceeds to stamp the session
// committed and returns the doctor redirect exactly like slice i.
func TestCommit_Bundle_ReloadSuccess(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	reloader := &fakeReloader{ok: true}
	w.Reloader = reloader
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if reloader.calls != 1 {
		t.Errorf("expected exactly one reload trigger, got %d", reloader.calls)
	}
	if result.URL != composerDoctorURL("ai-news-digest") {
		t.Errorf("url = %q, want the doctor/setup redirect", result.URL)
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID == nil || *stored.CommittedProjectID != "ai-news-digest" {
		t.Errorf("session not stamped committed: %+v", stored.CommittedProjectID)
	}
	for _, rel := range []string{
		"projects/ai-news-digest.yaml",
		"swarms/ai-news-digest-swarm.md",
		"workflows/research-digest.md",
	} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); statErr != nil {
			t.Errorf("expected %s to remain live after a successful reload: %v", rel, statErr)
		}
	}
}

// TestCommit_Bundle_ReloadTimeout_RollsBack: design §7's "hot-reload
// rejects or times out" row. The reloader reports NOT ok (timeout /
// deferred), so the full journaled rollback must run — the live tree
// ends up byte-identical to its pre-commit state — and the session is
// left resumable with the reload error recorded, never committed.
func TestCommit_Bundle_ReloadTimeout_RollsBack(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	w.Reloader = &fakeReloader{ok: false} // no err set: "timed out" path
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the reload timeout to fail the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}

	for _, rel := range []string{
		"projects/ai-news-digest.yaml",
		"swarms/ai-news-digest-swarm.md",
		"workflows/research-digest.md",
	} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s rolled back after a reload timeout, stat err = %v", rel, statErr)
		}
	}
	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir gone after rollback, stat err = %v", statErr)
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID != nil {
		t.Errorf("session must NOT be committed after a reload rollback, got %v", *stored.CommittedProjectID)
	}
	if stored.Bundle == nil {
		t.Error("session.Bundle must stay intact for a resumable retry")
	}
	if stored.BundleCommitFailedAt == nil {
		t.Error("expected the commit-failed-resumable marker to be stamped")
	}
	if stored.BundleCommitError == "" {
		t.Error("expected the reload error recorded into the session")
	}
}

// TestCommit_Bundle_ReloadRejection_RollsBack: same rollback contract
// as the timeout case, but the reloader instead reports a hard
// validate/activate rejection (ok=false, err set) — the design's
// "reload rejects" half of the same failure-mode row.
func TestCommit_Bundle_ReloadRejection_RollsBack(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	rejectErr := errors.New("activation blocked: in-flight tasks")
	w.Reloader = &fakeReloader{ok: false, err: rejectErr}
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected the reload rejection to fail the commit")
	}
	if !errors.Is(err, ErrBundleCommitFailed) {
		t.Errorf("expected ErrBundleCommitFailed, got %v", err)
	}

	for _, rel := range []string{
		"projects/ai-news-digest.yaml",
		"swarms/ai-news-digest-swarm.md",
		"workflows/research-digest.md",
	} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s rolled back after a reload rejection, stat err = %v", rel, statErr)
		}
	}

	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID != nil {
		t.Error("session must NOT be committed after a rejected reload")
	}
	if stored.BundleCommitError == "" || !strings.Contains(stored.BundleCommitError, "activation blocked") {
		t.Errorf("expected the reload rejection reason recorded, got %q", stored.BundleCommitError)
	}
}

// TestCommit_Bundle_NilReloader_DoesNotHardFail is the documented
// degradation: without a Reloader wired at all (CE/minimal wiring),
// the commit still lands its files and returns success — it just
// never triggers a synchronous reload, relying on the daemon's own
// watcher/next reload the same way slice i behaved before the
// Reloader field existed.
func TestCommit_Bundle_NilReloader_DoesNotHardFail(t *testing.T) {
	w, store, liveDir := wizardForBundleCommit(t)
	if w.Reloader != nil {
		t.Fatal("test setup: expected a nil Reloader by default")
	}
	sessionID := pinReadyBundleSession(t, store, validComposedBundle())

	result, err := w.Commit(context.Background(), sessionID, "op_1")
	if err != nil {
		t.Fatalf("Commit with a nil Reloader must not hard-fail: %v", err)
	}
	if result.ProjectID != "ai-news-digest" {
		t.Errorf("project id = %q, want ai-news-digest", result.ProjectID)
	}
	for _, rel := range []string{
		"projects/ai-news-digest.yaml",
		"swarms/ai-news-digest-swarm.md",
		"workflows/research-digest.md",
	} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); statErr != nil {
			t.Errorf("expected %s to land even without a reloader: %v", rel, statErr)
		}
	}
	stored, _ := store.Get(context.Background(), sessionID)
	if stored.CommittedProjectID == nil {
		t.Error("expected the session committed even without a reloader")
	}
}

// TestTriggerPostCommitReload_UsesConfiguredTimeout: ReloadTimeout, when
// set, is what's passed to the Reloader (not always the 30s default) —
// this is the "injectable short deadline" the brief calls for so a
// future real-reloader integration test never needs a genuine 30 s
// wait to exercise this path.
func TestTriggerPostCommitReload_UsesConfiguredTimeout(t *testing.T) {
	reloader := &fakeReloader{ok: true}
	w := &Wizard{Reloader: reloader, ReloadTimeout: 5 * time.Millisecond}
	if err := w.triggerPostCommitReload(); err != nil {
		t.Fatalf("triggerPostCommitReload: %v", err)
	}
	if reloader.lastDur != 5*time.Millisecond {
		t.Errorf("expected the configured ReloadTimeout to be passed through, got %v", reloader.lastDur)
	}
}

// TestTriggerPostCommitReload_DefaultTimeout: an unset ReloadTimeout
// falls back to the design's 30 s deadline.
func TestTriggerPostCommitReload_DefaultTimeout(t *testing.T) {
	reloader := &fakeReloader{ok: true}
	w := &Wizard{Reloader: reloader}
	if err := w.triggerPostCommitReload(); err != nil {
		t.Fatalf("triggerPostCommitReload: %v", err)
	}
	if reloader.lastDur != defaultReloadTimeout {
		t.Errorf("expected the default 30s timeout, got %v", reloader.lastDur)
	}
}
