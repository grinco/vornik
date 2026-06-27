package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/sessionstore"
)

// fakeChanSessionRepo is a one-off in-memory ChannelSessionRepository
// for the per-channel write-through tests. The webchat package has
// its own copy because cross-package test helpers would require an
// exported test-helper package.
type fakeChanSessionRepo struct {
	mu   sync.Mutex
	rows map[string]*persistence.ChannelSession
}

func newFakeChanSessionRepo() *fakeChanSessionRepo {
	return &fakeChanSessionRepo{rows: map[string]*persistence.ChannelSession{}}
}

func chKey(kind, id string) string { return kind + "/" + id }

func (f *fakeChanSessionRepo) Load(_ context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[chKey(kind, sessionID)]; ok {
		cp := *row
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeChanSessionRepo) Save(_ context.Context, kind, sessionID, activeProject string, history []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[chKey(kind, sessionID)] = &persistence.ChannelSession{
		Kind:          kind,
		SessionID:     sessionID,
		ActiveProject: activeProject,
		History:       append([]byte(nil), history...),
		UpdatedAt:     time.Now(),
	}
	return nil
}

func (f *fakeChanSessionRepo) Delete(_ context.Context, kind, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, chKey(kind, sessionID))
	return nil
}

// TestEmailSessionStore_AppendWritesThroughToPersister: the
// post-turn write goes to both the in-memory map and the DB.
// Tested separately from the in-memory-only case so the existing
// container_email_receiver_test cases keep their no-persister
// invariant.
func TestEmailSessionStore_AppendWritesThroughToPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	store := newEmailSessionStore(nil, "proj-mail", nil)
	store.SetPersister(sessionstore.New(repo, "email", zerolog.Nop()))

	msgs := []chat.Message{
		{Role: "user", Content: "subject: status?"},
		{Role: "assistant", Content: "on it"},
	}
	require.NoError(t, store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "thread-abc"},
		dispatcher.Result{Messages: msgs}))

	row, err := repo.Load(context.Background(), "email", "thread-abc")
	require.NoError(t, err)
	assert.Equal(t, "proj-mail", row.ActiveProject)
	var got []chat.Message
	require.NoError(t, json.Unmarshal(row.History, &got))
	require.Len(t, got, 2)
}

// TestEmailSessionStore_LoadHydratesFromPersister: replica
// failover — fresh in-memory cache, DB pre-populated by a prior
// daemon. Load should re-hydrate.
func TestEmailSessionStore_LoadHydratesFromPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	prior := []chat.Message{{Role: "user", Content: "from-prior-replica"}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "email", "thread-z", "proj-mail", raw))

	store := newEmailSessionStore(nil, "proj-mail", nil)
	store.SetPersister(sessionstore.New(repo, "email", zerolog.Nop()))

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "thread-z"})
	require.NoError(t, err)
	require.Len(t, sess.History, 1)
	assert.Equal(t, "from-prior-replica", sess.History[0].Content)
}

// TestSlackSessionStore_AppendWritesThroughToPersister: slack's
// per-channel session_id (team/channel#thread_ts) round-trips
// through the DB with project pinning intact.
func TestSlackSessionStore_AppendWritesThroughToPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	store := newSlackSessionStore(nil, "proj-slack")
	store.SetPersister(sessionstore.New(repo, "slack", zerolog.Nop()))

	require.NoError(t, store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "T1/C2#1234.567"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "hey bot"}}}))

	row, err := repo.Load(context.Background(), "slack", "T1/C2#1234.567")
	require.NoError(t, err)
	assert.Equal(t, "proj-slack", row.ActiveProject)
}

// TestSlackSessionStore_LoadHydratesFromPersister mirrors the
// email-side replica-failover case.
func TestSlackSessionStore_LoadHydratesFromPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	prior := []chat.Message{{Role: "user", Content: "slack-prior"}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "slack", "T1/C2#1234.567", "proj-slack", raw))

	store := newSlackSessionStore(nil, "proj-slack")
	store.SetPersister(sessionstore.New(repo, "slack", zerolog.Nop()))

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "T1/C2#1234.567"})
	require.NoError(t, err)
	require.Len(t, sess.History, 1)
	assert.Equal(t, "slack-prior", sess.History[0].Content)
}

