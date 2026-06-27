package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestToFloat64_AllNumericTypes — the helper accepts every Go
// numeric kind that json.Unmarshal can emit + the few extras
// agents may produce. Used by the plausibility numeric-equality
// branch in matchesAny.
func TestToFloat64_AllNumericTypes(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2.5), 2.5, true},
		{int(3), 3.0, true},
		{int32(4), 4.0, true},
		{int64(5), 5.0, true},
		{uint(6), 6.0, true},
		{uint32(7), 7.0, true},
		{uint64(8), 8.0, true},
		// non-numeric kinds must not convert
		{"9", 0, false},
		{true, 0, false},
		{nil, 0, false},
		{[]int{1, 2}, 0, false},
		{map[string]int{"k": 1}, 0, false},
	}
	for _, c := range cases {
		got, ok := toFloat64(c.in)
		assert.Equal(t, c.ok, ok, "input=%v ok mismatch", c.in)
		if ok {
			assert.InDelta(t, c.want, got, 1e-9, "input=%v float64 mismatch", c.in)
		}
	}
}

// TestIsEmptyValueY — every type the LLM may emit:
//   - nil → empty
//   - "" / whitespace-only string → empty
//   - empty slice / map → empty
//   - numeric zero (across kinds) → empty (deliberate: zero is "missing")
//   - bool false → NOT empty (legitimate value)
//   - non-zero numbers → not empty
//   - struct or pointer or other types → not empty (fall-through)
func TestIsEmptyValueY(t *testing.T) {
	assert.True(t, isEmptyValue(nil))
	assert.True(t, isEmptyValue(""))
	assert.True(t, isEmptyValue("   "))
	assert.True(t, isEmptyValue([]any{}))
	assert.True(t, isEmptyValue(map[string]any{}))
	assert.True(t, isEmptyValue(float64(0)))
	assert.True(t, isEmptyValue(float32(0)))
	assert.True(t, isEmptyValue(int(0)))
	assert.True(t, isEmptyValue(int32(0)))
	assert.True(t, isEmptyValue(int64(0)))

	// non-empty cases
	assert.False(t, isEmptyValue("x"))
	assert.False(t, isEmptyValue([]any{1}))
	assert.False(t, isEmptyValue(map[string]any{"k": "v"}))
	assert.False(t, isEmptyValue(float64(0.001)))
	assert.False(t, isEmptyValue(int(1)))

	// bool is NEVER empty — false is a meaningful value
	assert.False(t, isEmptyValue(false))
	assert.False(t, isEmptyValue(true))

	// unknown types — fall through to non-empty
	type unknown struct{}
	assert.False(t, isEmptyValue(unknown{}))
}

// TestJSONTypeNameY — the human-readable type tag used in the
// missing-keys diagnostic the operator sees on schema violations.
func TestJSONTypeNameY(t *testing.T) {
	assert.Equal(t, "null", jsonTypeName(nil))
	assert.Equal(t, "bool", jsonTypeName(true))
	assert.Equal(t, "bool", jsonTypeName(false))
	assert.Equal(t, "number", jsonTypeName(float64(1.5)))
	assert.Equal(t, "number", jsonTypeName(float32(2.5)))
	assert.Equal(t, "number", jsonTypeName(int(1)))
	assert.Equal(t, "number", jsonTypeName(int32(1)))
	assert.Equal(t, "number", jsonTypeName(int64(1)))
	assert.Equal(t, "string", jsonTypeName("x"))
	assert.Equal(t, "array", jsonTypeName([]any{1}))
	assert.Equal(t, "object", jsonTypeName(map[string]any{"k": "v"}))

	// Unknown Go type — returns Go-formatted type name as a
	// fallback. Important: not "null", not "object" — operator
	// must see the real type so they can diagnose.
	type unknownT struct{}
	got := jsonTypeName(unknownT{})
	assert.Contains(t, got, "unknownT")
}

// TestFindSwarmRole_NilSwarm — guarded against nil swarm
// pointer; returns a descriptive error so the caller can surface
// it as a config issue.
func TestFindSwarmRole_NilSwarm(t *testing.T) {
	_, err := findSwarmRole(nil, "lead")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "swarm config is not available")
}

// TestFindSwarmRole_NotFound — role missing from the catalogue
// surfaces a structured error naming both the role and the
// swarm — operator-actionable diagnostic.
func TestFindSwarmRole_NotFound(t *testing.T) {
	swarm := &registry.Swarm{ID: "s1", Roles: []registry.SwarmRole{
		{Name: "lead"},
		{Name: "coder"},
	}}
	_, err := findSwarmRole(swarm, "reviewer")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reviewer")
	assert.Contains(t, err.Error(), "s1")
}

// TestFindSwarmRole_HappyPath — returns a pointer to the
// in-slice role, not a copy. Used by the executor to mutate
// derived state on the role; a returned copy would silently
// drop those mutations.
func TestFindSwarmRole_HappyPath(t *testing.T) {
	swarm := &registry.Swarm{ID: "s1", Roles: []registry.SwarmRole{
		{Name: "lead"},
		{Name: "coder"},
	}}
	r, err := findSwarmRole(swarm, "coder")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, "coder", r.Name)
	// Sanity: pointer is from the swarm's slice — mutating its
	// .Name surfaces in swarm.Roles[1].
	r.Name = "mutated"
	assert.Equal(t, "mutated", swarm.Roles[1].Name)
}

