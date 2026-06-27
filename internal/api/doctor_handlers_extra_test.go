package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/lib/pq" // register pq driver for the closed-DB trick

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/config"
)

// closedDB returns a *sql.DB that's been opened and immediately closed.
// Every Query / Exec returns "sql: database is closed" without touching
// the network, which is exactly what the DB-error branches in the
// doctor checks need to exercise.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return db
}

// minimalProjectConfigs writes a single valid project + swarm +
// workflow under root, returning that root for the doctor checks
// that need a config dir to be present and load-clean.
func minimalProjectConfigs(t *testing.T, root string, projectExtra string) {
	t.Helper()
	files := map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "vornik-agent:latest" }
---
`,
		"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`,
		"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
` + projectExtra,
	}
	writeAll(t, root, files)
}

// ---------------------------------------------------------------
// Constructor / setters
// ---------------------------------------------------------------

func TestNewDoctorHandlers_AssignsDB(t *testing.T) {
	db := &sql.DB{}
	h := NewDoctorHandlers(db)
	require.NotNil(t, h)
	assert.Same(t, db, h.db, "constructor must store the DB pointer for the checks to use")
}

func TestDoctorHandlers_Setters_StoreValues(t *testing.T) {
	h := NewDoctorHandlers(nil)
	h.SetConfigDir("/etc/vornik/configs")
	h.SetConfigPath("/etc/vornik/config.yaml")
	h.SetPricingPath("/etc/vornik/pricing.yaml")
	assert.Equal(t, "/etc/vornik/configs", h.configDir)
	assert.Equal(t, "/etc/vornik/config.yaml", h.configPath)
	assert.Equal(t, "/etc/vornik/pricing.yaml", h.pricingPath)
}

func TestSetServerConfig_NilIsNoop(t *testing.T) {
	h := &DoctorHandlers{}
	// Pin pre-call state so we can verify the nil branch returns
	// without touching any field.
	h.serverAddress = "preserve-me"
	h.SetServerConfig(nil)
	assert.Equal(t, "preserve-me", h.serverAddress, "nil config must early-return without clobbering captured state")
}

func TestSetServerConfig_PrefersChatModelOverAgentLLM(t *testing.T) {
	h := &DoctorHandlers{}
	h.SetServerConfig(&config.Config{
		Chat: config.ChatConfig{Model: "primary"},
		Runtime: config.RuntimeConfig{
			AgentLLM: config.AgentLLMConfig{Model: "fallback"},
		},
		Telegram: config.TelegramConfig{DispatcherProjectID: "p1"},
	})
	assert.Equal(t, "primary", h.dispatcherChatModel, "chat.model wins when set")
	assert.Equal(t, "p1", h.dispatcherProjectID)

	// Falls back to agent_llm.model when chat.model is empty.
	h2 := &DoctorHandlers{}
	h2.SetServerConfig(&config.Config{
		Runtime: config.RuntimeConfig{
			AgentLLM: config.AgentLLMConfig{Model: "fallback-only"},
		},
	})
	assert.Equal(t, "fallback-only", h2.dispatcherChatModel)
}

// ---------------------------------------------------------------
// DB-dependent checks — closed-DB error path
// ---------------------------------------------------------------
// Each check has a Query/Exec at its top that errors when the DB is
// closed. We exercise that branch here, which doesn't prove the happy
// path but does cover the "query failed" → ERROR DoctorCheck return
// path. The schema-drift, secrets-hygiene, and worktree tests cover
// the happy-path / config-driven branches separately.

func TestCheckStaleLeases_QueryError(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkStaleLeases(t.Context(), false)
	assert.Equal(t, "stale_leases", got.Name)
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "query failed")
}

func TestCheckOrphanedWatchers_QueryError(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkOrphanedWatchers(t.Context(), false)
	assert.Equal(t, "orphaned_watchers", got.Name)
	assert.Equal(t, "ERROR", got.Status)
}

func TestCheckStuckExecutions_QueryError(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkStuckExecutions(t.Context(), false)
	assert.Equal(t, "stuck_executions", got.Name)
	assert.Equal(t, "ERROR", got.Status)
}

func TestCheckTaskStateAudit_QueryError(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkTaskStateAudit(t.Context(), false)
	assert.Equal(t, "task_state_audit", got.Name)
	assert.Equal(t, "ERROR", got.Status)
}

func TestCheckDatabaseSchema_AllMissingWhenClosed(t *testing.T) {
	// A closed DB makes every "table exists?" query fail, so every
	// required table + index ends up in the missing list. This pins
	// the message format and the missing/expected counts.
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkDatabaseSchema(t.Context())
	assert.Equal(t, "database_schema", got.Name)
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "missing tables/indexes")
	// At least one table:* and one index:* item.
	hasTable, hasIndex := false, false
	for _, it := range got.Items {
		if strings.HasPrefix(it, "table:") {
			hasTable = true
		}
		if strings.HasPrefix(it, "index:") {
			hasIndex = true
		}
	}
	assert.True(t, hasTable, "must report at least one missing table")
	assert.True(t, hasIndex, "must report at least one missing index")
}

func TestCheckOrphanFKRows_AllProbesFail(t *testing.T) {
	h := &DoctorHandlers{db: closedDB(t)}
	got := h.checkOrphanFKRows(t.Context(), false)
	assert.Equal(t, "orphan_fk_rows", got.Name)
	// Every probe fails on its count query, so all three end up as
	// "probe failed" items. totalOrphans stays 0 → status is OK
	// with a benign message (intentional: probe failure shouldn't
	// page the operator like a real orphan would).
	assert.Equal(t, "OK", got.Status)
	assert.Equal(t, "no orphan FK rows across audit/usage/watchers", got.Message)
}

// ---------------------------------------------------------------
// Pure-logic checks (no DB required)
// ---------------------------------------------------------------

func TestCheckConfigValidation_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkConfigValidation()
	assert.Equal(t, "config_validation", got.Name)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no config directory")
}

func TestCheckConfigValidation_InvalidMarkdown(t *testing.T) {
	// A swarms/*.md file with malformed frontmatter trips
	// reg.Stage's loader. (A nonexistent directory is treated as
	// an empty config set by the loader, which is intentional
	// behaviour.) Stale `.yaml` files are silently ignored
	// post-YAML-removal (2026-05-17), so a "broken YAML" test no
	// longer exercises the same path — the SWARM.md parser is the
	// new entry point that surfaces malformed config.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "broken.md"),
		[]byte("not even a frontmatter marker"), 0o644))
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigValidation()
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "failed to load configs")
}

func TestCheckConfigValidation_ValidationError(t *testing.T) {
	// Configs load fine but ValidateStaged finds cross-reference
	// inconsistency: project references a swarm that doesn't exist.
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{
		"projects/orphan.yaml": `projectId: "orphan"
