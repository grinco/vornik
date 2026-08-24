package a2a

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

// readyAfter: 1 puts the double in its NOT-READY state, which is the miss the
// SSE poller races. It must look like production's miss, not like a nil pair.
func TestA2ADoubles_MissContract(t *testing.T) {
	repotest.AssertMiss(t, "ExecutionRepository.GetByTaskID", func() (*persistence.Execution, error) {
		return (&delayedExecLookup{readyAfter: 1}).GetByTaskID(context.Background(), "task-absent")
	})
}
