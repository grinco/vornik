package executor

import (
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// recordingAuditRepo captures the ToolAuditFilter handed to List so
// tests can assert that runVerifiers scoped the query correctly.
// Returns whatever Entries the test pre-loaded.
type recordingAuditRepo struct {
	mu      sync.Mutex
	filters []persistence.ToolAuditFilter
	entries []*persistence.ToolAuditEntry
}

func (r *recordingAuditRepo) Log(_ context.Context, _ *persistence.ToolAuditEntry) error {
	return nil
}

func (r *recordingAuditRepo) List(_ context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filters = append(r.filters, f)
	return r.entries, nil
}

func (r *recordingAuditRepo) CountByTool(_ context.Context, _ string) (map[string]int64, error) {
	return nil, nil
}

// TestRunVerifiers_NilWorkflowsResolverReturnsNil — without a
// workflow resolver, runVerifiers has no project source so it
// can't look up Verifiers config. Returns nil silently.
func TestRunVerifiers_NilWorkflowsResolverReturnsNil(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"},
		"step-1", []byte(`{}`), "")
	assert.NoError(t, err)
}

// TestRunVerifiers_ProjectNotFoundReturnsNil — resolver doesn't
// know the project → no verifiers to run.
func TestRunVerifiers_ProjectNotFoundReturnsNil(t *testing.T) {
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{}}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "missing"},
		&persistence.Execution{ID: "e1"},
		"step-1", []byte(`{}`), "")
	assert.NoError(t, err)
}

// TestRunVerifiers_NoVerifiersConfiguredReturnsNil — the project
// exists but its Verifiers slice is empty, so there's nothing to do.
func TestRunVerifiers_NoVerifiersConfiguredReturnsNil(t *testing.T) {
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1"},
		},
	}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"},
		"step-1", []byte(`{}`), "")
	assert.NoError(t, err)
}

// TestRunVerifiers_AllMalformedConfigsReturnsNil — every Verifiers
// entry is malformed, ConfigsFromMaps drops them, and runVerifiers
// returns nil rather than reporting a failure (operator config issue,
// not an agent bug).
func TestRunVerifiers_AllMalformedConfigsReturnsNil(t *testing.T) {
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				// Verifier configs that ConfigsFromMaps cannot interpret.
				Verifiers: []map[string]any{
					{"not-a-valid-type": true},
				},
			},
		},
	}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"},
		"step-1", []byte(`{}`), "")
	assert.NoError(t, err)
}

// TestRunVerifiers_AuditFilteredByStepID — guards Bug A from T-87bf:
// the verifier must see only THIS step's audit rows, not the whole
// execution's. Without the step filter, the recover step inherits the
// research step's 4 blocked fetches and re-fails the same verifier.
func TestRunVerifiers_AuditFilteredByStepID(t *testing.T) {
	ar := &recordingAuditRepo{}
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				// One real verifier so runVerifiers reaches the audit
				// fetch (with no verifiers it short-circuits earlier).
				Verifiers: []map[string]any{
					{"type": "no_status_429_in_audit", "name": "guard"},
				},
			},
		},
	}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver, auditRepo: ar}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "exec-77"},
		"recover", []byte(`{}`), "")
	assert.NoError(t, err)
	require.Len(t, ar.filters, 1)
	f := ar.filters[0]
	require.NotNil(t, f.ExecutionID)
	assert.Equal(t, "exec-77", *f.ExecutionID)
	require.NotNil(t, f.StepID, "verifier must scope audit by step_id (T-87bf regression guard)")
	assert.Equal(t, "recover", *f.StepID)
}

// TestRunVerifiers_AuditNotFilteredByStepIDWhenEmpty — defensive: if a
// caller hands an empty stepID we don't push an empty-string filter
// (would match zero rows); we omit the step filter entirely.
func TestRunVerifiers_AuditNotFilteredByStepIDWhenEmpty(t *testing.T) {
	ar := &recordingAuditRepo{}
	resolver := &MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {
				ID: "p1",
				Verifiers: []map[string]any{
					{"type": "no_status_429_in_audit", "name": "guard"},
				},
			},
		},
	}
	e := &Executor{logger: zerolog.Nop(), workflows: resolver, auditRepo: ar}
	err := e.runVerifiers(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "exec-77"},
		"", []byte(`{}`), "")
	assert.NoError(t, err)
	require.Len(t, ar.filters, 1)
	assert.Nil(t, ar.filters[0].StepID)
}