swarmId: "does-not-exist"
defaultWorkflowId: "missing"
`,
	})
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigValidation()
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "config validation failed")
	require.NotEmpty(t, got.Items)
}

func TestCheckConfigValidation_ValidConfigs(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "")
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigValidation()
	assert.Equal(t, "OK", got.Status, "items=%v", got.Items)
}

func TestCheckPricingCoverage_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkPricingCoverage()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no config dir")
}

func TestCheckPricingCoverage_NoPricingPath(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "")
	h := &DoctorHandlers{configDir: dir}
	got := h.checkPricingCoverage()
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "pricing.yaml path not configured")
}

func TestCheckPricingCoverage_LoadFailure(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "")
	// Pricing path resolves to a directory rather than a file — pricing.Load fails.
	h := &DoctorHandlers{configDir: dir, pricingPath: dir}
	got := h.checkPricingCoverage()
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "load pricing table")
}

func TestCheckPricingCoverage_MissingEntry(t *testing.T) {
	dir := t.TempDir()
	// Swarm with a model that won't appear in the pricing table.
	writeAll(t, dir, map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "x" }
    model: "made-up-model-not-in-pricing"
---
`,
		"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`,
		"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
`,
	})
	pricingPath := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(pricingPath, []byte(`models:
  different-model:
    input: 1.0
    output: 2.0
`), 0o644))

	h := &DoctorHandlers{configDir: dir, pricingPath: pricingPath}
	got := h.checkPricingCoverage()
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "missing from pricing.yaml")
	require.NotEmpty(t, got.Items)
	assert.Contains(t, got.Items[0], "made-up-model-not-in-pricing")
}

func TestCheckPricingCoverage_AllCovered(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir, map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "x" }
    model: "real-model"
---
`,
		"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`,
		"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
