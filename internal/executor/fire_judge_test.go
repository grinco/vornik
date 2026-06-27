package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubJudgeRunner records every Run call so fireJudgeIfEnabled tests
// can assert the async fire-or-skip decision deterministically.
type stubJudgeRunner struct {
	mu    sync.Mutex
	calls int
	err   error
	done  chan struct{}
}

func (s *stubJudgeRunner) Run(_ context.Context, _ *persistence.Task) error {
	s.mu.Lock()
	s.calls++
	defer s.mu.Unlock()
	if s.done != nil {
		close(s.done)
	}
	return s.err
}

func TestFireJudgeIfEnabled_NilTaskIsNoop(t *testing.T) {
	r := &stubJudgeRunner{}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r}
	e.fireJudgeIfEnabled(nil)
	assert.Equal(t, 0, r.calls)
}

func TestFireJudgeIfEnabled_NoRunnerIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no judgeRunner
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1"})
}

func TestFireJudgeIfEnabled_NoWorkflowResolverIsNoop(t *testing.T) {
	r := &stubJudgeRunner{}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1"})
	assert.Equal(t, 0, r.calls)
}

func TestFireJudgeIfEnabled_ProjectNotFoundIsNoop(t *testing.T) {
	r := &stubJudgeRunner{}
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{}}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r, workflows: resolver}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "missing"})
	assert.Equal(t, 0, r.calls)
}

func TestFireJudgeIfEnabled_DisabledOnProjectIsNoop(t *testing.T) {
	r := &stubJudgeRunner{}
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{
		"p1": {HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: false}},
	}}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r, workflows: resolver}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1"})
	assert.Equal(t, 0, r.calls)
}

func TestFireJudgeIfEnabled_EnabledFiresAsyncRunner(t *testing.T) {
	done := make(chan struct{})
	r := &stubJudgeRunner{done: done}
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{
		"p1": {HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}},
	}}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r, workflows: resolver}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusCompleted})
	// fireJudgeIfEnabled spawns a goroutine; wait on the done channel
	// from inside Run so the test doesn't time out.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("judge runner never fired")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Equal(t, 1, r.calls)
}

func TestFireJudgeIfEnabled_RunnerErrorIsLogged(t *testing.T) {
	done := make(chan struct{})
	r := &stubJudgeRunner{err: errors.New("judge crash"), done: done}
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{
		"p1": {HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}},
	}}
	e := &Executor{logger: zerolog.Nop(), judgeRunner: r, workflows: resolver}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusFailed})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("judge runner never fired")
	}
	require.Equal(t, 1, r.calls)
}
