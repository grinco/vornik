package rag

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The doubles here must agree with production about what absence looks like.
// Each key below is MissErrNotFound; each double answered (nil, nil), which is
// LOOSER than production and certifies a caller's miss path without executing
// it. The same cleanup exposed live defects in internal/executor (four lineage
// walks) and internal/watchdog (the vanished-task short-circuit) — see
// https://docs.vornik.io §8.

func TestRAGDoubles_MissContract(t *testing.T) {
	ctx := context.Background()
	t.Run("Get", func(t *testing.T) {
		repotest.AssertMiss(t, "ExtractedDocumentRepository.Get", func() (*persistence.ExtractedDocument, error) {
			return (&fakeExtractedDocRepo{}).Get(ctx, "doc-absent")
		})
	})
	t.Run("GetByArtifact", func(t *testing.T) {
		repotest.AssertMiss(t, "ExtractedDocumentRepository.GetByArtifact", func() (*persistence.ExtractedDocument, error) {
			return (&fakeExtractedDocRepo{}).GetByArtifact(ctx, "artifact-absent")
		})
	})
}
