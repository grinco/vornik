package verifier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type stubBlobReader struct {
	body []byte
	err  error
}

func (s *stubBlobReader) Retrieve(_ context.Context, _ string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.body, nil
}

// TestDefaultBodyReader_PrefersBlobReader — once SetBlobReader has
// wired the backend-aware seam, defaultBodyReader must read through
// it instead of opening artifact.StoragePath off the local disk. This
// is the verifier-side half of the phase-4 S3 migration; without it,
// any verifier that calls readArtifactBody (artifact_min_entries,
// regex matchers, etc.) fails on S3-backed deployments.
func TestDefaultBodyReader_PrefersBlobReader(t *testing.T) {
	// Restore the global on exit so concurrent tests stay clean.
	prev := blobReader
	defer SetBlobReader(prev)

	SetBlobReader(&stubBlobReader{body: []byte("from-backend")})
	art := &persistence.Artifact{ID: "a1", Name: "n", StoragePath: "/nonexistent"}
	got, err := defaultBodyReader(art)
	if err != nil {
		t.Fatalf("defaultBodyReader: %v", err)
	}
	if string(got) != "from-backend" {
		t.Errorf("got %q, want \"from-backend\"", got)
	}
}

// TestDefaultBodyReader_BlobReaderErrorPropagates — backend errors
// must surface to the caller, not get masked by a fall-through to
// the local-disk branch.
func TestDefaultBodyReader_BlobReaderErrorPropagates(t *testing.T) {
	prev := blobReader
	defer SetBlobReader(prev)

	SetBlobReader(&stubBlobReader{err: errors.New("simulated backend timeout")})
	art := &persistence.Artifact{ID: "a1", Name: "n", StoragePath: "/whatever"}
	_, err := defaultBodyReader(art)
	if err == nil || !strings.Contains(err.Error(), "simulated backend timeout") {
		t.Errorf("expected backend timeout to propagate, got %v", err)
	}
}

// TestDefaultBodyReader_BlobReaderCapExceeded — the 1-MiB cap applies
// to backend reads too. A massive body must produce the same explicit
// "exceeds verifier read cap" error the local-disk branch produces;
// otherwise a verifier that's supposed to limit-read a 100-line list
// could process a runaway artifact.
func TestDefaultBodyReader_BlobReaderCapExceeded(t *testing.T) {
	prev := blobReader
	defer SetBlobReader(prev)

	// One byte over the cap.
	tooBig := make([]byte, maxVerifierBodyBytes+1)
	SetBlobReader(&stubBlobReader{body: tooBig})
	art := &persistence.Artifact{ID: "a1", Name: "huge.txt", StoragePath: "/whatever"}
	_, err := defaultBodyReader(art)
	if err == nil || !strings.Contains(err.Error(), "exceeds verifier read cap") {
		t.Errorf("expected cap-exceeded error, got %v", err)
	}
}
