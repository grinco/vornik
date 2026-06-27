package memory

import (
	"context"
	"errors"
	"testing"
)

// stubBlobReader satisfies ArtifactBlobReader and records every
// call so the worker tests can prove SetArtifactBlobReader is
// actually consulted (rather than the legacy os.ReadFile fallback).
type stubBlobReader struct {
	calls map[string]int
	body  []byte
	err   error
}

func (s *stubBlobReader) Retrieve(_ context.Context, artifactID string) ([]byte, error) {
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	s.calls[artifactID]++
	if s.err != nil {
		return nil, s.err
	}
	return s.body, nil
}

// TestReadArtifactBytes_PrefersBlobReader — when SetArtifactBlobReader
// has been wired, readArtifactBytes must call it instead of falling
// through to os.ReadFile. Without this, S3-backed deployments would
// silently keep reading the (non-existent) StoragePath off the
// local disk.
func TestReadArtifactBytes_PrefersBlobReader(t *testing.T) {
	w := &IngestWorker{}
	stub := &stubBlobReader{body: []byte("from-backend")}
	w.SetArtifactBlobReader(stub)

	got, err := w.readArtifactBytes(context.Background(), "art_42", "/nonexistent/path")
	if err != nil {
		t.Fatalf("readArtifactBytes: %v", err)
	}
	if string(got) != "from-backend" {
		t.Errorf("body = %q, want \"from-backend\"", got)
	}
	if stub.calls["art_42"] != 1 {
		t.Errorf("blob reader call count for art_42 = %d, want 1", stub.calls["art_42"])
	}
}

// TestReadArtifactBytes_FallsBackToFilesystem covers the legacy
// path: when the blob reader isn't wired, readArtifactBytes reads
// from StoragePath via os.ReadFile. The test passes a non-existent
// path to confirm the os.ReadFile branch is what's hit (the error
// it returns is filesystem-shaped, not a stub error).
func TestReadArtifactBytes_FallsBackToFilesystem(t *testing.T) {
	w := &IngestWorker{}
	// No SetArtifactBlobReader call.
	_, err := w.readArtifactBytes(context.Background(), "art_999", "/this/path/does/not/exist")
	if err == nil {
		t.Fatalf("expected an os.ReadFile error on nonexistent path, got nil")
	}
}

// TestReadArtifactBytes_PropagatesBlobError — when the wired blob
// reader returns an error, readArtifactBytes must surface it (not
// silently fall back to os.ReadFile, which would mask S3 outages
// behind a confusing "no such file" error).
func TestReadArtifactBytes_PropagatesBlobError(t *testing.T) {
	w := &IngestWorker{}
	stub := &stubBlobReader{err: errors.New("simulated s3 timeout")}
	w.SetArtifactBlobReader(stub)

	_, err := w.readArtifactBytes(context.Background(), "art_x", "/wherever")
	if err == nil || err.Error() != "simulated s3 timeout" {
		t.Errorf("expected the stub error to propagate, got %v", err)
	}
}
