package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/registry"
)

// recordingArtifactStore captures every artifact the executor tries to
// persist so tests can assert which source paths made it through the
// safepath filter.
type recordingArtifactStore struct {
	mu          sync.Mutex
	seenNames   []string
	seenSources []string
}

func (r *recordingArtifactStore) Store(_ context.Context, _, _, _, name, sourcePath string) (*persistence.Artifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seenNames = append(r.seenNames, name)
	r.seenSources = append(r.seenSources, sourcePath)
	return &persistence.Artifact{Name: name, StoragePath: sourcePath}, nil
}

// Retrieve satisfies the wider ArtifactStore interface that gained
// a read method when the executor started routing memory-ingest
// reads through the backend-aware Store. Tests in this file don't
// exercise the read path, so the stub returns ErrNotImplemented to
// surface accidental use.
func (r *recordingArtifactStore) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("recordingArtifactStore: Retrieve not implemented in this test")
}

// TestRecordLLMUsageFromResult covers the happy path, a missing-usage
// result (old agent image), and malformed JSON.
func TestRecordLLMUsageFromResult(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	t.Run("records usage when present", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		e := &Executor{metrics: m, logger: zerolog.Nop()}

		body := []byte(`{"usage":{"prompt_tokens":1200,"completion_tokens":450,"cache_creation_tokens":100,"cache_read_tokens":800,"iterations":7}}`)
		e.recordLLMUsageFromResult(context.Background(), task, exec, "step_1", "coder", "qwen-coder", body)

		assert.Equal(t, 1200.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen-coder", "prompt")))
		assert.Equal(t, 450.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen-coder", "completion")))
		assert.Equal(t, 100.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen-coder", "cache_creation")))
		assert.Equal(t, 800.0, testutil.ToFloat64(m.LLMTokensTotal.WithLabelValues("p1", "coder", "qwen-coder", "cache_read")))
		assert.Equal(t, 7.0, testutil.ToFloat64(m.LLMIterationsTotal.WithLabelValues("p1", "coder", "qwen-coder")))
	})

	t.Run("silent on missing usage block (old agent image)", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		e := &Executor{metrics: m, logger: zerolog.Nop()}

		body := []byte(`{"status":"COMPLETED","message":"done"}`)
		require.NotPanics(t, func() {
			e.recordLLMUsageFromResult(context.Background(), task, exec, "step_1", "coder", "m", body)
		})
	})

	t.Run("silent on malformed JSON", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		e := &Executor{metrics: m, logger: zerolog.Nop()}

		require.NotPanics(t, func() {
			e.recordLLMUsageFromResult(context.Background(), task, exec, "step_1", "coder", "m", []byte(`{not valid json`))
		})
	})

	t.Run("emits cost when pricing table configured", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		dir := t.TempDir()
		path := filepath.Join(dir, "pricing.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`
models:
  qwen-coder:
    input: 0.15
    output: 0.60
`), 0o644))
		table, err := pricing.Load(path)
		require.NoError(t, err)

		e := &Executor{metrics: m, pricing: table, logger: zerolog.Nop()}

		body := []byte(`{"usage":{"prompt_tokens":1000000,"completion_tokens":500000,"iterations":3}}`)
		e.recordLLMUsageFromResult(context.Background(), task, exec, "step_1", "coder", "qwen-coder", body)

		// 1M prompt × $0.15 + 500K completion × $0.60 = $0.15 + $0.30 = $0.45
		assert.InDelta(t, 0.45, testutil.ToFloat64(m.LLMCostUSDTotal.WithLabelValues("p1", "coder", "qwen-coder")), 1e-9)
		assert.InDelta(t, 0.45, testutil.ToFloat64(m.ModelCostUSDTotal.WithLabelValues("coder", "qwen-coder")), 1e-9)
	})

	t.Run("persists row when usage repo is wired", func(t *testing.T) {
		registry := prometheus.NewRegistry()
		m := NewMetrics(registry)
		repo := newStubLLMUsageRepo()

		dir := t.TempDir()
		path := filepath.Join(dir, "pricing.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`
models:
  qwen-coder: { input: 0.15, output: 0.60 }
`), 0o644))
		table, err := pricing.Load(path)
		require.NoError(t, err)

		e := &Executor{metrics: m, pricing: table, llmUsageRepo: repo, logger: zerolog.Nop()}
		body := []byte(`{"usage":{"prompt_tokens":1000000,"completion_tokens":500000,"cache_creation_tokens":100000,"cache_read_tokens":200000,"iterations":3}}`)
		e.recordLLMUsageFromResult(context.Background(), task, exec, "step_42", "coder", "qwen-coder", body)

		require.Len(t, repo.rows, 1)
		row := repo.rows[0]
		assert.Equal(t, "p1", row.ProjectID)
		require.NotNil(t, row.TaskID)
		assert.Equal(t, "t1", *row.TaskID)
		require.NotNil(t, row.ExecutionID)
		assert.Equal(t, "e1", *row.ExecutionID)
		assert.Equal(t, "step_42", row.StepID)
		assert.Equal(t, "coder", row.Role)
		assert.Equal(t, "qwen-coder", row.Model)
		assert.Equal(t, int64(1_000_000), row.PromptTokens)
		assert.Equal(t, int64(500_000), row.CompletionTokens)
		assert.Equal(t, int64(100_000), row.CacheCreationTokens)
		assert.Equal(t, int64(200_000), row.CacheReadTokens)
		assert.Equal(t, 3, row.Iterations)
		assert.InDelta(t, 0.47175, row.CostUSD, 1e-9)
		assert.Equal(t, persistence.TaskLLMUsageSourceWorkflowStep, row.Source)
		assert.Nil(t, row.SessionID)
	})
}

// stubLLMUsageRepo is a trivial in-memory TaskLLMUsageRepository for tests.
type stubLLMUsageRepo struct {
	rows []*persistence.TaskLLMUsage
}

func newStubLLMUsageRepo() *stubLLMUsageRepo { return &stubLLMUsageRepo{} }

func (s *stubLLMUsageRepo) Record(_ context.Context, u *persistence.TaskLLMUsage) error {
	s.rows = append(s.rows, u)
	return nil
}
func (s *stubLLMUsageRepo) Upsert(_ context.Context, u *persistence.TaskLLMUsage) error {
	// Test stub: replace any existing row with the same ID;
	// otherwise append. Mirrors the postgres ON CONFLICT (id)
	// DO UPDATE semantics so callers exercising the streaming
	// path see the same single-row outcome they'd see in prod.
	for i, r := range s.rows {
		if r.ID == u.ID {
			s.rows[i] = u
			return nil
		}
	}
	s.rows = append(s.rows, u)
	return nil
}
func (s *stubLLMUsageRepo) List(_ context.Context, _ persistence.TaskLLMUsageFilter) ([]*persistence.TaskLLMUsage, error) {
	return s.rows, nil
}
func (s *stubLLMUsageRepo) SumCostByProject(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	var total float64
	for _, r := range s.rows {
		total += r.CostUSD
	}
	return total, nil
}
func (s *stubLLMUsageRepo) SumCost(_ context.Context, _, _ time.Time) (float64, error) {
	var total float64
	for _, r := range s.rows {
		total += r.CostUSD
	}
	return total, nil
}
func (s *stubLLMUsageRepo) AggregateByRoleModel(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.RoleModelSpend, error) {
	return nil, nil
}

// Spend deep-dive aggregations — empty stubs to satisfy the
// interface; the executor doesn't exercise these.
func (s *stubLLMUsageRepo) AggregateByProject(_ context.Context, _, _ time.Time, _ int) ([]persistence.ProjectSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) AggregateBySource(_ context.Context, _, _ time.Time, _ string) ([]persistence.SourceSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TimeSeriesByDay(_ context.Context, _, _ time.Time, _ string) ([]persistence.DailySpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TopTasks(_ context.Context, _, _ time.Time, _ int, _ string) ([]persistence.TaskSpend, error) {
	return nil, nil
}
func (s *stubLLMUsageRepo) TaskCostBreakdown(_ context.Context, _ string) ([]persistence.StepSpend, error) {
	return nil, nil
}

func TestEffectiveRoleModel(t *testing.T) {
	e := &Executor{config: DefaultConfig()}
	e.config.AgentLLMEnv = map[string]string{"VORNIK_LLM_MODEL": "global-model"}

	assert.Equal(t, "role-model", e.effectiveRoleModel(&registry.SwarmRole{
		Model: "role-model",
		Runtime: registry.SwarmRoleRuntime{
			EnvVars: map[string]string{"VORNIK_LLM_MODEL": "env-model"},
		},
	}))
	assert.Equal(t, "env-model", e.effectiveRoleModel(&registry.SwarmRole{
		Runtime: registry.SwarmRoleRuntime{
			EnvVars: map[string]string{"VORNIK_LLM_MODEL": "env-model"},
		},
	}))
	assert.Equal(t, "global-model", e.effectiveRoleModel(&registry.SwarmRole{}))
	assert.Equal(t, "", e.effectiveRoleModel(nil))
}

func TestInjectCostEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  fast-model:
    input: 0.07
    output: 0.40
`), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)

	env := map[string]string{}
	injectCostEnv(env, table, "fast-model")
	assert.Equal(t, "0.07", env["VORNIK_LLM_COST_INPUT_PER_M"])
	assert.Equal(t, "0.4", env["VORNIK_LLM_COST_OUTPUT_PER_M"])

	// Unknown model falls back to default (zero in this table) — agent logs 0.0000.
	env2 := map[string]string{}
	injectCostEnv(env2, table, "unknown")
	assert.Equal(t, "0", env2["VORNIK_LLM_COST_INPUT_PER_M"])

	// Nil table is a no-op (doesn't panic, doesn't write).
	env3 := map[string]string{"other": "value"}
	injectCostEnv(env3, nil, "fast-model")
	_, hasInput := env3["VORNIK_LLM_COST_INPUT_PER_M"]
	assert.False(t, hasInput)
	assert.Equal(t, "value", env3["other"])

	// Empty model is a no-op.
	env4 := map[string]string{}
	injectCostEnv(env4, table, "")
	assert.Empty(t, env4)
}

func TestClassifyStepOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, "cancelled", classifyStepOutcome(ctx, context.Canceled))

	deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer deadlineCancel()
	assert.Equal(t, "timeout", classifyStepOutcome(deadlineCtx, context.DeadlineExceeded))

	assert.Equal(t, "success", classifyStepOutcome(context.Background(), nil))
	assert.Equal(t, "timeout", classifyStepOutcome(context.Background(), errors.New("container wait timeout: deadline exceeded")))
	assert.Equal(t, "failed", classifyStepOutcome(context.Background(), errors.New("container exited with code 1")))
}

// TestPersistArtifactsRefusesEscapingSymlink locks in the symlink-escape
// defence added to persistArtifacts. If the agent container plants a
// symlink inside its workspace that points at a sensitive host file, the
// scanner must drop it rather than persisting the link target.
func TestPersistArtifactsRefusesEscapingSymlink(t *testing.T) {
	// A directory outside the workspace that the agent should not be able
	// to reach via the artifact scanner.
	outside := t.TempDir()
	secret := filepath.Join(outside, "host-secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("do not read"), 0o600))

	workspace := t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))

	// One legitimate artifact in the canonical output directory.
	goodPath := filepath.Join(outDir, "plan.md")
	require.NoError(t, os.WriteFile(goodPath, []byte("# plan"), 0o644))

	// A malicious symlink also in the output directory, pointing outside
	// the workspace. The executor must refuse this.
	require.NoError(t, os.Symlink(secret, filepath.Join(outDir, "leak.txt")))

	// A workspace-root symlink escaping too.
	require.NoError(t, os.Symlink(secret, filepath.Join(workspace, "side-leak.txt")))

	// And a legitimate file at the workspace root (agent-created via
	// file_write) — this must still be picked up.
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("hi"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{
		artifactStore: store,
		logger:        zerolog.Nop(),
	}

	// stepStart in the past — file mtimes are "now", so the project-
	// persisted mtime gate (which doesn't apply here anyway, there's
	// no project/ subdir) won't reject the legitimate files.
	stepStart := time.Now().Add(-5 * time.Second)
	// No project tree in this test — effectiveProjectDir is "".
	_, err := e.persistArtifacts(context.Background(), "exec-1", "proj-1", "task-1", workspace, "", stepStart, nil)
	require.NoError(t, err)

	// Accept plan.md and notes.txt; refuse anything that resolved to the
	// outside secret.
	for _, src := range store.seenSources {
		require.False(t,
			filepath.Clean(src) == filepath.Clean(secret),
			"executor persisted a path resolving to the outside secret: %s", src)
	}
	// As of 2026-05-16 the executor's persistArtifacts disambiguates
	// every harvested name with a `-YYYYMMDD-XXXX` suffix so multi-task
	// collisions can't pollute memory retrieval. Assert on the
	// pre-suffix stem+ext rather than the literal filename so the
	// test isn't time-sensitive.
	require.Len(t, store.seenNames, 2, "expected exactly two legitimate files persisted; got %v", store.seenNames)
	gotStems := make(map[string]bool, 2)
	for _, name := range store.seenNames {
		stem, _ := splitArtifactStem(name)
		// Strip the disambig suffix off the stem so we compare
		// the operator-visible logical name. The suffix shape is
		// fixed (-YYYYMMDD-XXXX); strip 14 chars when present.
		if len(stem) > 14 && stem[len(stem)-14] == '-' && stem[len(stem)-5] == '-' {
			stem = stem[:len(stem)-14]
		}
		_, ext := splitArtifactStem(name)
		gotStems[stem+ext] = true
	}
	require.True(t, gotStems["plan.md"], "expected plan.md (any disambig variant); got %v", store.seenNames)
	require.True(t, gotStems["notes.txt"], "expected notes.txt (any disambig variant); got %v", store.seenNames)
}

