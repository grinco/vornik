package erasure

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Complete erasure of a source artifact — GDPR Art 17, increment 5 slice 5b.
//
// Erase() deliberately stops at the derived data: extraction rows, the memory
// chunks built from them, and the extraction storage directories. That is the
// right boundary for retention pruning, where the artifact is the thing being
// kept, and `vornikctl erase artifact` prints "the artifact row itself is
// untouched" to make the boundary visible.
//
// It is the wrong boundary for an Art 17 request. The uploaded file is the most
// direct copy of the subject's data, and its NAME is frequently personal data on
// its own — "mri-scan-jane-doe.pdf" identifies a person and a health condition
// before anyone opens it. An erasure that destroyed the OCR text and left the
// scan on disk would report success over the very file it was asked to remove.
//
// So this file adds the complete operation, and keeps Erase() untouched.

// ArtifactStore is the narrow slice of the artifact repository needed to remove
// the artifact itself.
type ArtifactStore interface {
	// ArtifactStoragePath returns where the artifact's own bytes live. An empty
	// string means no stored file is recorded.
	ArtifactStoragePath(ctx context.Context, artifactID string) (string, error)
	// DeleteArtifactRow removes the artifacts row.
	DeleteArtifactRow(ctx context.Context, artifactID string) error
}

// EraseIncludingArtifact runs the derived cascade, then removes the artifact's own
// stored file and its row.
//
// ORDERING: derived data, then bytes, then the row. The row is deleted LAST for
// the same reason Erase() removes directories before rows — the database row is
// the only thing that records where the bytes are, so deleting it first turns a
// failed file delete into an orphan nobody can find again. Every failure therefore
// leaves the erasure retryable rather than half-done and untraceable.
//
// Containment is enforced on the artifact path exactly as on extraction
// directories: the path comes out of the database, and an unbounded delete is one
// bad row away from removing the wrong tree.
func (s *Service) EraseIncludingArtifact(ctx context.Context, artifactID string) (*Result, error) {
	if s == nil || s.Artifacts == nil {
		return nil, fmt.Errorf("erasure: EraseIncludingArtifact requires an ArtifactStore — " +
			"without it the artifact row and its stored file cannot be removed, and reporting " +
			"an Art 17 erasure without them would be false")
	}
	if err := s.check(artifactID); err != nil {
		return nil, err
	}

	// Resolve the blob location BEFORE anything is destroyed: if this fails we
	// must not proceed, because a deleted row with an unremoved file is an
	// orphan with no pointer left to find it by.
	blobPath, err := s.Artifacts.ArtifactStoragePath(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("erasure: resolve storage path for artifact %s: %w — "+
			"nothing has been deleted", artifactID, err)
	}
	var target string
	if strings.TrimSpace(blobPath) != "" {
		if target, err = s.safeTarget(blobPath); err != nil {
			return nil, fmt.Errorf("erasure: refusing to erase artifact %s: %w", artifactID, err)
		}
	}

	// The derived cascade first — it is the part with the most moving pieces and
	// it is already idempotent, so a failure here leaves the artifact intact and
	// the whole operation retryable.
	res, err := s.Erase(ctx, artifactID)
	if err != nil {
		return nil, err
	}

	if target != "" {
		if _, statErr := os.Stat(target); statErr == nil {
			remove := s.removeAll
			if remove == nil {
				remove = os.RemoveAll
			}
			if err := remove(target); err != nil {
				return nil, fmt.Errorf("erasure: remove stored file for artifact %s (%s): %w — "+
					"derived data was erased and the artifact ROW IS RETAINED so the file remains "+
					"findable; re-run to finish", artifactID, target, err)
			}
			res.BlobsRemoved++
		}
		// An already-absent file is not an error: the erasure must be retryable.
	}

	if err := s.Artifacts.DeleteArtifactRow(ctx, artifactID); err != nil {
		return nil, fmt.Errorf("erasure: delete artifact row %s: %w — derived data and the stored "+
			"file were removed; re-run to finish", artifactID, err)
	}
	res.ArtifactRowDeleted = true
	return res, nil
}
