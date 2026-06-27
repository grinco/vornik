package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// stubMemoryIndexer records every IngestText call.
type stubMemoryIndexer struct {
	mu           sync.Mutex
	calls        []ingestCall
	scopePatches []scopePatchCall
	err          error
}

type ingestCall struct {
	projectID  string
	taskID     string
	artifactID string
	sourceName string
	content    string
}

type scopePatchCall struct {
	projectID  string
	artifactID string
	repoScope  string
}

func (s *stubMemoryIndexer) IngestText(_ context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, ingestCall{projectID, taskID, artifactID, sourceName, content})
	return s.err
}

// PatchScopeByArtifact satisfies the MemoryIndexer interface after
// migration 75/76 grew the executor → indexer surface. The stub just
// records the call so the new B-4 scope-stamping tests can assert
// what scope was applied.
func (s *stubMemoryIndexer) PatchScopeByArtifact(_ context.Context, projectID, artifactID, repoScope string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopePatches = append(s.scopePatches, scopePatchCall{projectID, artifactID, repoScope})
	return nil
}

func TestIngestTradingActivity_NilIndexerIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no memoryIndexer
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.ingestTradingActivity(context.Background(), task, exec, []byte(`{"placed":[{"symbol":"AAPL"}]}`))
	// No panic, nothing more to assert — the function silently no-ops.
}

func TestIngestTradingActivity_EmptyResultIsNoop(t *testing.T) {
	idx := &stubMemoryIndexer{}
	e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.ingestTradingActivity(context.Background(), task, exec, nil)
	e.ingestTradingActivity(context.Background(), task, exec, []byte{})
	assert.Empty(t, idx.calls)
}

func TestIngestTradingActivity_InvalidJSONIsNoop(t *testing.T) {
	idx := &stubMemoryIndexer{}
	e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.ingestTradingActivity(context.Background(), task, exec, []byte(`{not-json`))
	assert.Empty(t, idx.calls)
}

func TestIngestTradingActivity_AllArraysEmptyIsNoop(t *testing.T) {
	idx := &stubMemoryIndexer{}
	e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	body := []byte(`{"placed":[],"skipped":[],"fills_observed":[]}`)
	e.ingestTradingActivity(context.Background(), task, exec, body)
	assert.Empty(t, idx.calls)
}

func TestIngestTradingActivity_RendersAllSections(t *testing.T) {
	idx := &stubMemoryIndexer{}
	e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
	task := &persistence.Task{ID: "t1", ProjectID: "proj-1"}
	exec := &persistence.Execution{ID: "exec-1"}
	body := []byte(`{
		"placed":[
			{"symbol":"AAPL","broker_order_id":"bo-1","status":"submitted"},
			{"symbol":"MSFT","broker_order_id":"bo-2","status":"filled"}
		],
		"skipped":[
			{"symbol":"NVDA","reason":"cooldown","detail":"3h cooling"},
			{"symbol":"TSLA","reason":"limit_violated","cancel_reason":"price_drift","cancel_detail":"$5 over"}
		],
		"fills_observed":[
			{"symbol":"GOOG","qty":12.5,"price":175.50}
		]
	}`)

	e.ingestTradingActivity(context.Background(), task, exec, body)

	require.Len(t, idx.calls, 1)
	c := idx.calls[0]
	assert.Equal(t, "proj-1", c.projectID)
	assert.Equal(t, "t1", c.taskID)
	assert.Equal(t, "", c.artifactID)
	assert.Equal(t, "trading-activity-exec-1.md", c.sourceName)

	// All three sections rendered with their values.
	body2 := c.content
	assert.Contains(t, body2, "## Fills observed")
	assert.Contains(t, body2, "GOOG: 12.5000 @ $175.50")
	assert.Contains(t, body2, "## Orders placed")
	assert.Contains(t, body2, "AAPL: status=submitted broker_id=bo-1")
	assert.Contains(t, body2, "MSFT: status=filled broker_id=bo-2")
	assert.Contains(t, body2, "## Skipped / cancelled")
	assert.Contains(t, body2, "NVDA: cooldown (3h cooling)")
	assert.Contains(t, body2, "TSLA: limit_violated · cancel_reason=price_drift ($5 over)")
	// Source line carries the execution ID.
	assert.True(t, strings.Contains(body2, "exec-1"))
}

func TestIngestTradingActivity_IndexerErrorIsLogged(t *testing.T) {
	idx := &stubMemoryIndexer{err: errors.New("indexer down")}
	e := &Executor{logger: zerolog.Nop(), memoryIndexer: idx}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	// Best-effort: error is silenced via logger.
	body := []byte(`{"placed":[{"symbol":"AAPL","status":"submitted"}]}`)
	assert.NotPanics(t, func() {
		e.ingestTradingActivity(context.Background(), task, exec, body)
	})
}
