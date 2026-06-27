package webchat

import (
	"context"
	"encoding/json"
	"strings"
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

// fakeChannelSessionRepo: in-memory ChannelSessionRepository for
// the webchat write-through tests. Mirrors the Postgres semantics
// on save (upsert) + load (ErrNotFound when missing).
type fakeChannelSessionRepo struct {
	mu   sync.Mutex
	rows map[string]*persistence.ChannelSession
}

func newFakeChannelSessionRepo() *fakeChannelSessionRepo {
	return &fakeChannelSessionRepo{rows: map[string]*persistence.ChannelSession{}}
}

func key(kind, id string) string { return kind + "/" + id }

func (f *fakeChannelSessionRepo) Load(_ context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[key(kind, sessionID)]; ok {
		cp := *row
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeChannelSessionRepo) Save(_ context.Context, kind, sessionID, activeProject string, history []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key(kind, sessionID)] = &persistence.ChannelSession{
		Kind:          kind,
		SessionID:     sessionID,
		ActiveProject: activeProject,
		History:       append([]byte(nil), history...),
		UpdatedAt:     time.Now(),
	}
	return nil
}

func (f *fakeChannelSessionRepo) Delete(_ context.Context, kind, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, key(kind, sessionID))
	return nil
}

// TestSessionStore_AppendWritesThroughToPersister: a post-turn
// Append should write the history to the DB AND to the
// in-memory map. Replicas restarting can then re-hydrate from
// the DB while the current daemon serves from cache.
func TestSessionStore_AppendWritesThroughToPersister(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	s := NewSessionStore(nil, "proj-x")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))

	msgs := []chat.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	err := s.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "browser-1"},
		dispatcher.Result{Messages: msgs},
	)
	require.NoError(t, err)

	row, err := repo.Load(context.Background(), "webchat", "browser-1")
	require.NoError(t, err)
	var got []chat.Message
	require.NoError(t, json.Unmarshal(row.History, &got))
	require.Equal(t, len(msgs), len(got))
	assert.Equal(t, "hello", got[1].Content)
	assert.Equal(t, "proj-x", row.ActiveProject)
}

// TestSessionStore_LoadHydratesFromPersisterWhenCacheEmpty:
// the restart-recovery + replica-failover path. Cache is empty
// (fresh process); a Load for a session previously saved by
// another daemon should re-populate the cache and return the
// persisted history.
func TestSessionStore_LoadHydratesFromPersisterWhenCacheEmpty(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	// Pre-seed as if another daemon (or pre-restart self) had saved.
	prior := []chat.Message{{Role: "user", Content: "from-prior-replica"}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "webchat", "browser-2", "proj-y", raw))

	s := NewSessionStore(nil, "proj-y")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))

	sess, err := s.Load(context.Background(), conversation.ChannelMessage{SessionID: "browser-2"})
	require.NoError(t, err)
	require.Len(t, sess.History, 1)
	assert.Equal(t, "from-prior-replica", sess.History[0].Content)

	// Cache should now be warm — subsequent Loads don't hit the
	// repo. We can't easily assert that without instrumentation,
	// but a second Load returning the same content is the
	// observable contract.
	again, err := s.Load(context.Background(), conversation.ChannelMessage{SessionID: "browser-2"})
	require.NoError(t, err)
	require.Len(t, again.History, 1)
}

// TestSessionStore_AppendEmptyMessagesPreservesPersistedRow:
// the defensive "skip empty post-turn" branch must not wipe the
// already-persisted history. A dispatcher error producing an
// empty Result.Messages would otherwise destroy the
// conversation across replicas.
func TestSessionStore_AppendEmptyMessagesPreservesPersistedRow(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	// Pre-seed a real row.
	prior := []chat.Message{{Role: "user", Content: "preserved"}}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "webchat", "browser-3", "proj", raw))

	s := NewSessionStore(nil, "proj")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))

	err := s.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "browser-3"},
		dispatcher.Result{Messages: nil},
	)
	require.NoError(t, err)

	// Row must still carry the pre-seeded history.
	row, err := repo.Load(context.Background(), "webchat", "browser-3")
	require.NoError(t, err)
	var got []chat.Message
	require.NoError(t, json.Unmarshal(row.History, &got))
	require.Equal(t, prior, got)
}

// TestSessionStore_ResetDeletesPersistedRow: "clear chat" must
// also wipe the DB so a replica failover doesn't replay the
// just-cleared conversation.
func TestSessionStore_ResetDeletesPersistedRow(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	s := NewSessionStore(nil, "proj")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))

	require.NoError(t, s.Append(context.Background(),
		conversation.ChannelMessage{SessionID: "browser-4"},
		dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "x"}}},
	))
	// Pre-condition: row exists.
	_, err := repo.Load(context.Background(), "webchat", "browser-4")
	require.NoError(t, err)

	s.Reset("browser-4")

	// Row should be gone.
	_, err = repo.Load(context.Background(), "webchat", "browser-4")
	require.ErrorIs(t, err, persistence.ErrNotFound)
}

// TestSessionStore_ContextTier_NoBudgetMeansPeak — Slice 4 contract:
// a deployment without a chatContextBudget produces TierPeak ("no
// signal") regardless of history size, and ContextHeadroomPct stays
// at zero. The chat panel hides the badge in this case, matching the
// legacy chat surface byte-for-byte.
func TestSessionStore_ContextTier_NoBudgetMeansPeak(t *testing.T) {
	s := NewSessionStore(nil, "p1")
	// Plenty of history but no budget — tier stays PEAK.
	s.history["sess-a"] = []chat.Message{
		{Role: "user", Content: strings.Repeat("x", 100_000)},
	}
	sess, err := s.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "sess-a",
		Text:      "hi",
	})
	require.NoError(t, err)
	assert.Equal(t, chat.TierPeak, sess.ContextTier)
	assert.Zero(t, sess.ContextHeadroomPct,
		"no-budget deployment must leave headroom at zero so the UI hides the badge")
}

