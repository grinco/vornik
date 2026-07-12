package projectwizard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/registry"
)

func writeFilesToDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for target, body := range files {
		full := filepath.Join(dir, target)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// minimalLiveConfig writes a single, fully valid, self-consistent
// project+swarm+workflow trio to dir, mimicking a pre-existing live
// registry the composer's staged validation must layer over.
func minimalLiveConfig(t *testing.T, dir string) {
	t.Helper()
	writeFilesToDir(t, dir, map[string]string{
		"projects/legacy.yaml": "projectId: \"legacy\"\ndisplayName: \"Legacy\"\nswarmId: \"legacy-swarm\"\ndefaultWorkflowId: \"legacy-wf\"\n",
		"swarms/legacy-swarm.md": "---\nswarmId: \"legacy-swarm\"\nleadRole: lead\nroles:\n" +
			"    - name: \"lead\"\n      runtime:\n        image: \"vornik-agent:latest\"\n---\n# legacy\n",
		"workflows/legacy-wf.md": "---\nworkflowId: \"legacy-wf\"\nentrypoint: \"go\"\nsteps:\n  go:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"go do it\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n# legacy wf\n",
	})
}

func TestStageBundleForValidation_Valid(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("renderMaterializedBundle: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected valid bundle to pass staged validation, errors=%v", res.Errors)
	}
}

func TestStageBundleForValidation_DanglingDefaultWorkflowId(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	mb.Project.DefaultWorkflowID = "does-not-exist"
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected dangling defaultWorkflowId to fail staged validation")
	}
	if !anyContains(res.Errors, "non-existent workflow") {
		t.Errorf("expected a non-existent-workflow error, got %v", res.Errors)
	}
}

func TestStageBundleForValidation_StepRoleDangling(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	mb.Workflows[0].Steps["gather"] = registry.WorkflowStep{Type: "agent", Role: "ghost-role", Prompt: "gather it", OnSuccess: "write", OnFail: "failed"}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected dangling step role to fail staged validation")
	}
	if !anyContains(res.Errors, "not present in swarm") {
		t.Errorf("expected a role-not-present error, got %v", res.Errors)
	}
}

func TestStageBundleForValidation_LeadRoleInvalid(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	mb.Swarm.LeadRole = "ghost-lead"
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected invalid leadRole to fail staged validation")
	}
	if !anyContains(res.Errors, "leadRole") {
		t.Errorf("expected a leadRole error, got %v", res.Errors)
	}
}

func TestStageBundleForValidation_UnreachableEntrypoint(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	// Add a step disconnected from the entrypoint's reachable set.
	mb.Workflows[0].Steps["orphan"] = registry.WorkflowStep{Type: "agent", Role: "researcher", Prompt: "orphaned", OnSuccess: "done", OnFail: "failed"}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected an unreachable step to fail staged validation")
	}
	if !anyContains(res.Errors, "not reachable") {
		t.Errorf("expected a not-reachable error, got %v", res.Errors)
	}
}

func TestStageBundleForValidation_ZeroWorkflows(t *testing.T) {
	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	mb.Workflows = nil
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// No workflow file rendered at all; the project's
	// defaultWorkflowId now dangles.
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected a zero-workflow bundle to fail staged validation")
	}
}

func TestStageBundleForValidation_LayersOverLiveConfig(t *testing.T) {
	liveDir := t.TempDir()
	minimalLiveConfig(t, liveDir)

	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation(liveDir, files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected the bundle to validate cleanly alongside an unrelated live project, errors=%v", res.Errors)
	}
	// Staged validation must not have touched the live tree.
	entries, err := os.ReadDir(filepath.Join(liveDir, "projects"))
	if err != nil {
		t.Fatalf("read live projects dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "legacy.yaml" {
		t.Errorf("live config tree was modified by staged validation: %v", entries)
	}
}

func TestLiveEntityIDsFromConfigDir(t *testing.T) {
	liveDir := t.TempDir()
	minimalLiveConfig(t, liveDir)
	ids, err := liveEntityIDsFromConfigDir(liveDir)
	if err != nil {
		t.Fatalf("liveEntityIDsFromConfigDir: %v", err)
	}
	if !ids.Projects["legacy"] || !ids.Swarms["legacy-swarm"] || !ids.Workflows["legacy-wf"] {
		t.Errorf("expected legacy entity ids present, got %+v", ids)
	}
}

func TestLiveEntityIDsFromConfigDir_Empty(t *testing.T) {
	ids, err := liveEntityIDsFromConfigDir("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids.Projects) != 0 {
		t.Errorf("expected empty set for blank configDir, got %+v", ids)
	}
}

func TestCollisionCheckBundle_AgainstRealLiveConfig(t *testing.T) {
	liveDir := t.TempDir()
	minimalLiveConfig(t, liveDir)
	ids, err := liveEntityIDsFromConfigDir(liveDir)
	if err != nil {
		t.Fatalf("liveEntityIDsFromConfigDir: %v", err)
	}
	bundleIDsCollide := bundleIDs{ProjectID: "legacy", SwarmID: "fresh-swarm", WorkflowIDs: []string{"fresh-wf"}}
	errs := collisionCheckBundle(bundleIDsCollide, ids)
	if !anyContains(errs, `projectId "legacy"`) {
		t.Errorf("expected a collision against the live 'legacy' project, got %v", errs)
	}
	if !strings.Contains(strings.Join(errs, ";"), "legacy") {
		t.Errorf("expected the error to name the colliding id, got %v", errs)
	}
}

// captureZerologOutput redirects the global zerolog logger to buf for
// the duration of the test, restoring the original logger on cleanup.
func captureZerologOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = orig })
	return &buf
}

