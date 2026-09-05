package dispatcher

import (
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// ctxRespectingChatAuditRepo behaves like a real database driver: every
// write observes the context it is handed and fails when that context is
// already done. The pre-existing stubChatAuditRepo ignores ctx entirely,
// which is precisely why the incident below went unnoticed by the
// existing suite.
type ctxRespectingChatAuditRepo struct {
	mu           sync.Mutex
	entries      []*persistence.ChatAuditEntry
	prompts      map[string]string
	insertCtxErr error
	promptCtxErr error
}

func newCtxRespectingChatAuditRepo() *ctxRespectingChatAuditRepo {
	return &ctxRespectingChatAuditRepo{prompts: map[string]string{}}
}

func (s *ctxRespectingChatAuditRepo) Insert(ctx context.Context, e *persistence.ChatAuditEntry) error {
	if err := ctx.Err(); err != nil {
		s.mu.Lock()
		s.insertCtxErr = err
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	return nil
}

func (s *ctxRespectingChatAuditRepo) List(_ context.Context, _ persistence.ChatAuditFilter) ([]*persistence.ChatAuditEntry, error) {
	return nil, nil
}

func (s *ctxRespectingChatAuditRepo) SavePrompt(ctx context.Context, body string) (string, error) {
	if err := ctx.Err(); err != nil {
		s.mu.Lock()
		s.promptCtxErr = err
		s.mu.Unlock()
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := persistence.HashChatSystemPrompt(body)
	s.prompts[hash] = body
	return hash, nil
}

func (s *ctxRespectingChatAuditRepo) GetByID(_ context.Context, _ string) (*persistence.ChatAuditEntry, error) {
	return nil, persistence.ErrNotFound
}

func (s *ctxRespectingChatAuditRepo) GetChatAuditsByTurnIDs(_ context.Context, _ []string) (map[string]persistence.ChatAuditEntry, error) {
	return map[string]persistence.ChatAuditEntry{}, nil
}

func (s *ctxRespectingChatAuditRepo) GetPrompt(_ context.Context, _ string) (string, error) {
	return "", nil
}

// TestChatAuditTurn_FinishSurvivesExpiredTurnContext pins the fix for the
// 2026-08-05 lost-deliverable incident on janka-companion.
//
// A Slack-originated task (task_20260805170431_955e423704153971) produced a
// finished report that never reached the requester. Root cause: the chat turn
// ran for ten minutes and exhausted its own context, and finish() was
// deferred on that SAME context — so the chat_audit_log insert failed with
// "context deadline exceeded" and the row was never written.
//
// That row is not merely an audit trail: chatorigin.ResolveForTurn looks the
// task's chat_turn_id up in it to find the originating channel. Without it the
// narrator's completion push and the UI's "Send to chat" button both conclude
// the task has no chat origin, so a completed deliverable is silently
// undeliverable — permanently, since nothing ever backfills the row.
//
// The audit write must therefore outlive the turn's context.
func TestChatAuditTurn_FinishSurvivesExpiredTurnContext(t *testing.T) {
	repo := newCtxRespectingChatAuditRepo()
	a := &Agent{logger: zerolog.Nop(), chatAuditRepo: repo}

	turn := newChatAuditTurn(a)
	require.NotNil(t, turn)
	turn.captureRequest("system-prompt-body", "please research Mounjaro", "lead")

	// The turn burned its whole budget before finish() ran — exactly the
	// state the deferred audit write observed in the incident.
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	turn.finish(expired, Request{
		OriginatingChannel:   "slack",
		OriginatingSessionID: "T03/D0B#main",
		Project:              "companion-janka",
	}, Result{Text: "done"})

	require.Len(t, repo.entries, 1,
		"chat_audit_log row must be written even though the turn context was already done — "+
			"without it the task's chat origin is unresolvable and its deliverable cannot be sent")
	assert.NoError(t, repo.insertCtxErr, "Insert must not observe a done context")
	assert.Equal(t, "slack:T03/D0B#main", repo.entries[0].ChatID)

	// The system prompt is stored on the same detached context.
	assert.NoError(t, repo.promptCtxErr, "SavePrompt must not observe a done context")
	assert.Len(t, repo.prompts, 1)
}
