package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/memoryfirewall"
)

func freshMetrics() *Metrics {
	return NewMetrics(prometheus.NewRegistry())
}

func newTestIndexer(t *testing.T) (*Indexer, sqlmock.Sqlmock, func()) {
	t.Helper()
	r, mock, cleanup := newRepo(t)
	cfg := Config{ChunkTokens: 512, ChunkOverlap: 0}
	idx := NewIndexer(cfg, r, nil, zerolog.Nop())
	return idx, mock, cleanup
}

func TestNewIndexer_Fields(t *testing.T) {
	r, _, cleanup := newRepo(t)
	defer cleanup()
	cfg := Config{ChunkTokens: 256}
	idx := NewIndexer(cfg, r, nil, zerolog.Nop())
	if idx.cfg.ChunkTokens != 256 || idx.repo != r {
		t.Fatalf("wiring: %+v", idx)
	}
	idx.setMetrics(freshMetrics())
	if idx.metrics == nil {
		t.Fatal("metrics not wired")
	}
}

func TestChunkID_StableAndDifferentInputsDiffer(t *testing.T) {
	a := chunkID("p", "art", "src.md", 0)
	b := chunkID("p", "art", "src.md", 0)
	if a != b || len(a) != 32 {
		t.Fatalf("not stable / wrong length: %q %q", a, b)
	}
	if a == chunkID("p", "art", "src.md", 1) {
		t.Fatal("index collision")
	}
}

func TestIngestText_EmptyAndZeroChunks(t *testing.T) {
	idx, _, cleanup := newTestIndexer(t)
	defer cleanup()
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", ""); err != nil {
		t.Fatal(err)
	}
	// Whitespace produces zero chunks → also no-op.
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", "   "); err != nil {
		t.Fatal(err)
	}
}

func TestIngestText_DefaultsAppliedAndUpsertEnqueue(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	idx.cfg.ChunkTokens = 0
	idx.cfg.ChunkOverlap = -1

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", "hello world from a single chunk"); err != nil {
		t.Fatal(err)
	}
}

// TestIngestText_AutoStampPoliciesDisabledByDefault — pins the
// 2026.5.9 follow-on default: IngestText does NOT auto-stamp
// firewall policy columns unless SetAutoStampPolicies(true)
// was called. Keeps Pipeline-path tests (which use
// `mock.ExpectExec("UPDATE project_memory_chunks")` regexes)
// stable across the migration.
func TestIngestText_AutoStampPoliciesDisabledByDefault(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO UPDATE expectation — auto-stamp is off by default.

	if err := idx.IngestText(context.Background(), "p", "t", "a", "operator_correction_x", "fresh ingest"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("default-off: %v", err)
	}
}

// TestIngestText_AutoStampPoliciesOptInRuns — when the opt-in
// is set, IngestText runs the post-insert UPDATE that stamps
// default firewall policy onto newly-inserted chunks.
// operator_correction_ source name → ProvenanceOperatorCorrection
// → Sensitivity=internal + Trust=95.
func TestIngestText_AutoStampPoliciesOptInRuns(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	idx.SetAutoStampPolicies(true)

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := idx.IngestText(context.Background(), "p", "t", "a", "operator_correction_x", "fresh ingest"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("opt-in: %v", err)
	}
}

