//go:build integration

package cli

// Integration coverage for `vornikctl retention` (retention.go). The
// command loads a registry of projects (so per-project overrides
// apply) then runs the retention.Sweeper in preview or apply mode
// against the live DB. We build a minimal valid configs/ layout and
// point VORNIK_CONFIGS_DIR at it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func retention_reset() {
	retentionProject, retentionApply, retentionJSON = "", false, false
}

// retentionWriteConfigs writes a minimal project+swarm+workflow triple
// into a temp configs/ dir and points VORNIK_CONFIGS_DIR at it.
// Returns the project ID.
func retentionWriteConfigs(t *testing.T, projectID string) {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	swarmID := projectID + "-swarm"
	wfID := projectID + "-wf"
	mustWrite(t, filepath.Join(root, "projects", projectID+".yaml"),
		"projectId: "+projectID+"\ndisplayName: "+projectID+"\nswarmId: "+swarmID+"\ndefaultWorkflowId: "+wfID+"\n")
	mustWrite(t, filepath.Join(root, "swarms", swarmID+".md"),
		"---\nswarmId: "+swarmID+"\nroles:\n  - name: worker\n    runtime:\n      image: fake-agent\n---\n")
	mustWrite(t, filepath.Join(root, "workflows", wfID+".md"),
		"---\nworkflowId: "+wfID+"\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n")
	t.Setenv("VORNIK_CONFIGS_DIR", root)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestIntegration_Retention_PreviewTable(t *testing.T) {
	dbcovSetup(t)
	retention_reset()
	proj := dbcovUniqueProject("ret-preview")
	retentionWriteConfigs(t, proj)

	out, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err != nil {
		t.Fatalf("retention preview: %v", err)
	}
	if !strings.Contains(out, "preview only") {
		t.Errorf("expected preview banner:\n%s", out)
	}
	if !strings.Contains(out, "project") || !strings.Contains(out, "llm_usage") {
		t.Errorf("expected table header:\n%s", out)
	}
	if !strings.Contains(out, proj) {
		t.Errorf("project row missing:\n%s", out)
	}
}

func TestIntegration_Retention_JSON(t *testing.T) {
	dbcovSetup(t)
	retention_reset()
	proj := dbcovUniqueProject("ret-json")
	retentionWriteConfigs(t, proj)

	retentionJSON = true
	out, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err != nil {
		t.Fatalf("retention json: %v", err)
	}
	var parsed struct {
		Applied  bool `json:"applied"`
		Projects []struct {
			Project string `json:"project"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if parsed.Applied {
		t.Errorf("expected applied=false in preview")
	}
	if len(parsed.Projects) != 1 || parsed.Projects[0].Project != proj {
		t.Fatalf("unexpected projects: %+v", parsed.Projects)
	}
}

func TestIntegration_Retention_ProjectFilterNotFound(t *testing.T) {
	dbcovSetup(t)
	retention_reset()
	proj := dbcovUniqueProject("ret-filter")
	retentionWriteConfigs(t, proj)

	retentionProject = "definitely-not-a-real-project"
	_, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected project-not-found, got %v", err)
	}
}

func TestIntegration_Retention_ProjectFilterMatch(t *testing.T) {
	dbcovSetup(t)
	retention_reset()
	proj := dbcovUniqueProject("ret-match")
	retentionWriteConfigs(t, proj)

	retentionProject = proj
	out, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err != nil {
		t.Fatalf("retention filtered: %v", err)
	}
	if !strings.Contains(out, proj) {
		t.Errorf("filtered project missing:\n%s", out)
	}
}

func TestIntegration_Retention_ApplyMode(t *testing.T) {
	db := dbcovSetup(t)
	retention_reset()
	proj := dbcovUniqueProject("ret-apply")
	retentionWriteConfigs(t, proj)

	// Seed an aged tool_audit_log row that should be pruned by the
	// 30-day default. project_memory_chunks is NEVER pruned, so we
	// also seed an aged chunk and assert it survives.
	if _, err := db.Exec(`INSERT INTO tool_audit_log
		(id, project_id, task_id, execution_id, tool_name, created_at)
		VALUES ($1, $2, 'task-x', 'exec-x', 'shell', now() - interval '400 days')`,
		"tal-"+proj, proj); err != nil {
		t.Fatalf("seed tool_audit_log: %v", err)
	}
	aliveChunk := dbcovSeedChunk(t, db, proj, "keep", "survives", "")
	dbcovCleanupProject(t, db, proj)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tool_audit_log WHERE project_id=$1`, proj) })

	retentionApply = true
	out, err := dbcovCapture(t, func() error { return runRetention(retentionCmd, nil) })
	if err != nil {
		t.Fatalf("retention apply: %v", err)
	}
	if !strings.Contains(out, "applying retention") {
		t.Errorf("expected apply banner:\n%s", out)
	}
	// Memory chunk must survive (retention never prunes chunks).
	var chunkN int
	_ = db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE id=$1`, aliveChunk).Scan(&chunkN)
	if chunkN != 1 {
		t.Errorf("retention pruned a memory chunk (must never): %d", chunkN)
	}
}
