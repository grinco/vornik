package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestComposerRecoveryDoctorAdapter_LeftoverJournal_ProjectPresent
// covers the "commit fully landed, cleanup pending" branch: the
// project-doctor's composer_commit check should get a heads-up detail
// (found=true) that does NOT claim the project is broken.
func TestComposerRecoveryDoctorAdapter_LeftoverJournal_ProjectPresent(t *testing.T) {
	dir := t.TempDir()
	projectID := "adapter-landed-proj"
	writeAdapterTestJournal(t, dir, "sess-1", projectID)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		t.Fatalf("seed projects dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", projectID+".yaml"), []byte("projectId: "+projectID+"\n"), 0o600); err != nil {
		t.Fatalf("seed live project file: %v", err)
	}

	adapter := composerRecoveryDoctorAdapter{liveConfigDir: dir}
	found, detail := adapter.LeftoverJournal(projectID)
	if !found {
		t.Fatal("expected the leftover journal to be found")
	}
	if detail == "" {
		t.Error("expected a non-empty detail")
	}
}

// TestComposerRecoveryDoctorAdapter_LeftoverJournal_ProjectAbsent
// covers the partial-commit branch: found=true with a detail
// describing the pending rollback.
func TestComposerRecoveryDoctorAdapter_LeftoverJournal_ProjectAbsent(t *testing.T) {
	dir := t.TempDir()
	projectID := "adapter-partial-proj"
	writeAdapterTestJournal(t, dir, "sess-2", projectID)

	adapter := composerRecoveryDoctorAdapter{liveConfigDir: dir}
	found, detail := adapter.LeftoverJournal(projectID)
	if !found {
		t.Fatal("expected the leftover journal to be found")
	}
	if detail == "" {
		t.Error("expected a non-empty detail")
	}
}

// TestComposerRecoveryDoctorAdapter_NoLeftover_NotFound: a clean tree
// (or an unrelated project id) reports not-found.
func TestComposerRecoveryDoctorAdapter_NoLeftover_NotFound(t *testing.T) {
	dir := t.TempDir()
	adapter := composerRecoveryDoctorAdapter{liveConfigDir: dir}
	if found, _ := adapter.LeftoverJournal("no-such-project"); found {
		t.Error("expected not-found on a clean tree")
	}
}

// TestComposerRecoveryDoctorAdapter_EmptyLiveConfigDir_NotFound: no
// live config dir wired (minimal/CE wiring) degrades to not-found
// rather than erroring — matches every other Deps-field nil degrade.
func TestComposerRecoveryDoctorAdapter_EmptyLiveConfigDir_NotFound(t *testing.T) {
	adapter := composerRecoveryDoctorAdapter{}
	if found, detail := adapter.LeftoverJournal("anything"); found || detail != "" {
		t.Errorf("expected found=false, detail=\"\" with no live config dir, got found=%v detail=%q", found, detail)
	}
}

// TestNewComposerRecoveryChecker_ResolvesFromContainer confirms the
// wiring helper resolves the live config dir from the container the
// same way the rest of the composer wiring does (resolveRegistryConfigDir),
// rather than needing its own separate resolution logic.
func TestNewComposerRecoveryChecker_ResolvesFromContainer(t *testing.T) {
	dir := seedComposerConfigDir(t)
	projectID := "checker-proj"
	writeAdapterTestJournal(t, dir, "sess-3", projectID)

	c := &Container{}
	checker := newComposerRecoveryChecker(c)
	found, _ := checker.LeftoverJournal(projectID)
	if !found {
		t.Error("expected the checker to resolve the live config dir via VORNIK_CONFIGS_DIR and find the leftover journal")
	}
}

func writeAdapterTestJournal(t *testing.T, configDir, sessionID, projectID string) {
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
	if err := os.WriteFile(filepath.Join(stageDir, ".composer-commit-"+sessionID+".json"), raw, 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
}
