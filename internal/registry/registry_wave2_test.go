// registry_wave2_test.go — SECOND-pass coverage for the in-memory
// registry's resolution + reload primitives and the per-project
// config-block getters that still had thin/zero coverage after the
// first wave.
//
// Scope guard (do NOT duplicate): Workflow.Validate /
// Swarm.Validate / project consent matrices (CanCall /
// AcceptsCallsFrom / AllowsSpawnTemplate) and the
// ResolveCronTaskType / ResolveBacklogFilePath / NormalizedAutonomyMode
// happy-path matrices are already covered elsewhere. This file targets:
//
//   - registry resolution error branches (GetProjectWithSwarm /
//     GetProjectWithWorkflow / ResolveProjectConfig — project-missing,
//     swarm-missing, workflow-missing legs).
//   - hot-reload staging lifecycle error guards (Reload with no
//     configDir, ValidateStaged / ActivateStaged / DiffStaged with
//     no staged snapshot) and the merge/replace semantics of a
//     successful Load over an existing active set.
//   - transient-workflow fallback resolution + replace + deregister.
//   - the scheduler-facing map getters that had 0% coverage:
//     ProjectConcurrencyLimits, ProjectPriorities, ArchivedProjectIDs.
//   - nil-safe / default-returning config-block getters:
//     ProjectFirewall.Enabled, ArchivedAtTime, EffectiveMaxCallDepth
//     defaulting, GetStats.
//
// All fixtures are built in-memory via SeedForTest or direct map
// mutation under New() so the active maps are populated WITHOUT a
// swarm/workflow entry where the test wants to exercise the
// dangling-reference error legs.

package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// w2seed installs the given projects as the registry's active set and
// also wires the supplied swarms/workflows into the active maps so
// resolution can succeed where the test wants it to. SeedForTest only
// touches projects, so swarm/workflow wiring is done here directly.
func w2seed(t *testing.T, projects map[string]*Project, swarms map[string]*Swarm, workflows map[string]*Workflow) *Registry {
	t.Helper()
	r := New()
	SeedForTest(r, projects)
	r.mu.Lock()
	if swarms != nil {
		r.swarms = swarms
		r.active.swarms = swarms
	}
	if workflows != nil {
		r.workflows = workflows
		r.active.workflows = workflows
	}
	r.mu.Unlock()
	return r
}

// --- resolution: error legs --------------------------------------------------

// TestW2RegGetProjectWithSwarm_MissingProject — unknown project id
// returns nil/nil and a "project not found" error, never a panic.
func TestW2RegGetProjectWithSwarm_MissingProject(t *testing.T) {
	r := New()
	p, s, err := r.GetProjectWithSwarm("ghost")
	require.Error(t, err)
	assert.Nil(t, p)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), "ghost")
}

// TestW2RegGetProjectWithSwarm_MissingSwarm — the project exists but
// its SwarmID dangles. The contract returns the project (non-nil) plus
// a nil swarm and an error naming the missing swarm, so callers can
// still surface the project in an error banner.
func TestW2RegGetProjectWithSwarm_MissingSwarm(t *testing.T) {
	r := w2seed(t,
		map[string]*Project{"p1": {ID: "p1", SwarmID: "absent-swarm", DefaultWorkflowID: "w1"}},
		nil, nil)
	p, s, err := r.GetProjectWithSwarm("p1")
	require.Error(t, err)
	require.NotNil(t, p, "project must be returned even when its swarm dangles")
	assert.Equal(t, "p1", p.ID)
	assert.Nil(t, s)
	assert.Contains(t, err.Error(), "absent-swarm")
}

// TestW2RegGetProjectWithWorkflow_MissingWorkflow — symmetric to the
// swarm leg: project returned, workflow nil, error names the workflow.
func TestW2RegGetProjectWithWorkflow_MissingWorkflow(t *testing.T) {
	r := w2seed(t,
		map[string]*Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "absent-wf"}},
		map[string]*Swarm{"s1": {ID: "s1"}}, nil)
	p, w, err := r.GetProjectWithWorkflow("p1")
	require.Error(t, err)
	require.NotNil(t, p)
	assert.Nil(t, w)
	assert.Contains(t, err.Error(), "absent-wf")
}

// TestW2RegGetProjectWithWorkflow_MissingProject — unknown id returns
// nil/nil/err.
func TestW2RegGetProjectWithWorkflow_MissingProject(t *testing.T) {
	r := New()
	p, w, err := r.GetProjectWithWorkflow("ghost")
	require.Error(t, err)
	assert.Nil(t, p)
	assert.Nil(t, w)
}

