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
	"vornik.io/vornik/internal/registry"
)

// inMemArtifactRepo is a configurable artifactRepo that returns
// pre-seeded artifacts for List. Sufficient to drive the ingest
// path without a Postgres dependency.
type inMemArtifactRepo struct {
	mu        sync.Mutex
	artifacts []*persistence.Artifact
	listErr   error
	created   []*persistence.Artifact
	lastFilt  persistence.ArtifactFilter
}

func (r *inMemArtifactRepo) Create(_ context.Context, a *persistence.Artifact) error {
	r.mu.Lock()
	r.created = append(r.created, a)
	r.mu.Unlock()
	return nil
}
func (r *inMemArtifactRepo) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (r *inMemArtifactRepo) List(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastFilt = f
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.artifacts, nil
}

// stubIngestQueue captures every Enqueue call so tests can assert
// the queue path was taken. Used to verify the executor routes
// through the Phase-1 ingest queue when wired.
type stubIngestQueue struct {
	mu    sync.Mutex
	items []*persistence.IngestQueueItem
	err   error
}

func (s *stubIngestQueue) Enqueue(_ context.Context, it *persistence.IngestQueueItem) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, it)
	return s.err
}

func writeArtifactFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// TestIngestOutputArtifacts_NilIndexer — without memoryIndexer
// the helper short-circuits. This is the production default for
// deployments that don't enable memory.
func TestIngestOutputArtifacts_NilIndexer(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	require.NotPanics(t, func() {
		e.ingestOutputArtifacts(context.Background(),
			&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"})
	})
}

// TestIngestOutputArtifacts_RepoError — best-effort: a List
// failure is logged and the function returns without ingesting.
func TestIngestOutputArtifacts_RepoListError(t *testing.T) {
	mi := &stubMemoryIndexer{}
	ar := &inMemArtifactRepo{listErr: errors.New("db blip")}
	e := &Executor{
		memoryIndexer: mi,
		artifactRepo:  ar,
		logger:        zerolog.Nop(),
	}
	require.NotPanics(t, func() {
		e.ingestOutputArtifacts(context.Background(),
			&persistence.Task{ID: "t"}, &persistence.Execution{ID: "x"})
	})
	assert.Empty(t, mi.calls, "List error must short-circuit ingestion")
}

// TestIngestOutputArtifacts_FiltersNonOutputAndTranscripts — the
// helper must skip:
//   - nil rows (defensive)
//   - INPUT-class artifacts (only OUTPUT goes to memory)
//   - *-response.md transcripts (execution logs, not content)
//   - non-markdown extensions
//   - artifacts without a StoragePath set
//   - empty files
func TestIngestOutputArtifacts_Filters(t *testing.T) {
	dir := t.TempDir()
	keep := writeArtifactFile(t, dir, "summary.md", "# Real content")
	empty := writeArtifactFile(t, dir, "empty.md", "")

	ar := &inMemArtifactRepo{
		artifacts: []*persistence.Artifact{
			nil, // defensive nil entry
			{ID: "a-input", Name: "input.md", ArtifactClass: persistence.ArtifactClassInput, StoragePath: keep},
			{ID: "a-transcript", Name: "plan-response.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: keep},
			{ID: "a-non-md", Name: "report.txt", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: keep},
			{ID: "a-no-path", Name: "no-path.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: ""},
			{ID: "a-empty", Name: "empty.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: empty},
			{ID: "a-keep", Name: "summary.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: keep},
		},
	}
	mi := &stubMemoryIndexer{}
	e := &Executor{
		memoryIndexer: mi,
		artifactRepo:  ar,
		logger:        zerolog.Nop(),
	}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "task-1", ProjectID: "proj-1"},
		&persistence.Execution{ID: "exec-1"})

	// Only the legitimate OUTPUT markdown row passes every filter.
	require.Len(t, mi.calls, 1, "exactly one ingest call must survive filtering")
	assert.Equal(t, "proj-1", mi.calls[0].projectID)
	assert.Equal(t, "task-1", mi.calls[0].taskID)
	assert.Equal(t, "a-keep", mi.calls[0].artifactID)
	assert.Equal(t, "summary.md", mi.calls[0].sourceName)
	assert.Contains(t, mi.calls[0].content, "Real content")
}

// TestIngestOutputArtifacts_MissingFile_LogsAndContinues — when
// the artifact row points to a path that doesn't exist on disk
// (sweeper deleted the file, FS race), the helper logs and moves
// on. This protects the executor from one bad row stranding the
// rest of the run.
func TestIngestOutputArtifacts_MissingFile(t *testing.T) {
	dir := t.TempDir()
	keep := writeArtifactFile(t, dir, "summary.md", "valid content")

	ar := &inMemArtifactRepo{
		artifacts: []*persistence.Artifact{
			{ID: "a-gone", Name: "gone.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: filepath.Join(dir, "no-such-file.md")},
			{ID: "a-keep", Name: "summary.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: keep},
		},
	}
	mi := &stubMemoryIndexer{}
	e := &Executor{
		memoryIndexer: mi,
		artifactRepo:  ar,
		logger:        zerolog.Nop(),
	}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})
	require.Len(t, mi.calls, 1, "missing file must not abort ingestion of subsequent valid artifacts")
	assert.Equal(t, "a-keep", mi.calls[0].artifactID)
}

// TestIngestOutputArtifacts_QueuePath_HappyPath — when the
// ingestQueue is wired, the helper enqueues per artifact and
// does NOT call the synchronous memoryIndexer.
func TestIngestOutputArtifacts_QueuePath(t *testing.T) {
	dir := t.TempDir()
	p := writeArtifactFile(t, dir, "doc.md", "queued payload")

	ar := &inMemArtifactRepo{
		artifacts: []*persistence.Artifact{
			{ID: "a1", Name: "doc.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p},
		},
	}
	mi := &stubMemoryIndexer{}
	q := &stubIngestQueue{}
	e := &Executor{
		memoryIndexer: mi,
		artifactRepo:  ar,
		ingestQueue:   q,
		workflows: &MockWorkflowResolver{workflows: map[string]*registry.Workflow{
			"wf-x": {
				ID: "wf-x",
				Steps: map[string]registry.WorkflowStep{
					"write": {Type: "agent", Role: "writer"},
				},
			},
		}},
		logger: zerolog.Nop(),
	}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x", WorkflowID: "wf-x", CompletedSteps: []string{"write"}})

	require.Len(t, q.items, 1, "queue path must enqueue one item per artifact")
	assert.Empty(t, mi.calls, "queue success must skip synchronous IngestText")
	assert.Equal(t, "p", q.items[0].ProjectID)
	assert.Equal(t, "a1", q.items[0].SourceArtifactID)
	assert.Equal(t, "writer", q.items[0].ProducerRole,
		"producer_role must be derived from the workflow step → role mapping")
}

// TestIngestOutputArtifacts_QueueErrorFallsBackToSync — when
// Enqueue fails, the helper falls back to IngestText. The
// fallback recorder (a counter callback) must fire so dashboards
// can see the regression.
func TestIngestOutputArtifacts_QueueErrorFallbackSync(t *testing.T) {
	dir := t.TempDir()
	p := writeArtifactFile(t, dir, "doc.md", "content")

	ar := &inMemArtifactRepo{
		artifacts: []*persistence.Artifact{
			{ID: "a1", Name: "doc.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: p},
		},
	}
	mi := &stubMemoryIndexer{}
	q := &stubIngestQueue{err: errors.New("queue down")}

	var fbProjects []string
	e := &Executor{
		memoryIndexer: mi,
		artifactRepo:  ar,
		ingestQueue:   q,
		ingestEnqueueFallbackRecorder: func(projectID string) {
			fbProjects = append(fbProjects, projectID)
		},
		logger: zerolog.Nop(),
	}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p"},
		&persistence.Execution{ID: "x"})

	require.Len(t, mi.calls, 1, "queue error must fall back to synchronous IngestText")
	assert.Equal(t, []string{"p"}, fbProjects,
		"fallback recorder must be invoked with the affected project ID")
}