`,
	})
	pricingPath := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(pricingPath, []byte(`models:
  real-model:
    input: 1.0
    output: 2.0
`), 0o644))

	h := &DoctorHandlers{configDir: dir, pricingPath: pricingPath}
	got := h.checkPricingCoverage()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "pricing entries")
}

func TestCheckAutonomyBudgetGuard_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkAutonomyBudgetGuard()
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no config dir")
}

func TestCheckAutonomyBudgetGuard_AutonomyDisabledSkipped(t *testing.T) {
	dir := t.TempDir()
	// Project has autonomy disabled — must not appear in unguarded list.
	minimalProjectConfigs(t, dir, "")
	h := &DoctorHandlers{configDir: dir}
	got := h.checkAutonomyBudgetGuard()
	assert.Equal(t, "OK", got.Status)
}

func TestCheckAutonomyBudgetGuard_UnguardedProject(t *testing.T) {
	dir := t.TempDir()
	// Project with autonomy enabled, NO budget caps — must WARN.
	minimalProjectConfigs(t, dir, `
autonomy:
  enabled: true
  goal: "do stuff"
`)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkAutonomyBudgetGuard()
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "no $ cap")
	require.NotEmpty(t, got.Items)
	assert.Contains(t, got.Items[0], "p")
}

func TestCheckAutonomyBudgetGuard_GuardedProject(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, `
autonomy:
  enabled: true
  goal: "do stuff"
budget:
  daily_hard_usd: 10.0
`)
	h := &DoctorHandlers{configDir: dir}
	got := h.checkAutonomyBudgetGuard()
	assert.Equal(t, "OK", got.Status)
}

// ---------------------------------------------------------------
// Budget utilisation (config dir branches; DB query path tolerates errors)
// ---------------------------------------------------------------

func TestCheckBudgetUtilisation_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkBudgetUtilisation(t.Context())
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no config dir")
}

func TestCheckBudgetUtilisation_NoBudgetsConfigured(t *testing.T) {
	dir := t.TempDir()
	// Projects without budget caps are skipped — no warning expected.
	minimalProjectConfigs(t, dir, "")
	h := &DoctorHandlers{configDir: dir, db: closedDB(t)}
	got := h.checkBudgetUtilisation(t.Context())
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no projects near a budget cap")
}

func TestCheckBudgetUtilisation_DBErrorTolerated(t *testing.T) {
	dir := t.TempDir()
	// Project HAS a budget cap, so the DB query path fires. Closed DB
	// returns an error, which the check is designed to swallow (it
	// won't page the operator just because the budget query errored —
	// that's a separate health check). Net effect: status OK.
	minimalProjectConfigs(t, dir, `
budget:
  daily_hard_usd: 10.0
  monthly_hard_usd: 100.0
