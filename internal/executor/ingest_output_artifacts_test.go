package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// listingArtifactRepo lets the test control what List returns plus error
// injection. Other methods are unused by ingestOutputArtifacts.
type listingArtifactRepo struct {
	mu       sync.Mutex
	listRows []*persistence.Artifact
	listErr  error
}

func (s *listingArtifactRepo) Create(_ context.Context, _ *persistence.Artifact) error { return nil }
func (s *listingArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *listingArtifactRepo) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.listRows, nil
}

func TestIngestOutputArtifacts_NilIndexerIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()} // no memoryIndexer
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.ingestOutputArtifacts(context.Background(), task, exec)
	// No panic, nothing more to assert.
}

func TestIngestOutputArtifacts_ListErrorIsLoggedAndContinues(t *testing.T) {
	idx := &stubMemoryIndexer{}
	arts := &listingArtifactRepo{listErr: errors.New("db down")}
	e := &Executor{
		logger:        zerolog.Nop(),
		memoryIndexer: idx,
		artifactRepo:  arts,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	e.ingestOutputArtifacts(context.Background(), task, exec)
	assert.Empty(t, idx.calls)
}

// TestIngestOutputArtifacts_FiltersAndIngestsMarkdown exercises the
// happy path: only OUTPUT artifacts with .md extensions and non-zero
// content go through; transcript / non-md / empty / non-existent
// rows are skipped.
func TestIngestOutputArtifacts_FiltersAndIngestsMarkdown(t *testing.T) {
	idx := &stubMemoryIndexer{}
	tmp := t.TempDir()
	// Write a markdown artifact to disk.
	mdPath := filepath.Join(tmp, "research.md")
	require.NoError(t, os.WriteFile(mdPath, []byte("# Research findings\nLorem ipsum"), 0o600))
	// Write a non-md artifact too — should be filtered out by extension.
	binPath := filepath.Join(tmp, "data.bin")
	require.NoError(t, os.WriteFile(binPath, []byte("binary"), 0o600))
	// Empty file — skipped.
	emptyPath := filepath.Join(tmp, "empty.md")
	require.NoError(t, os.WriteFile(emptyPath, []byte{}, 0o600))
	// Transcript path — skipped via isTranscriptArtifact.
	transcriptPath := filepath.Join(tmp, "plan-response.md")
	require.NoError(t, os.WriteFile(transcriptPath, []byte("# transcript"), 0o600))

	rows := []*persistence.Artifact{
		// OUTPUT + .md + present on disk → should be ingested.
		{ID: "a1", Name: "research.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: mdPath},
		// Not OUTPUT class → skipped.
		{ID: "a2", Name: "input.md", ArtifactClass: persistence.ArtifactClassInput, StoragePath: mdPath},
		// Wrong extension → skipped.
		{ID: "a3", Name: "data.bin", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: binPath},
		// Transcript name → skipped.
		{ID: "a4", Name: "plan-response.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: transcriptPath},
		// Empty StoragePath → skipped.
		{ID: "a5", Name: "x.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: ""},
		// Empty content → skipped.
		{ID: "a6", Name: "empty.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: emptyPath},
		// Read failure (path doesn't exist) → skipped.
		{ID: "a7", Name: "ghost.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: "/var/no/such/path.md"},
		// Nil row → skipped.
		nil,
	}
	arts := &listingArtifactRepo{listRows: rows}
	e := &Executor{
		logger:        zerolog.Nop(),
		memoryIndexer: idx,
		artifactRepo:  arts,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "proj-1"}
	exec := &persistence.Execution{ID: "exec-1"}
	e.ingestOutputArtifacts(context.Background(), task, exec)

	// Only the legitimate research.md should land in the indexer.
	require.Len(t, idx.calls, 1)
	c := idx.calls[0]
	assert.Equal(t, "proj-1", c.projectID)
	assert.Equal(t, "t1", c.taskID)
	assert.Equal(t, "a1", c.artifactID)
	assert.Equal(t, "research.md", c.sourceName)
	assert.Contains(t, c.content, "Lorem ipsum")
}

// TestIngestOutputArtifacts_IndexerErrorIsBestEffort — IngestText
// errors are logged but the iteration continues.
func TestIngestOutputArtifacts_IndexerErrorIsBestEffort(t *testing.T) {
	idx := &stubMemoryIndexer{err: errors.New("indexer down")}
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "research.md")
	require.NoError(t, os.WriteFile(mdPath, []byte("content"), 0o600))
	mdPath2 := filepath.Join(tmp, "summary.md")
	require.NoError(t, os.WriteFile(mdPath2, []byte("summary"), 0o600))

	rows := []*persistence.Artifact{
		{ID: "a1", Name: "research.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: mdPath},
		{ID: "a2", Name: "summary.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: mdPath2},
	}
	arts := &listingArtifactRepo{listRows: rows}
	e := &Executor{
		logger:        zerolog.Nop(),
		memoryIndexer: idx,
		artifactRepo:  arts,
	}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "exec1"}
	assert.NotPanics(t, func() {
		e.ingestOutputArtifacts(context.Background(), task, exec)
	})
	// Both attempted despite the first one returning an error.
	assert.Len(t, idx.calls, 2)
}
