package ui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// timeFromHM builds a UTC time on a fixed test date — used by the
// unified-timeline merge test (2026-05-26) to control message +
// hint ordering deterministically.
func timeFromHM(h, m int) time.Time {
	return time.Date(2026, 5, 26, h, m, 0, 0, time.UTC)
}

// scratchpadRig is a TaskScratchpadRepository stub for the
// loadConversationView happy-path test (scratchpad + phase
// tracker populate).
type scratchpadRig struct {
	sp  *persistence.TaskScratchpad
	err error
}

func (r *scratchpadRig) Get(context.Context, string) (*persistence.TaskScratchpad, error) {
	return r.sp, r.err
}
func (r *scratchpadRig) Upsert(context.Context, *persistence.TaskScratchpad) error { return nil }

// extendedMessageRepo extends fakeUIMessageRepo to return seeded
// messages + an open checkpoint, exercising the conversation-view
// happy-path branches.
type extendedMessageRepo struct {
	fakeUIMessageRepo
	msgs       []*persistence.TaskMessage
	checkpoint *persistence.TaskMessage
	listErr    error
}

func (r *extendedMessageRepo) List(context.Context, persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return r.msgs, r.listErr
}
func (r *extendedMessageRepo) GetOpenCheckpoint(context.Context, string) (*persistence.TaskMessage, error) {
	return r.checkpoint, nil
}

// hintRepoStub returns a fixed list of task-scoped hints for the
// loadConversationView merge test. Other methods are unused.
type hintRepoStub struct {
	hints []*persistence.ExecutionHint
	err   error
}

func (h *hintRepoStub) Insert(context.Context, *persistence.ExecutionHint) error { return nil }
func (h *hintRepoStub) ConsumePending(context.Context, string, string, string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}
func (h *hintRepoStub) ListByExecution(context.Context, string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}
func (h *hintRepoStub) ListForExecution(context.Context, string, string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}
func (h *hintRepoStub) ListPendingForTask(context.Context, string) ([]*persistence.ExecutionHint, error) {
	return nil, nil
}
func (h *hintRepoStub) ListByTask(context.Context, string) ([]*persistence.ExecutionHint, error) {
	return h.hints, h.err
}

// TestLoadConversationView_MergesHintsIntoTimeline — 2026-05-26
// unified-timeline refactor: task-scoped steering hints surface as
// synthetic TaskMessage rows with MessageKind="hint" interleaved
// with the actual messages by CreatedAt. Pins the merge + ordering
// + applied/pending state badge so the template's switch on
// MessageKind picks up the hint case.
func TestLoadConversationView_MergesHintsIntoTimeline(t *testing.T) {
	t0 := timeFromHM(10, 0)
	t1 := timeFromHM(10, 5)
	t2 := timeFromHM(10, 10)
	applied := timeFromHM(10, 7)
	msgs := &extendedMessageRepo{
		msgs: []*persistence.TaskMessage{
			{ID: "m1", TaskID: "t1", Content: "first", CreatedAt: t0,
				AuthorKind:  persistence.TaskMessageAuthorOperator,
				MessageKind: persistence.TaskMessageKindMessage},
			{ID: "m2", TaskID: "t1", Content: "second", CreatedAt: t2,
				AuthorKind:  persistence.TaskMessageAuthorLead,
				MessageKind: persistence.TaskMessageKindNote},
		},
	}
	hints := &hintRepoStub{
		hints: []*persistence.ExecutionHint{
			{ID: "h1", TaskID: "t1", Content: "use Reuters", CreatedAt: t1, AppliedAt: &applied},
			{ID: "h2", TaskID: "t1", Content: "skip portal X", CreatedAt: t2},
		},
	}
	srv := NewServer(
		WithTaskMessageRepository(msgs),
		WithHintRepository(hints),
	)
	got := srv.loadConversationView(context.Background(),
		&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})
	// 2 messages + 2 hints → 4 timeline entries.
	if !assert.Len(t, got.Messages, 4, "messages and hints must merge into one timeline") {
		return
	}
	// Order: m1 (10:00) → h1 (10:05) → m2 (10:10) AND h2 (10:10).
	// Tied timestamps use stable order — h2 after m2 (the slice was
	// built by appending hints after messages).
	assert.Equal(t, "m1", got.Messages[0].ID)
	assert.Equal(t, "hintmsg_h1", got.Messages[1].ID)
	assert.Equal(t, persistence.TaskMessageKindHint, got.Messages[1].MessageKind,
		"synthetic hint rows must carry MessageKind=hint so the template's switch picks the right colour + badge")
	assert.Contains(t, got.Messages[1].Content, "applied", "applied hint must show the state badge")
	assert.Contains(t, got.Messages[1].Content, "use Reuters")
	assert.Equal(t, persistence.TaskMessageAuthorOperator, got.Messages[1].AuthorKind)
	// Pending hint shows the pending marker.
	pendingIdx := 3
	if got.Messages[2].ID == "hintmsg_h2" {
		pendingIdx = 2
	}
	assert.Contains(t, got.Messages[pendingIdx].Content, "pending")
}

// TestLoadConversationView_NoHintRepoFallsBackToMessagesOnly —
// legacy path: when the hint repo isn't wired the merge is a no-op
// and the page renders messages only, same as before the refactor.
func TestLoadConversationView_NoHintRepoFallsBackToMessagesOnly(t *testing.T) {
	repo := &extendedMessageRepo{
		msgs: []*persistence.TaskMessage{
			{ID: "m1", TaskID: "t1", Content: "hi"},
		},
	}
	srv := NewServer(WithTaskMessageRepository(repo))
	got := srv.loadConversationView(context.Background(),
		&persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted})
	assert.Len(t, got.Messages, 1)
	// No hint rows in the timeline — repo isn't wired so the merge
	// is a clean no-op.
	for _, m := range got.Messages {
		assert.NotEqual(t, persistence.TaskMessageKindHint, m.MessageKind,
			"no hint rows should appear when hintRepo isn't wired")
	}
}

