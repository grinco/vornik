// Tests for resolveInputFileSource — the create_task helper that
// distinguishes "host file path" from "artifact ID" in the
// input_files list, resolving IDs to the artifact's on-disk
// StoragePath before snapshotting. Regression guard for the
// 2026-05-21 bug where the LLM passed `email-att-<hex>` from the
// attachment-plumbing prompt into input_files and the executor
// rejected it as "outside allowed roots."
package dispatcher

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type stubArtifactRepoForResolve struct {
	byID    map[string]*persistence.Artifact
	wantErr error
}

// Get is the only ArtifactRepository method resolveInputFileSource
// needs. Other methods are no-ops to satisfy the interface.
func (s *stubArtifactRepoForResolve) Get(_ context.Context, id string) (*persistence.Artifact, error) {
	if s.wantErr != nil {
		return nil, s.wantErr
	}
	if a, ok := s.byID[id]; ok {
		return a, nil
	}
	return nil, persistence.ErrNotFound
}
func (s *stubArtifactRepoForResolve) GetByHash(_ context.Context, _ string) (*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepoForResolve) List(_ context.Context, _ persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
	return nil, nil
}
func (s *stubArtifactRepoForResolve) Create(_ context.Context, _ *persistence.Artifact) error {
	return nil
}
func (s *stubArtifactRepoForResolve) Delete(_ context.Context, _ string) error { return nil }
func (s *stubArtifactRepoForResolve) DeleteByExecutionID(_ context.Context, _ string) error {
	return nil
}
func (s *stubArtifactRepoForResolve) UpdateTaskID(_ context.Context, _, _ string) error { return nil }

// TestResolveInputFileSource_PathWithSlashPassesThrough — real
// file paths (Telegram uploads land under /tmp/vornik-uploads/...)
// must NOT trigger an artifact lookup; the host path is the
// authoritative source.
func TestResolveInputFileSource_PathWithSlashPassesThrough(t *testing.T) {
	te := &ToolExecutor{artifactRepo: &stubArtifactRepoForResolve{}}
	cases := []string{
		"/tmp/upload.pdf",
		"/opt/vornik/.local/share/vornik/workspaces/x/uploads/y.txt",
		"relative/path/file.bin",
	}
	for _, src := range cases {
		got := te.resolveInputFileSource(context.Background(), src)
		if got != src {
			t.Errorf("path %q: got %q, want pass-through", src, got)
		}
	}
}

// TestResolveInputFileSource_ArtifactIDResolves — when the LLM
// passes a bare artifact ID (no slash) and the repo has a matching
// row, return the artifact's StoragePath so the snapshot reads
// from the real file.
func TestResolveInputFileSource_ArtifactIDResolves(t *testing.T) {
	repo := &stubArtifactRepoForResolve{
		byID: map[string]*persistence.Artifact{
			"email-att-2bd029f351c8e72b": {
				ID:          "email-att-2bd029f351c8e72b",
				Name:        "book.epub",
				StoragePath: "/opt/vornik/email-attachments/x/book.epub",
			},
		},
	}
	te := &ToolExecutor{artifactRepo: repo}
	got := te.resolveInputFileSource(context.Background(), "email-att-2bd029f351c8e72b")
	want := "/opt/vornik/email-attachments/x/book.epub"
	if got != want {
		t.Errorf("resolution: got %q, want %q", got, want)
	}
}

// TestResolveInputFileSource_UnknownIDPassesThrough — a no-slash
// value that doesn't match any artifact row falls through
// unchanged. Containerstaging will then reject it with the same
// "outside allowed roots" error as before; we don't try to
// silently rewrite operator-supplied strings.
func TestResolveInputFileSource_UnknownIDPassesThrough(t *testing.T) {
	repo := &stubArtifactRepoForResolve{byID: map[string]*persistence.Artifact{}}
	te := &ToolExecutor{artifactRepo: repo}
	got := te.resolveInputFileSource(context.Background(), "not-a-real-id")
	if got != "not-a-real-id" {
		t.Errorf("unknown ID: got %q, want pass-through", got)
	}
}

// TestResolveInputFileSource_RepoErrorFallsThrough — transient
// repo failures must not mask the original input; return src
// verbatim so the failure mode is "container staging rejected
// path" instead of "lookup failed mid-call."
func TestResolveInputFileSource_RepoErrorFallsThrough(t *testing.T) {
	repo := &stubArtifactRepoForResolve{wantErr: errors.New("db down")}
	te := &ToolExecutor{artifactRepo: repo}
	got := te.resolveInputFileSource(context.Background(), "email-att-x")
	if got != "email-att-x" {
		t.Errorf("repo error: got %q, want pass-through", got)
	}
}

// TestResolveInputFileSource_NilRepoFallsThrough — defensive
// guard so test fixtures + minimal-deployment paths don't panic
// when artifactRepo is unset.
func TestResolveInputFileSource_NilRepoFallsThrough(t *testing.T) {
	te := &ToolExecutor{}
	got := te.resolveInputFileSource(context.Background(), "anything")
	if got != "anything" {
		t.Errorf("nil repo: got %q, want pass-through", got)
	}
}

// TestResolveInputFileSource_EmptyStoragePathFallsThrough — an
// artifact row that's missing StoragePath (degenerate) shouldn't
// produce an empty source path. Fall through to the original
// string so the failure surfaces clearly.
func TestResolveInputFileSource_EmptyStoragePathFallsThrough(t *testing.T) {
	repo := &stubArtifactRepoForResolve{
		byID: map[string]*persistence.Artifact{
			"art-no-path": {ID: "art-no-path", Name: "x", StoragePath: ""},
		},
	}
	te := &ToolExecutor{artifactRepo: repo}
	got := te.resolveInputFileSource(context.Background(), "art-no-path")
	if got != "art-no-path" {
		t.Errorf("empty storage path: got %q, want pass-through", got)
	}
}
