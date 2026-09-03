package autonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	vornikapi "vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/backlogfile"
	"vornik.io/vornik/internal/config"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

// TestBacklogDepositToAutonomyTick_Integration is the single sqlite
// integration test https://docs.vornik.io
// design.md §Testing promises: "deposit -> BACKLOG.md line -> backlog
// tick -> task created with workflow_id: backlog-item."
//
// Why this test exists (final-review finding M1): the prior coverage of
// this pipeline was two halves that never touched — internal/api's
// deposit tests never ran tickBacklog, and this package's
// TestTickBacklog_StampsForgeJob_SlugAndTitleFromCleanedItem
// (backlog_forge_test.go) hand-typed a BACKLOG.md line that MIRRORS
// renderBacklogDepositLine's format by eye. If the renderer's line
// shape (internal/api/backlog_deposit_handlers.go's
// renderBacklogDepositLine) ever drifted from what
// parseBacklogItemKindTitle (same file) or backlogItemTitle (this
// package) expect to parse, no existing test would fail — the
// hand-typed fixture would just keep matching the old format.
//
// This test closes that gap by driving the REAL deposit HTTP handler
// (vornikapi.Server.BacklogDeposit) to produce the BACKLOG.md line,
// then running the REAL tickBacklog (this package, same process) against
// the file it wrote. It lives in package autonomy (not internal/api)
// because tickBacklog is unexported and internal/api has no reason to
// import internal/autonomy (constructing the reverse dependency here,
// test-file-only, does not create an import cycle: internal/api has no
// production dependency on internal/autonomy).
func TestBacklogDepositToAutonomyTick_Integration(t *testing.T) {
	ctx := context.Background()

	// --- registry: one project wired for Mode="backlog" with an
	// explicit workflow_id, a swarm satisfying that workflow's role,
	// and GitHub outbound creds so the forge_job stamp also gets
	// exercised (the M1 assertion below checks its Slug/Title too).
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "swarms"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "workflows"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "project.yaml"), []byte(`
projectId: project-1
displayName: Project
swarmId: swarm-1
defaultWorkflowId: wf-1
defaultPriority: 42
autonomy:
  mode: backlog
  workflow_id: backlog-item
github:
  app_id: 1
  installation_id: 2
  private_key_path: /k.pem
  repo: o/r
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte(`---
swarmId: swarm-1
roles:
  - name: worker
    runtime:
      image: test-image
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte(`---
workflowId: wf-1
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "backlog-item.md"), []byte(`---
workflowId: backlog-item
entrypoint: run
steps:
  run:
    type: agent
    prompt: "do work"
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---
`), 0o644))

	reg := registry.New()
	require.NoError(t, reg.Load(root))
	project := reg.GetProject("project-1")
	require.NotNil(t, project, "project must load from the fixture registry")

	workspaceRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceRoot, "project-1"), 0o755))
	backlogPath := filepath.Join(workspaceRoot, "project-1", "BACKLOG.md")

	// A real sqlite-backed TaskRepository (design doc: "Integration
	// (single test, sqlite)"), shared by the deposit handler's guard
	// stack and the autonomy tick's create_task path.
	db, err := sqlite.Connect(ctx, sqlite.DefaultConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, db.Migrate(ctx))
	taskRepo := sqlite.NewTaskRepository(db.DB)

	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)

	// The shared backlogfile.Store instance the design doc requires:
	// "MUST be the same instance the HTTP deposit endpoint holds
	// (Container.BacklogStore) so the two share per-project locks."
	store := backlogfile.NewStore()

	server := vornikapi.NewServer(
		vornikapi.WithLogger(zerolog.Nop()),
		vornikapi.WithProjectRegistry(reg),
		vornikapi.WithConfig(&config.Config{Runtime: config.RuntimeConfig{ProjectWorkspacePath: workspaceRoot}}),
		vornikapi.WithBacklogStore(store),
		vornikapi.WithSecrets(det, nil),
		vornikapi.WithTaskRepository(taskRepo),
	)

	// --- Step 1: deposit via the REAL HTTP handler. This is the exact
	// wire shape internal/api/backlog_deposit_handlers.go's
	// backlogDepositRequest expects; no render function is
	// hand-mirrored here.
	depositBody, err := json.Marshal(map[string]any{
		"project_id": "project-1",
		"task_id":    "task-1",
		"kind":       "bug",
		"title":      "cache eviction thrashes under load",
		"detail":     "the LRU evicts hot keys during traffic spikes",
		"evidence":   "metrics dashboard panel 7",
	})
	require.NoError(t, err)
	depositReq := httptest.NewRequest(http.MethodPost, "/api/v1/internal/backlog-deposit", bytes.NewReader(depositBody))
	rec := httptest.NewRecorder()
	server.BacklogDeposit(rec, depositReq)
	require.Equal(t, http.StatusOK, rec.Code)

	var depositResp struct {
		Status string `json:"status"`
		Item   string `json:"item"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &depositResp))
	require.Equal(t, "accepted", depositResp.Status, "deposit must be accepted for the tick to have anything to consume")
	require.Contains(t, depositResp.Item, "cache eviction thrashes under load")

	// The deposit lands unconditionally as "- [?]" (proposed) — verify
	// that landed on disk before promoting it, so a future regression
	// in the unconditional-marker contract fails here, not silently.
	raw, err := os.ReadFile(backlogPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), "- [?] "+depositResp.Item)

	// --- Step 2: operator approval. Per the design doc ("deposits
	// always land - [?]; the operator flips to - [ ] — the gate is
	// unconditional this slice, no config knob"), promotion is a manual
	// file edit — internal/backlogfile.Store exposes no Promote/Approve
	// method (by design; see its marker-grammar doc comment). Simulate
	// exactly that hand edit.
	promoted := strings.Replace(string(raw), "- [?] "+depositResp.Item, "- [ ] "+depositResp.Item, 1)
	require.NoError(t, os.WriteFile(backlogPath, []byte(promoted), 0o600))

	// --- Step 3: run the REAL backlog tick, sharing the same registry,
	// workspace, backlogfile.Store, and taskRepo the deposit used.
	mgr := New(nil, reg, taskRepo, nil,
		WithWorkspacePath(workspaceRoot),
		WithBacklogStore(store),
	)
	require.NoError(t, mgr.tickBacklog(ctx, project, time.Now()))

	// --- Assertions: a task was created, carrying workflow_id
	// "backlog-item" and a forge_job whose Slug/Title derive from the
	// deposited title — the load-bearing property being pinned is that
	// none of this required hand-typing the BACKLOG.md line format; it
	// came from the real render -> real parse round trip.
	tasks, err := taskRepo.List(ctx, persistence.TaskFilter{ProjectID: &project.ID, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, tasks, 1, "the promoted item must have been dispatched as exactly one task")

	task := tasks[0]
	require.NotNil(t, task.WorkflowID)
	require.Equal(t, "backlog-item", *task.WorkflowID)

	var payload struct {
		ForgeJob *forgeapi.ForgeJob `json:"forge_job"`
	}
	require.NoError(t, json.Unmarshal(task.Payload, &payload))
	require.NotNil(t, payload.ForgeJob, "project.GitHub is Enabled() with a resolvable outbound repo — a forge job must be stamped")
	require.Equal(t, "cache eviction thrashes under load", payload.ForgeJob.Title,
		"forge_job.Title must derive from the deposited title via the REAL render+parse round trip, not a hand-typed fixture")
	require.Equal(t, "cache-eviction-thrashes-under-load", payload.ForgeJob.Slug)
	require.Equal(t, "backlog", payload.ForgeJob.Kind)
	require.Equal(t, "o/r", payload.ForgeJob.Repo)

	// The dispatched line must now be marked "- [~]" in-flight with a
	// "(task: ...)" annotation (dispatch stamps [~], not [x]; the reconciler
	// flips it on the task's terminal — LLD 2026-07-12-backlog-success-
	// terminal-stamp).
	final, err := os.ReadFile(backlogPath)
	require.NoError(t, err)
	require.Contains(t, string(final), "- [~] "+depositResp.Item+" (task: "+task.ID+")")
}
