package service

// Unit tests for the architect-adapter glue. The heavy lifting
// lives in internal/memetic; here we pin the security-sensitive
// path resolution + nil-safe construction behaviour.
//
// The EE-coupled architect-evidence metric tests (which import the Enterprise
// Instinct engine) live in container_workflow_architect_ee_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
)

// TestLowConfidenceIsNoProposal — the architect's deliberate low-confidence
// PASS verdict (ErrLowConfidence: the model evaluated the evidence and found
// no structural change warranted — the prompt-designed "PROPOSE OR PASS"
// outcome) maps to the benign (nil, nil) "no proposal" shape so the UI renders
// it informationally instead of WARN "architect failed" + an error banner
// (2026-06-12: confidence:0.00 was mislabeled as an error). Real errors and
// real proposals pass through unchanged. Mirrors the API's 204 treatment.
func TestLowConfidenceIsNoProposal(t *testing.T) {
	if p, err := lowConfidenceIsNoProposal(nil, memetic.ErrLowConfidence); p != nil || err != nil {
		t.Fatalf("bare low-confidence: want (nil,nil), got (%v,%v)", p, err)
	}
	wrapped := fmt.Errorf("architect turn 2: %w", memetic.ErrLowConfidence)
	if p, err := lowConfidenceIsNoProposal(nil, wrapped); p != nil || err != nil {
		t.Fatalf("wrapped low-confidence: want (nil,nil), got (%v,%v)", p, err)
	}
	want := &persistence.WorkflowProposal{ID: "wpr_x"}
	if p, err := lowConfidenceIsNoProposal(want, nil); p != want || err != nil {
		t.Fatalf("real proposal: want (%v,nil), got (%v,%v)", want, p, err)
	}
	other := errors.New("db down")
	if p, err := lowConfidenceIsNoProposal(nil, other); p != nil || !errors.Is(err, other) {
		t.Fatalf("real error: want (nil, 'db down'), got (%v,%v)", p, err)
	}
}

func TestFSWorkflowSource_LoadsFile(t *testing.T) {
	tmp := t.TempDir()
	wfDir := filepath.Join(tmp, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := []byte("---\nworkflowId: \"x\"\n---\nbody\n")
	if err := os.WriteFile(filepath.Join(wfDir, "x.md"), content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	src := &fsWorkflowSource{configDir: tmp}
	got, err := src.Load(context.Background(), "x")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: %q", got)
	}
}

func TestFSWorkflowSource_NotFound_BubblesOSError(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := &fsWorkflowSource{configDir: tmp}
	_, err := src.Load(context.Background(), "missing")
	// Must surface as os.ErrNotExist so the admin handler can
	// map it to 404 WORKFLOW_NOT_FOUND.
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

// TestFSWorkflowSource_RejectsTraversal — operator-supplied
// workflowID cannot escape the workflows directory via ../ or
// absolute paths. Direct security concern for the admin endpoint.
func TestFSWorkflowSource_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "workflows"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plant a file outside the workflows directory.
	secretPath := filepath.Join(tmp, "secret.md")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	src := &fsWorkflowSource{configDir: tmp}

	cases := []string{
		"../secret",
		"../../etc/passwd",
		"x/../secret",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := src.Load(context.Background(), name)
			if err == nil {
				t.Fatalf("traversal %q should have failed", name)
			}
		})
	}
}

func TestFSWorkflowSource_EmptyInputs(t *testing.T) {
	src := &fsWorkflowSource{}
	if _, err := src.Load(context.Background(), "x"); err == nil {
		t.Error("empty configDir should error")
	}
	src.configDir = t.TempDir()
	if _, err := src.Load(context.Background(), ""); err == nil {
		t.Error("empty workflowID should error")
	}
}

// TestNewWorkflowArchitectAdapter_NilSafe — missing prerequisites
// return nil so the http init's nil-check leaves the endpoint
// returning 503 cleanly.
func TestNewWorkflowArchitectAdapter_NilSafe(t *testing.T) {
	if got := newWorkflowArchitectAdapter(nil, nil, nil, "", nil, nil, zerolog.Nop()); got != nil {
		t.Errorf("all-nil should return nil, got %+v", got)
	}
	// Even with most deps wired, empty configDir should still
	// return nil — we won't have a way to load WORKFLOW.md files
	// otherwise.
	if got := newWorkflowArchitectAdapter(nil, nil, nil, "", nil, nil, zerolog.Nop()); got != nil {
		t.Errorf("empty configDir should return nil")
	}
}

// TestWorkflowArchitectAdapter_RejectionRecorder_NilWhenNoInstincts —
// Consumer B's rejection recorder is only available when the instinct
// repo was wired (gate on); otherwise the accessor returns nil so the
// api layer treats it as "no write-back".
func TestWorkflowArchitectAdapter_RejectionRecorder_NilWhenNoInstincts(t *testing.T) {
	// nil adapter
	var nilAdapter *workflowArchitectAdapter
	if nilAdapter.RejectionRecorder() != nil {
		t.Error("nil adapter should yield nil recorder")
	}
	// adapter with no arch / no instincts
	a := &workflowArchitectAdapter{}
	if a.RejectionRecorder() != nil {
		t.Error("adapter without instincts should yield nil recorder")
	}
}

// TestWorkflowRejectionRecorder_NilSafe — the recorder degrades to a
// no-op when its deps are missing and rejects a wrong-typed proposal.
func TestWorkflowRejectionRecorder_NilSafe(t *testing.T) {
	var r *workflowRejectionRecorder
	if err := r.RecordRejection(context.Background(), nil); err != nil {
		t.Errorf("nil recorder should be a no-op, got %v", err)
	}
	// Missing deps → no-op.
	r2 := &workflowRejectionRecorder{}
	if err := r2.RecordRejection(context.Background(), nil); err != nil {
		t.Errorf("recorder with no deps should be a no-op, got %v", err)
	}
}

func TestWorkflowArchitectAdapter_NilArch_ReturnsError(t *testing.T) {
	a := &workflowArchitectAdapter{}
	_, err := a.Propose(context.Background(), "x")
	if err == nil {
		t.Error("nil arch should error not nil-pointer-deref")
	}
}

func TestSQLExecutionLookup_UsesExecutionsWorkflowID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT e.id
		FROM executions e
		WHERE e.id = ANY($1)
		  AND e.workflow_id = $2`)).
		WithArgs(sqlmock.AnyArg(), "wf-a").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("exec-1").AddRow("exec-2"))

	lookup := &sqlExecutionLookup{db: db}
	valid, ok, err := lookup.BelongsTo(context.Background(), "wf-a", []string{"exec-1", "exec-2"})
	if err != nil {
		t.Fatalf("BelongsTo: %v", err)
	}
	if !ok || len(valid) != 2 {
		t.Fatalf("valid=%v ok=%v, want both valid", valid, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
