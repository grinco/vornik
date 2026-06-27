package autonomy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// fenceGate satisfies the autonomy LeaderGate (IsLeader) AND the
// leaderelection.EpochVerifier capability (VerifyEpoch) so the epoch
// fence inside createAutonomousTask exercises the fail-closed branch.
// IsLeader returns true throughout: the point of the fence is that a
// TTL-expired-but-paused leader can still report a cached IsLeader=true,
// and only the epoch re-read (VerifyEpoch) catches the supersession
// (review finding B1).
type fenceGate struct {
	verifyOK  bool
	verifyErr error
}

func (g fenceGate) IsLeader() bool { return true }

func (g fenceGate) VerifyEpoch(context.Context) (bool, int64, error) {
	return g.verifyOK, 0, g.verifyErr
}

func fenceTestProject() *registry.Project {
	return &registry.Project{
		ID:              "fence-proj",
		DefaultPriority: 50,
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Goal:    "Build things",
		},
	}
}

// TestCreateAutonomousTask_FenceClosed_SupersededEpoch pins review B1:
// a stale leader whose epoch was bumped by a successor (VerifyEpoch
// ok=false) must NOT create a task, even though its cheap IsLeader()
// still reports true.
func TestCreateAutonomousTask_FenceClosed_SupersededEpoch(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithLeaderGate(fenceGate{verifyOK: false}))

	err := m.createAutonomousTask(context.Background(), fenceTestProject(),
		`{"prompt":"Implement feature X","type":"feature"}`, time.Now())

	require.NoError(t, err, "fence should skip cleanly, not error")
	assert.Empty(t, repo.createdTasks(), "superseded leader must not create a task")
}

// TestCreateAutonomousTask_FenceClosed_VerifyError pins the fail-closed
// behaviour: an unreadable lock row (VerifyEpoch err != nil) also
// suppresses creation.
func TestCreateAutonomousTask_FenceClosed_VerifyError(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithLeaderGate(fenceGate{verifyErr: assert.AnError}))

	err := m.createAutonomousTask(context.Background(), fenceTestProject(),
		`{"prompt":"Implement feature X","type":"feature"}`, time.Now())

	require.NoError(t, err, "fence read error should skip cleanly, not error")
	assert.Empty(t, repo.createdTasks(), "lock-read failure must not create a task")
}

// TestCreateAutonomousTask_FenceOpen_CurrentEpoch confirms the fence
// lets a still-current leader (VerifyEpoch ok=true) through.
func TestCreateAutonomousTask_FenceOpen_CurrentEpoch(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithLeaderGate(fenceGate{verifyOK: true}))

	err := m.createAutonomousTask(context.Background(), fenceTestProject(),
		`{"prompt":"Implement feature X","type":"feature"}`, time.Now())

	require.NoError(t, err)
	assert.Len(t, repo.createdTasks(), 1, "current leader should create the task")
}

// TestCreateAutonomousTask_NonVerifierGate confirms a plain IsLeader-only
// gate (no VerifyEpoch) preserves pre-fence behaviour: the task is created.
func TestCreateAutonomousTask_NonVerifierGate(t *testing.T) {
	repo := &mockTaskRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithLeaderGate(plainLeaderGate{}))

	err := m.createAutonomousTask(context.Background(), fenceTestProject(),
		`{"prompt":"Implement feature X","type":"feature"}`, time.Now())

	require.NoError(t, err)
	assert.Len(t, repo.createdTasks(), 1, "non-verifier gate is pre-fence: task proceeds")
}

// plainLeaderGate implements only IsLeader() — no VerifyEpoch.
type plainLeaderGate struct{}

func (plainLeaderGate) IsLeader() bool { return true }
