package verifier

import (
	"context"
	"fmt"
	"io"
	"os"

	"vornik.io/vornik/internal/persistence"
)

// readArtifactBody reads the bytes of a stored artifact for
// verifiers that need to inspect content (artifact_min_entries
// and similar). Lives here rather than in verifier.go so the
// pure rule logic stays free of filesystem deps and can be
// stubbed in tests via setBodyReader().
//
// Capped at 1 MiB — verifiers count list-item lines, not parse
// the whole file. A larger body indicates the verifier is being
// abused for a job better done elsewhere (e.g. the agent should
// pre-summarise into a smaller artifact).
const maxVerifierBodyBytes = 1 << 20

// BlobReader is the backend-aware Retrieve seam (satisfied by
// *artifacts.Store). When wired via SetBlobReader, defaultBodyReader
// routes through it so S3 deployments work. Nil keeps the legacy
// direct-disk path.
type BlobReader interface {
	Retrieve(ctx context.Context, artifactID string) ([]byte, error)
}

var blobReader BlobReader

// SetBlobReader wires the backend-aware reader at boot. Idempotent
// + thread-safe assuming a single init call; production wiring
// happens in the service container.
func SetBlobReader(r BlobReader) { blobReader = r }

// bodyReader is overridable so tests can stub readArtifactBody
// without writing files to disk. Production keeps the default;
// tests assign their own.
var bodyReader = defaultBodyReader

func defaultBodyReader(a *persistence.Artifact) ([]byte, error) {
	if a == nil {
		return nil, fmt.Errorf("nil artifact")
	}
	if a.StoragePath == "" {
		return nil, fmt.Errorf("artifact %q has no storage path", a.Name)
	}
	// Backend-aware path when wired. Bypasses the 1-MiB cap with the
	// same LimitReader-style check on the returned slice — Retrieve
	// reads the full body but we only need the first MiB.
	if blobReader != nil {
		b, err := blobReader.Retrieve(context.Background(), a.ID)
		if err != nil {
			return nil, err
		}
		if len(b) > maxVerifierBodyBytes {
			return nil, fmt.Errorf("artifact %q exceeds verifier read cap of %d bytes", a.Name, maxVerifierBodyBytes)
		}
		return b, nil
	}
	f, err := os.Open(a.StoragePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxVerifierBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxVerifierBodyBytes {
		return nil, fmt.Errorf("artifact %q exceeds verifier read cap of %d bytes", a.Name, maxVerifierBodyBytes)
	}
	return b, nil
}

func readArtifactBody(a *persistence.Artifact) (string, error) {
	b, err := bodyReader(a)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
