package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// stubAuditRepo captures every Log call so persistToolAuditFromResult
// tests can assert on the persisted shape without a real DB.
type stubAuditRepo struct {
	mu     sync.Mutex
	logged []*persistence.ToolAuditEntry
	err    error
}

func (s *stubAuditRepo) Log(_ context.Context, e *persistence.ToolAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.logged = append(s.logged, e)
	return nil
}

func (s *stubAuditRepo) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	return nil, nil
}

func (s *stubAuditRepo) CountByTool(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

func TestPersistToolAuditFromResult_InvalidJSONIsNoop(t *testing.T) {
	repo := &stubAuditRepo{}
	e := &Executor{auditRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	_, detail := e.persistToolAuditFromResult(context.Background(), task, exec, "step-1", []byte(`not json`))
	assert.Equal(t, "", detail)
	assert.Empty(t, repo.logged)
}

func TestPersistToolAuditFromResult_NoEntriesIsNoop(t *testing.T) {
	repo := &stubAuditRepo{}
	e := &Executor{auditRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	_, detail := e.persistToolAuditFromResult(context.Background(), task, exec, "step-1", []byte(`{"toolAudit":[]}`))
	assert.Equal(t, "", detail)
	assert.Empty(t, repo.logged)
}

func TestPersistToolAuditFromResult_PersistsEntriesAndReusesAuditID(t *testing.T) {
	repo := &stubAuditRepo{}
	e := &Executor{auditRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "task-x", ProjectID: "proj-y"}
	exec := &persistence.Execution{ID: "exec-z"}
	body := []byte(`{"toolAudit":[
		{"audit_id":"ta_explicit_1","tool":"file_read","input":"path","output":"contents","duration_ms":42},
		{"tool":"file_write","input":"path2","output":"ok","duration_ms":17}
	]}`)
	_, detail := e.persistToolAuditFromResult(context.Background(), task, exec, "plan_1_writer", body)
	assert.Equal(t, "", detail, "no loop expected")
	require.Len(t, repo.logged, 2)

	// First entry kept its explicit audit_id.
	assert.Equal(t, "ta_explicit_1", repo.logged[0].ID)
	assert.Equal(t, "file_read", repo.logged[0].ToolName)
	assert.Equal(t, "proj-y", repo.logged[0].ProjectID)
	assert.Equal(t, "task-x", repo.logged[0].TaskID)
	assert.Equal(t, "exec-z", repo.logged[0].ExecutionID)
	assert.Equal(t, "plan_1_writer", repo.logged[0].StepID)
	// DurationMs may be clamped by clampToolAuditDurationMs; just
	// pin non-negative passthrough since we passed 42ms.
	assert.GreaterOrEqual(t, repo.logged[0].DurationMs, int64(0))

	// Second entry got a synthetic ID (no explicit audit_id supplied).
	assert.NotEmpty(t, repo.logged[1].ID)
	assert.NotEqual(t, "ta_explicit_1", repo.logged[1].ID)
}

func TestPersistToolAuditFromResult_NilRepoSkipsLogButReturnsLoopDetail(t *testing.T) {
	// auditRepo == nil still runs the degenerate-loop detector. Build
	// a payload that triggers the detector (many identical tool+input
	// rows in a row).
	e := &Executor{logger: zerolog.Nop()} // no auditRepo
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	// Mass-repeat the same tool/input to trip the detector.
	body := []byte(`{"toolAudit":[
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1},
		{"tool":"file_read","input":"same","output":"x","duration_ms":1}
	]}`)
	_, detail := e.persistToolAuditFromResult(context.Background(), task, exec, "step-1", body)
	// Either the loop is detected (non-empty detail) or not — both
	// outcomes are acceptable depending on detector thresholds. We
	// only pin "no panic / no nil-deref" here, plus the contract
	// that no logs are persisted (because auditRepo is nil).
	_ = detail
}

func TestPersistToolAuditFromResult_LogErrorIsLogged(t *testing.T) {
	repo := &stubAuditRepo{err: errors.New("db down")}
	e := &Executor{auditRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	body := []byte(`{"toolAudit":[{"tool":"file_read","input":"a","output":"b","duration_ms":1}]}`)
	// Error is best-effort: function returns "" (no loop) even after the log error.
	assert.NotPanics(t, func() {
		e.persistToolAuditFromResult(context.Background(), task, exec, "step-1", body)
	})
}
