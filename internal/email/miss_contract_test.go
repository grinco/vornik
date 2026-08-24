package email

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

func TestEmailDoubles_MissContract(t *testing.T) {
	ctx := context.Background()
	t.Run("Get", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.Get", func() (*persistence.Artifact, error) {
			return (&fakeArtifactRepo{}).Get(ctx, "artifact-absent")
		})
	})
	t.Run("GetByHash", func(t *testing.T) {
		repotest.AssertMiss(t, "ArtifactRepository.GetByHash", func() (*persistence.Artifact, error) {
			return (&fakeArtifactRepo{}).GetByHash(ctx, "sha256:absent")
		})
	})
}
