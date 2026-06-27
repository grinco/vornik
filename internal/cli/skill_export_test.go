package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// startFakeDaemon spins up an httptest server that responds with
// the fixtures the export CLI needs: project config, workflow,
// swarm. The fixture set is deliberately small — the export's
// real complexity is the assembly logic, not the HTTP layer.
func startFakeDaemon(t *testing.T, projectID, workflowID, swarmID string) *httptest.Server {
	t.Helper()
	wf := registry.Workflow{
		ID:          workflowID,
		DisplayName: "Research",
		Description: "Two-step linear pipeline.",
		Version:     "1.0.0",
		Entrypoint:  "research",
		Steps: map[string]registry.WorkflowStep{
			"research": {
				Type:      "agent",
				Role:      "researcher",
				Prompt:    "Find facts.",
				OnSuccess: "done",
				OnFail:    "failed",
			},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
	sw := registry.Swarm{
		ID:          swarmID,
		DisplayName: "Assistant",
		Roles: []registry.SwarmRole{
			{Name: "researcher", Description: "Research role", SystemPrompt: "You research."},
			{Name: "unused", Description: "Spare role", SystemPrompt: "Spare."},
		},
	}
	projectCfg := map[string]any{
		"projectId":         projectID,
		"swarmId":           swarmID,
		"defaultWorkflowId": workflowID,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/"+projectID+"/config", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(projectCfg)
	})
	mux.HandleFunc("/api/v1/workflows/"+workflowID, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(wf)
	})
	mux.HandleFunc("/api/v1/swarms/"+swarmID, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sw)
	})
	return httptest.NewServer(mux)
}

func setSwarmctlAPIEnv(t *testing.T, server *httptest.Server) {
	t.Helper()
	t.Setenv("VORNIK_API_URL", server.URL)
	t.Setenv("VORNIK_API_KEY", "test-key")
}

func TestSkillExport_HappyPath(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	out := captureExportOutput(t, "demo/research", func() {})

	// Parse the emitted bytes back through the registry parser as
	// the end-to-end sanity check — the CLI's actual job is to
	// produce something the importer can swallow.
	skill, err := registry.ParseSwarmSkill([]byte(out), "exported.md")
	if err != nil {
		t.Fatalf("parse exported bytes: %v\n---\n%s", err, out)
	}
	if skill.Workflow == nil {
		t.Fatal("exported skill has no workflow")
	}
	if skill.Workflow.ID != "research" {
		t.Errorf("workflow id: got %q want research", skill.Workflow.ID)
	}
	if len(skill.Roles) != 1 || skill.Roles[0].Name != "researcher" {
		t.Errorf("expected only the referenced 'researcher' role, got %d roles: %#v", len(skill.Roles), skill.Roles)
	}
	if skill.Roles[0].SystemPrompt != "You research." {
		t.Errorf("role system prompt round-trip lost: %q", skill.Roles[0].SystemPrompt)
	}
}

func TestSkillExport_StandardModeStripsVornikBlock(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	out := captureExportOutput(t, "demo/research", func() {
		skillExportStandard = true
	})
	if strings.Contains(out, "metadata:") {
		t.Errorf("--standard output should not contain 'metadata:' block:\n%s", out)
	}
	if !strings.Contains(out, "## Prompts") {
		t.Errorf("body sections should still render in --standard:\n%s", out)
	}
}

func TestSkillExport_AuthorLicenseOverrides(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	out := captureExportOutput(t, "demo/research", func() {
		skillExportAuthor = "vadim@example.com"
		skillExportLicense = "Apache-2.0"
	})
	if !strings.Contains(out, "author: vadim@example.com") {
		t.Errorf("author flag should land in output:\n%s", out)
	}
	if !strings.Contains(out, "license: Apache-2.0") {
		t.Errorf("license flag should land in output:\n%s", out)
	}
}

func TestSkillExport_WriteToFile(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	dir := t.TempDir()
	outPath := dir + "/out.md"
	resetExportFlags()
	skillExportOutput = outPath
	defer resetExportFlags()
	err := runSkillExport(skillExportCmd, []string{"demo/research"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Errorf("expected file at %s: %v", outPath, err)
	}
}

func TestSkillExport_BadArg(t *testing.T) {
	resetExportFlags()
	defer resetExportFlags()
	err := runSkillExport(skillExportCmd, []string{"no-slash"})
	if err == nil || !strings.Contains(err.Error(), "<project>/<workflow>") {
		t.Errorf("expected project/workflow error, got %v", err)
	}
}

func TestSkillExport_ProjectNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"NOT_FOUND","message":"missing"}}`, http.StatusNotFound)
	}))
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	resetExportFlags()
	defer resetExportFlags()
	err := runSkillExport(skillExportCmd, []string{"ghost/wf"})
	if err == nil {
		t.Errorf("expected 404 error, got nil")
	}
}

// captureExportOutput runs the export CLI against the configured
// fake daemon and returns the stdout bytes. The opts callback is
// the place to set per-test flag overrides.
func captureExportOutput(t *testing.T, arg string, opts func()) string {
	t.Helper()
	resetExportFlags()
	defer resetExportFlags()
	opts()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	skillExportCmd.SetOut(w)
	err = runSkillExport(skillExportCmd, []string{arg})
	_ = w.Close()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	buf := make([]byte, 64*1024)
	n, _ := r.Read(buf)
	_ = r.Close()
	return string(buf[:n])
}

func resetExportFlags() {
	skillExportOutput = ""
	skillExportStandard = false
	skillExportAuthor = ""
	skillExportLicense = ""
	skillExportVersion = ""
}