// TestPersistArtifacts_ReturnsHarvestedOutputs is part of the task e9a5
// fix. persistArtifacts now returns the harvested {name, sourcePath}
// entries so the workflow loop can re-stage them into the NEXT step's
// ephemeral container. The names are the ORIGINAL on-disk names (NOT the
// disambiguated store names — the next step's role reads by the logical
// filename), and the sourcePaths are the durable store StoragePath
// returned by Store(), which lives under an allowed staging root.
func TestPersistArtifacts_ReturnsHarvestedOutputs(t *testing.T) {
	workspace := t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "research.md"), []byte("# r"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	stepStart := time.Now().Add(-5 * time.Second)
	out, err := e.persistArtifacts(context.Background(), "exec-out", "proj-1", "task-1", workspace, "", stepStart, nil)
	require.NoError(t, err)

	require.Len(t, out, 1, "expected one harvested output; got %v", out)
	// Original logical name, not the disambiguated store name.
	require.Equal(t, "research.md", out[0]["name"])
	// sourcePath is the store StoragePath. recordingArtifactStore echoes
	// the sourcePath it was handed back as StoragePath, so the returned
	// entry's sourcePath must equal what the store recorded.
	require.NotEmpty(t, out[0]["sourcePath"])
	require.Equal(t, store.seenSources[0], out[0]["sourcePath"])
}

// TestPersistArtifacts_ReturnsRepoFallbackOutputs covers the e9a5 return
// contract on the artifactRepo-only path (no store wired): the returned
// sourcePath is the harvested file's own host path (there is no store
// StoragePath to use), and the original name is preserved.
func TestPersistArtifacts_ReturnsRepoFallbackOutputs(t *testing.T) {
	workspace := t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	src := filepath.Join(outDir, "research.md")
	require.NoError(t, os.WriteFile(src, []byte("# r"), 0o644))

	repo := &stubArtifactRepo{}
	e := &Executor{artifactRepo: repo, logger: zerolog.Nop()}

	out, err := e.persistArtifacts(context.Background(), "exec-out", "proj-1", "task-1", workspace, "", time.Now().Add(-time.Second), nil)
	require.NoError(t, err)

	require.Len(t, out, 1)
	require.Equal(t, "research.md", out[0]["name"])
	require.Equal(t, filepath.Clean(src), filepath.Clean(out[0]["sourcePath"]))
}

