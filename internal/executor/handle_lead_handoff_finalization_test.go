package executor

import (
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// captureCompletionNotifier records the last NotifyTaskCompleted call.
type captureCompletionNotifier struct {
	mu      sync.Mutex
	task    *persistence.Task
	success bool
	message string
	calls   int
}

func (c *captureCompletionNotifier) NotifyTaskCompleted(_ context.Context, t *persistence.Task, success bool, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.task = t
	c.success = success
	c.message = message
}

func TestHandleLeadHandoffFinalization_UsesDefaultMessageWhenResultEmpty(t *testing.T) {
	notif := &captureCompletionNotifier{}
	e := &Executor{
		logger:   zerolog.Nop(),
		notifier: notif,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "cid1", nil)

	require.Equal(t, 1, notif.calls)
	assert.Same(t, task, notif.task)
	assert.True(t, notif.success)
	assert.Contains(t, notif.message, "AWAITING_INPUT")
}

func TestHandleLeadHandoffFinalization_OverridesMessageFromResult(t *testing.T) {
	notif := &captureCompletionNotifier{}
	e := &Executor{
		logger:   zerolog.Nop(),
		notifier: notif,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	result := []byte(`{"message":"need approval on plan"}`)
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "cid1", result)

	require.Equal(t, 1, notif.calls)
	assert.Equal(t, "need approval on plan", notif.message)
}

func TestHandleLeadHandoffFinalization_InvalidJSONKeepsDefault(t *testing.T) {
	notif := &captureCompletionNotifier{}
	e := &Executor{
		logger:   zerolog.Nop(),
		notifier: notif,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.handleLeadHandoffFinalization(context.Background(), task, exec, "cid1", []byte(`{not-json`))

	require.Equal(t, 1, notif.calls)
	assert.Contains(t, notif.message, "AWAITING_INPUT")
}

func TestHandleLeadHandoffFinalization_NilNotifierIsSafe(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no notifier
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	assert.NotPanics(t, func() {
		e.handleLeadHandoffFinalization(context.Background(), task, exec, "cid1", nil)
	})
}