`)
	h := &DoctorHandlers{configDir: dir, db: closedDB(t)}
	got := h.checkBudgetUtilisation(t.Context())
	assert.Equal(t, "OK", got.Status)
}

// ---------------------------------------------------------------
// Orphan worktrees
// ---------------------------------------------------------------

func TestCheckOrphanWorktrees_NoWorkspacesRoot(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkOrphanWorktrees(false)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no workspaces root")
}

func TestCheckOrphanWorktrees_RootNotReadable(t *testing.T) {
	// workspacesRoot set but doesn't exist → scanOrphanWorktrees
	// returns an error → check reports skip with OK (operator-tunable
	// boundary: a missing workspaces root might just mean the daemon
	// hasn't created any worktrees yet, not a real defect).
	h := &DoctorHandlers{workspacesRoot: "/nonexistent/vornik/workspaces"}
	got := h.checkOrphanWorktrees(false)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "not readable")
}

func TestCheckOrphanWorktrees_EmptyRoot(t *testing.T) {
	// Empty workspaces root → no findings, OK with the projects-checked
	// count in the message.
	h := &DoctorHandlers{workspacesRoot: t.TempDir(), db: closedDB(t)}
	got := h.checkOrphanWorktrees(false)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no orphan worktree dirs")
}

func TestCheckOrphanWorktrees_FindsOrphansWithClosedDB(t *testing.T) {
	// Create a project workspace with a stray .worktrees/task_xyz/.
	// With a closed DB, taskStatus returns an error that's NOT
	// sql.ErrNoRows — scanOrphanWorktrees skips silently. So findings
	// should be empty and status OK. This exercises the project-walk
	// path (vs. the bare-empty-root branch above).
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "proj-a", ".worktrees", "task_orphan"), 0o755))
	h := &DoctorHandlers{workspacesRoot: root, db: closedDB(t)}
	got := h.checkOrphanWorktrees(false)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "no orphan worktree dirs across 1 project")
}

// ---------------------------------------------------------------
// External-dependency checks (podman / agent images)
// ---------------------------------------------------------------

func TestCheckPodmanConfig_NotInPath(t *testing.T) {
	// Override PATH so exec.LookPath("podman") fails. This is the
	// dominant production-environment failure mode (podman uninstalled
	// or removed from PATH), which the check exists to catch.
	t.Setenv("PATH", t.TempDir())
	h := &DoctorHandlers{}
	got := h.checkPodmanConfig(t.Context())
	assert.Equal(t, "podman_config", got.Name)
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "podman not found in PATH")
}

func TestCheckAgentImages_NoConfigDir(t *testing.T) {
	h := &DoctorHandlers{}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "agent_images", got.Name)
	assert.Equal(t, "OK", got.Status)
	assert.Contains(t, got.Message, "skipping image check")
}

func TestCheckAgentImages_LoadFailure(t *testing.T) {
	// configDir set but the swarms dir contains malformed SWARM.md
	// — LoadSwarms returns an error that surfaces as ERROR. (Stale
	// `.yaml` files are silently ignored post-YAML-removal, so the
	// failure trigger now lives on the MD frontmatter parse path.)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "swarms"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "broken.md"), []byte("not even a frontmatter marker"), 0o644))
	h := &DoctorHandlers{configDir: dir}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "ERROR", got.Status)
	assert.Contains(t, got.Message, "failed to load swarms")
}

func TestCheckAgentImages_PodmanMissing(t *testing.T) {
	dir := t.TempDir()
	minimalProjectConfigs(t, dir, "")
	t.Setenv("PATH", t.TempDir()) // remove podman
	h := &DoctorHandlers{configDir: dir}
	got := h.checkAgentImages(t.Context())
	assert.Equal(t, "WARNING", got.Status)
	assert.Contains(t, got.Message, "podman not found")
}

// ---------------------------------------------------------------
// RunDoctor HTTP handler — end-to-end shape
// ---------------------------------------------------------------

func TestRunDoctor_RejectsNonPOST(t *testing.T) {
	h := NewDoctorHandlers(closedDB(t))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/doctor", nil)
	h.RunDoctor(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestRunDoctor_ReturnsReportShape(t *testing.T) {
	// With a closed DB + no config dir + no podman, most checks error
	// or skip with informational status. The handler should still
	// produce a well-formed report.
	t.Setenv("PATH", t.TempDir())
	h := NewDoctorHandlers(closedDB(t))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor", nil)
	h.RunDoctor(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var report DoctorReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.NotEmpty(t, report.Timestamp)
	assert.NotEmpty(t, report.Checks, "must contain at least one check")
	assert.NotEmpty(t, report.Summary)

	// Every check must have a non-empty name and a known status.
	for _, c := range report.Checks {
		assert.NotEmpty(t, c.Name)
		assert.Contains(t, []string{"OK", "WARNING", "ERROR"}, c.Status, "check %q: invalid status %q", c.Name, c.Status)
	}
}

func TestRunDoctor_FixQueryParamParsed(t *testing.T) {
	// fix=true is a URL query parameter — confirm parsing handles
	// both forms. We don't have a way to verify a fix was applied
	// without a real DB, but the endpoint must accept the parameter
	// and produce a valid report shape rather than rejecting it.
	t.Setenv("PATH", t.TempDir())
	h := NewDoctorHandlers(closedDB(t))

	for _, q := range []string{"?fix=true", "?fix=false", ""} {
		t.Run("query="+q, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor"+q, nil)
			h.RunDoctor(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			var report DoctorReport
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
			assert.NotEmpty(t, report.Summary)
		})
	}
}