// TestW2RegResolveProjectConfig_AllThreeLegs — exercises every failure
// leg (missing project, missing swarm, missing workflow) plus the full
// happy path in one matrix so the three-way resolver's branches are all
// hit.
func TestW2RegResolveProjectConfig_AllThreeLegs(t *testing.T) {
	cases := []struct {
		name      string
		projects  map[string]*Project
		swarms    map[string]*Swarm
		workflows map[string]*Workflow
		lookup    string
		wantErr   string
		wantOK    bool
	}{
		{
			name:     "missing project",
			projects: map[string]*Project{},
			lookup:   "p1",
			wantErr:  "not found",
		},
		{
			name:      "missing swarm",
			projects:  map[string]*Project{"p1": {ID: "p1", SwarmID: "s-missing", DefaultWorkflowID: "w1"}},
			workflows: map[string]*Workflow{"w1": {ID: "w1"}},
			lookup:    "p1",
			wantErr:   "s-missing",
		},
		{
			name:     "missing workflow",
			projects: map[string]*Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "w-missing"}},
			swarms:   map[string]*Swarm{"s1": {ID: "s1"}},
			lookup:   "p1",
			wantErr:  "w-missing",
		},
		{
			name:      "all resolved",
			projects:  map[string]*Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "w1"}},
			swarms:    map[string]*Swarm{"s1": {ID: "s1"}},
			workflows: map[string]*Workflow{"w1": {ID: "w1"}},
			lookup:    "p1",
			wantOK:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := w2seed(t, c.projects, c.swarms, c.workflows)
			p, s, w, err := r.ResolveProjectConfig(c.lookup)
			if c.wantOK {
				require.NoError(t, err)
				assert.NotNil(t, p)
				assert.NotNil(t, s)
				assert.NotNil(t, w)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
			assert.Nil(t, p)
			assert.Nil(t, s)
			assert.Nil(t, w)
		})
	}
}

// --- hot-reload lifecycle guards ---------------------------------------------

// TestW2RegReload_NoConfigDir — Reload on a registry that was never
// Load()ed (no configDir) must error rather than silently re-reading
// the empty string as a directory.
func TestW2RegReload_NoConfigDir(t *testing.T) {
	r := New()
	err := r.Reload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no config directory")
}

// TestW2RegValidateStaged_NoStaged — ValidateStaged with nothing staged
// is an explicit error, not a nil pass.
func TestW2RegValidateStaged_NoStaged(t *testing.T) {
	r := New()
	err := r.ValidateStaged()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no staged")
}

// TestW2RegActivateStaged_NoStaged — promoting with nothing staged is
// an error and must NOT clobber the active set.
func TestW2RegActivateStaged_NoStaged(t *testing.T) {
	r := w2seed(t,
		map[string]*Project{"keep": {ID: "keep", SwarmID: "s1", DefaultWorkflowID: "w1"}},
		nil, nil)
	err := r.ActivateStaged()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no staged")
	// Active set untouched.
	assert.NotNil(t, r.GetProject("keep"))
}

// TestW2RegDiffStaged_NoStaged — DiffStaged without a staged snapshot
// returns a zero diff and an error.
func TestW2RegDiffStaged_NoStaged(t *testing.T) {
	r := New()
	diff, err := r.DiffStaged()
	require.Error(t, err)
	assert.False(t, diff.HasChanges())
}

// TestW2RegStripInvalidFromStaged_NilStagedIsNoop — stripping with no
// staged snapshot is a nil-returning no-op (defensive guard).
func TestW2RegStripInvalidFromStaged_NilStagedIsNoop(t *testing.T) {
	r := New()
	assert.Nil(t, r.StripInvalidFromStaged())
}

// --- merge/replace semantics of a successful Load over an existing set -------

// TestW2RegLoadReplacesActiveWholesale — loading a fresh config dir over
// an already-populated registry REPLACES the active maps wholesale: IDs
// only present in the prior set must disappear, not linger. This pins
// the "reload is replace, not merge" invariant for the single-dir path.
func TestW2RegLoadReplacesActiveWholesale(t *testing.T) {
	// Prior active set carries a "stale" project that the new dir won't.
	r := w2seed(t,
		map[string]*Project{
			"stale": {ID: "stale", SwarmID: "s1", DefaultWorkflowID: "w1"},
		},
		map[string]*Swarm{"s1": {ID: "s1"}},
		map[string]*Workflow{"w1": {ID: "w1"}})
	require.NotNil(t, r.GetProject("stale"))

	// New config dir declares only "fresh".
	root := t.TempDir()
	layerHelper(t, root, "fresh", "sf", "wf")
	require.NoError(t, r.Load(root))

	assert.NotNil(t, r.GetProject("fresh"), "newly loaded project must be present")
	assert.Nil(t, r.GetProject("stale"), "reload must replace, not merge — stale id should be gone")
	assert.Equal(t, root, r.GetConfigDir(), "configDir tracks the last-loaded directory")
}

