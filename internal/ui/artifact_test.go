// Package ui: tests for the ArtifactDownload handler. Filesystem
// fixtures are stamped under t.TempDir() so the path-validation
// branches are exercised end-to-end without depending on the
// production artifact tree.
package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

func TestArtifactDownload_MissingIDOrRepo(t *testing.T) {
	cases := []struct {
		name    string
		repo    persistence.ArtifactRepository
		urlPath string
	}{
		{"no-repo", nil, "/artifacts/abc"},
		{"empty-id", &mocks.MockArtifactRepository{}, "/artifacts/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(WithArtifactRepository(tc.repo))
			req := httptest.NewRequest(http.MethodGet, tc.urlPath, nil)
			rec := httptest.NewRecorder()
			srv.ArtifactDownload(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status: got %d, want 404", rec.Code)
			}
		})
	}
}

func TestArtifactDownload_RepoError(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return nil, persistence.ErrNotFound
		},
	}
	srv := NewServer(WithArtifactRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestArtifactDownload_EmptyStoragePath(t *testing.T) {
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{ID: "a1", Name: "out.txt"}, nil
		},
	}
	srv := NewServer(WithArtifactRepository(repo))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (empty storage path)", rec.Code)
	}
}

func TestArtifactDownload_PathEscape_Forbidden(t *testing.T) {
	tmpdir := t.TempDir()
	// Write a file outside the artifact base so the path-escape
	// check has something to catch.
	outsidePath := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write outsidePath: %v", err)
	}
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{ID: "a1", Name: "outside.txt", StoragePath: outsidePath}, nil
		},
	}
	srv := NewServer(WithArtifactRepository(repo), WithArtifactBasePath(tmpdir))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", rec.Code)
	}
}

func TestArtifactDownload_MissingFile(t *testing.T) {
	tmpdir := t.TempDir()
	missingPath := filepath.Join(tmpdir, "missing.txt")
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{ID: "a1", Name: "missing.txt", StoragePath: missingPath}, nil
		},
	}
	srv := NewServer(WithArtifactRepository(repo), WithArtifactBasePath(tmpdir))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 (file missing)", rec.Code)
	}
}

func TestArtifactDownload_Success(t *testing.T) {
	tmpdir := t.TempDir()
	storagePath := filepath.Join(tmpdir, "out.txt")
	if err := os.WriteFile(storagePath, []byte("contents"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mime := "text/plain"
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			return &persistence.Artifact{ID: "a1", Name: "out.txt", StoragePath: storagePath, MimeType: &mime}, nil
		},
	}
	srv := NewServer(WithArtifactRepository(repo), WithArtifactBasePath(tmpdir))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a1", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type: got %q, want text/plain", got)
	}
	cd := rec.Header().Get("Content-Disposition")
	if len(cd) < 11 || cd[:11] != "attachment;" {
		t.Errorf("Content-Disposition: got %q, want attachment; prefix", cd)
	}
}

func TestArtifactDownload_Success_DefaultsMimeOctetStream(t *testing.T) {
	tmpdir := t.TempDir()
	storagePath := filepath.Join(tmpdir, "blob.bin")
	if err := os.WriteFile(storagePath, []byte{0x00, 0xff}, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	repo := &mocks.MockArtifactRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Artifact, error) {
			// MimeType nil
			return &persistence.Artifact{ID: "a2", Name: "blob.bin", StoragePath: storagePath}, nil
		},
	}
	srv := NewServer(WithArtifactRepository(repo), WithArtifactBasePath(tmpdir))
	req := httptest.NewRequest(http.MethodGet, "/artifacts/a2", nil)
	rec := httptest.NewRecorder()
	srv.ArtifactDownload(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type: got %q, want octet-stream", got)
	}
}
