package ui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stubLogSource is a TaskLogSource impl that returns canned text.
type stubLogSource struct {
	text string
	err  error
}

func (s *stubLogSource) TaskLogs(_ context.Context, _ string, _ int) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.text, nil
}

// TestFetchTaskLogs_PrefersLogSource — when both sources exist,
// the live log source wins so the operator sees the most recent
// output the worker emitted.
func TestFetchTaskLogs_PrefersLogSource(t *testing.T) {
	src := &stubLogSource{text: "from-log-source"}
	errMsg := "from-exec-repo"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ErrorMessage: &errMsg}, nil
		},
	}
	srv := NewServer(WithTaskLogSource(src), WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	assert.Equal(t, "from-log-source", got)
}

// TestFetchTaskLogs_FallsBackToExecRepo — log source errors → use
// the execution row's error message field as a last-resort log.
func TestFetchTaskLogs_FallsBackToExecRepo(t *testing.T) {
	src := &stubLogSource{err: errors.New("not running anymore")}
	errMsg := "step failed"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ErrorMessage: &errMsg}, nil
		},
	}
	srv := NewServer(WithTaskLogSource(src), WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	assert.Equal(t, "step failed", got)
}

// TestFetchTaskLogs_EmptyLogSourceFallsThrough — log source returns
// whitespace; the handler treats that as "no logs" and falls back.
func TestFetchTaskLogs_EmptyLogSourceFallsThrough(t *testing.T) {
	src := &stubLogSource{text: "   \n  "}
	errMsg := "fallback"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ErrorMessage: &errMsg}, nil
		},
	}
	srv := NewServer(WithTaskLogSource(src), WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	assert.Equal(t, "fallback", got)
}

// TestFetchTaskLogs_NoSourcesAtAll — empty-state string keeps the
// SSE stream alive (otherwise the partial would 500).
func TestFetchTaskLogs_NoSourcesAtAll(t *testing.T) {
	srv := NewServer()
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	assert.Equal(t, "No logs available yet.", got)
}

// TestFetchTaskLogs_TailTrimming — long logs get trimmed to last N
// lines, preserving operator-readable tail.
func TestFetchTaskLogs_TailTrimming(t *testing.T) {
	long := strings.Join([]string{"line1", "line2", "line3", "line4", "line5"}, "\n")
	src := &stubLogSource{text: long}
	srv := NewServer(WithTaskLogSource(src))
	got := srv.fetchTaskLogs(context.Background(), "t1", 3)
	lines := strings.Split(got, "\n")
	assert.Equal(t, 3, len(lines))
	assert.Equal(t, "line3", lines[0])
	assert.Equal(t, "line5", lines[2])
}

// TestTaskLogsStream_BlankTaskID404 — guards against handler being
// dispatched with an empty path component.
func TestTaskLogsStream_BlankTaskID404(t *testing.T) {
	srv := NewServer()
	req := httptest.NewRequest("GET", "/tasks//logs", nil)
	rec := httptest.NewRecorder()
	srv.TaskLogsStream(rec, req, "")
	assert.Equal(t, 404, rec.Code)
}

// TestTaskLogsStream_ImmediateContextCancelExits — context cancel
// must short-circuit the polling loop without leaking goroutines.
// We exercise this by cancelling before the first send completes
// (the handler does at least one fetch, then selects on ctx.Done()).
func TestTaskLogsStream_ImmediateContextCancelExits(t *testing.T) {
	srv := NewServer(WithTaskLogSource(&stubLogSource{text: "hi"}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	req := httptest.NewRequestWithContext(ctx, "GET", "/tasks/t1/logs", nil)
	rec := httptest.NewRecorder()
	srv.TaskLogsStream(rec, req, "t1")
	// The handler returned; that's the assertion. Body shape
	// is best-effort — what matters is no goroutine remained.
	assert.Equal(t, 200, rec.Code)
}

// TestLogHTML_EscapesAndColorsLevels — bracketed levels become
// coloured spans; tags don't leak through.
func TestLogHTML_EscapesAndColorsLevels(t *testing.T) {
	html := logHTML("[INFO] hello <script>")
	assert.Contains(t, html, "text-blue-400")
	assert.Contains(t, html, "&lt;script&gt;")
	assert.NotContains(t, html, "<script>")
}

func TestLogHTML_EmptyShowsPlaceholder(t *testing.T) {
	html := logHTML("")
	assert.Contains(t, html, "No logs available yet.")
}

func TestSseData_OneLineHasTrailingBlankLine(t *testing.T) {
	got := sseData("alpha")
	assert.Equal(t, "data: alpha\n\n", got)
}

func TestSseData_MultiLineAllPrefixed(t *testing.T) {
	got := sseData("alpha\nbeta")
	assert.Equal(t, "data: alpha\ndata: beta\n\n", got)
}

func TestTrimLogLines_NoMaxReturnsOriginal(t *testing.T) {
	got := trimLogLines("a\nb\nc", 0)
	assert.Equal(t, "a\nb\nc", got)
}

func TestTrimLogLines_ShorterThanMaxReturnsOriginal(t *testing.T) {
	got := trimLogLines("a\nb", 5)
	assert.Equal(t, "a\nb", got)
}

func TestTrimLogLines_LongerThanMaxKeepsTail(t *testing.T) {
	got := trimLogLines("a\nb\nc\nd\ne", 2)
	assert.Equal(t, "d\ne", got)
}
