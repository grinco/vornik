package executor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestLastStepOrUnknown — empty input yields the sentinel, non-empty
// yields the last entry. Pin both so a future "use len(>=2)" tweak
// trips the test.
func TestLastStepOrUnknown(t *testing.T) {
	assert.Equal(t, "unknown", lastStepOrUnknown(nil))
	assert.Equal(t, "unknown", lastStepOrUnknown([]string{}))
	assert.Equal(t, "step-1", lastStepOrUnknown([]string{"step-1"}))
	assert.Equal(t, "step-3", lastStepOrUnknown([]string{"step-1", "step-2", "step-3"}))
}

// TestRoleNames — picks role.Name out of a SwarmRole slice.
func TestRoleNames(t *testing.T) {
	assert.Empty(t, roleNames(nil))
	assert.Empty(t, roleNames([]registry.SwarmRole{}))
	roles := []registry.SwarmRole{{Name: "coder"}, {Name: "reviewer"}, {Name: "tester"}}
	assert.Equal(t, []string{"coder", "reviewer", "tester"}, roleNames(roles))
}

// TestGitHEAD — returns "" for empty input and for non-git dirs.
// Real git invocation is exercised by integration tests; here we only
// pin the input-validation + fallback contract.
func TestGitHEAD(t *testing.T) {
	assert.Equal(t, "", gitHEAD(context.Background(), ""))

	// A scratch tmpdir without a .git → exec fails → "".
	tmp := t.TempDir()
	assert.Equal(t, "", gitHEAD(context.Background(), tmp))
}

// TestJSONTypeName covers every documented branch including the
// fmt.Sprintf("%T") fallback.
func TestJSONTypeName(t *testing.T) {
	type tc struct {
		in   any
		want string
	}
	cases := []tc{
		{nil, "null"},
		{true, "bool"},
		{false, "bool"},
		{float64(3.14), "number"},
		{float32(1.0), "number"},
		{int(7), "number"},
		{int32(7), "number"},
		{int64(7), "number"},
		{"hello", "string"},
		{[]any{1, 2}, "array"},
		{map[string]any{"a": 1}, "object"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, jsonTypeName(c.in), "input %v", c.in)
	}
	// Fallback: a custom type → "%T" rendering.
	type customType struct{ X int }
	got := jsonTypeName(customType{X: 1})
	assert.Contains(t, got, "customType")
}

// TestToFloat64 covers every numeric branch + the non-numeric default.
func TestToFloat64(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(1.5), 1.5, true},
		{float32(2.0), 2.0, true},
		{int(3), 3.0, true},
		{int32(4), 4.0, true},
		{int64(5), 5.0, true},
		{uint(6), 6.0, true},
		{uint32(7), 7.0, true},
		{uint64(8), 8.0, true},
		{"not a number", 0, false},
		{nil, 0, false},
		{true, 0, false},
		{[]any{1.0}, 0, false},
	}
	for _, tc := range cases {
		got, ok := toFloat64(tc.in)
		assert.Equal(t, tc.ok, ok, "input %v", tc.in)
		if ok {
			assert.Equal(t, tc.want, got)
		}
	}
}

// TestIsEmptyValue covers all the documented type branches.
func TestIsEmptyValue(t *testing.T) {
	t.Run("nil is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue(nil))
	})
	t.Run("whitespace string is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue("   \t\n"))
	})
	t.Run("non-whitespace string is non-empty", func(t *testing.T) {
		assert.False(t, isEmptyValue("a"))
	})
	t.Run("empty slice is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue([]any{}))
	})
	t.Run("non-empty slice is non-empty", func(t *testing.T) {
		assert.False(t, isEmptyValue([]any{1}))
	})
	t.Run("empty map is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue(map[string]any{}))
	})
	t.Run("non-empty map is non-empty", func(t *testing.T) {
		assert.False(t, isEmptyValue(map[string]any{"a": 1}))
	})
	t.Run("zero float64 is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue(float64(0)))
	})
	t.Run("non-zero float is non-empty", func(t *testing.T) {
		assert.False(t, isEmptyValue(float64(1.5)))
	})
	t.Run("zero int is empty", func(t *testing.T) {
		assert.True(t, isEmptyValue(0))
		assert.True(t, isEmptyValue(int32(0)))
		assert.True(t, isEmptyValue(int64(0)))
	})
	t.Run("bool is never empty regardless of value", func(t *testing.T) {
		assert.False(t, isEmptyValue(true))
		assert.False(t, isEmptyValue(false))
	})
}

