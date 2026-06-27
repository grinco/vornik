//go:build integration
// +build integration

package integration_test

// Integration test for the leader-lease fence token (Slice D, Task 4).
// Proves that a resumed stale leader is rejected by the epoch fence
// after a takeover — review finding B1.
//
// The test follows the harness established in clustering_test.go:
//   - skipIfNoDB(t) as the skip guard when TEST_DATABASE_URL is absent.
//   - connectDB(t) for the *sql.DB handle.
//   - Unique prefix via time.Now().UnixNano() to avoid collisions.
//   - t.Cleanup to delete only the rows this test created.
//   - postgres.NewLeaderLockRepository + leaderelection.New + BootstrapAcquire
//     — the same construction pattern as TestClusteringIntegration_TwoElectorsLeaderHandover.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence/postgres"
)

// TestClusteringIntegration_FenceRejectsStaleLeader proves that a resumed
// stale leader — one that re-acquires a lock that a newer replica already
// took over — is blocked by the epoch fence.
//
// Steps:
//  1. A acquires the lock; record epoch eA (≥ 1).
//  2. Force a takeover by expiring A's lock via raw SQL so B can win.
//  3. B acquires; record epoch eB; assert eB > eA (epoch bumped on takeover).
//  4. VerifyEpoch: A gets ok=false, curEpoch=eB; B gets ok=true, curEpoch=eB.
//  5. DangerousWriteAllowed: A → false (stale leader blocked); B → true.
func TestClusteringIntegration_FenceRejectsStaleLeader(t *testing.T) {
	skipIfNoDB(t)

	db := connectDB(t)
	defer db.Close()

	prefix := fmt.Sprintf("cl-fence-%d", time.Now().UnixNano())
	workerID := prefix + "-wk"

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM daemon_leader_locks WHERE worker_id LIKE $1`, prefix+"%")
	})

	repo := postgres.NewLeaderLockRepository(db)
	const ttl = 30 * time.Second
	a := leaderelection.New(repo, workerID, prefix+"-inst-A", ttl, zerolog.Nop())
	b := leaderelection.New(repo, workerID, prefix+"-inst-B", ttl, zerolog.Nop())

	ctx := context.Background()

	// Step 1: A acquires leadership.
	a.BootstrapAcquire(ctx)
	require.True(t, a.IsLeader(), "instance A must hold the lease after BootstrapAcquire")

	eA := a.Epoch()
	require.GreaterOrEqual(t, eA, int64(1), "A's epoch must be ≥1 after acquire")

	// Step 2: Force a takeover by expiring A's lock via raw SQL.
	// The Acquire WHERE-guard requires expires_at < NOW(); setting it to
	// renewed_at - 1 hour guarantees B wins on the next Acquire call.
	_, err := db.ExecContext(ctx,
		`UPDATE daemon_leader_locks SET expires_at = renewed_at - interval '1 hour' WHERE worker_id = $1`,
		workerID,
	)
	require.NoError(t, err, "back-dating expires_at must succeed")

	// Step 3: B acquires the takeover; epoch must increase.
	b.BootstrapAcquire(ctx)
	require.True(t, b.IsLeader(), "instance B must win the expired lock")

	eB := b.Epoch()
	assert.Greater(t, eB, eA, "epoch must increase on takeover (eB > eA)")

	// Step 4: Verify epoch outcomes from both electors.

	// A is now the stale leader: its holderID no longer matches the lock row.
	okA, curA, errA := a.VerifyEpoch(ctx)
	require.NoError(t, errA, "VerifyEpoch must not error for stale A")
	assert.False(t, okA, "stale leader A must not pass VerifyEpoch (ok=false)")
	assert.Equal(t, eB, curA, "curEpoch seen by A must equal B's epoch")

	// B is the current leader: holderID matches and epoch is current.
	okB, curB, errB := b.VerifyEpoch(ctx)
	require.NoError(t, errB, "VerifyEpoch must not error for current leader B")
	assert.True(t, okB, "current leader B must pass VerifyEpoch (ok=true)")
	assert.Equal(t, eB, curB, "curEpoch seen by B must equal B's epoch")

	// Step 5: DangerousWriteAllowed honours the fence.
	proceedA, _ := leaderelection.DangerousWriteAllowed(ctx, a)
	assert.False(t, proceedA, "stale leader A must be blocked by DangerousWriteAllowed")

	proceedB, _ := leaderelection.DangerousWriteAllowed(ctx, b)
	assert.True(t, proceedB, "current leader B must be allowed by DangerousWriteAllowed")
}
