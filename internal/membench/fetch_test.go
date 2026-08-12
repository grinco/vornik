package membench

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Dataset fetch with SHA-256 pinning (design §5.4). Datasets are not committed;
// they are downloaded and verified, so a result can never be silently attributed
// to different input data.

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestFetch_StoresVerifiedPayload — the happy path.
func TestFetch_StoresVerifiedPayload(t *testing.T) {
	payload := []byte(`[{"question_id":"q1"}]`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	err := FetchDataset(srv.Client(), srv.URL, dest, sha256Of(payload))
	if err != nil {
		t.Fatalf("FetchDataset: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("stored %q, want %q", got, payload)
	}
}

// TestFetch_HashMismatchRefusesAndReportsBoth is the point of pinning. The error
// must print expected AND actual: "checksum failed" alone leaves an operator with
// no way to tell an upstream revision from a truncated download.
func TestFetch_HashMismatchRefusesAndReportsBoth(t *testing.T) {
	payload := []byte("actual content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	wrong := sha256Of([]byte("something else"))

	err := FetchDataset(srv.Client(), srv.URL, dest, wrong)
	if err == nil {
		t.Fatal("a hash mismatch was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, wrong) {
		t.Errorf("error %q omits the expected hash", msg)
	}
	if !strings.Contains(msg, sha256Of(payload)) {
		t.Errorf("error %q omits the actual hash, so an operator cannot tell an "+
			"upstream revision from a truncated download", msg)
	}
}

// TestFetch_MismatchLeavesNoFile — a rejected payload must not be left on disk,
// or the next run loads unverified data believing it was checked.
func TestFetch_MismatchLeavesNoFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("actual"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	_ = FetchDataset(srv.Client(), srv.URL, dest, sha256Of([]byte("expected")))

	if _, err := os.Stat(dest); err == nil {
		t.Error("a hash-rejected payload was left on disk; the next run would load " +
			"unverified data believing it had been checked")
	}
}

// TestFetch_DoesNotClobberAGoodFileOnMismatch — re-fetching against a changed
// upstream must not destroy the verified copy already present.
func TestFetch_DoesNotClobberAGoodFileOnMismatch(t *testing.T) {
	good := []byte("verified earlier")
	dest := filepath.Join(t.TempDir(), "ds.json")
	if err := os.WriteFile(dest, good, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream changed"))
	}))
	defer srv.Close()

	_ = FetchDataset(srv.Client(), srv.URL, dest, sha256Of(good))

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the existing verified file was removed: %v", err)
	}
	if string(got) != string(good) {
		t.Errorf("existing verified file was overwritten with unverified content: %q", got)
	}
}

// TestFetch_EmptyExpectedHashRefused — fetching without a pin defeats the
// mechanism. Better to fail than to download something unverifiable.
func TestFetch_EmptyExpectedHashRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	if err := FetchDataset(srv.Client(), srv.URL, dest, ""); err == nil {
		t.Error("an empty expected hash was accepted; the pin is the whole point")
	}
}

// TestFetch_HTTPErrorSurfaces — a 404 must not be written to disk as if it were
// the dataset.
func TestFetch_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	err := FetchDataset(srv.Client(), srv.URL, dest, sha256Of([]byte("whatever")))
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not report the status code", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("an error page was written to the dataset path")
	}
}

// TestFetch_CaseInsensitiveHash — operators paste checksums from mixed sources.
// Rejecting an uppercase hex digest would be a gratuitous failure.
func TestFetch_CaseInsensitiveHash(t *testing.T) {
	payload := []byte("content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "ds.json")
	upper := strings.ToUpper(sha256Of(payload))
	if err := FetchDataset(srv.Client(), srv.URL, dest, upper); err != nil {
		t.Errorf("an uppercase expected hash was rejected: %v", err)
	}
}

// TestVerifyFile_ReportsHash — the local-verification path used before a run, so
// an already-fetched file is re-checked rather than trusted.
func TestVerifyFile_ReportsHash(t *testing.T) {
	payload := []byte("local file")
	path := filepath.Join(t.TempDir(), "ds.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := VerifyFile(path, sha256Of(payload))
	if err != nil {
		t.Fatalf("VerifyFile: %v", err)
	}
	if got != sha256Of(payload) {
		t.Errorf("VerifyFile returned %q, want the file's hash", got)
	}

	if _, err := VerifyFile(path, sha256Of([]byte("different"))); err == nil {
		t.Error("VerifyFile accepted a mismatched hash")
	}
}

// TestVerifyFile_MissingFile — a clear error, since a missing dataset must not
// read as an empty one.
func TestVerifyFile_MissingFile(t *testing.T) {
	if _, err := VerifyFile(filepath.Join(t.TempDir(), "nope"), "abc"); err == nil {
		t.Error("VerifyFile on a missing file reported success")
	}
}

// TestFetch_UndreatableDestDirErrors — a dest under a regular file cannot be
// created. Failing here rather than mid-download keeps a partial file from being
// left where a later run would load it.
func TestFetch_UncreatableDestDirErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	dest := filepath.Join(blocker, "sub", "ds.json")
	if err := FetchDataset(srv.Client(), srv.URL, dest, sha256Of([]byte("payload"))); err == nil {
		t.Error("FetchDataset accepted a destination under a regular file")
	}
}

// TestFetch_UnreachableHostErrors — a connection failure must surface, not leave
// a zero-byte dataset behind.
func TestFetch_UnreachableHostErrors(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ds.json")
	err := FetchDataset(&http.Client{}, "http://127.0.0.1:0/ds.json", dest, sha256Of([]byte("x")))
	if err == nil {
		t.Fatal("an unreachable host reported success")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a failed fetch left a file at the dataset path")
	}
}

// TestFetch_NilClientUsesDefault — the CLI may not construct a client; a nil one
// must not panic.
func TestFetch_NilClientUsesDefault(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "ds.json")
	// Unreachable on purpose: we are asserting no panic, not a successful fetch.
	if err := FetchDataset(nil, "http://127.0.0.1:0/ds.json", dest, "abc"); err == nil {
		t.Error("expected an error from an unreachable host")
	}
}