// TestProvenanceFromSourceName pins the heuristic mapping used
// by the post-insert stamp. Drifts here would silently change
// chunk sensitivity defaults; the test catches that.
func TestProvenanceFromSourceName(t *testing.T) {
	cases := map[string]memoryfirewall.ProvenanceSource{
		"operator_correction_20260529": memoryfirewall.ProvenanceOperatorCorrection,
		"chat_turn_abc123":             memoryfirewall.ProvenanceChatTurn,
		"chat_turn":                    memoryfirewall.ProvenanceChatTurn,
		"self_consolidate_p1":          memoryfirewall.ProvenanceSelfConsolidated,
		"consolidate":                  memoryfirewall.ProvenanceSelfConsolidated,
		"external_fetch_arxiv":         memoryfirewall.ProvenanceExternalFetch,
		"web_scrape_2026_05_29":        memoryfirewall.ProvenanceExternalFetch,
		"companion_remember_x":         memoryfirewall.ProvenanceCompanionRemember,
		"remember_x":                   memoryfirewall.ProvenanceCompanionRemember,
		"extracted_document_5":         memoryfirewall.ProvenanceIngestedArtifact,
		"ingest_pdf_123":               memoryfirewall.ProvenanceIngestedArtifact,
		"task_output_xyz":              memoryfirewall.ProvenanceWorkflowOutput,
		"workflow_step_5":              memoryfirewall.ProvenanceWorkflowOutput,
		"unrecognised.md":              memoryfirewall.ProvenanceWorkflowOutput,
	}
	for in, want := range cases {
		got := provenanceFromSourceName(in)
		if got != want {
			t.Errorf("provenanceFromSourceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIngestText_UpsertErrorPropagates(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	idx.setMetrics(freshMetrics())

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnError(errors.New("db down"))
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", "some content"); err == nil {
		t.Fatal("expected error")
	}
}

func TestIngestText_EnqueueErrorSwallowed(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnError(errors.New("queue down"))

	// Enqueue failure is non-fatal — chunks were stored.
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", "some content"); err != nil {
		t.Fatalf("enqueue err should be swallowed: %v", err)
	}
}

func TestIngestText_WithMetricsSuccess(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	idx.setMetrics(freshMetrics())

	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.IngestText(context.Background(), "p", "t", "a", "s.md", "hello world content here"); err != nil {
		t.Fatal(err)
	}
}

func TestIngestFile(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	tmp := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(tmp, []byte("content from file"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("INSERT INTO project_memory_chunks").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.IngestFile(context.Background(), "p", "t", "a", "doc.md", tmp); err != nil {
		t.Fatal(err)
	}

	// Missing file.
	if err := idx.IngestFile(context.Background(), "p", "t", "a", "x.md", "/no/such"); err == nil {
		t.Fatal("expected error")
	}
}

func TestIndexer_AdminMethodsNilSafe(t *testing.T) {
	var nilIdx *Indexer
	if err := nilIdx.MarkVerifiedByArtifact(context.Background(), "", "", ""); err != nil {
		t.Fatal()
	}
	if n, err := nilIdx.SupersedeBySameSource(context.Background(), "", "", "", "", "", ""); n != 0 || err != nil {
		t.Fatal()
	}
	if err := nilIdx.PatchPolicyByArtifact(context.Background(), "", "", "", 0, "", "", nil, ""); err != nil {
		t.Fatal()
	}
	// Indexer with nil repo.
	idx := &Indexer{}
	if err := idx.MarkVerifiedByArtifact(context.Background(), "p", "a", "r"); err != nil {
		t.Fatal()
	}
	if n, err := idx.SupersedeBySameSource(context.Background(), "p", "c", "s", "t", "a", "epoch-1"); n != 0 || err != nil {
		t.Fatal()
	}
	if err := idx.PatchPolicyByArtifact(context.Background(), "p", "a", "c", 0.5, "r", "e", nil, ""); err != nil {
		t.Fatal()
	}
}

func TestIndexer_AdminMethodsDelegateToRepo(t *testing.T) {
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()

	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a", "r").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.MarkVerifiedByArtifact(context.Background(), "p", "a", "r"); err != nil {
		t.Fatal(err)
	}

	// Supersede now passes (project, class, task, artifact, legacy_exact, like_pattern)
	// after the 2026-05-16 disambig-aware match update. For source_name="s"
	// (un-disambig'd), stem="s", ext="", so legacy_exact="s" and
	// like_pattern="s-________-____".
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "c", "t", "a", "s", "s-________-____", "epoch-1").
		WillReturnResult(sqlmock.NewResult(0, 5))
	n, err := idx.SupersedeBySameSource(context.Background(), "p", "c", "s", "t", "a", "epoch-1")
	if err != nil || n != 5 {
		t.Fatalf("got %d %v", n, err)
	}

	now := time.Now()
	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a", "research", float32(0.7), "scout", "exec", now, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := idx.PatchPolicyByArtifact(context.Background(), "p", "a", "research", 0.7, "scout", "exec", &now, ""); err != nil {
		t.Fatal(err)
	}
}
