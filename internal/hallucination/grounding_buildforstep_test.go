package hallucination

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// sliceAuditLister is a slice-backed AuditLister for unit tests.
// Pre-loaded entries are returned verbatim from List; errOnList lets
// a test exercise the partial-context error branch.
type sliceAuditLister struct {
	entries   []*persistence.ToolAuditEntry
	errOnList error
}

func (s *sliceAuditLister) List(_ context.Context, _ persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	if s.errOnList != nil {
		return nil, s.errOnList
	}
	return s.entries, nil
}

// sliceArtifactLister is the artifact-side equivalent.
type sliceArtifactLister struct {
	artifacts []*persistence.Artifact
	errOnList error
}

func (s *sliceArtifactLister) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	if s.errOnList != nil {
		return nil, s.errOnList
	}
	return s.artifacts, nil
}

// TestBuildForStep_HappyPath pins the assembled GroundingContext for
// a representative audit-entry + artifact set. URLs are extracted
// from BOTH input and output of each audit entry (a regression that
// only scans input would silently miss the "I fetched X" grounding
// when the URL only appears in the response body).
func TestBuildForStep_HappyPath(t *testing.T) {
	audits := &sliceAuditLister{
		entries: []*persistence.ToolAuditEntry{
			{
				ToolName:   "web_fetch",
				ToolInput:  `{"url":"https://input.example.org/a"}`,
				ToolOutput: "redirected to https://output.example.org/b\n",
			},
			{
				ToolName:   "memory_search",
				ToolInput:  `{"query":"janka"}`,
				ToolOutput: "[]",
			},
		},
	}
	arts := &sliceArtifactLister{
		artifacts: []*persistence.Artifact{
			{Name: "research.md", ArtifactClass: "output"},
			{Name: "summary.md", ArtifactClass: "output"},
		},
	}

	gc, err := BuildForStep(context.Background(), audits, arts, "exec_1", "task_1")
	if err != nil {
		t.Fatalf("BuildForStep: %v", err)
	}
	if gc == nil {
		t.Fatal("nil GroundingContext")
	}

	// Tool call names captured in order.
	if len(gc.ToolCallNames) != 2 || gc.ToolCallNames[0] != "web_fetch" {
		t.Errorf("ToolCallNames = %v", gc.ToolCallNames)
	}
	// Both URLs (input + output) land in FetchedURLs, lowercased.
	for _, want := range []string{"https://input.example.org/a", "https://output.example.org/b"} {
		if _, ok := gc.FetchedURLs[want]; !ok {
			t.Errorf("FetchedURLs missing %q (got %v)", want, gc.FetchedURLs)
		}
	}
	// Tool outputs concatenated.
	if gc.ToolOutputs == "" {
		t.Error("ToolOutputs is empty — audit body should have been concatenated")
	}
	// Artifact names indexed.
	for _, name := range []string{"research.md", "summary.md"} {
		if _, ok := gc.ArtifactNames[name]; !ok {
			t.Errorf("ArtifactNames missing %q (got %v)", name, gc.ArtifactNames)
		}
	}
}

// TestBuildForStep_NilListersReturnsEmptyContext — when neither audit
// nor artifact source is wired, BuildForStep must still return a
// non-nil context with initialised maps. Downstream rules call into
// the map literally; nil maps would panic.
func TestBuildForStep_NilListersReturnsEmptyContext(t *testing.T) {
	gc, err := BuildForStep(context.Background(), nil, nil, "exec_1", "task_1")
	if err != nil {
		t.Errorf("nil listers: unexpected error: %v", err)
	}
	if gc == nil {
		t.Fatal("nil context returned")
	}
	for name, m := range map[string]map[string]struct{}{
		"FetchedURLs":        gc.FetchedURLs,
		"ArtifactNames":      gc.ArtifactNames,
		"KnownTaskIDs":       gc.KnownTaskIDs,
		"KnownProjectIDs":    gc.KnownProjectIDs,
		"KnownArtifactNames": gc.KnownArtifactNames,
	} {
		if m == nil {
			t.Errorf("%s is nil — caller-side maps will panic on write", name)
		}
	}
}

// TestBuildForStep_AuditErrorReturnsPartialContext — the audit-side
// error must propagate to the caller AND leave the partial context
// available. Comment in BuildForStep says "running the detector on
// partial context is strictly safer than skipping it" — pinning that
// behaviour with a test so a future refactor doesn't drop to nil.
func TestBuildForStep_AuditErrorReturnsPartialContext(t *testing.T) {
	audits := &sliceAuditLister{errOnList: errors.New("simulated DB outage")}
	gc, err := BuildForStep(context.Background(), audits, nil, "exec_1", "task_1")
	if err == nil {
		t.Fatal("audit error: expected non-nil error")
	}
	if gc == nil {
		t.Error("audit error: partial context lost (got nil)")
	}
}

// TestBuildForStep_EmptyExecIDSkipsAudits — when executionID is
// empty, the audit fetch must be skipped (it would otherwise return
// nothing useful AND consume a DB round-trip). The dispatcher's
// chat path calls without execID; verify it doesn't error.
func TestBuildForStep_EmptyExecIDSkipsAudits(t *testing.T) {
	audits := &sliceAuditLister{errOnList: errors.New("should not be called")}
	gc, err := BuildForStep(context.Background(), audits, nil, "" /* exec */, "task_1")
	if err != nil {
		t.Errorf("empty exec: %v", err)
	}
	if gc == nil {
		t.Fatal("nil context")
	}
	if len(gc.ToolCallNames) != 0 {
		t.Errorf("ToolCallNames populated with empty execID: %v", gc.ToolCallNames)
	}
}

// TestMergeKnownIDs_InitialisesMaps — the merge-known-IDs entry point
// is called from the dispatcher AFTER BuildForStep on contexts that
// may have nil maps. Verify the helper initialises them defensively
// (mirrors the comment in MergeKnownIDs).
func TestMergeKnownIDs_InitialisesMaps(t *testing.T) {
	gc := &GroundingContext{} // all fields nil
	gc.MergeKnownIDs([]string{"p1"}, []string{"t1"}, []string{"a.md"})
	if _, ok := gc.KnownProjectIDs["p1"]; !ok {
		t.Errorf("KnownProjectIDs missing entry: %v", gc.KnownProjectIDs)
	}
	if _, ok := gc.KnownTaskIDs["t1"]; !ok {
		t.Errorf("KnownTaskIDs missing entry: %v", gc.KnownTaskIDs)
	}
	if _, ok := gc.KnownArtifactNames["a.md"]; !ok {
		t.Errorf("KnownArtifactNames missing entry: %v", gc.KnownArtifactNames)
	}
}