// TestPersistArtifacts_RegistersProjectPersistedTree locks in the 2026-05-18
// fix: writer agents producing PDF/DOCX deliverables under
// <workspace>/project/artifacts/out/ must get DB artifact rows just like
// files under the ephemeral artifacts/out/ tree. Pre-fix the project-
// persisted files lived on disk but never made it into artifact_repo,
// so the Telegram document fan-out (sendArtifactsToWatchers) and the
// /ui/projects/<id>/artifacts list both rendered empty even though the
// PDF was right there on the host filesystem.
func TestPersistArtifacts_RegistersProjectPersistedTree(t *testing.T) {
	workspace := t.TempDir()
	projectOutDir := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(projectOutDir, 0o755))

	// Fresh-mtime PDF in the project-persisted tree.
	pdfPath := filepath.Join(projectOutDir, "deliverable.pdf")
	require.NoError(t, os.WriteFile(pdfPath, []byte("%PDF-1.7 fake"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{
		artifactStore: store,
		logger:        zerolog.Nop(),
	}

	stepStart := time.Now().Add(-5 * time.Second)
	// effectiveProjectDir mirrors the worktree path (or, in fallback
	// mode, the project's persistent root). Tests historically set
	// up their fixture at `workspace/project/...` so pass the
	// matching `workspace/project` here.
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, err := e.persistArtifacts(context.Background(), "exec-pdf", "proj-1", "task-1", workspace, effectiveProjectDir, stepStart, nil)
	require.NoError(t, err)

	require.Len(t, store.seenNames, 1, "expected exactly one artifact registered; got %v", store.seenNames)
	// Source path must resolve into the project-persisted tree, not
	// the ephemeral artifacts/out/.
	require.Equal(t, filepath.Clean(pdfPath), filepath.Clean(store.seenSources[0]))
	// Logical name (post-disambig) must end in .pdf so downstream
	// MIME detection and the Telegram document fan-out treat it as
	// a binary deliverable rather than a markdown response.
	require.True(t, strings.HasSuffix(store.seenNames[0], ".pdf"),
		"expected .pdf extension preserved through disambig; got %q", store.seenNames[0])
}

// TestPersistArtifacts_SkipsProjectPersistedFilesPredatingExecution proves
// the mtime gate: the project-persisted tree survives across tasks, so a
// `foo.pdf` written by a prior task and never modified since must NOT be
// re-registered as the current task's artifact. Without this filter every
// new task would scoop up every stale deliverable under the tree and
// silently re-register them, multiplying artifact rows on each run.
func TestPersistArtifacts_SkipsProjectPersistedFilesPredatingExecution(t *testing.T) {
	workspace := t.TempDir()
	projectOutDir := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(projectOutDir, 0o755))

	// Old file from a prior task — mtime set well before stepStart.
	stalePath := filepath.Join(projectOutDir, "stale-from-prior-task.pdf")
	require.NoError(t, os.WriteFile(stalePath, []byte("%PDF-1.7 stale"), 0o644))
	oldTime := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(stalePath, oldTime, oldTime))

	// Fresh file from the current step — must be registered.
	freshPath := filepath.Join(projectOutDir, "fresh-this-run.pdf")
	require.NoError(t, os.WriteFile(freshPath, []byte("%PDF-1.7 fresh"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{
		artifactStore: store,
		logger:        zerolog.Nop(),
	}

	// stepStart is "now"; stale's mtime (2h ago) is comfortably before
	// the cutoff, fresh's mtime (just now) is after.
	stepStart := time.Now()
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, err := e.persistArtifacts(context.Background(), "exec-pdf", "proj-1", "task-1", workspace, effectiveProjectDir, stepStart, nil)
	require.NoError(t, err)

	require.Len(t, store.seenNames, 1, "expected only the fresh file; got %v", store.seenNames)
	require.Equal(t, filepath.Clean(freshPath), filepath.Clean(store.seenSources[0]),
		"expected the fresh file's path, not the stale one")
}

// stubArtifactRepo records every Create call so tests can assert
// against the persisted Artifact rows when the executor takes the
// fallback (no artifactStore configured) code path.
type stubArtifactRepo struct {
	rows []*persistence.Artifact
}

func (s *stubArtifactRepo) Create(_ context.Context, a *persistence.Artifact) error {
	s.rows = append(s.rows, a)
	return nil
}
func (s *stubArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepo) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return s.rows, nil
}

// TestPersistArtifacts_UsesArtifactRepoFallback exercises the
// secondary persistence path: when the executor has no artifactStore
// wired but does have an artifactRepo, files become persistence.Artifact
// rows directly. The fallback is the same code path the older vornik
// builds used before the artifact-store abstraction landed; it must
// stay green so service.Container.initExecutor can drop the store
// in degraded-deploy scenarios without losing artifact registration.
func TestPersistArtifacts_UsesArtifactRepoFallback(t *testing.T) {
	workspace := t.TempDir()
	projectOutDir := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(projectOutDir, 0o755))
	pdf := filepath.Join(projectOutDir, "x.pdf")
	require.NoError(t, os.WriteFile(pdf, []byte("%PDF"), 0o644))

	repo := &stubArtifactRepo{}
	e := &Executor{artifactRepo: repo, logger: zerolog.Nop()}

	stepStart := time.Now().Add(-5 * time.Second)
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, perr := e.persistArtifacts(context.Background(), "exec_42", "proj-1", "task-1", workspace, effectiveProjectDir, stepStart, nil)
	require.NoError(t, perr)
	require.Len(t, repo.rows, 1)
	row := repo.rows[0]
	require.Equal(t, "proj-1", row.ProjectID)
	require.NotNil(t, row.TaskID)
	require.Equal(t, "task-1", *row.TaskID)
	require.NotNil(t, row.ExecutionID)
	require.Equal(t, "exec_42", *row.ExecutionID)
	require.Equal(t, persistence.ArtifactClassOutput, row.ArtifactClass)
	require.Equal(t, filepath.Clean(pdf), filepath.Clean(row.StoragePath))
	require.True(t, strings.HasSuffix(row.Name, ".pdf"), "ext preserved through disambig; got %q", row.Name)
}

// TestPersistArtifacts_SkipsDirectoriesUnderProjectPersistedTree pins
// the IsDir() guard in source #3. A subdirectory accidentally placed
// under project/artifacts/out/ (e.g. an agent's intermediate scratch
// folder) must be skipped — registering a directory as an artifact
// row would fail downstream when the document fan-out tries to open
// it as a file.
func TestPersistArtifacts_SkipsDirectoriesUnderProjectPersistedTree(t *testing.T) {
	workspace := t.TempDir()
	projectOutDir := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(projectOutDir, 0o755))

	// Subdirectory that must be ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(projectOutDir, "subdir"), 0o755))

	// One legitimate file alongside it.
	filePath := filepath.Join(projectOutDir, "ok.pdf")
	require.NoError(t, os.WriteFile(filePath, []byte("%PDF"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	stepStart := time.Now().Add(-5 * time.Second)
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, perr := e.persistArtifacts(context.Background(), "exec-d", "proj-1", "task-1", workspace, effectiveProjectDir, stepStart, nil)
	require.NoError(t, perr)
	require.Len(t, store.seenSources, 1, "subdirectory must not be registered; got %v", store.seenSources)
	require.Equal(t, filepath.Clean(filePath), filepath.Clean(store.seenSources[0]))
}

// TestPersistArtifacts_ZeroStepStartDisablesMtimeGate exercises the
// stepStart.IsZero() short-circuit. A caller that passes a zero time
// (e.g. tests, or a code path that hasn't captured a step anchor)
// gets the gate disabled so files of any mtime under the project
// tree are registered. The mtime gate is opt-in via a non-zero
// stepStart — without this branch a zero time would silently filter
// everything.
func TestPersistArtifacts_ZeroStepStartDisablesMtimeGate(t *testing.T) {
	workspace := t.TempDir()
	projectOutDir := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(projectOutDir, 0o755))

	old := filepath.Join(projectOutDir, "old.pdf")
	require.NoError(t, os.WriteFile(old, []byte("%PDF"), 0o644))
	oldTime := time.Now().Add(-24 * time.Hour)
	require.NoError(t, os.Chtimes(old, oldTime, oldTime))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	// Zero time disables the gate — old file still registered.
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, perr := e.persistArtifacts(context.Background(), "exec-z", "proj-1", "task-1", workspace, effectiveProjectDir, time.Time{}, nil)
	require.NoError(t, perr)
	require.Len(t, store.seenSources, 1, "zero stepStart must disable mtime gate; got %v", store.seenSources)
}

// TestPersistArtifacts_DisambiguatesAcrossPaths covers the dedup edge
// case: a file named X.md exists in BOTH the ephemeral artifacts/out/
// AND the project-persisted project/artifacts/out/ tree. Disambiguation
// gives each its own `{stem}-YYYYMMDD-XXXX{ext}` suffix tied to
// execution_id, but both copies land in this single run. The function's
// contract is "register everything we found"; the disambiguator
// resolves the name collision so two rows exist with distinct names
// and distinct storage paths.
func TestPersistArtifacts_DisambiguatesAcrossPaths(t *testing.T) {
	workspace := t.TempDir()
	ephemeralOut := filepath.Join(workspace, "artifacts", "out")
	projectOut := filepath.Join(workspace, "project", "artifacts", "out")
	require.NoError(t, os.MkdirAll(ephemeralOut, 0o755))
	require.NoError(t, os.MkdirAll(projectOut, 0o755))

	// Same logical name under both trees.
	ephemeralPath := filepath.Join(ephemeralOut, "research.md")
	require.NoError(t, os.WriteFile(ephemeralPath, []byte("# ephemeral"), 0o644))
	projectPath := filepath.Join(projectOut, "research.md")
	require.NoError(t, os.WriteFile(projectPath, []byte("# project"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{
		artifactStore: store,
		logger:        zerolog.Nop(),
	}

	stepStart := time.Now().Add(-5 * time.Second)
	effectiveProjectDir := filepath.Join(workspace, "project")
	_, err := e.persistArtifacts(context.Background(), "exec-disambig", "proj-1", "task-1", workspace, effectiveProjectDir, stepStart, nil)
	require.NoError(t, err)

	// Both copies should be registered. With the same executionID and
	// same UTC date the disambig'd names collide on the suffix, but the
	// storage paths are distinct — that's enough for the artifact_repo
	// to keep them as separate rows. (If the operator cares about name
	// uniqueness they can be teased apart by storage path or, for the
	// real production path, by per-execution disambiguation that uses
	// fresh mtimes inside the format.)
	require.Len(t, store.seenSources, 2, "expected both copies registered; got %v", store.seenSources)
	gotPaths := make(map[string]bool, 2)
	for _, src := range store.seenSources {
		gotPaths[filepath.Clean(src)] = true
	}
	require.True(t, gotPaths[filepath.Clean(ephemeralPath)], "expected ephemeral path registered; got %v", store.seenSources)
	require.True(t, gotPaths[filepath.Clean(projectPath)], "expected project-persisted path registered; got %v", store.seenSources)
}

// TestPersistArtifacts_ProjectDirOutsideWorkspace pins the 2026-05-18
// regression fix: in production the project-persisted dir is the
// worktree path (e.g. /var/lib/vornik/workspaces/<proj>/.worktrees/
// <task>) which has nothing to do with workspaceDir's path (a
// per-execution /tmp/vornik-exec-* directory). Pre-fix the function
// walked `workspaceDir/project/artifacts/out` on the host filesystem
// — but that subdirectory only carries the worktree's content via a
// bind mount inside the container's namespace, NOT on the host. So
// every PDF/DOCX/HTML the writer dropped into the worktree's
// artifacts/out was invisible to persistArtifacts, the artifacts UI
// listing rendered empty, and Telegram never got the deliverable
// links. This test passes a worktree path that is NOT a subdir of
// workspaceDir and asserts the file is still found and registered.
func TestPersistArtifacts_ProjectDirOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir() // ephemeral /tmp/vornik-exec-*/workspace stand-in

	// Worktree path lives completely outside workspace — the production
	// host paths look like /var/lib/vornik/workspaces/<proj>/.worktrees/
	// <task>, sibling to workspace's /tmp/vornik-exec-* dir.
	worktreeDir := t.TempDir()
	worktreeOutDir := filepath.Join(worktreeDir, "artifacts", "out")
	require.NoError(t, os.MkdirAll(worktreeOutDir, 0o755))

	// Two deliverables a writer step would drop into the worktree.
	pdfPath := filepath.Join(worktreeOutDir, "biocentrum-cv.pdf")
	require.NoError(t, os.WriteFile(pdfPath, []byte("%PDF-1.7 fake"), 0o644))
	htmlPath := filepath.Join(worktreeOutDir, "biocentrum-cv.html")
	require.NoError(t, os.WriteFile(htmlPath, []byte("<html></html>"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	stepStart := time.Now().Add(-5 * time.Second)
	_, err := e.persistArtifacts(context.Background(), "exec-worktree", "proj-1", "task-1", workspace, worktreeDir, stepStart, nil)
	require.NoError(t, err)

	gotPaths := make(map[string]bool, 2)
	for _, src := range store.seenSources {
		gotPaths[filepath.Clean(src)] = true
	}
	require.True(t, gotPaths[filepath.Clean(pdfPath)], "PDF in worktree-side artifacts/out must be registered; got %v", store.seenSources)
	require.True(t, gotPaths[filepath.Clean(htmlPath)], "HTML in worktree-side artifacts/out must be registered; got %v", store.seenSources)
	require.Len(t, store.seenSources, 2, "expected both worktree deliverables registered; got %v", store.seenSources)
}

// TestPersistArtifacts_SnapshotSkipsUnchangedCrossTaskPollution
// reproduces the 2026-05-21 incident: a per-task git worktree is
// created from master, which contains files merged in from prior
// tasks. `git checkout` sets the checked-out files' mtimes to the
// checkout time, so the mtime gate (originally the only defence)
// lets those files through. Without the hash-snapshot path,
// persistArtifacts would register a stale deliverable.md as the
// current step's output — which is exactly what happened in
// T-1a83's research step (it captured T-7986's write-step
// deliverable byte-for-byte). The snapshot taken at step start
// records the inherited file's hash; the post-step check sees
// the bytes are identical and skips the row.
func TestPersistArtifacts_SnapshotSkipsUnchangedCrossTaskPollution(t *testing.T) {
	workspace := t.TempDir()
	worktreeDir := t.TempDir()
	worktreeOutDir := filepath.Join(worktreeDir, "artifacts", "out")
	require.NoError(t, os.MkdirAll(worktreeOutDir, 0o755))

	// "Inherited" file: bytes from the previous task's write step.
	// Mtime is recent (mimics fresh git checkout).
	staleDeliverable := filepath.Join(worktreeOutDir, "deliverable.md")
	staleContent := []byte("# inherited from previous task — UNCHANGED this step\n")
	require.NoError(t, os.WriteFile(staleDeliverable, staleContent, 0o644))

	// Genuinely new file this step writes (no entry in the
	// snapshot).
	freshResearch := filepath.Join(worktreeOutDir, "research.md")
	require.NoError(t, os.WriteFile(freshResearch, []byte("brand new\n"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	// Snapshot at "step start". Contains the stale deliverable
	// (because it was inherited at checkout time, before the step
	// agent ran) but NOT research.md (the agent wrote it during
	// the step).
	snap := ArtifactDirSnapshot{
		"deliverable.md": sha256Hex(staleContent),
	}

	// stepStart is in the past so the mtime gate doesn't filter
	// the stale file out — the worktree checkout would have given
	// it a fresh mtime in production too. The hash snapshot is
	// the load-bearing gate.
	stepStart := time.Now().Add(-time.Hour)
	_, perr := e.persistArtifacts(
		context.Background(), "exec-snap", "proj-1", "task-1",
		workspace, worktreeDir, stepStart, snap,
	)
	require.NoError(t, perr)

	gotPaths := map[string]bool{}
	for _, src := range store.seenSources {
		gotPaths[filepath.Clean(src)] = true
	}
	require.True(t, gotPaths[filepath.Clean(freshResearch)],
		"fresh research.md must be registered: %v", store.seenSources)
	require.False(t, gotPaths[filepath.Clean(staleDeliverable)],
		"unchanged inherited deliverable.md must be SKIPPED: %v", store.seenSources)
}

// TestPersistArtifacts_SnapshotRegistersModifiedFile — when a file
// was in the snapshot but its contents changed during the step,
// the step DID modify it and it must be registered. This is the
// normal "writer overwrites the draft" flow.
func TestPersistArtifacts_SnapshotRegistersModifiedFile(t *testing.T) {
	workspace := t.TempDir()
	worktreeDir := t.TempDir()
	worktreeOutDir := filepath.Join(worktreeDir, "artifacts", "out")
	require.NoError(t, os.MkdirAll(worktreeOutDir, 0o755))

	deliverable := filepath.Join(worktreeOutDir, "deliverable.md")
	oldContent := []byte("# draft from research step\n")
	require.NoError(t, os.WriteFile(deliverable, oldContent, 0o644))
	snap := ArtifactDirSnapshot{
		"deliverable.md": sha256Hex(oldContent),
	}

	// Step runs and overwrites the file with the final polished
	// version (write step).
	require.NoError(t, os.WriteFile(deliverable, []byte("# polished by writer\n"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}
	_, perr := e.persistArtifacts(
		context.Background(), "exec-modify", "proj-1", "task-1",
		workspace, worktreeDir, time.Now().Add(-time.Hour), snap,
	)
	require.NoError(t, perr)

	require.Len(t, store.seenSources, 1,
		"modified file must be registered: %v", store.seenSources)
	require.Equal(t, deliverable, store.seenSources[0])
}

// TestSnapshotArtifactDir_HashesFilesAndSkipsDirs — pin the
// snapshot helper's behaviour: every regular file gets a hex
// SHA-256, directories are excluded, missing dirs return empty.
func TestSnapshotArtifactDir_HashesFilesAndSkipsDirs(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}

	// Missing dir returns empty (non-nil).
	got := e.SnapshotArtifactDir(filepath.Join(t.TempDir(), "nope"))
	require.NotNil(t, got)
	require.Empty(t, got)

	// Populated dir.
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(a, []byte("aaa"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("bbb"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir"), 0o755))

	got = e.SnapshotArtifactDir(dir)
	require.Len(t, got, 2, "subdir must not contribute")
	require.Equal(t, sha256Hex([]byte("aaa")), got["a.md"])
	require.Equal(t, sha256Hex([]byte("bbb")), got["b.txt"])
}

// sha256Hex is a tiny test helper for stable expected values.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestPersistArtifacts_EmptyEffectiveProjectDir guards the degraded
// path: when effectiveProjectDir is empty (no project dir resolvable —
// shouldn't happen in production but the function must not panic),
// the project-tree walk is skipped and only sources #1 + #2 fire.
func TestPersistArtifacts_EmptyEffectiveProjectDir(t *testing.T) {
	workspace := t.TempDir()
	outDir := filepath.Join(workspace, "artifacts", "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "result.md"), []byte("# r"), 0o644))

	store := &recordingArtifactStore{}
	e := &Executor{artifactStore: store, logger: zerolog.Nop()}

	_, perr := e.persistArtifacts(context.Background(), "exec-empty", "proj-1", "task-1", workspace, "", time.Now().Add(-time.Second), nil)
	require.NoError(t, perr)
	require.Len(t, store.seenSources, 1, "ephemeral-only path should still register the one artifact; got %v", store.seenSources)
}

// Tests for clampToolAuditDurationMs — see executor/artifacts.go and
// migration 22 (tool_audit_log.duration_ms widened to BIGINT after
// agent harness ms_now() drift produced 1.6e12 ms values that
// overflowed the prior INTEGER column).
func TestClampToolAuditDurationMs(t *testing.T) {
	exec := &Executor{logger: zerolog.Nop()}
	cases := []struct {
		name     string
		reported int64
		want     int64
	}{
		{"zero passes", 0, 0},
		{"reasonable 100ms passes", 100, 100},
		{"reasonable 30s passes", 30_000, 30_000},
		{"one hour boundary passes", 3_600_000, 3_600_000},
		{"just over one hour clamps", 3_600_001, 0},
		{"absolute timestamp clamps (the actual incident)", 1_600_352_997_448, 0},
		{"negative delta clamps (sign-flipped)", -1_600_353_005_060, 0},
		{"negative tiny delta clamps", -1, 0},
	}
	for _, tc := range cases {
		got := clampToolAuditDurationMs(exec, "test_tool", "exec_test", tc.reported)
		if got != tc.want {
			t.Errorf("%s: clampToolAuditDurationMs(%d) = %d, want %d", tc.name, tc.reported, got, tc.want)
		}
	}
}

// Nil-safe path — a caller that passes a nil Executor (defensive test
// scaffolding) shouldn't panic on the warn-log call. The clamp value
// is what matters, not the log emission.
func TestClampToolAuditDurationMs_NilExecutor(t *testing.T) {
	if got := clampToolAuditDurationMs(nil, "test_tool", "exec_test", 1_600_000_000_000); got != 0 {
		t.Errorf("nil exec overflow: got %d, want 0", got)
	}
	if got := clampToolAuditDurationMs(nil, "test_tool", "exec_test", 50); got != 50 {
		t.Errorf("nil exec passthrough: got %d, want 50", got)
	}
}

// TestDisambiguateArtifactName_TableDriven covers every shape the
// helper might see from a workspace harvest: extensions, no
// extension, multi-dot names, leading-dot, and the short-execID
// boundary case. Pins the format so a future refactor can't
// quietly change what operators see in the UI.
func TestDisambiguateArtifactName_TableDriven(t *testing.T) {
	when := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		execID      string
		want        string
		description string
	}{
		{"report.md", "exec_xyz_1a2b", "report-20260516-1a2b.md", "basic ext case"},
		{"CHANGELOG", "exec_xyz_1a2b", "CHANGELOG-20260516-1a2b", "no extension"},
		{"report.tar.gz", "exec_xyz_1a2b", "report.tar-20260516-1a2b.gz", "multi-dot: last dot wins"},
		{".env", "exec_xyz_1a2b", ".env-20260516-1a2b", "leading-dot file treated as ext-less"},
		{"a", "exec_xyz_1a2b", "a-20260516-1a2b", "single-char stem"},
		{"x.txt", "abc", "x-20260516-abc.txt", "short execID (< 4 chars) used verbatim"},
		{"x.txt", "1a2b", "x-20260516-1a2b.txt", "exactly-4-char execID used verbatim"},
		// Real-world IDs are long; the suffix is the LAST 4 chars
		// of the input. Pinning a realistic shape proves the
		// substring slice matches the UI's shortID convention
		// (T-XXXX) so operators can correlate filenames back to
		// executions by eye.
		{"research.md", "execution_20260516120000_abcd1234efgh5678", "research-20260516-5678.md", "realistic long execID"},
	}
	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			got := disambiguateArtifactName(c.name, c.execID, when)
			if got != c.want {
				t.Errorf("disambiguateArtifactName(%q, %q) = %q, want %q", c.name, c.execID, got, c.want)
			}
		})
	}
}

// TestSplitArtifactStem covers the stem/ext split contract that
// disambiguateArtifactName depends on. Pins stem+ext == name
// (the round-trip invariant) plus the leading-dot special-case
// that distinguishes `.env` from `foo.bar`.
func TestSplitArtifactStem(t *testing.T) {
	cases := []struct {
		name     string
		wantStem string
		wantExt  string
	}{
		{"report.md", "report", ".md"},
		{"report.tar.gz", "report.tar", ".gz"},
		{"CHANGELOG", "CHANGELOG", ""},
		{".env", ".env", ""},
		{".dockerignore", ".dockerignore", ""},
		{"", "", ""},
		{"a", "a", ""},
		{"a.b", "a", ".b"},
		{"a..b", "a.", ".b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stem, ext := splitArtifactStem(c.name)
			if stem != c.wantStem || ext != c.wantExt {
				t.Errorf("splitArtifactStem(%q) = (%q, %q), want (%q, %q)", c.name, stem, ext, c.wantStem, c.wantExt)
			}
			// Round-trip: stem + ext must rebuild the original.
			if stem+ext != c.name {
				t.Errorf("round-trip failed: stem+ext=%q, want %q", stem+ext, c.name)
			}
		})
	}
}

// TestDisambiguateArtifactName_RoundTrip ensures the suffix is
// deterministic given fixed inputs. Same name + same execID +
// same time always produces the same output — supersession
// depends on this so re-running a task on the same UTC day
// generates a predictable filename.
func TestDisambiguateArtifactName_RoundTrip(t *testing.T) {
	when := time.Date(2026, 5, 16, 14, 30, 0, 0, time.UTC)
	a := disambiguateArtifactName("research.md", "exec_xyz_1a2b", when)
	b := disambiguateArtifactName("research.md", "exec_xyz_1a2b", when)
	if a != b {
		t.Errorf("non-deterministic output: %q vs %q", a, b)
	}
}

// TestStripDisambiguationSuffix_RoundTrip pins the round-trip
// contract used by isTranscriptArtifact: disambig({stem}{ext}) →
// strip → {stem}{ext}. Hex casing must not affect detection, and
// inputs that don't look disambiguated come back unchanged.
func TestStripDisambiguationSuffix_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"research-20260515-1a2b.md", "research.md"},
		{"route-response-20260515-0f96.md", "route-response.md"},
		{"CHANGELOG-20260515-1a2b", "CHANGELOG"},         // extension-less
		{"report.tar-20260515-1a2b.gz", "report.tar.gz"}, // double-dot ext
		{".env-20260515-1a2b", ".env"},                   // leading-dot file
		{"PLAIN.md", "PLAIN.md"},                         // no suffix → no change
		{"research-20260515.md", "research-20260515.md"}, // missing -XXXX → no change
		{"research-1a2b.md", "research-1a2b.md"},         // missing date → no change
		// Defensive: a project-supplied name that happens to contain
		// `-YYYYMMDD-XXXX` mid-stem must NOT be stripped — only
		// the END-anchored suffix matters.
		{"request-20260515-cycle-foo.md", "request-20260515-cycle-foo.md"},
		// Hex case insensitivity — disambig always lowercases but a
		// hand-typed filename might use uppercase; strip should still
		// recognise it.
		{"research-20260515-1A2B.md", "research.md"},
	}
	for _, tc := range cases {
		if got := stripDisambiguationSuffix(tc.name); got != tc.want {
			t.Errorf("strip(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestIsTranscriptArtifact_HandlesOldAndDisambiguatedNames is the
// regression test for the 2026-05-15 incident: post-disambig
// transcript artifacts (e.g. `route-response-20260515-0f96.md`)
// silently slipped past the legacy `HasSuffix("-response.md")`
// filter and got ingested as memory chunks with empty
// producer_role. After the fix the filter normalises through
// stripDisambiguationSuffix first.
func TestIsTranscriptArtifact_HandlesOldAndDisambiguatedNames(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Old-format transcripts (pre-2026-05-15) still match.
		{"plan-response.md", true},
		{"route-response.md", true},
		{"write-response.md", true},
		// Post-disambig transcripts MUST be filtered.
		{"plan-response-20260515-1a2b.md", true},
		{"route-response-20260515-0f96.md", true},
		{"research-response-20260515-7be8.md", true},
		{"write-response-20260514-f706.md", true},
		// Legitimate output artifacts MUST NOT be filtered, even
		// after disambiguation appends the same trailing pattern.
		{"research.md", false},
		{"research-20260515-1a2b.md", false},
		{"digest-2026-05-20-20260515-a80b.md", false},
		{"scan-linkedin-jobs-cz-2026-05-14.md", false},
		// Defensive: a project document literally called "response.md"
		// (no dash prefix) should NOT be skipped. The contract is
		// `<step>-response.md`, the leading dash matters.
		{"response.md", false},
		// Non-markdown transcripts (if they ever exist) are out of
		// scope for the filter — it only ever runs on `.md` files
		// upstream; assert the bare-string contract here anyway.
		{"plan-response.txt", false},
	}
	for _, tc := range cases {
		if got := isTranscriptArtifact(tc.name); got != tc.want {
			t.Errorf("isTranscriptArtifact(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func (s *stubLLMUsageRepo) SumCostByAPIKey(_ context.Context, _ string, _, _ time.Time) (float64, error) {
	return 0, nil
}

func (s *stubLLMUsageRepo) MeanCostByWorkflow(_ context.Context, _, _ string, _, _ time.Time) (float64, int, error) {
	return 0, 0, nil
}

// TestStoreOneArtifact_Origin_TaskOutput verifies that storeOneArtifact
// sets Origin = ArtifactOriginTaskOutput on the artifact row it creates
// via the artifactRepo fallback path (regression guard for Slice 2 of
// outputguard-provenance-design.md).
func TestStoreOneArtifact_Origin_TaskOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "output.md")
	require.NoError(t, os.WriteFile(src, []byte("content"), 0o644))

	var got *persistence.Artifact
	repo := &capturingArtifactRepoForOrigin{onCreate: func(a *persistence.Artifact) { got = a }}
	e := &Executor{
		artifactRepo: repo,
		logger:       zerolog.Nop(),
	}

	entry, err := e.storeOneArtifact(context.Background(), "exec-1", "proj-1", "task-1", "output.md", src, time.Now())
	require.NoError(t, err)
	require.NotNil(t, entry)

	require.NotNil(t, got)
	if got.Origin != persistence.ArtifactOriginTaskOutput {
		t.Errorf("Origin = %q, want task_output", got.Origin)
	}
}

// capturingArtifactRepoForOrigin implements the executor.ArtifactRepository
// interface and calls onCreate for every Create so tests can inspect the
// Artifact row without a real database.
type capturingArtifactRepoForOrigin struct {
	onCreate func(*persistence.Artifact)
}

func (r *capturingArtifactRepoForOrigin) Create(_ context.Context, a *persistence.Artifact) error {
	if r.onCreate != nil {
		r.onCreate(a)
	}
	return nil
}
func (r *capturingArtifactRepoForOrigin) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, persistence.ErrNotFound
}
func (r *capturingArtifactRepoForOrigin) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