// TestGitHubSessionStore_AppendWritesThroughToPersister: github's
// `owner/repo#issues/N` session_id format round-trips and the
// per-session project resolver supplies the active_project.
func TestGitHubSessionStore_AppendWritesThroughToPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	store := newGitHubSessionStore(nil, "proj-github")
	store.SetPersister(sessionstore.New(repo, "github", zerolog.Nop()))

	require.NoError(t, store.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "acme/api#issues/42"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "@vornik run tests"}}}))

	row, err := repo.Load(context.Background(), "github", "acme/api#issues/42")
	require.NoError(t, err)
	assert.Equal(t, "proj-github", row.ActiveProject)
}

// TestGitHubSessionStore_LoadHydratesFromPersister.
func TestGitHubSessionStore_LoadHydratesFromPersister(t *testing.T) {
	repo := newFakeChanSessionRepo()
	prior := []chat.Message{{Role: "assistant", Content: "github-prior"}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "github", "acme/api#issues/42", "proj-github", raw))

	store := newGitHubSessionStore(nil, "proj-github")
	store.SetPersister(sessionstore.New(repo, "github", zerolog.Nop()))

	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/42"})
	require.NoError(t, err)
	require.Len(t, sess.History, 1)
	assert.Equal(t, "github-prior", sess.History[0].Content)
}

// TestPersisterEmptyAppendPreservesPersistedRow:
// the defensive "skip empty post-turn" branch must not wipe a
// previously persisted row across all three channel stores —
// they share the same guard so a regression in one likely
// breaks the others.
func TestPersisterEmptyAppendPreservesPersistedRow(t *testing.T) {
	type chanCase struct {
		name string
		kind string
		mk   func(*sessionstore.Persister) dispatcher.SessionStore
	}
	cases := []chanCase{
		{
			name: "email",
			kind: "email",
			mk: func(p *sessionstore.Persister) dispatcher.SessionStore {
				s := newEmailSessionStore(nil, "p", nil)
				s.SetPersister(p)
				return s
			},
		},
		{
			name: "slack",
			kind: "slack",
			mk: func(p *sessionstore.Persister) dispatcher.SessionStore {
				s := newSlackSessionStore(nil, "p")
				s.SetPersister(p)
				return s
			},
		},
		{
			name: "github",
			kind: "github",
			mk: func(p *sessionstore.Persister) dispatcher.SessionStore {
				s := newGitHubSessionStore(nil, "p")
				s.SetPersister(p)
				return s
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeChanSessionRepo()
			persister := sessionstore.New(repo, tc.kind, zerolog.Nop())
			prior := []chat.Message{{Role: "user", Content: "preserved"}}
			raw, _ := json.Marshal(prior)
			require.NoError(t, repo.Save(context.Background(), tc.kind, "sess-1", "p", raw))

			store := tc.mk(persister)
			require.NoError(t, store.Append(context.Background(),
				conversation.ChannelMessage{SessionID: "sess-1"},
				dispatcher.Result{Messages: nil}))

			row, err := repo.Load(context.Background(), tc.kind, "sess-1")
			require.NoError(t, err)
			var got []chat.Message
			require.NoError(t, json.Unmarshal(row.History, &got))
			assert.Equal(t, prior, got)
		})
	}
}

// TestChannelSessionPersister_NilContainerSafe: helper must
// degrade gracefully when the container or its repos haven't
// been initialised (test wiring + degraded boot path).
func TestChannelSessionPersister_NilContainerSafe(t *testing.T) {
	var c *Container
	if got := c.channelSessionPersister("webchat"); got != nil {
		t.Errorf("nil container should produce nil persister, got %v", got)
	}
}