// TestTaskWorkflowIDY — falls back to "default-workflow" when:
//   - task is nil
//   - workflow id is nil
//   - workflow id is empty
//   - workflow id is the LLM placeholder "-"
func TestTaskWorkflowIDY(t *testing.T) {
	assert.Equal(t, "default-workflow", taskWorkflowID(nil))
	assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{}))

	empty := ""
	assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{WorkflowID: &empty}))

	dash := "-"
	assert.Equal(t, "default-workflow", taskWorkflowID(&persistence.Task{WorkflowID: &dash}),
		"LLM placeholder '-' must be sanitised to the default")

	real := "research"
	assert.Equal(t, "research", taskWorkflowID(&persistence.Task{WorkflowID: &real}))
}

// TestSnapshotWorkspaceRef_EmptyDir — guard returns "" without
// invoking git.
func TestSnapshotWorkspaceRef_EmptyDir(t *testing.T) {
	assert.Equal(t, "", snapshotWorkspaceRef(""))
}

// TestSnapshotWorkspaceRef_NotARepo — when .git doesn't exist
// under dir, the helper short-circuits to "" (no fork). Used at
// recovery time to skip workspaces that aren't git repos.
func TestSnapshotWorkspaceRef_NotARepo(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, "", snapshotWorkspaceRef(dir),
		"non-repo dir must short-circuit before invoking git")
}

// TestSnapshotWorkspaceRef_RealRepo — initializes a real git
// repo under t.TempDir(), commits, then asserts the helper
// returns the HEAD SHA. Verifies the happy path runs git
// correctly (no subprocess injection, output trimmed).
func TestSnapshotWorkspaceRef_RealRepo(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// Use a deterministic empty env subset so user config
		// doesn't change behaviour.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	runGit("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644))
	runGit("add", "f.txt")
	runGit("commit", "-q", "-m", "init")

	got := snapshotWorkspaceRef(dir)
	assert.Len(t, got, 40, "real HEAD SHA must be 40 hex chars")
}

// TestResetWorkspace_EmptyArgsNoOp — both empty dir and empty
// ref short-circuit silently. Used by the recovery path when
// no snapshot was captured (older executions pre-snapshot wiring).
func TestResetWorkspace_EmptyArgs(t *testing.T) {
	require.NoError(t, resetWorkspace(context.Background(), "", "ref", zerolog.Nop()))
	require.NoError(t, resetWorkspace(context.Background(), "dir", "", zerolog.Nop()))
	require.NoError(t, resetWorkspace(context.Background(), "", "", zerolog.Nop()))
}

// TestWorkflowEffectiveWorkspaceDirY — three resolution branches:
//   - plan.worktreeDir present → use it
//   - plan present, worktreeDir empty, but no ProjectWorkspacePath → ""
//   - plan + ProjectWorkspacePath + project ID → join them
//   - nil plan / nil task / nil executor → ""
func TestWorkflowEffectiveWorkspaceDirY(t *testing.T) {
	// Both nil → ""
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, nil, nil))
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, &executionPlan{}, nil))
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, nil, &persistence.Task{}))

	// plan.worktreeDir wins
	e := &Executor{config: &Config{ProjectWorkspacePath: "/ws"}}
	plan := &executionPlan{worktreeDir: "/wt/abc"}
	task := &persistence.Task{ProjectID: "p1"}
	assert.Equal(t, "/wt/abc", workflowEffectiveWorkspaceDir(e, plan, task))

	// No worktreeDir → join workspace + project
	plan2 := &executionPlan{}
	assert.Equal(t, filepath.Join("/ws", "p1"), workflowEffectiveWorkspaceDir(e, plan2, task))

	// No workspace path → ""
	e2 := &Executor{config: &Config{ProjectWorkspacePath: ""}}
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(e2, plan2, task))

	// Empty project ID → ""
	assert.Equal(t, "", workflowEffectiveWorkspaceDir(e, plan2, &persistence.Task{}))

	// nil executor with plan that has worktreeDir → use the
	// worktreeDir (no need to consult executor)
	assert.Equal(t, "/wt/abc", workflowEffectiveWorkspaceDir(nil, plan, task))
}

// TestWorktreeInUseByContainer_EmptyDir — guard against
// unhelpful podman invocation when the worktreeDir is unknown.
func TestWorktreeInUseByContainer_EmptyDir(t *testing.T) {
	assert.False(t, worktreeInUseByContainer(context.Background(), ""))
}

// TestProjectCleanExcludeDir_Variants — pin every short-circuit
// + the happy path. The function is tiny but its behaviour gates
// the operator's untracked files surviving a workspace cleanup.
func TestProjectCleanExcludeDir_Variants(t *testing.T) {
	// Empty input
	assert.Equal(t, "", projectCleanExcludeDir(""))
	assert.Equal(t, "", projectCleanExcludeDir("   "))

	// Absolute path — refused for safety
	assert.Equal(t, "", projectCleanExcludeDir("/etc/passwd"))

	// ".." traversal — refused
	assert.Equal(t, "", projectCleanExcludeDir("../escape"))
	assert.Equal(t, "", projectCleanExcludeDir(".."))

	// Root-level path ("X.md") — returns "" so cleanup
	// doesn't get a "." exclude that would no-op it.
	assert.Equal(t, "", projectCleanExcludeDir("README.md"))

	// .autonomy/ paths — already covered by default excludes
	assert.Equal(t, "", projectCleanExcludeDir(".autonomy/context.md"))
	assert.Equal(t, "", projectCleanExcludeDir(".autonomy/subdir/file.md"))

	// Real operator-overridden path — returns the parent dir
	assert.Equal(t, "ops", projectCleanExcludeDir("ops/notes.md"))
	assert.Equal(t, filepath.Join("ops", "sub"), projectCleanExcludeDir("ops/sub/file.md"))
}