// TestStageBundleForValidation_PerEntityValidationError_NeverLeaksTempPath
// is the companion-review finding-3 regression test: a per-entity
// registry validation failure (invalid leadRole) must still surface
// its real, actionable message to the operator — but, once a live
// config dir is wired (the layered-path case that made
// registry.LoadFromPaths wrap failures as `load layer %q: %w`,
// embedding the internal staging temp dir), that message must never
// contain the temp dir path.
func TestStageBundleForValidation_PerEntityValidationError_NeverLeaksTempPath(t *testing.T) {
	liveDir := t.TempDir()
	minimalLiveConfig(t, liveDir)

	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	mb.Swarm.LeadRole = "ghost-lead"
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation(liveDir, files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected invalid leadRole to fail staged validation")
	}
	if !anyContains(res.Errors, "leadRole") {
		t.Errorf("expected the real leadRole error to still surface, got %v", res.Errors)
	}
	joined := strings.Join(res.Errors, ";")
	if strings.Contains(joined, "vornik-composer-stage") || strings.Contains(joined, os.TempDir()) {
		t.Errorf("operator-facing error must never leak the internal staging path, got %v", res.Errors)
	}
}

// TestStageBundleForValidation_UnexpectedFatalError_MasksPathAndLogs is
// the companion-review finding-3 regression test for the actually
// dangerous case: a genuinely unexpected registry error (not one of
// the known per-entity validation types) must never reach the
// operator-facing envelope verbatim — it may embed the internal
// staging temp dir (or, as reproduced here, the bogus live config
// path). The raw error must still be logged server-side so it isn't
// lost for debugging.
func TestStageBundleForValidation_UnexpectedFatalError_MasksPathAndLogs(t *testing.T) {
	buf := captureZerologOutput(t)

	// A regular FILE (not a directory) as the live config dir forces
	// registry.LoadFromPaths into the two-path branch (whose failures
	// are wrapped as `load layer %q: ...`) with a fatal, non-
	// ValidationError, non-per-entity-validation error (ENOTDIR
	// reading "<file>/projects").
	liveConfigFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(liveConfigFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation(liveConfigFile, files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if res.OK {
		t.Fatal("expected the fatal registry error to fail staged validation")
	}
	if len(res.Errors) != 1 || res.Errors[0] != "bundle validation failed" {
		t.Errorf("expected a generic operator-facing message, got %v", res.Errors)
	}
	if strings.Contains(strings.Join(res.Errors, ";"), liveConfigFile) {
		t.Error("operator-facing error must never contain the internal path")
	}
	if !strings.Contains(buf.String(), liveConfigFile) {
		t.Errorf("expected the raw error (with path) to be logged server-side, got %q", buf.String())
	}
}

// TestStageBundleForValidation_RemoveAllFailure_WarnLoggedNotFatal is
// the companion-review finding-5 regression test: a cleanup (RemoveAll)
// failure on the staging temp dir must never affect the already-
// computed validation OUTCOME, and must not be silently discarded —
// it's warn-logged as a temp-dir-accumulation signal.
func TestStageBundleForValidation_RemoveAllFailure_WarnLoggedNotFatal(t *testing.T) {
	buf := captureZerologOutput(t)

	origRemoveAll := removeAllFn
	t.Cleanup(func() { removeAllFn = origRemoveAll })
	simulatedErr := errors.New("simulated: permission denied removing staging dir")
	removeAllFn = func(string) error { return simulatedErr }

	mb, _, err := materializeBundle(validComposedBundle(), testArchetypes())
	if err != nil {
		t.Fatalf("materializeBundle: %v", err)
	}
	files, err := renderMaterializedBundle(mb)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	res, err := stageBundleForValidation("", files)
	if err != nil {
		t.Fatalf("stageBundleForValidation: %v", err)
	}
	if !res.OK {
		t.Fatalf("expected a valid bundle to still pass staged validation despite a cleanup failure, errors=%v", res.Errors)
	}
	if !strings.Contains(buf.String(), simulatedErr.Error()) {
		t.Errorf("expected the RemoveAll failure to be warn-logged, got %q", buf.String())
	}
}

func TestStageBundleForValidation_RejectsUnsafeTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "escape.yaml")
	_, err := stageBundleForValidation(root, map[string]string{"projects/../../escape.yaml": "x"})
	if err == nil {
		t.Fatal("expected unsafe target to be rejected")
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe validation target must not write outside staging dir, stat=%v", statErr)
	}
}
