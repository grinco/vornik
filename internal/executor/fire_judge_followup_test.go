package executor

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestFireJudgeIfEnabled_NilTask — defensive guard. Used during
// teardown paths where the task pointer may have been cleared.
func TestFireJudgeIfEnabled_NilTask(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.fireJudgeIfEnabled(nil)
	})
}

// TestFireJudgeIfEnabled_NoRunner — runner not wired. Production
// default when the chat-proxy client failed to initialize at
// executor build time. Logs at debug; no goroutine spawned.
func TestFireJudgeIfEnabled_NoRunner(t *testing.T) {
	e := &Executor{
		// Wire the resolver so we get past the no-resolver guard.
		workflows: &MockWorkflowResolver{},
		logger:    zerolog.Nop(),
	}
	// judgeRunner left nil — this is the path under test.
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	require.NotPanics(t, func() {
		e.fireJudgeIfEnabled(task)
	})
}

// TestFireJudgeIfEnabled_NoWorkflowResolver — workflows nil.
func TestFireJudgeIfEnabled_NoWorkflowResolver(t *testing.T) {
	e := &Executor{
		judgeRunner: newRecordingJudge(),
		logger:      zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	require.NotPanics(t, func() {
		e.fireJudgeIfEnabled(task)
	})
}

// TestFireJudgeIfEnabled_ProjectNotFound — workflows wired but
// the project ID doesn't exist (deleted or config-reload race).
// Logs at debug; no goroutine spawned.
func TestFireJudgeIfEnabled_ProjectNotFound(t *testing.T) {
	rec := newRecordingJudge()
	e := &Executor{
		judgeRunner: rec,
		workflows:   &MockWorkflowResolver{projects: map[string]*registry.Project{}},
		logger:      zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t", ProjectID: "missing"}
	e.fireJudgeIfEnabled(task)
	assert.Empty(t, rec.snapshot(),
		"missing project must not trigger the judge")
}

// TestFireJudgeIfEnabled_JudgeNotEnabledOnProject — project
// exists but project.HallucinationJudge.Enabled is false.
// Opt-in flag must gate the goroutine.
func TestFireJudgeIfEnabled_JudgeOptedOut(t *testing.T) {
	rec := newRecordingJudge()
	e := &Executor{
		judgeRunner: rec,
		workflows: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"p": {ID: "p", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: false}},
			},
		},
		logger: zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t", ProjectID: "p"}
	e.fireJudgeIfEnabled(task)
	assert.Empty(t, rec.snapshot(),
		"judge must not fire when project.HallucinationJudge.Enabled is false")
}
