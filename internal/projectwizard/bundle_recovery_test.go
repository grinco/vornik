package projectwizard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// seedLeftoverJournal writes a staging dir + commit journal for
// sessionID under liveDir, listing files' keys as journal targets (in
// dependency order, mirroring stageAndCommitBundle's own journal
// shape). It does NOT touch the live tree itself — callers decide
// which of files' rel paths (if any) to additionally write into
// liveDir to simulate how far the crashed commit got.
func seedLeftoverJournal(t *testing.T, liveDir, sessionID string, files map[string]string) {
	t.Helper()
	stageDir := stagingDirFor(liveDir, sessionID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatalf("seed staging dir: %v", err)
	}
	journal := commitJournal{SessionID: sessionID}
	for _, rel := range orderedRelPaths(files) {
		journal.Targets = append(journal.Targets, journalTarget{
			RelPath:     rel,
			StagingPath: filepath.Join(stageDir, filepath.FromSlash(rel)),
		})
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	if err := os.WriteFile(journalPathFor(liveDir, sessionID), raw, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}

// writeLiveFiles lands the given rel paths (a subset of files) into
// liveDir, simulating exactly how far a crashed rename sequence got.
func writeLiveFiles(t *testing.T, liveDir string, files map[string]string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		body, ok := files[rel]
		if !ok {
			t.Fatalf("writeLiveFiles: %s not in files map", rel)
		}
		full := filepath.Join(liveDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func seedRecoverySession(t *testing.T, store *fakeSessionStore, sessionID string) {
	t.Helper()
	if err := store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID:         sessionID,
		OperatorID: "op_1",
		Bundle:     []byte(`{"placeholder":true}`),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

// TestRecoverComposerCommits_PartialCommit_RollsBack: the project file
// never landed (crash between the swarm's rename and the project
// file's) — the ABSENT branch. Recovery must remove the workflow +
// swarm that DID land, delete the staging dir + journal, and mark the
// session commit-failed-resumable.
func TestRecoverComposerCommits_PartialCommit_RollsBack(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)
	sessionID := "pw_partial"
	seedLeftoverJournal(t, liveDir, sessionID, files)
	writeLiveFiles(t, liveDir, files, "workflows/research-digest.md", "swarms/ai-news-digest-swarm.md")
	// An unrelated live file that must survive untouched.
	if err := os.MkdirAll(filepath.Join(liveDir, "projects"), 0o700); err != nil {
		t.Fatalf("seed unrelated dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "projects", "unrelated.yaml"), []byte("untouched"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}

	store := newFakeStore()
	seedRecoverySession(t, store, sessionID)
	metrics := NewMetrics(prometheus.NewRegistry())

	recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, metrics, zerolog.Nop())
	if err != nil {
		t.Fatalf("RecoverComposerCommits: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered entry, got %d", len(recovered))
	}
	if recovered[0].Outcome != RecoveryOutcomeRolledBack {
		t.Errorf("outcome = %q, want %q", recovered[0].Outcome, RecoveryOutcomeRolledBack)
	}
	if recovered[0].SessionID != sessionID {
		t.Errorf("session id = %q, want %q", recovered[0].SessionID, sessionID)
	}

	for _, rel := range []string{"workflows/research-digest.md", "swarms/ai-news-digest-swarm.md", "projects/ai-news-digest.yaml"} {
		if _, statErr := os.Stat(filepath.Join(liveDir, rel)); !os.IsNotExist(statErr) {
			t.Errorf("expected %s rolled back, stat err = %v", rel, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(liveDir, "projects", "unrelated.yaml")); statErr != nil {
		t.Errorf("recovery must leave non-journal files untouched: %v", statErr)
	}
	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir removed, stat err = %v", statErr)
	}

	stored, gErr := store.Get(context.Background(), sessionID)
	if gErr != nil {
		t.Fatalf("get session: %v", gErr)
	}
	if stored.CommittedProjectID != nil {
		t.Error("a rolled-back commit must NOT stamp the session committed")
	}
	if stored.BundleCommitFailedAt == nil || stored.BundleCommitError == "" {
		t.Error("expected the session marked commit-failed-resumable")
	}
	if got := composerCommitsMetricValue(t, metrics, composerCommitTier3, string(RecoveryOutcomeRolledBack)); got != 1 {
		t.Errorf("recovered_rolledback metric = %.0f, want 1", got)
	}
}

// TestRecoverComposerCommits_CleanupInterrupted_ProjectPresent_NeverDeletesLiveFiles
// is THE load-bearing edge case: every journal-listed file, INCLUDING
// the project file, is already live (the commit fully landed; only
// the post-success staging-dir cleanup was interrupted). Recovery
// must NOT delete a single live file — only the staging dir + journal
// — and must stamp the session committed.
func TestRecoverComposerCommits_CleanupInterrupted_ProjectPresent_NeverDeletesLiveFiles(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)
	sessionID := "pw_landed"
	seedLeftoverJournal(t, liveDir, sessionID, files)
	// ALL journal targets already landed, including the project file.
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	writeLiveFiles(t, liveDir, files, rels...)

	store := newFakeStore()
	seedRecoverySession(t, store, sessionID)
	metrics := NewMetrics(prometheus.NewRegistry())

	recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, metrics, zerolog.Nop())
	if err != nil {
		t.Fatalf("RecoverComposerCommits: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered entry, got %d", len(recovered))
	}
	if recovered[0].Outcome != RecoveryOutcomeCommitted {
		t.Errorf("outcome = %q, want %q", recovered[0].Outcome, RecoveryOutcomeCommitted)
	}

	// THE assertion: every live file the crashed commit landed must
	// still be there, byte-identical, after recovery.
	for rel, body := range files {
		got, rErr := os.ReadFile(filepath.Join(liveDir, rel))
		if rErr != nil {
			t.Errorf("recovery must NEVER delete a live file: %s missing (%v)", rel, rErr)
			continue
		}
		if string(got) != body {
			t.Errorf("recovery must never modify a live file's contents: %s changed", rel)
		}
	}
	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir + journal removed, stat err = %v", statErr)
	}

	stored, gErr := store.Get(context.Background(), sessionID)
	if gErr != nil {
		t.Fatalf("get session: %v", gErr)
	}
	if stored.CommittedProjectID == nil || *stored.CommittedProjectID != "ai-news-digest" {
		t.Errorf("expected the session stamped committed to ai-news-digest, got %+v", stored.CommittedProjectID)
	}
	if stored.BundleCommitFailedAt != nil {
		t.Error("a fully-landed recovered commit must not carry a commit-failed marker")
	}
	if got := composerCommitsMetricValue(t, metrics, composerCommitTier3, string(RecoveryOutcomeCommitted)); got != 1 {
		t.Errorf("recovered_committed metric = %.0f, want 1", got)
	}
}

// TestRecoverComposerCommits_Idempotent: running recovery a second
// time over the same liveConfigDir must be a no-op — the first run's
// cleanup already removed the journal + staging dir, so there is
// nothing left to scan.
func TestRecoverComposerCommits_Idempotent(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)
	sessionID := "pw_idempotent"
	seedLeftoverJournal(t, liveDir, sessionID, files)
	writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

	store := newFakeStore()
	seedRecoverySession(t, store, sessionID)
	metrics := NewMetrics(prometheus.NewRegistry())

	first, err := RecoverComposerCommits(context.Background(), liveDir, store, metrics, zerolog.Nop())
	if err != nil {
		t.Fatalf("first RecoverComposerCommits: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 recovered entry on the first run, got %d", len(first))
	}

	second, err := RecoverComposerCommits(context.Background(), liveDir, store, metrics, zerolog.Nop())
	if err != nil {
		t.Fatalf("second RecoverComposerCommits: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("expected a no-op second run, got %d entries", len(second))
	}
}

// TestRecoverComposerCommits_NoLeftovers_NoOp: a clean live tree with
// no staging root at all yields no error and no recovered entries.
func TestRecoverComposerCommits_NoLeftovers_NoOp(t *testing.T) {
	liveDir := t.TempDir()
	store := newFakeStore()
	recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
	if err != nil {
		t.Fatalf("RecoverComposerCommits: %v", err)
	}
	if len(recovered) != 0 {
		t.Errorf("expected no recovered entries on a clean tree, got %d", len(recovered))
	}
}

// TestRecoverComposerCommits_EmptyLiveConfigDir_NoOp: an unwired
// liveConfigDir (minimal/test wiring) must degrade to a no-op rather
// than erroring.
func TestRecoverComposerCommits_EmptyLiveConfigDir_NoOp(t *testing.T) {
	recovered, err := RecoverComposerCommits(context.Background(), "", newFakeStore(), NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
	if err != nil {
		t.Fatalf("expected no error with an empty liveConfigDir, got %v", err)
	}
	if recovered != nil {
		t.Errorf("expected nil recovered slice, got %+v", recovered)
	}
}

// TestFindLeftoverJournalForProject: the project-doctor detection
// primitive finds a journal by the project id it names, and reports
// not-found for a project with no leftover journal.
func TestFindLeftoverJournalForProject(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)
	sessionID := "pw_doctor"
	seedLeftoverJournal(t, liveDir, sessionID, files)
	writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

	lj, found, err := FindLeftoverJournalForProject(liveDir, "ai-news-digest")
	if err != nil {
		t.Fatalf("FindLeftoverJournalForProject: %v", err)
	}
	if !found {
		t.Fatal("expected the leftover journal to be found")
	}
	if lj.SessionID != sessionID {
		t.Errorf("session id = %q, want %q", lj.SessionID, sessionID)
	}
	if lj.ProjectFileLive(liveDir) {
		t.Error("the project file was not written live in this test; ProjectFileLive must be false")
	}

	_, found, err = FindLeftoverJournalForProject(liveDir, "some-other-project")
	if err != nil {
		t.Fatalf("FindLeftoverJournalForProject (no match): %v", err)
	}
	if found {
		t.Error("expected no match for an unrelated project id")
	}
}

// TestFindLeftoverJournals_MalformedJournal_SkippedNotFatal: a
// corrupt journal file must not abort the scan or the recovery sweep
// — it's logged and skipped so one bad journal can't hide every other
// session's recovery.
func TestRecoverComposerCommits_MalformedJournal_SkippedNotFatal(t *testing.T) {
	liveDir := t.TempDir()
	badSessionID := "pw_bad"
	stageDir := stagingDirFor(liveDir, badSessionID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatalf("seed staging dir: %v", err)
	}
	if err := os.WriteFile(journalPathFor(liveDir, badSessionID), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write malformed journal: %v", err)
	}

	// A second, well-formed leftover alongside the malformed one must
	// still be recovered.
	files := bundleFilesForCommitTest(t)
	goodSessionID := "pw_good"
	seedLeftoverJournal(t, liveDir, goodSessionID, files)
	writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

	store := newFakeStore()
	seedRecoverySession(t, store, goodSessionID)

	recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
	if err != nil {
		t.Fatalf("RecoverComposerCommits must not fail on a malformed journal: %v", err)
	}
	if len(recovered) != 1 || recovered[0].SessionID != goodSessionID {
		t.Errorf("expected only the well-formed journal recovered, got %+v", recovered)
	}
	// The malformed journal's staging dir is left alone (never
	// understood by this scan) rather than being blindly removed.
	if _, statErr := os.Stat(stageDir); statErr != nil {
		t.Errorf("expected the malformed journal's staging dir left untouched, stat err = %v", statErr)
	}
}

// TestLeftoverJournal_EmptyProjectRelPath: a journal that (in theory —
// shouldn't occur from a real stageAndCommitBundle run) carries no
// project-file target must never be treated as "live" or resolve to a
// project id; it is always the ABSENT/rollback branch.
func TestLeftoverJournal_EmptyProjectRelPath(t *testing.T) {
	lj := LeftoverJournal{}
	if lj.ProjectID() != "" {
		t.Errorf("expected empty ProjectID, got %q", lj.ProjectID())
	}
	if lj.ProjectFileLive(t.TempDir()) {
		t.Error("expected ProjectFileLive=false with no ProjectRelPath")
	}
}

// TestFindLeftoverJournals_SkipsNonDirEntries: a stray regular file
// directly under .composer-staging/ (never created by
// stageAndCommitBundle, which only ever creates per-session
// directories) must be skipped, not mistaken for a session dir.
func TestFindLeftoverJournals_SkipsNonDirEntries(t *testing.T) {
	liveDir := t.TempDir()
	root := filepath.Join(liveDir, composerStagingDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("seed staging root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray-file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed stray file: %v", err)
	}
	leftovers, err := findLeftoverJournals(liveDir, zerolog.Nop())
	if err != nil {
		t.Fatalf("findLeftoverJournals: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("expected the stray non-dir entry skipped, got %+v", leftovers)
	}
}

// TestFindLeftoverJournals_ScanRootError_Propagates: a ReadDir failure
// OTHER than "does not exist" (e.g. the staging root path is occupied
// by a plain file, not a directory) must propagate as an error rather
// than being silently swallowed like the "does not exist" case.
func TestFindLeftoverJournals_ScanRootError_Propagates(t *testing.T) {
	liveDir := t.TempDir()
	root := filepath.Join(liveDir, composerStagingDirName)
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}

	if _, err := findLeftoverJournals(liveDir, zerolog.Nop()); err == nil {
		t.Fatal("expected a scan error when the staging root is not a directory")
	}
	if _, err := RecoverComposerCommits(context.Background(), liveDir, newFakeStore(), NewMetrics(prometheus.NewRegistry()), zerolog.Nop()); err == nil {
		t.Error("expected RecoverComposerCommits to propagate the scan error")
	}
	if _, _, err := FindLeftoverJournalForProject(liveDir, "anything"); err == nil {
		t.Error("expected FindLeftoverJournalForProject to propagate the scan error")
	}
}

// TestFindLeftoverJournals_JournalReadError_SkippedNotFatal: a journal
// path that can't be read for a reason OTHER than "does not exist"
// (here: the journal path is itself a directory) is logged and
// skipped, exactly like the malformed-JSON case, rather than aborting
// the whole scan.
func TestFindLeftoverJournals_JournalReadError_SkippedNotFatal(t *testing.T) {
	liveDir := t.TempDir()
	sessionID := "pw_journal_is_dir"
	// The journal path itself is a directory, not a file -> os.ReadFile
	// fails with something other than IsNotExist.
	if err := os.MkdirAll(journalPathFor(liveDir, sessionID), 0o700); err != nil {
		t.Fatalf("seed journal-path-as-dir: %v", err)
	}

	leftovers, err := findLeftoverJournals(liveDir, zerolog.Nop())
	if err != nil {
		t.Fatalf("findLeftoverJournals must not fail on an unreadable journal: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("expected the unreadable journal skipped, got %+v", leftovers)
	}
}

// TestRecoverComposerCommits_NilSessions_StillActsOnDisk: a nil
// SessionStore (session-stamping is best-effort) must not panic or
// block the on-disk recovery in either branch.
func TestRecoverComposerCommits_NilSessions_StillActsOnDisk(t *testing.T) {
	t.Run("project present", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_nilsessions_landed"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		rels := make([]string, 0, len(files))
		for rel := range files {
			rels = append(rels, rel)
		}
		writeLiveFiles(t, liveDir, files, rels...)

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, nil, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeCommitted {
			t.Fatalf("expected 1 recovered_committed entry, got %+v", recovered)
		}
		for rel, body := range files {
			got, rErr := os.ReadFile(filepath.Join(liveDir, rel))
			if rErr != nil || string(got) != body {
				t.Errorf("expected %s to remain live and unchanged", rel)
			}
		}
	})

	t.Run("project absent", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_nilsessions_partial"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, nil, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeRolledBack {
			t.Fatalf("expected 1 recovered_rolledback entry, got %+v", recovered)
		}
		if _, statErr := os.Stat(filepath.Join(liveDir, "workflows/research-digest.md")); !os.IsNotExist(statErr) {
			t.Errorf("expected the landed workflow rolled back, stat err = %v", statErr)
		}
	})
}

// TestRecoverComposerCommits_CommitToHardError_LoggedNotFatal: a
// CommitTo failure that is NEITHER ErrInvalidTransition nor
// ErrNotFound (a genuine store error) must be logged into the
// recovered entry's Detail, not silently dropped or escalated to an
// error return — the on-disk cleanup already happened and must not be
// undone just because the session stamp failed.
func TestRecoverComposerCommits_CommitToHardError_LoggedNotFatal(t *testing.T) {
	liveDir := t.TempDir()
	files := bundleFilesForCommitTest(t)
	sessionID := "pw_committo_fails"
	seedLeftoverJournal(t, liveDir, sessionID, files)
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	writeLiveFiles(t, liveDir, files, rels...)

	store := newFakeStore()
	store.errOn = "CommitTo"
	seedRecoverySession(t, store, sessionID)

	recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
	if err != nil {
		t.Fatalf("RecoverComposerCommits: %v", err)
	}
	if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeCommitted {
		t.Fatalf("expected the outcome to still report recovered_committed (files are live regardless), got %+v", recovered)
	}
	if recovered[0].Detail == "" {
		t.Error("expected a detail describing the failed session stamp")
	}
	// Staging dir must STILL be gone — a session-stamp failure never
	// undoes the on-disk cleanup.
	if _, statErr := os.Stat(stagingDirFor(liveDir, sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected staging dir removed regardless of the CommitTo failure, stat err = %v", statErr)
	}
}

// TestRecoverComposerCommits_SessionGetOrUpdateError_LoggedNotFatal:
// on the ABSENT (rollback) branch, a session store error while loading
// or updating the session must not fail the on-disk rollback — it's
// logged, and the recovered entry still reports rolled back.
func TestRecoverComposerCommits_SessionGetOrUpdateError_LoggedNotFatal(t *testing.T) {
	t.Run("get fails", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_getfails"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

		store := newFakeStore()
		store.errOn = "Get"
		seedRecoverySession(t, store, sessionID)

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeRolledBack {
			t.Fatalf("expected recovered_rolledback despite the Get failure, got %+v", recovered)
		}
		if _, statErr := os.Stat(filepath.Join(liveDir, "workflows/research-digest.md")); !os.IsNotExist(statErr) {
			t.Errorf("expected the on-disk rollback to proceed despite the session Get failure, stat err = %v", statErr)
		}
	})

	t.Run("update fails", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_updatefails"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")

		store := newFakeStore()
		store.errOn = "Update"
		seedRecoverySession(t, store, sessionID)

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeRolledBack {
			t.Fatalf("expected recovered_rolledback despite the Update failure, got %+v", recovered)
		}
	})
}

