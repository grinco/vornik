package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// stubArtifactStore returns canned bytes per artifact ID for the
// synchronous (no-queue) ingest fallback.
type stubArtifactStore struct {
	bytesByID map[string][]byte
}

func (s *stubArtifactStore) Store(_ context.Context, _, _, _, _, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactStore) Retrieve(_ context.Context, id string) ([]byte, error) {
	return s.bytesByID[id], nil
}

func ingestInputPayload(t *testing.T, ids, files []string, repoScope string) []byte {
	t.Helper()
	ctx := map[string]any{"inputArtifactIDs": ids, "inputFiles": files}
	if repoScope != "" {
		ctx["repo_scope"] = repoScope
	}
	b, err := json.Marshal(map[string]any{"context": ctx})
	require.NoError(t, err)
	return b
}

// TestIngestInputArtifacts_EnqueuesByID is the core of the hardened
// (agent-free) bulk-ingest path: each staged input artifact is enqueued
// by ID with its repo_scope, no LLM and no file bytes in this process.
func TestIngestInputArtifacts_EnqueuesByID(t *testing.T) {
	mi := &stubMemoryIndexer{}
	q := &stubIngestQueue{}
	e := &Executor{
		memoryIndexer: mi,
		ingestQueue:   q,
		workflows: &MockWorkflowResolver{workflows: map[string]*registry.Workflow{
			"companion-rag-ingest": {ID: "companion-rag-ingest", IngestInputArtifacts: true},
		}},
		logger: zerolog.Nop(),
	}
	task := &persistence.Task{
		ID: "t", ProjectID: "p",
		Payload: ingestInputPayload(t,
			[]string{"in_a", "in_b"},
			[]string{"/store/in_a/CHANGELOG.md", "/store/in_b/notes.md"},
			"github.com/grinco/vornik"),
	}
	exec := &persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"}

	e.ingestInputArtifacts(context.Background(), task, exec)

	require.Len(t, q.items, 2, "one enqueue per staged input artifact")
	assert.Empty(t, mi.calls, "queue success must skip synchronous IngestText")
	assert.Equal(t, "in_a", q.items[0].SourceArtifactID)
	assert.Equal(t, "in_b", q.items[1].SourceArtifactID)
	require.NotNil(t, q.items[0].RepoScope)
	assert.Equal(t, "github.com/grinco/vornik", *q.items[0].RepoScope)
	assert.Equal(t, "rag-ingester", q.items[0].ProducerRole, "step-less workflow defaults producer role")
}

// TestIngestInputArtifacts_SyncFallback — no queue wired: retrieve bytes
// by ID and ingest directly, stamping repo_scope.
func TestIngestInputArtifacts_SyncFallback(t *testing.T) {
	mi := &stubMemoryIndexer{}
	store := &stubArtifactStore{bytesByID: map[string][]byte{"in_a": []byte("# doc\nbody")}}
	e := &Executor{
		memoryIndexer: mi,
		artifactStore: store,
		logger:        zerolog.Nop(),
	}
	task := &persistence.Task{
		ID: "t", ProjectID: "p",
		Payload: ingestInputPayload(t, []string{"in_a"}, []string{"/store/in_a/doc.md"}, "repo/x"),
	}
	exec := &persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"}

	e.ingestInputArtifacts(context.Background(), task, exec)

	require.Len(t, mi.calls, 1, "sync fallback must IngestText once")
	assert.Equal(t, "in_a", mi.calls[0].artifactID)
	assert.Equal(t, "doc.md", mi.calls[0].sourceName, "name derived from inputFiles basename")
	assert.Equal(t, "# doc\nbody", mi.calls[0].content)
	require.Len(t, mi.scopePatches, 1, "repo_scope must be stamped on the synchronously-ingested chunks")
	assert.Equal(t, "repo/x", mi.scopePatches[0].repoScope)
}

// TestIngestInputArtifacts_NoInputs — a task with no staged input
// artifacts is a clean no-op (never enqueues / ingests).
func TestIngestInputArtifacts_NoInputs(t *testing.T) {
	mi := &stubMemoryIndexer{}
	q := &stubIngestQueue{}
	e := &Executor{memoryIndexer: mi, ingestQueue: q, logger: zerolog.Nop()}
	e.ingestInputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p", Payload: []byte(`{"context":{}}`)},
		&persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"})
	assert.Empty(t, q.items)
	assert.Empty(t, mi.calls)
}
