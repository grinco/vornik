package memory

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Absence must look the same here as it does in production. Each key below is
// MissErrNotFound; each double answered (nil, nil), which is looser and lets a
// caller that never handles ErrNotFound pass. See
// https://docs.vornik.io
func TestMemoryDoubles_MissContract(t *testing.T) {
	ctx := context.Background()

	t.Run("ArtifactRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.Get", func() (*persistence.Artifact, error) {
			return (&fakeArtifactRepo{}).Get(ctx, "artifact-absent")
		})
	})
	t.Run("KnowledgeEdgeRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "KnowledgeEdgeRepository.Get", func() (*persistence.KnowledgeEdge, error) {
			return (&fakeEdgeRepo{}).Get(ctx, "edge-absent")
		})
	})
	t.Run("KnowledgeEntityRepository.GetByCanonical", func(t *testing.T) {
		repotest.AssertMiss(t, "KnowledgeEntityRepository.GetByCanonical", func() (*persistence.KnowledgeEntity, error) {
			return (&fakeEntityRepo{}).GetByCanonical(ctx, "p1", "PERSON", "absent")
		})
	})
	t.Run("CorpusEpochRepository.GetEpoch", func(t *testing.T) {
		repotest.AssertMiss(t, "CorpusEpochRepository.GetEpoch", func() (*persistence.CorpusEpoch, error) {
			return (&fakeEpochs{}).GetEpoch(ctx, "project-absent")
		})
	})
	t.Run("MemoryQuarantineRepository.Get", func(t *testing.T) {
		repotest.AssertMiss(t, "MemoryQuarantineRepository.Get", func() (*persistence.MemoryQuarantineItem, error) {
			return (&fakeQuarantine{}).Get(ctx, "item-absent")
		})
	})
}
