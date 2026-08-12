package membench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Dataset fetch with SHA-256 pinning (design §5.4).
//
// Datasets are deliberately not committed: they are large, and they belong to
// their publishers. They are downloaded and verified against a pinned digest so a
// result can never be silently attributed to different input data — an upstream
// revision would otherwise change every score with nothing in the manifest to
// show why.

// maxDatasetBytes bounds a download so a redirect to something enormous cannot
// exhaust the disk. LongMemEval's full haystack set is a few hundred MB; 4 GiB is
// generous headroom while still being a bound.
const maxDatasetBytes = 4 << 30

// FetchDataset downloads url to dest and verifies its SHA-256 against expected.
//
// On any failure — HTTP status, short read, hash mismatch — dest is left exactly
// as it was. That matters in two directions: a rejected payload must not be left
// behind for the next run to load believing it was checked, and an already-verified
// file must not be destroyed by a re-fetch against a changed upstream.
func FetchDataset(client *http.Client, url, dest, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("refusing to fetch %s without an expected SHA-256: an "+
			"unpinned dataset defeats the point of pinning", url)
	}
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Get(url) //nolint:noctx // caller-driven CLI fetch; cancellation is process-level
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: http %d", url, resp.StatusCode)
	}

	// Download to a sibling temp file, then rename only after verification. The
	// rename is what makes "never clobber a good file" true rather than aspirational.
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("create dataset dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".partial-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxDatasetBytes)); err != nil {
		cleanup()
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}

	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		_ = os.Remove(tmpName)
		// Both hashes in the message: "checksum failed" alone leaves an operator
		// unable to tell an upstream revision from a truncated download, which are
		// very different problems.
		return fmt.Errorf("dataset %s failed verification:\n  expected %s\n  actual   %s\n"+
			"Either the upstream file changed (update the pin deliberately) or the "+
			"download was truncated (retry)", url, strings.TrimSpace(expected), actual)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("install dataset at %s: %w", dest, err)
	}
	return nil
}

// VerifyFile re-checks an already-present dataset and returns its actual hash.
//
// Called before a run rather than trusting that whatever is on disk is what was
// fetched: a file can be replaced between fetch and use, and the manifest records
// the hash of what was actually read.
func VerifyFile(path, expected string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open dataset %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read dataset %s: %w", path, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))

	if expected != "" && !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		return actual, fmt.Errorf("dataset %s does not match its pin:\n  expected %s\n  actual   %s",
			path, strings.TrimSpace(expected), actual)
	}
	return actual, nil
}
