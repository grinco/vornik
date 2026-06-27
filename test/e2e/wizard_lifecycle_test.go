//go:build e2e_http
// +build e2e_http

// Package e2e_test — wizard lifecycle black-box e2e.
//
// These tests boot the REAL compiled vornik binary against a throwaway
// SQLite database + temp config tree and drive it over HTTP (both the
// /api and /ui servers, which are separate servers composed in the
// daemon). They guard the two project-creation flows end-to-end so we
// don't regress the bugs fixed in 2026-05-30:
//
//  1. create-via-wizard → commit → project loads → delete via UI.
//     Exercises template-anchored commit (project.yaml + swarm.md +
//     workflow.md materialised from a vetted template) and the
//     synchronous registry reload (the commit redirect must resolve
//     immediately, not race the file-watcher).
//  2. start wizard → abandon mid-creation → cancel → drafts banner
//     disappears.
//
// LLM-free: the converse turn needs a model, so instead of driving
// converse we seed a wizard-session row through the real SQLite repo
// (same write path the daemon uses) — representing the state a converse
// turn would have produced. autonomy-safe: the bundled e2e template is
// human-driven (no autonomy block) so the scheduler never tries to
// spawn a podman container in CI.
//
// Gated behind the `e2e_http` build tag so `go test ./...` and the
// podman-backed `e2e` suite are unaffected. Run with:
//
//	go test -tags=e2e_http ./test/e2e/...
package e2e_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"vornik.io/vornik/internal/persistence"
	sqliterepo "vornik.io/vornik/internal/persistence/sqlite"
)

const (
	e2eOperatorID   = "local:dev" // matches defaultSingleTenantOperatorID (auth off)
	e2eTemplateSlug = "e2e-basic"
)

var (
	httpBase string // http://127.0.0.1:<port>
	dbPath   string // SQLite file the daemon owns
)

// TestMain builds the daemon, lays out a temp config tree, boots the
// real binary, waits for readiness, runs the tests, then tears down.
func TestMain(m *testing.M) {
	code, err := runE2E(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[e2e] setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runE2E(m *testing.M) (int, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "[e2e] skipping on", runtime.GOOS)
		return 0, nil
	}
	root := repoRoot()
	work, err := os.MkdirTemp("", "vornik-e2e-*")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(work)

	binPath := filepath.Join(work, "vornik")
	// When GOCOVERDIR is set (CI's coverage job, or a local
	// `make test-coverage-e2e` run) build a coverage-instrumented
	// binary so the daemon's composition root — initHTTPServer and the
	// other container wiring that only the booted process exercises —
	// is counted in the merged coverage gate. The instrumented binary
	// writes covdata to GOCOVERDIR on graceful exit (see teardown).
	buildArgs := []string{"build"}
	if os.Getenv("GOCOVERDIR") != "" {
		// -covermode=atomic matches the unit + integration lanes so
		// scripts/merge-coverage.awk can merge all three profiles
		// (it refuses a mode mismatch).
		buildArgs = append(buildArgs, "-cover", "-coverpkg=./...", "-covermode=atomic")
	}
	buildArgs = append(buildArgs, "-o", binPath, "./cmd/vornik")
	build := exec.Command("go", buildArgs...)
	build.Dir = root
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return 1, fmt.Errorf("build vornik: %w", err)
	}

	configsTree := filepath.Join(work, "configs")
	dbPath = filepath.Join(work, "vornik.db")
	port := freePort()
	httpBase = fmt.Sprintf("http://127.0.0.1:%d", port)

	if err := layoutConfigTree(configsTree, filepath.Join(work, "vornik.yaml"), dbPath, work, port); err != nil {
		return 1, fmt.Errorf("layout config: %w", err)
	}

	logFile, _ := os.Create(filepath.Join(work, "daemon.log"))
	daemon := exec.Command(binPath)
	daemon.Dir = work
	daemon.Env = append(os.Environ(),
		"VORNIK_CONFIG="+filepath.Join(work, "vornik.yaml"),
		"VORNIK_CONFIGS_DIR="+configsTree,
	)
	if logFile != nil {
		daemon.Stdout, daemon.Stderr = logFile, logFile
		defer logFile.Close()
	}
	if err := daemon.Start(); err != nil {
		return 1, fmt.Errorf("start daemon: %w", err)
	}
	defer func() {
		// Graceful SIGTERM first: vornik handles it via signal.
		// NotifyContext and runs a clean shutdown, which lets a
		// -cover-instrumented binary flush covdata to GOCOVERDIR.
		// SIGKILL would lose the coverage profile entirely. Fall
		// back to Kill if it doesn't exit promptly.
		_ = daemon.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = daemon.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = daemon.Process.Kill()
			<-done
		}
	}()

	if err := waitHealthy(httpBase, 40*time.Second); err != nil {
		// Surface the daemon log so a CI failure is debuggable.
		if b, rerr := os.ReadFile(filepath.Join(work, "daemon.log")); rerr == nil {
			fmt.Fprintln(os.Stderr, "[e2e] daemon log:\n"+string(b))
		}
		return 1, fmt.Errorf("daemon never became healthy: %w", err)
	}
	return m.Run(), nil
}

