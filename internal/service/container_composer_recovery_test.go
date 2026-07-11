package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/storage"
)

// seedComposerConfigDir builds a minimal projects/swarms/workflows
// layout (the shape resolveRegistryConfigDir/hasRegistryLayout
// requires to recognise a directory as the registry config root) and
// points VORNIK_CONFIGS_DIR at it for the duration of the test.
func seedComposerConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("seed %s: %v", sub, err)
		}
	}
	t.Setenv("VORNIK_CONFIGS_DIR", dir)
	return dir
}

// seedComposerLeftoverJournal writes a minimal leftover staging dir +
// commit journal directly (no dependency on the projectwizard
// package's unexported helpers), naming a single "projects/<id>.yaml"
// target — enough for the ProjectID()/ProjectFileLive() branch this
// test exercises.
func seedComposerLeftoverJournal(t *testing.T, configDir, sessionID, projectID string) {
	t.Helper()
	stageDir := filepath.Join(configDir, ".composer-staging", sessionID)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatalf("seed staging dir: %v", err)
	}
	journal := map[string]any{
		"session_id": sessionID,
		"targets": []map[string]string{
			{"rel_path": "projects/" + projectID + ".yaml", "staging_path": filepath.Join(stageDir, "projects", projectID+".yaml")},
		},
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("marshal journal: %v", err)
	}
	journalPath := filepath.Join(stageDir, ".composer-commit-"+sessionID+".json")
	if err := os.WriteFile(journalPath, raw, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}

// TestContainer_RecoverComposerCommits_RollsBackPartialCommit exercises
// the full boot-recovery wiring (Container.recoverComposerCommits ->
// projectwizard.RecoverComposerCommits) against a real temp config
// dir: a leftover journal whose project file was never written to the
// live tree must be rolled back (a no-op here, since nothing landed)
// and the session marked commit-failed-resumable.
func TestContainer_RecoverComposerCommits_RollsBackPartialCommit(t *testing.T) {
	configDir := seedComposerConfigDir(t)
	sessionID, projectID := "pw_boot_partial", "boot-partial-proj"
	seedComposerLeftoverJournal(t, configDir, sessionID, projectID)

	store := newFakeWizardSessionStore()
	if err := store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID: sessionID, OperatorID: "op_1", Bundle: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	c := &Container{
		Logger:               zerolog.Nop(),
		repos:                &storage.Repositories{ProjectWizardSessions: store},
		projectWizardMetrics: projectwizard.NewMetrics(prometheus.NewRegistry()),
	}

	c.recoverComposerCommits(context.Background())

	if _, statErr := os.Stat(filepath.Join(configDir, ".composer-staging", sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected the staging dir removed, stat err = %v", statErr)
	}
	stored, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.CommittedProjectID != nil {
		t.Error("a rolled-back recovery must not commit the session")
	}
	if stored.BundleCommitFailedAt == nil {
		t.Error("expected the session marked commit-failed-resumable")
	}
}

// TestContainer_RecoverComposerCommits_FinishesLandedCommit is the
// load-bearing edge case at the boot-wiring level: the project file IS
// present in the live tree (the commit fully landed; only cleanup was
// interrupted). Recovery must finish the cleanup and stamp the session
// committed WITHOUT deleting the live project file.
func TestContainer_RecoverComposerCommits_FinishesLandedCommit(t *testing.T) {
	configDir := seedComposerConfigDir(t)
	sessionID, projectID := "pw_boot_landed", "boot-landed-proj"
	seedComposerLeftoverJournal(t, configDir, sessionID, projectID)
	liveProjectFile := filepath.Join(configDir, "projects", projectID+".yaml")
	if err := os.WriteFile(liveProjectFile, []byte("projectId: "+projectID+"\n"), 0o600); err != nil {
		t.Fatalf("seed live project file: %v", err)
	}

	store := newFakeWizardSessionStore()
	if err := store.Insert(context.Background(), &persistence.ProjectWizardSession{
		ID: sessionID, OperatorID: "op_1", Bundle: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	c := &Container{
		Logger:               zerolog.Nop(),
		repos:                &storage.Repositories{ProjectWizardSessions: store},
		projectWizardMetrics: projectwizard.NewMetrics(prometheus.NewRegistry()),
	}

	c.recoverComposerCommits(context.Background())

	if _, statErr := os.Stat(liveProjectFile); statErr != nil {
		t.Errorf("recovery must NEVER delete the live project file, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(configDir, ".composer-staging", sessionID)); !os.IsNotExist(statErr) {
		t.Errorf("expected the staging dir removed, stat err = %v", statErr)
	}
	stored, err := store.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.CommittedProjectID == nil || *stored.CommittedProjectID != projectID {
		t.Errorf("expected the session stamped committed to %q, got %+v", projectID, stored.CommittedProjectID)
	}
}

// TestContainer_RecoverComposerCommits_NoReposOrConfigDir_NoOp: the
// nil-safe wiring guard — no repos, or no resolvable config dir, must
// return without panicking (minimal wiring / CE / tests).
func TestContainer_RecoverComposerCommits_NoReposOrConfigDir_NoOp(t *testing.T) {
	(&Container{Logger: zerolog.Nop()}).recoverComposerCommits(context.Background())

	seedComposerConfigDir(t)
	(&Container{Logger: zerolog.Nop()}).recoverComposerCommits(context.Background())
}

// TestContainer_RecoverComposerCommits_NilContainer_NoOp: a nil
// *Container (defensive guard, mirrors every other nil-receiver-safe
// helper in this package) must not panic.
func TestContainer_RecoverComposerCommits_NilContainer_NoOp(t *testing.T) {
	t.Helper()
	var c *Container
	c.recoverComposerCommits(context.Background())
}

// TestContainer_RecoverComposerCommits_ScanError_LoggedNotFatal: a
// scan failure (the .composer-staging path occupied by a plain file
// instead of a directory) must be logged and swallowed — boot must
// never fail because of this best-effort sweep.
func TestContainer_RecoverComposerCommits_ScanError_LoggedNotFatal(t *testing.T) {
	configDir := seedComposerConfigDir(t)
	if err := os.WriteFile(filepath.Join(configDir, ".composer-staging"), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	store := newFakeWizardSessionStore()
	c := &Container{
		Logger:               zerolog.Nop(),
		repos:                &storage.Repositories{ProjectWizardSessions: store},
		projectWizardMetrics: projectwizard.NewMetrics(prometheus.NewRegistry()),
	}
	// Must not panic; the scan error is logged and swallowed.
	c.recoverComposerCommits(context.Background())
}
