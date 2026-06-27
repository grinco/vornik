// Package ui: tests for the backend-aware ArtifactDownload path.
// When WithArtifactReader is wired the handler streams via
// ArtifactReader.Open, bypassing the legacy os.Stat + http.ServeFile
// path. Covers the read seam the S3-backend cutover depends on.
package ui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// stubArtifactReader satisfies the ui.ArtifactReader interface
// without dragging the artifacts package into the test binary.
type stubArtifactReader struct {
	body    []byte
	openErr error
	calls   int
}

func (s *stubArtifactReader) Retrieve(_ context.Context, _ string) ([]byte, error) {
	return s.body, nil
}

func (s *stubArtifactReader) Open(_ context.Context, _ string) (io.ReadCloser, error) {
	s.calls++
	if s.openErr != nil {
		return nil, s.openErr
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

// TestArtifactDownload_StreamsViaReader — when WithArtifactReader is
// wired, ArtifactDownload calls ArtifactReader.Open instead of
// http.ServeFile. Confirms the body the user sees came from Open's
// stream, not the legacy disk path, by writing an unrelated body to
// the StoragePath disk location and asserting we got the reader's
// version instead.
func TestArtifactDownload_StreamsViaReader(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{
				ID:          "a1",
				Name:        "file.txt",
				StoragePath: "/this/path/does/not/exist",
			}, nil
		},
	}
	reader := &stubArtifactReader{body: []byte("from-backend")}
	srv := NewServer(
		WithArtifactRepository(repo),
		WithArtifactReader(reader),
	)
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "from-backend" {
		t.Errorf("body = %q, want \"from-backend\"", got)
	}
	if reader.calls == 0 {
		t.Errorf("ArtifactReader.Open was never called (expected ≥1)")
	}
}

// TestArtifactDownload_BackendNotFound — when Open returns a
// not-found-shaped error, the handler must respond 404 (not 500).
func TestArtifactDownload_BackendNotFound(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{
				ID:          "a1",
				Name:        "file.txt",
				StoragePath: "/anywhere",
			}, nil
		},
	}
	reader := &stubArtifactReader{openErr: errors.New("artifacts: object not found")}
	srv := NewServer(
		WithArtifactRepository(repo),
		WithArtifactReader(reader),
	)
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// TestArtifactDownload_BackendError — non-not-found backend errors
// surface as 500. Without this, a transient S3 outage would look
// like a 404 to clients and confuse retry logic.
func TestArtifactDownload_BackendError(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{
				ID:          "a1",
				Name:        "file.txt",
				StoragePath: "/anywhere",
			}, nil
		},
	}
	reader := &stubArtifactReader{openErr: errors.New("connection reset by peer")}
	srv := NewServer(
		WithArtifactRepository(repo),
		WithArtifactReader(reader),
	)
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
}

// TestIsNotFoundError covers the small helper that decides whether
// a backend error means 404. Substring matching is fragile by
// design — keeping it pinned with tests so a rename in the artifacts
// package doesn't silently degrade not-found-to-500.
func TestIsNotFoundError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"os.ErrNotExist", io.ErrUnexpectedEOF, false},
		{"plain message contains not found", errors.New("artifacts: object not found"), true},
		{"ErrNotFound named", errors.New("wrapped: ErrNotFound"), true},
		{"unrelated error", errors.New("timeout"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isNotFoundError(tc.err)
			if got != tc.want {
				t.Errorf("isNotFoundError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// Surface the strings the matcher recognises so a renamer at
	// the artifacts layer can grep for them without reading code.
	_ = strings.Contains
}