// TestSessionStore_ContextTier_BandsAcrossUsage exercises the four
// bands using known token counts against a 1_000-token budget. Pins
// the per-band cut-offs: PEAK <50%, GOOD <75%, DEGRADING <90%, POOR
// from 90% up.
func TestSessionStore_ContextTier_BandsAcrossUsage(t *testing.T) {
	cases := []struct {
		name      string
		contentCh int // history-content chars (chars/4 = tokens)
		want      chat.ContextTier
	}{
		{"empty (0 tok) → PEAK", 0, chat.TierPeak},
		{"40% used (400 tok) → PEAK", 1_600, chat.TierPeak},
		{"60% used (600 tok) → GOOD", 2_400, chat.TierGood},
		{"80% used (800 tok) → DEGRADING", 3_200, chat.TierDegrading},
		{"95% used (950 tok) → POOR", 3_800, chat.TierPoor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSessionStore(nil, "p1")
			s.ContextBudget = 1_000
			if c.contentCh > 0 {
				s.history["sess"] = []chat.Message{
					{Role: "user", Content: strings.Repeat("x", c.contentCh)},
				}
			}
			sess, err := s.Load(context.Background(), conversation.ChannelMessage{
				SessionID: "sess",
				Text:      "", // inbound text doesn't tip the band in these fixtures
			})
			require.NoError(t, err)
			assert.Equal(t, c.want, sess.ContextTier,
				"history chars=%d → expected tier %s", c.contentCh, c.want)
		})
	}
}

// TestSessionStore_ContextHeadroomPct_RidesAlongsideTier — when the
// budget is configured, Load populates ContextHeadroomPct with the
// matching [0, 100] value. The dispatcher consumes this for the
// histogram + the UI surfaces it in the tooltip.
func TestSessionStore_ContextHeadroomPct_RidesAlongsideTier(t *testing.T) {
	s := NewSessionStore(nil, "p1")
	s.ContextBudget = 1_000
	// 600 tokens used → 40% headroom.
	s.history["sess"] = []chat.Message{
		{Role: "user", Content: strings.Repeat("x", 2_400)},
	}
	sess, err := s.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "sess",
	})
	require.NoError(t, err)
	assert.InDelta(t, 40.0, sess.ContextHeadroomPct, 0.001)
	assert.Equal(t, chat.TierGood, sess.ContextTier)
}

// TestSessionStore_InboundTextContributesToTokenCount — the inbound
// message's own text adds to the per-turn estimate, so a small history
// + huge prompt still tips the tier. Catches a regression where the
// store would forget the new user turn when bucketing.
func TestSessionStore_InboundTextContributesToTokenCount(t *testing.T) {
	s := NewSessionStore(nil, "p1")
	s.ContextBudget = 1_000
	// Empty history; the inbound prompt alone is enough to tip
	// DEGRADING (800/1000 used).
	sess, err := s.Load(context.Background(), conversation.ChannelMessage{
		SessionID: "sess",
		Text:      strings.Repeat("x", 3_200), // 800 tokens
	})
	require.NoError(t, err)
	assert.Equal(t, chat.TierDegrading, sess.ContextTier,
		"inbound text must be folded into the per-turn estimate")
}

// TestEstimateTokens_CharsOver4 — pure-function sanity check on the
// helper. Validates the chars/4 heuristic and the empty-slice edge
// case.
func TestEstimateTokens_CharsOver4(t *testing.T) {
	assert.Equal(t, 0, estimateTokens(nil))
	assert.Equal(t, 0, estimateTokens([]chat.Message{}))
	assert.Equal(t, 25, estimateTokens([]chat.Message{
		{Content: strings.Repeat("x", 100)},
	}))
	assert.Equal(t, 25+50, estimateTokens([]chat.Message{
		{Content: strings.Repeat("x", 100)},
		{Content: strings.Repeat("y", 200)},
	}))
}

// TestSessionStore_HistoryHydratesFromPersisterWhenCacheEmpty pins the
// web-chat page-load fix: on a cold cache (daemon restart / replica /
// first visit) the GET render reads history via History(), which must
// read-through from the persister just like Load() — otherwise the page
// renders an empty thread and the conversation only reappears after the
// first message warms the cache through Load().
func TestSessionStore_HistoryHydratesFromPersisterWhenCacheEmpty(t *testing.T) {
	repo := newFakeChannelSessionRepo()
	prior := []chat.Message{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}
	raw, _ := json.Marshal(prior)
	require.NoError(t, repo.Save(context.Background(), "webchat", "browser-hist", "proj-h", raw))

	s := NewSessionStore(nil, "proj-h")
	s.SetPersister(sessionstore.New(repo, "webchat", zerolog.Nop()))

	// Cold cache: History (the GET page-load path) must still return the
	// persisted conversation without a prior Load/Append warming it.
	got := s.History("browser-hist")
	require.Len(t, got, 2, "History must read through to the persister on a cache miss")
	assert.Equal(t, "earlier question", got[0].Content)
	assert.Equal(t, "earlier answer", got[1].Content)

	// Unknown session with no persisted row → empty, no error/panic.
	assert.Empty(t, s.History("never-existed"))
}

// TestSessionStore_HistoryNoPersisterStillWorks: without a persister
// (in-memory-only deployments) History returns the cache as before.
func TestSessionStore_HistoryNoPersisterStillWorks(t *testing.T) {
	s := NewSessionStore(nil, "proj")
	assert.Empty(t, s.History("x"))
}