func TestLoadConversationView_DisabledWhenNoRepo(t *testing.T) {
	srv := NewServer()
	got := srv.loadConversationView(context.Background(), &persistence.Task{ID: "t1"})
	assert.False(t, got.Enabled)
}

func TestLoadConversationView_NilTaskShortCircuit(t *testing.T) {
	srv := NewServer(WithTaskMessageRepository(&fakeUIMessageRepo{}))
	got := srv.loadConversationView(context.Background(), nil)
	assert.True(t, got.Enabled)
	assert.Nil(t, got.Messages)
}

func TestLoadConversationView_HappyPathPopulatesMessages(t *testing.T) {
	repo := &extendedMessageRepo{
		msgs: []*persistence.TaskMessage{
			{ID: "m1", TaskID: "t1", Content: "hi"},
			{ID: "m2", TaskID: "t1", Content: "hello"},
		},
	}
	srv := NewServer(WithTaskMessageRepository(repo))
	got := srv.loadConversationView(context.Background(),
		&persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted})
	assert.True(t, got.Enabled)
	assert.Len(t, got.Messages, 2)
}

func TestLoadConversationView_ListErrorSilentlyEmpty(t *testing.T) {
	repo := &extendedMessageRepo{listErr: errors.New("db down")}
	srv := NewServer(WithTaskMessageRepository(repo))
	got := srv.loadConversationView(context.Background(),
		&persistence.Task{ID: "t1"})
	assert.True(t, got.Enabled)
	assert.Nil(t, got.Messages)
}

func TestLoadConversationView_OpenCheckpointPopulates(t *testing.T) {
	cp := &persistence.TaskMessage{
		ID:       "cp-1",
		TaskID:   "t1",
		Metadata: []byte(`{"kind":"choice","question":"approve?"}`),
	}
	open := "cp-1"
	repo := &extendedMessageRepo{checkpoint: cp}
	srv := NewServer(WithTaskMessageRepository(repo))
	task := &persistence.Task{ID: "t1", OpenCheckpointID: &open, Status: persistence.TaskStatusAwaitingInput}
	got := srv.loadConversationView(context.Background(), task)
	if assert.NotNil(t, got.OpenCheckpoint) {
		assert.Equal(t, "cp-1", got.OpenCheckpoint.ID)
	}
	if assert.NotNil(t, got.CheckpointPayload) {
		assert.Equal(t, "choice", got.CheckpointPayload.Kind)
		assert.Equal(t, "approve?", got.CheckpointPayload.Question)
	}
}

func TestLoadConversationView_ScratchpadPopulatesPhaseTracker(t *testing.T) {
	currentPhase := "design"
	sp := &persistence.TaskScratchpad{
		TaskID:        "t1",
		Summary:       "running design pass",
		OpenQuestions: []byte(`["why is X slow?"]`),
		CurrentPhase:  &currentPhase,
		PhaseHistory:  []byte(`[{"name":"intake","status":"done"},{"name":"design","status":"active"}]`),
	}
	srv := NewServer(
		WithTaskMessageRepository(&fakeUIMessageRepo{}),
		WithTaskScratchpadRepository(&scratchpadRig{sp: sp}),
	)
	got := srv.loadConversationView(context.Background(),
		&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})
	if assert.NotNil(t, got.Scratchpad) {
		assert.Equal(t, "running design pass", got.Scratchpad.Summary)
	}
	assert.Equal(t, []string{"why is X slow?"}, got.ScratchpadOpenQuestions)
	if assert.Len(t, got.PhaseTracker, 2) {
		assert.Equal(t, "intake", got.PhaseTracker[0].Name)
		assert.False(t, got.PhaseTracker[0].IsCurrent)
		assert.Equal(t, "design", got.PhaseTracker[1].Name)
		assert.True(t, got.PhaseTracker[1].IsCurrent)
	}
}

func TestLoadConversationView_StatusFlags(t *testing.T) {
	srv := NewServer(WithTaskMessageRepository(&fakeUIMessageRepo{}))

	t.Run("completed is closeable", func(t *testing.T) {
		got := srv.loadConversationView(context.Background(),
			&persistence.Task{ID: "t1", Status: persistence.TaskStatusCompleted})
		assert.True(t, got.Closeable)
		assert.False(t, got.Pauseable)
		assert.False(t, got.Resumeable)
	})

	t.Run("running is pauseable", func(t *testing.T) {
		got := srv.loadConversationView(context.Background(),
			&persistence.Task{ID: "t1", Status: persistence.TaskStatusRunning})
		assert.False(t, got.Closeable)
		assert.True(t, got.Pauseable)
		assert.False(t, got.Resumeable)
	})

	t.Run("paused is resumeable", func(t *testing.T) {
		got := srv.loadConversationView(context.Background(),
			&persistence.Task{ID: "t1", Status: persistence.TaskStatusPaused})
		assert.False(t, got.Closeable)
		assert.False(t, got.Pauseable)
		assert.True(t, got.Resumeable)
	})

	t.Run("awaiting input is both closeable and pauseable", func(t *testing.T) {
		got := srv.loadConversationView(context.Background(),
			&persistence.Task{ID: "t1", Status: persistence.TaskStatusAwaitingInput})
		assert.True(t, got.Closeable)
		assert.True(t, got.Pauseable)
		assert.False(t, got.Resumeable)
	})
}