// TestRecoverComposerCommits_RemoveAllFails_LoggedNotFatal: a
// removeAllFn failure on the staging dir (either branch) must be
// logged, never escalated to an error return — the primary recovery
// action (rollback or cleanup) already happened; a retry next boot
// picks up the leftover staging dir.
func TestRecoverComposerCommits_RemoveAllFails_LoggedNotFatal(t *testing.T) {
	origRemoveAll := removeAllFn
	t.Cleanup(func() { removeAllFn = origRemoveAll })
	removeAllFn = func(string) error { return errInjectedRenameFailure }

	t.Run("project present", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_removeall_landed"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		rels := make([]string, 0, len(files))
		for rel := range files {
			rels = append(rels, rel)
		}
		writeLiveFiles(t, liveDir, files, rels...)
		store := newFakeStore()
		seedRecoverySession(t, store, sessionID)

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeCommitted {
			t.Fatalf("expected recovered_committed despite the removeAll failure, got %+v", recovered)
		}
	})

	t.Run("project absent", func(t *testing.T) {
		liveDir := t.TempDir()
		files := bundleFilesForCommitTest(t)
		sessionID := "pw_removeall_partial"
		seedLeftoverJournal(t, liveDir, sessionID, files)
		writeLiveFiles(t, liveDir, files, "workflows/research-digest.md")
		store := newFakeStore()
		seedRecoverySession(t, store, sessionID)

		recovered, err := RecoverComposerCommits(context.Background(), liveDir, store, NewMetrics(prometheus.NewRegistry()), zerolog.Nop())
		if err != nil {
			t.Fatalf("RecoverComposerCommits: %v", err)
		}
		if len(recovered) != 1 || recovered[0].Outcome != RecoveryOutcomeRolledBack {
			t.Fatalf("expected recovered_rolledback despite the removeAll failure, got %+v", recovered)
		}
		if _, statErr := os.Stat(filepath.Join(liveDir, "workflows/research-digest.md")); !os.IsNotExist(statErr) {
			t.Errorf("expected the file-level rollback to still happen even though removeAll(staging dir) failed, stat err = %v", statErr)
		}
	})
}