// --- Scenario 1: create → commit → live → delete via UI ---

func TestWizardCreateCommitLiveDelete(t *testing.T) {
	const projectID = "e2e-wizard-proj"
	proposal := `{"raw":{"projectId":"` + projectID + `","displayName":"E2E Wizard Project","topic":"widgets"}}`
	sid := seedSession(t, &persistence.ProjectWizardSession{
		ID:                sessionID("commit"),
		OperatorID:        e2eOperatorID,
		Transcript:        []byte("[]"),
		CurrentProposal:   []byte(proposal),
		SuggestedTemplate: e2eTemplateSlug,
		ReadyToCommit:     true,
	})

	// Commit via the API.
	resp, body := doReq(t, http.MethodPost, "/api/v1/projects/wizard/"+sid+"/commit", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("commit: HTTP %d, body=%s", resp.StatusCode, body)
	}
	var cr struct {
		ProjectID string `json:"project_id"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &cr); err != nil {
		t.Fatalf("commit body: %v (%s)", err, body)
	}
	if cr.ProjectID != projectID {
		t.Fatalf("project_id = %q, want %q", cr.ProjectID, projectID)
	}

	// The template-anchored commit must have written ALL three files
	// (project + swarm + workflow), not just the project YAML — this is
	// the swarmId-gap regression guard.
	for _, rel := range []string{
		"projects/" + projectID + ".yaml",
		"swarms/" + projectID + "-swarm.md",
		"workflows/" + projectID + "-wf.md",
	} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "configs", rel)); err != nil {
			t.Errorf("expected committed file %s: %v", rel, err)
		}
	}

	// The project must be live in the running daemon's registry NOW
	// (synchronous reload), not after the file-watcher catches up.
	resp, page := doReq(t, http.MethodGet, "/ui/projects/"+projectID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project detail: HTTP %d", resp.StatusCode)
	}
	if strings.Contains(strings.ToLower(page), "project not found") {
		t.Fatal("project detail rendered 'project not found' — registry did not pick up the commit")
	}
	if !strings.Contains(page, "E2E Wizard Project") {
		t.Errorf("project detail missing displayName")
	}

	// It also appears in the API project list.
	_, list := doReq(t, http.MethodGet, "/api/v1/projects", "", nil)
	if !strings.Contains(list, projectID) {
		t.Errorf("project %q absent from /api/v1/projects", projectID)
	}

	// Delete via the UI: archive (min grace) then delete-now.
	form := url.Values{"grace": {"1m"}, "reason": {"e2e cleanup"}}
	resp, ab := doReq(t, http.MethodPost, "/ui/projects/"+projectID+"/archive", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if resp.StatusCode >= 400 {
		t.Fatalf("archive via UI: HTTP %d, body=%s", resp.StatusCode, ab)
	}
	resp, dn := doReq(t, http.MethodPost, "/ui/projects/"+projectID+"/delete-now", "", nil)
	if resp.StatusCode >= 400 {
		t.Fatalf("delete-now via UI: HTTP %d, body=%s", resp.StatusCode, dn)
	}

	// After archive, the project detail reflects the archived lifecycle.
	_, page = doReq(t, http.MethodGet, "/ui/projects/"+projectID, "", nil)
	if !strings.Contains(strings.ToLower(page), "archiv") {
		t.Errorf("project detail does not reflect archived state after delete-now via UI")
	}
}

// --- Scenario 2: start → abandon → cancel → banner disappears ---

func TestWizardAbandonCancelBanner(t *testing.T) {
	base := bannerCount(t)

	sid := seedSession(t, &persistence.ProjectWizardSession{
		ID:            sessionID("abandon"),
		OperatorID:    e2eOperatorID,
		Transcript:    []byte(`[{"role":"user","content":"thinking about a thing","created_at":"2026-05-30T00:00:00Z"}]`),
		ReadyToCommit: false, // mid-creation, never committed
	})

	if got := bannerCount(t); got != base+1 {
		t.Fatalf("drafts banner after seeding a draft = %d, want %d", got, base+1)
	}

	resp, body := doReq(t, http.MethodPost, "/api/v1/projects/wizard/"+sid+"/cancel", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: HTTP %d, body=%s", resp.StatusCode, body)
	}

	if got := bannerCount(t); got != base {
		t.Fatalf("drafts banner after cancel = %d, want %d (cancelled draft must not count)", got, base)
	}
}

// --- helpers ---

var bannerRe = regexp.MustCompile(`(\d+)\s+unfinished`)

// bannerCount parses the "/ui/projects" wizard-drafts banner count.
// Returns 0 when the banner is absent (no unfinished drafts).
func bannerCount(t *testing.T) int {
	t.Helper()
	_, page := doReq(t, http.MethodGet, "/ui/projects", "", nil)
	m := bannerRe.FindStringSubmatch(page)
	if m == nil {
		return 0
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	return n
}

// seedSession inserts a wizard session through the real SQLite repo
// (same write path the daemon uses, so timestamp/column formats match)
// and returns its ID. Used in place of an LLM-backed converse turn.
func seedSession(t *testing.T, s *persistence.ProjectWizardSession) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	repo := sqliterepo.NewProjectWizardSessionRepository(db)
	if err := repo.Insert(context.Background(), s); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return s.ID
}

func doReq(t *testing.T, method, path, contentType string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, httpBase+path, body)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Don't auto-follow redirects — UI POSTs answer 303 and we assert
	// on the redirect status, not the followed page.
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("healthz not 200 within %s", timeout)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// sessionID returns a deterministic-enough unique id without relying on
// time-based randomness (which workflow scripts ban; fine here but kept
// simple). A monotonic counter keyed by tag avoids collisions across the
// two tests sharing one daemon.
var sidCounter int

func sessionID(tag string) string {
	sidCounter++
	return fmt.Sprintf("pw_e2e_%s_%03d", tag, sidCounter)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0) // .../test/e2e/wizard_lifecycle_test.go
	dir := filepath.Dir(file)
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	panic("repo root (go.mod) not found from " + file)
}

func layoutConfigTree(configsTree, configFile, dbFile, work string, port int) error {
	for _, sub := range []string{"projects", "swarms", "workflows", filepath.Join("project-templates", e2eTemplateSlug)} {
		if err := os.MkdirAll(filepath.Join(configsTree, sub), 0o755); err != nil {
			return err
		}
	}
	tmplDir := filepath.Join(configsTree, "project-templates", e2eTemplateSlug)
	files := map[string]string{
		filepath.Join(tmplDir, "template.yaml"):     e2eTemplateManifest,
		filepath.Join(tmplDir, "project.yaml.tmpl"): e2eProjectTmpl,
		filepath.Join(tmplDir, "swarm.md.tmpl"):     e2eSwarmTmpl,
		filepath.Join(tmplDir, "workflow.md.tmpl"):  e2eWorkflowTmpl,
		configFile: fmt.Sprintf(e2eDaemonConfig, port, dbFile, filepath.Join(work, "artifacts"), filepath.Join(work, "artifacts")),
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// e2eTemplateManifest + the three .tmpl files form a self-contained,
// human-driven (autonomy-off) template so the committed project loads
// cleanly (its swarm + workflow exist) and the scheduler never fires.
const e2eTemplateManifest = `displayName: "E2E Basic"
description: "Self-contained template for the wizard e2e (human-driven, no autonomy)."
parameters:
  - name: projectId
    type: string
    required: true
  - name: displayName
    type: string
    default: "E2E Project"
    required: true
  - name: topic
    type: string
    default: "things"
files:
  - source: project.yaml.tmpl
    target: "projects/{{.projectId}}.yaml"
  - source: swarm.md.tmpl
    target: "swarms/{{.projectId}}-swarm.md"
  - source: workflow.md.tmpl
    target: "workflows/{{.projectId}}-wf.md"
`

const e2eProjectTmpl = `projectId: "{{.projectId}}"
displayName: "{{.displayName}}"
swarmId: "{{.projectId}}-swarm"
defaultWorkflowId: "{{.projectId}}-wf"
defaultPriority: 30
maxConcurrentTasks: 1
permissions:
  secrets: []
  allowedTools:
    - "current_time"
`

const e2eSwarmTmpl = `---
swarmId: "{{.projectId}}-swarm"
displayName: "{{.displayName}} swarm"
leadRole: "lead"
roles:
  - name: "lead"
    description: "Single lead role for the e2e template ({{.topic}})."
    runtime:
      image: "vornik-agent:latest"
    permissions:
      allowedTools:
        - "current_time"
---

# {{.displayName}} swarm

## Role prompts

### lead

You are the lead for {{.displayName}}. Topic: {{.topic}}.
`

const e2eWorkflowTmpl = `---
workflowId: "{{.projectId}}-wf"
displayName: "{{.displayName}} workflow"
entrypoint: "run"
maxStepVisits: 1
steps:
  run:
    type: "agent"
    role: "lead"
    on_success: "done"
    on_fail: "failed"
    timeout: "5m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "e2e workflow failed"
---

# {{.displayName}} workflow

## Prompts

### run

Do the one thing for {{.displayName}}.
`

// e2eDaemonConfig is the dev-config shape: SQLite, loopback, auth off,
// every LLM/observability subsystem disabled. Args: port, dbPath,
// artifactsPath, artifactsPath.
const e2eDaemonConfig = `server:
  address: "127.0.0.1:%d"
  read_timeout: 30s
  write_timeout: 360s
database:
  driver: sqlite
  path: "%s"
storage:
  artifacts_path: "%s"
artifacts:
  artifacts_path: "%s"
scheduler:
  max_concurrent_tasks: 2
  lease_timeout: 5m
logging:
  level: info
  format: text
api:
  auth_enabled: false
metrics:
  enabled: false
tracing:
  enabled: false
# Chat is ENABLED only so the project wizard is wired (it requires a
# non-nil ChatClient). The endpoint is deliberately unreachable: this
# e2e seeds sessions + drives commit/cancel, none of which call the LLM
# (only converse does). Nothing here ever dials the endpoint.
chat:
  enabled: true
  provider: "http"
  endpoint: "http://127.0.0.1:1/v1"
  api_key: "e2e-dummy"
  model: "e2e-dummy-model"
memory:
  enabled: false
telegram:
  enabled: false
autonomy:
  default_evaluate_timeout: 5m
runtime:
  userns_mode: ""
  run_as_user: ""
mcp:
  servers: []
`
