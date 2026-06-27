package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// scratchpadRepoStub captures Get / Upsert so applyScratchpadUpdate
// can be exercised under each failure mode without sqlmock noise.
type scratchpadRepoStub struct {
	mu          sync.Mutex
	existing    *persistence.TaskScratchpad
	upserted    *persistence.TaskScratchpad
	getErr      error
	upsertErr   error
	upsertCount int
	getCount    int
}

func (s *scratchpadRepoStub) Get(_ context.Context, _ string) (*persistence.TaskScratchpad, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCount++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.existing, nil
}

func (s *scratchpadRepoStub) Upsert(_ context.Context, sp *persistence.TaskScratchpad) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertCount++
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserted = sp
	return nil
}

func TestApplyScratchpadUpdate_NilOutcomeIsNoop(t *testing.T) {
	repo := &scratchpadRepoStub{}
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}
	e.applyScratchpadUpdate(context.Background(), "t1", "exec1", nil)
	assert.Equal(t, 0, repo.upsertCount)
	assert.Equal(t, 0, repo.getCount)
}

func TestApplyScratchpadUpdate_NilScratchpadUpdateIsNoop(t *testing.T) {
	repo := &scratchpadRepoStub{}
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}
	e.applyScratchpadUpdate(context.Background(), "t1", "exec1", &LeadOutcome{})
	assert.Equal(t, 0, repo.upsertCount)
	assert.Equal(t, 0, repo.getCount)
}

func TestApplyScratchpadUpdate_NilRepoIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	outcome := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "x"}}
	// No taskScratchpadRepo wired → silent skip.
	e.applyScratchpadUpdate(context.Background(), "t1", "exec1", outcome)
}

func TestApplyScratchpadUpdate_GetErrorSkipsUpsert(t *testing.T) {
	repo := &scratchpadRepoStub{getErr: errors.New("db down")}
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}
	outcome := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "x"}}
	e.applyScratchpadUpdate(context.Background(), "t1", "exec1", outcome)
	assert.Equal(t, 0, repo.upsertCount)
}

func TestApplyScratchpadUpdate_NewRowWithAllFields(t *testing.T) {
	repo := &scratchpadRepoStub{} // existing == nil → new row created
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}
	outcome := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{
		Summary:       "summary text",
		Facts:         json.RawMessage(`{"k":"v"}`),
		OpenQuestions: []string{"q1", "q2"},
		CurrentPhase:  "design",
	}}
	e.applyScratchpadUpdate(context.Background(), "t1", "exec1", outcome)
	require.Equal(t, 1, repo.upsertCount)
	sp := repo.upserted
	require.NotNil(t, sp)
	assert.Equal(t, "t1", sp.TaskID)
	assert.Equal(t, "summary text", sp.Summary)
	assert.JSONEq(t, `{"k":"v"}`, string(sp.Facts))
	// OpenQuestions stored as marshalled JSON.
	assert.JSONEq(t, `["q1","q2"]`, string(sp.OpenQuestions))
	require.NotNil(t, sp.CurrentPhase)
	assert.Equal(t, "design", *sp.CurrentPhase)
	require.NotNil(t, sp.LastExecutionID)
	assert.Equal(t, "exec1", *sp.LastExecutionID)
}

func TestApplyScratchpadUpdate_PartialUpdatePreservesExisting(t *testing.T) {
	existingFacts := []byte(`{"old":"value"}`)
	existingQuestions := []byte(`["original"]`)
	prevPhase := "phase-1"
	repo := &scratchpadRepoStub{existing: &persistence.TaskScratchpad{
		TaskID:        "t1",
		Summary:       "old summary",
		Facts:         existingFacts,
		OpenQuestions: existingQuestions,
		CurrentPhase:  &prevPhase,
	}}
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}

	// Only summary is set — Facts / OpenQuestions / CurrentPhase
	// should carry through unchanged.
	outcome := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "new summary"}}
	e.applyScratchpadUpdate(context.Background(), "t1", "exec2", outcome)
	require.NotNil(t, repo.upserted)
	assert.Equal(t, "new summary", repo.upserted.Summary)
	assert.Equal(t, string(existingFacts), string(repo.upserted.Facts))
	assert.Equal(t, string(existingQuestions), string(repo.upserted.OpenQuestions))
	require.NotNil(t, repo.upserted.CurrentPhase)
	assert.Equal(t, "phase-1", *repo.upserted.CurrentPhase)
}

func TestApplyScratchpadUpdate_UpsertErrorIsLogged(t *testing.T) {
	repo := &scratchpadRepoStub{upsertErr: errors.New("write failed")}
	e := &Executor{taskScratchpadRepo: repo, logger: zerolog.Nop()}
	outcome := &LeadOutcome{ScratchpadUpdate: &ScratchpadUpdate{Summary: "x"}}
	// Best-effort: error doesn't propagate.
	assert.NotPanics(t, func() {
		e.applyScratchpadUpdate(context.Background(), "t1", "exec1", outcome)
	})
	assert.Equal(t, 1, repo.upsertCount)
}

// TestApplyPhaseTransitions — happy path inserts one TaskMessage per
// transition; nil repo / empty list are silent no-ops.
func TestApplyPhaseTransitions_HappyPath(t *testing.T) {
	msgRepo := &fakeMessageRepo{}
	e := &Executor{taskMessageRepo: msgRepo, logger: zerolog.Nop()}
	transitions := []PhaseTransition{
		{Phase: "research", Status: "enter"},
		{Phase: "research", Status: "exit"},
		{Phase: "design", Status: "skip"},
	}
	e.applyPhaseTransitions(context.Background(), "task-1", "exec-1", transitions)

	require.Len(t, msgRepo.inserted, 3)
	for i, msg := range msgRepo.inserted {
		assert.Equal(t, "task-1", msg.TaskID)
		require.NotNil(t, msg.ExecutionID)
		assert.Equal(t, "exec-1", *msg.ExecutionID)
		assert.Equal(t, persistence.TaskMessageAuthorLead, msg.AuthorKind)
		assert.Equal(t, persistence.TaskMessageKindPhaseMarker, msg.MessageKind)
		assert.Equal(t, transitions[i].Phase+" "+transitions[i].Status, msg.Content)
		// Metadata is the JSON-marshalled PhaseTransition.
		var pt PhaseTransition
		require.NoError(t, json.Unmarshal(msg.Metadata, &pt))
		assert.Equal(t, transitions[i], pt)
	}
}

func TestApplyPhaseTransitions_NilRepoIsNoop(t *testing.T) {
	e := &Executor{logger: zerolog.Nop()}
	e.applyPhaseTransitions(context.Background(), "t", "e", []PhaseTransition{{Phase: "x"}})
}

func TestApplyPhaseTransitions_EmptyListIsNoop(t *testing.T) {
	msgRepo := &fakeMessageRepo{}
	e := &Executor{taskMessageRepo: msgRepo, logger: zerolog.Nop()}
	e.applyPhaseTransitions(context.Background(), "t", "e", nil)
	assert.Empty(t, msgRepo.inserted)
}

func TestApplyPhaseTransitions_InsertErrorIsLogged(t *testing.T) {
	msgRepo := &fakeMessageRepo{insertErr: errors.New("db down")}
	e := &Executor{taskMessageRepo: msgRepo, logger: zerolog.Nop()}
	// Best-effort: error doesn't propagate.
	assert.NotPanics(t, func() {
		e.applyPhaseTransitions(context.Background(), "t", "e", []PhaseTransition{
			{Phase: "design", Status: "enter"},
		})
	})
}
