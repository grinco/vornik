// Package ui: tests for fetchTaskLogs — the helper that pulls log
// text from either the configured task-log source or the execution
// repo's ErrorMessage fallback.
package ui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

type stubTaskLogSource struct {
	logs string
	err  error
}

func (s *stubTaskLogSource) TaskLogs(_ context.Context, _ string, _ int) (string, error) {
	return s.logs, s.err
}

func TestFetchTaskLogs_NoSourcesConfigured(t *testing.T) {
	srv := &Server{}
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "No logs available yet") {
		t.Errorf("got %q", got)
	}
}

func TestFetchTaskLogs_TaskLogSourceSuccess(t *testing.T) {
	srv := NewServer(WithTaskLogSource(&stubTaskLogSource{logs: "line1\nline2\nline3"}))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "line1") {
		t.Errorf("got %q", got)
	}
}

func TestFetchTaskLogs_TaskLogSourceError_FallsThrough(t *testing.T) {
	// Log source returns error → handler falls through to exec
	// repo fallback. With both missing/erroring, we should get the
	// default message.
	srv := NewServer(WithTaskLogSource(&stubTaskLogSource{err: errors.New("source down")}))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "No logs available yet") {
		t.Errorf("got %q", got)
	}
}

func TestFetchTaskLogs_TaskLogSourceEmpty_FallsThrough(t *testing.T) {
	srv := NewServer(WithTaskLogSource(&stubTaskLogSource{logs: "   "}))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "No logs available yet") {
		t.Errorf("expected fallback when source returns whitespace; got %q", got)
	}
}

func TestFetchTaskLogs_ExecutionFallback(t *testing.T) {
	errMsg := "container OOM\nexit 137"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: "exec-1", ErrorMessage: &errMsg}, nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "container OOM") {
		t.Errorf("got %q", got)
	}
}

func TestFetchTaskLogs_ExecutionNotFound(t *testing.T) {
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return nil, errors.New("not found")
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	if !strings.Contains(got, "No logs available yet") {
		t.Errorf("got %q", got)
	}
}