// TestW2RegReload_PicksUpEditsAndDrops — after a first Load, mutating
// the directory and calling Reload re-reads it: a removed project file
// drops out, mirroring the daemon's SIGHUP path.
func TestW2RegReload_PicksUpEditsAndDrops(t *testing.T) {
	root := t.TempDir()
	layerHelper(t, root, "p1", "s1", "w1")
	layerHelper(t, root, "p2", "s2", "w2")

	r := New()
	require.NoError(t, r.Load(root))
	require.NotNil(t, r.GetProject("p1"))
	require.NotNil(t, r.GetProject("p2"))

	// Drop p2's project file, keep its swarm/workflow.
	require.NoError(t, os.Remove(filepath.Join(root, "projects", "p2.yaml")))
	require.NoError(t, r.Reload())

	assert.NotNil(t, r.GetProject("p1"), "surviving project stays after reload")
	assert.Nil(t, r.GetProject("p2"), "removed project file must drop on reload")
}

// --- transient-workflow fallback resolution ----------------------------------

// TestW2RegTransient_FallbackResolveAndReplace — a transient workflow
// is resolvable via GetWorkflow when absent from loaded config, gets its
// ID stamped to the registration key, can be re-registered (overwrite),
// and survives a Load (which replaces the loaded-config map but must not
// touch the transient map). DeregisterTransient then removes it.
func TestW2RegTransient_FallbackResolveAndReplace(t *testing.T) {
	r := New()
	const id = "adaptive-candidate-abc123"

	// Empty id / nil workflow are refused.
	require.Error(t, r.RegisterTransient("  ", &Workflow{}))
	require.Error(t, r.RegisterTransient(id, nil))

	require.NoError(t, r.RegisterTransient(id, &Workflow{ID: "whatever"}))
	got := r.GetWorkflow(id)
	require.NotNil(t, got, "transient workflow must be resolvable via GetWorkflow fallback")
	assert.Equal(t, id, got.ID, "RegisterTransient stamps the workflow's ID to the key for self-consistency")

	// A Load that replaces the loaded-config workflows map must NOT
	// drop the in-flight transient candidate.
	root := t.TempDir()
	layerHelper(t, root, "p1", "s1", "w1")
	require.NoError(t, r.Load(root))
	assert.NotNil(t, r.GetWorkflow(id), "transient survives a config reload")
	assert.NotNil(t, r.GetWorkflow("w1"), "loaded-config workflow resolves too")

	// Deregister removes it; deregistering an absent id is a safe no-op.
	r.DeregisterTransient(id)
	assert.Nil(t, r.GetWorkflow(id))
	r.DeregisterTransient("never-registered")
}

// TestW2RegTransient_LoadedConfigWinsOverTransient — when the same id
// exists in both the loaded config and the transient map, the loaded
// config wins (GetWorkflow checks workflows first).
func TestW2RegTransient_LoadedConfigWinsOverTransient(t *testing.T) {
	r := New()
	r.mu.Lock()
	r.workflows["dup"] = &Workflow{ID: "dup", Entrypoint: "from-config"}
	r.mu.Unlock()
	require.NoError(t, r.RegisterTransient("dup", &Workflow{ID: "dup", Entrypoint: "from-transient"}))

	got := r.GetWorkflow("dup")
	require.NotNil(t, got)
	assert.Equal(t, "from-config", got.Entrypoint, "loaded config must win over a same-id transient")
}

// --- scheduler-facing map getters (were 0% covered) --------------------------

// TestW2RegProjectConcurrencyLimits_OmitsUnset — only projects with a
// positive MaxConcurrentTasks appear; zero/unset projects are omitted
// (no per-project enforcement).
func TestW2RegProjectConcurrencyLimits_OmitsUnset(t *testing.T) {
	r := w2seed(t, map[string]*Project{
		"capped":    {ID: "capped", MaxConcurrentTasks: 3},
		"unlimited": {ID: "unlimited", MaxConcurrentTasks: 0},
	}, nil, nil)

	limits := r.ProjectConcurrencyLimits()
	assert.Equal(t, 3, limits["capped"])
	_, present := limits["unlimited"]
	assert.False(t, present, "projects with no concurrency cap must be omitted")
	assert.Len(t, limits, 1)
}