// TestShort returns the first 7 runes of a SHA; shorter inputs are
// unchanged.
func TestShort(t *testing.T) {
	assert.Equal(t, "", short(""))
	assert.Equal(t, "abc", short("abc"))
	assert.Equal(t, "abc1234", short("abc1234"))
	assert.Equal(t, "abc1234", short("abc1234567890"))
}

// TestTruncateStr is the symmetric helper for string truncation in
// workflow.go.
func TestTruncateStr(t *testing.T) {
	assert.Equal(t, "abc", truncateStr("abc", 10))
	assert.Equal(t, "abc", truncateStr("abc", 3))
	assert.Equal(t, "ab...", truncateStr("abcdef", 2))
}

// TestGitObjectExists exercises the empty-sha early return + the
// "not a repo" failure mode (exec succeeds but cat-file fails).
func TestGitObjectExists(t *testing.T) {
	tmp := t.TempDir()
	assert.False(t, gitObjectExists(context.Background(), tmp, ""), "empty sha → false")
	// Non-git dir → cat-file errors → false.
	assert.False(t, gitObjectExists(context.Background(), tmp, "deadbeef"))
}

// TestPreviewJSON_TruncationAndPassthrough covers both branches.
func TestPreviewJSON_TruncationAndPassthrough(t *testing.T) {
	t.Run("short body passes through", func(t *testing.T) {
		assert.Equal(t, `{"a":1}`, previewJSON([]byte(`{"a":1}`)))
	})
	t.Run("long body truncated with ellipsis", func(t *testing.T) {
		long := []byte(strings.Repeat("x", 400))
		got := previewJSON(long)
		assert.Equal(t, 303, len(got))
		assert.True(t, strings.HasSuffix(got, "..."))
	})
	t.Run("empty body returns empty", func(t *testing.T) {
		assert.Empty(t, previewJSON(nil))
	})
}

// TestOneLine covers newline collapsing + long-string truncation.
func TestOneLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no newlines", "hello world", "hello world"},
		{"single newline", "a\nb", "a ⏎ b"},
		{"multiple newlines", "a\nb\nc", "a ⏎ b ⏎ c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, oneLine(tc.in))
		})
	}
	// Truncation: when collapsed length exceeds 500, the head + suffix
	// land. The truncation marker is "..." (not the unicode ellipsis).
	t.Run("truncation appends ...", func(t *testing.T) {
		long := strings.Repeat("x", 600)
		got := oneLine(long)
		assert.Equal(t, 500, len(got))
		assert.True(t, strings.HasSuffix(got, "..."))
	})
}

// TestWorkflowEffectiveWorkspaceDir covers all four early-return
// branches plus the worktree-preferred-over-shared happy path.
func TestWorkflowEffectiveWorkspaceDir(t *testing.T) {
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}

	t.Run("nil plan returns empty", func(t *testing.T) {
		assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, nil, task))
	})
	t.Run("nil task returns empty", func(t *testing.T) {
		plan := &executionPlan{worktreeDir: "/some/wt"}
		assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, plan, nil))
	})
	t.Run("plan.worktreeDir wins regardless of executor", func(t *testing.T) {
		plan := &executionPlan{worktreeDir: "/tmp/wt-1"}
		assert.Equal(t, "/tmp/wt-1", workflowEffectiveWorkspaceDir(nil, plan, task))
	})
	t.Run("nil executor without worktree returns empty", func(t *testing.T) {
		plan := &executionPlan{}
		assert.Equal(t, "", workflowEffectiveWorkspaceDir(nil, plan, task))
	})
	t.Run("falls back to shared workspace path", func(t *testing.T) {
		e := &Executor{config: &Config{ProjectWorkspacePath: "/var/swarm/workspaces"}}
		plan := &executionPlan{}
		got := workflowEffectiveWorkspaceDir(e, plan, task)
		assert.Equal(t, filepath.Join("/var/swarm/workspaces", "p1"), got)
	})
	t.Run("missing workspace path returns empty", func(t *testing.T) {
		e := &Executor{config: &Config{}}
		plan := &executionPlan{}
		assert.Equal(t, "", workflowEffectiveWorkspaceDir(e, plan, task))
	})
	t.Run("missing project id returns empty", func(t *testing.T) {
		e := &Executor{config: &Config{ProjectWorkspacePath: "/var/swarm"}}
		plan := &executionPlan{}
		assert.Equal(t, "", workflowEffectiveWorkspaceDir(e, plan, &persistence.Task{ID: "t1"}))
	})
}
