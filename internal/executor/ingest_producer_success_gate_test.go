package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// oneMdArtifactRepo + oneMdOnDisk build the minimal fixture the gate tests
// need: a single OUTPUT .md artifact present on disk, so a non-skipped call
// reaches memoryIndexer.IngestText exactly once.
func oneMdOnDisk(t *testing.T) *listingArtifactRepo {
	t.Helper()
	tmp := t.TempDir()
	mdPath := filepath.Join(tmp, "findings.md")
	require.NoError(t, os.WriteFile(mdPath, []byte("# Findings\nthe right person is X"), 0o600))
	return &listingArtifactRepo{listRows: []*persistence.Artifact{
		{ID: "a1", Name: "findings.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: mdPath},
	}}
}

func executorWithGate(idx *stubMemoryIndexer, ar *listingArtifactRepo, gate *bool) *Executor {
	return &Executor{
		logger:        zerolog.Nop(),
		memoryIndexer: idx,
		artifactRepo:  ar,
		config:        &Config{RequireProducerSuccess: gate},
	}
}

// TestIngestOutputArtifacts_SkipsFailedTask — a FAILED producing task's
// OUTPUT artifacts are NOT ingested (LLD §12). The gate is default-on.
func TestIngestOutputArtifacts_SkipsFailedTask(t *testing.T) {
	idx := &stubMemoryIndexer{}
	ar := oneMdOnDisk(t)
	e := executorWithGate(idx, ar, nil) // nil → default-on
	task := &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusFailed}
	e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	assert.Empty(t, idx.calls, "a FAILED task's outputs must NOT be ingested")
}

// TestIngestOutputArtifacts_SkipsAwaitingInputTask — the incident case: a
// dossier task that parked at AWAITING_INPUT (it couldn't resolve the person)
// must NOT have its wrong-people candidates ingested.
func TestIngestOutputArtifacts_SkipsAwaitingInputTask(t *testing.T) {
	idx := &stubMemoryIndexer{}
	ar := oneMdOnDisk(t)
	e := executorWithGate(idx, ar, nil)
	task := &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusAwaitingInput}
	e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	assert.Empty(t, idx.calls, "a parked (AWAITING_INPUT) task's outputs must NOT be ingested")
}

// TestIngestOutputArtifacts_SkipsCancelledAndClosed — CANCELLED + CLOSED are
// also skipped (CLOSED = operator-judged close, per backlog "FAILED/CLOSED").
func TestIngestOutputArtifacts_SkipsCancelledAndClosed(t *testing.T) {
	for _, status := range []persistence.TaskStatus{persistence.TaskStatusCancelled, persistence.TaskStatusClosed, persistence.TaskStatusAwaitingExternal} {
		idx := &stubMemoryIndexer{}
		ar := oneMdOnDisk(t)
		e := executorWithGate(idx, ar, nil)
		task := &persistence.Task{ID: "t", ProjectID: "p", Status: status}
		e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
		assert.Empty(t, idx.calls, "status %s must skip ingest", status)
	}
}

// TestIngestOutputArtifacts_IngestsCompletedTask — a COMPLETED task's outputs
// ARE ingested (the success path).
func TestIngestOutputArtifacts_IngestsCompletedTask(t *testing.T) {
	idx := &stubMemoryIndexer{}
	ar := oneMdOnDisk(t)
	e := executorWithGate(idx, ar, nil)
	task := &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusCompleted}
	e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	require.Len(t, idx.calls, 1, "a COMPLETED task's single OUTPUT .md must be ingested")
	assert.Equal(t, "p", idx.calls[0].projectID)
}

// TestIngestOutputArtifacts_PassthroughWhenDisabled — explicit
// RequireProducerSuccess=false → a FAILED task IS ingested (byte-identical
// to pre-gate behaviour).
func TestIngestOutputArtifacts_PassthroughWhenDisabled(t *testing.T) {
	idx := &stubMemoryIndexer{}
	ar := oneMdOnDisk(t)
	off := false
	e := executorWithGate(idx, ar, &off)
	task := &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusFailed}
	e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	require.Len(t, idx.calls, 1, "disabled gate must passthrough — FAILED task ingests (pre-gate behaviour)")
}

// TestIngestOutputArtifacts_SkipMetricBumped — a skipped ingest bumps the
// vornik_executor_ingest_skipped_producer_failed_total{project_id,status} counter.
func TestIngestOutputArtifacts_SkipMetricBumped(t *testing.T) {
	reg := prometheus.NewRegistry()
	idx := &stubMemoryIndexer{}
	ar := oneMdOnDisk(t)
	e := &Executor{
		logger:        zerolog.Nop(),
		memoryIndexer: idx,
		artifactRepo:  ar,
		config:        &Config{}, // default-on (nil RequireProducerSuccess)
		metrics:       NewMetrics(reg),
	}
	task := &persistence.Task{ID: "t", ProjectID: "proj-gate", Status: persistence.TaskStatusAwaitingInput}
	e.ingestOutputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	assert.Empty(t, idx.calls)
	v := testutil.ToFloat64(e.metrics.IngestSkippedProducerFailedTotal.WithLabelValues("proj-gate", "awaiting_input"))
	assert.Equal(t, 1.0, v, "the skip counter must bump once for the parked task")
	// A different status labels separately.
	task2 := &persistence.Task{ID: "t2", ProjectID: "proj-gate", Status: persistence.TaskStatusFailed}
	e.ingestOutputArtifacts(context.Background(), task2, &persistence.Execution{ID: "x2"})
	v2 := testutil.ToFloat64(e.metrics.IngestSkippedProducerFailedTotal.WithLabelValues("proj-gate", "failed"))
	assert.Equal(t, 1.0, v2, "failed-status skips label separately")
	// Completed tasks do NOT bump the counter.
	task3 := &persistence.Task{ID: "t3", ProjectID: "proj-gate", Status: persistence.TaskStatusCompleted}
	e.ingestOutputArtifacts(context.Background(), task3, &persistence.Execution{ID: "x3"})
	vAwait := testutil.ToFloat64(e.metrics.IngestSkippedProducerFailedTotal.WithLabelValues("proj-gate", "awaiting_input"))
	assert.Equal(t, 1.0, vAwait, "completed tasks do not bump the skip counter")
}