// TestW2RegProjectPriorities_IncludesExplicitZero — every loaded project
// appears in the priorities map, INCLUDING explicit priority 0 (the
// highest-urgency setting). The scheduler distinguishes "present at 0"
// from "absent" so this getter must not drop zeros.
func TestW2RegProjectPriorities_IncludesExplicitZero(t *testing.T) {
	r := w2seed(t, map[string]*Project{
		"urgent": {ID: "urgent", DefaultPriority: 0},
		"low":    {ID: "low", DefaultPriority: 50},
	}, nil, nil)

	pr := r.ProjectPriorities()
	require.Len(t, pr, 2)
	v, present := pr["urgent"]
	assert.True(t, present, "explicit priority-0 project must be present in the map")
	assert.Equal(t, 0, v)
	assert.Equal(t, 50, pr["low"])
}

// TestW2RegArchivedProjectIDs_OnlyArchived — only projects flipped to
// the archived lifecycle status are returned; active ones are skipped.
func TestW2RegArchivedProjectIDs_OnlyArchived(t *testing.T) {
	r := w2seed(t, map[string]*Project{
		"live":    {ID: "live"},
		"shelved": {ID: "shelved", Lifecycle: ProjectLifecycle{Status: "archived"}},
	}, nil, nil)

	ids := r.ArchivedProjectIDs()
	assert.Equal(t, []string{"shelved"}, ids)
}

// --- nil-safe / default config-block getters ---------------------------------

// TestW2RegProjectFirewall_Enabled — empty Mode (and whitespace-only)
// means "inherit daemon default" → Enabled() is false; any explicit
// value flips it true.
func TestW2RegProjectFirewall_Enabled(t *testing.T) {
	assert.False(t, ProjectFirewall{Mode: ""}.Enabled(), "empty mode inherits daemon default")
	assert.False(t, ProjectFirewall{Mode: "   "}.Enabled(), "whitespace-only mode still inherits")
	assert.True(t, ProjectFirewall{Mode: "enforce"}.Enabled(), "explicit mode is an override")
	assert.True(t, ProjectFirewall{Mode: "advisory"}.Enabled())
}

// TestW2RegArchivedAtTime — parses a valid RFC3339 archivedAt, and
// returns zero/false for unset, unparseable, and nil-receiver inputs.
func TestW2RegArchivedAtTime(t *testing.T) {
	// Unset → zero/false.
	p := &Project{}
	tt, ok := p.ArchivedAtTime()
	assert.False(t, ok)
	assert.True(t, tt.IsZero())

	// Unparseable → zero/false (defensive parse, no panic).
	bad := &Project{Lifecycle: ProjectLifecycle{ArchivedAt: "not-a-timestamp"}}
	tt, ok = bad.ArchivedAtTime()
	assert.False(t, ok)
	assert.True(t, tt.IsZero())

	// Valid RFC3339 → parsed value, true.
	want := time.Date(2026, 5, 8, 17, 19, 0, 0, time.UTC)
	good := &Project{Lifecycle: ProjectLifecycle{ArchivedAt: want.Format(time.RFC3339)}}
	tt, ok = good.ArchivedAtTime()
	require.True(t, ok)
	assert.True(t, want.Equal(tt))

	// Nil receiver → zero/false, no panic.
	var nilP *Project
	tt, ok = nilP.ArchivedAtTime()
	assert.False(t, ok)
	assert.True(t, tt.IsZero())
}

// TestW2RegEffectiveMaxCallDepth_Defaults — nil receiver, zero, and
// negative all fall back to DefaultMaxCallDepth; a positive value is
// honoured verbatim.
func TestW2RegEffectiveMaxCallDepth_Defaults(t *testing.T) {
	var nilP *Project
	assert.Equal(t, DefaultMaxCallDepth, nilP.EffectiveMaxCallDepth())
	assert.Equal(t, DefaultMaxCallDepth, (&Project{MaxCallDepth: 0}).EffectiveMaxCallDepth())
	assert.Equal(t, DefaultMaxCallDepth, (&Project{MaxCallDepth: -5}).EffectiveMaxCallDepth())
	assert.Equal(t, 12, (&Project{MaxCallDepth: 12}).EffectiveMaxCallDepth())
}

// TestW2RegGetStats_ReflectsActiveCounts — GetStats reports the live
// counts of each entity class plus the config directory.
func TestW2RegGetStats_ReflectsActiveCounts(t *testing.T) {
	r := w2seed(t,
		map[string]*Project{"p1": {ID: "p1"}, "p2": {ID: "p2"}},
		map[string]*Swarm{"s1": {ID: "s1"}},
		map[string]*Workflow{"w1": {ID: "w1"}, "w2": {ID: "w2"}, "w3": {ID: "w3"}})

	st := r.GetStats()
	assert.Equal(t, 2, st.ProjectCount)
	assert.Equal(t, 1, st.SwarmCount)
	assert.Equal(t, 3, st.WorkflowCount)
}
