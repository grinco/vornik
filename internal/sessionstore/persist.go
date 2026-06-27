// Package sessionstore provides the shared read-through / write-
// through helper every channel SessionStore uses to persist
// conversation state across daemon restarts + replicas.
//
// The channel stores (webchat / email / slack / github) already
// hold an in-memory map for fast Load. This package wraps the
// persistence.ChannelSessionRepository so each store gets:
//
//   - Load: check in-memory cache first, fall back to DB, populate
//     cache from DB result, return history.
//   - Save: write through to DB, then update in-memory cache.
//
// A nil repo makes both calls no-op on the DB side — the in-memory
// map remains the only state. That preserves the pre-feature
// behaviour for tests and for deployments that opt out.
package sessionstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// Persister is the narrow surface a channel SessionStore depends
// on. Wraps persistence.ChannelSessionRepository with the
// channel-kind already bound + JSON marshalling done at this
// boundary so the channel code stays simple.
type Persister struct {
	repo   persistence.ChannelSessionRepository
	kind   string
	logger zerolog.Logger
}

// New constructs a Persister bound to a channel kind. The kind
// goes into the (kind, session_id) composite PK so different
// channels can't collide on a shared session_id (e.g. an integer
// chat_id reused as a webchat cookie hash). repo may be nil —
// every method then becomes a no-op + ErrNotFound, falling back
// to the caller's in-memory cache.
func New(repo persistence.ChannelSessionRepository, kind string, logger zerolog.Logger) *Persister {
	return &Persister{repo: repo, kind: kind, logger: logger}
}

// Load returns the persisted history + active project for
// sessionID. (nil, "", false, nil) when no DB row exists or when
// the repo is unwired — the channel falls back to its in-memory
// cache. Errors other than ErrNotFound propagate so the channel
// can decide whether to log + serve from cache or fail-soft.
func (p *Persister) Load(ctx context.Context, sessionID string) (history []chat.Message, activeProject string, found bool, err error) {
	if p == nil || p.repo == nil {
		return nil, "", false, nil
	}
	row, err := p.repo.Load(ctx, p.kind, sessionID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	if len(row.History) > 0 {
		if jsonErr := json.Unmarshal(row.History, &history); jsonErr != nil {
			// Corrupt-row tolerance: bad JSON shouldn't break the
			// channel. Log + serve empty history; next turn will
			// rewrite the row with valid bytes.
			p.logger.Warn().Err(jsonErr).
				Str("kind", p.kind).
				Str("session_id", sessionID).
				Msg("session_store: corrupt history JSON; serving empty")
			history = nil
		}
	}
	return history, row.ActiveProject, true, nil
}

// Save write-through-persists the post-turn history + active
// project. No-op when the repo is unwired. The channel's in-memory
// cache is the caller's responsibility — Save only writes the
// durable row.
//
// Empty history wipes the persisted row (callers typically guard
// against that in their own Append by skipping when
// Result.Messages is empty, mirroring the pre-existing defensive
// behaviour).
func (p *Persister) Save(ctx context.Context, sessionID, activeProject string, history []chat.Message) error {
	if p == nil || p.repo == nil {
		return nil
	}
	raw, err := json.Marshal(history)
	if err != nil {
		// Should never happen — chat.Message is plain JSON-tagged.
		// Treat as a soft failure: log + don't fail the turn.
		p.logger.Error().Err(err).
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: marshal history; skipping persist")
		return nil
	}
	if err := p.repo.Save(ctx, p.kind, sessionID, activeProject, raw); err != nil {
		// Log + soft-fail so a transient DB blip doesn't break the
		// user's conversation. The in-memory cache still has the
		// post-turn history; the next successful Save catches up.
		p.logger.Warn().Err(err).
			Str("kind", p.kind).
			Str("session_id", sessionID).
			Msg("session_store: persist failed; in-memory cache still authoritative")
		return err
	}
	return nil
}

// Delete removes the persisted row. Used by "clear chat" affordances.
func (p *Persister) Delete(ctx context.Context, sessionID string) error {
	if p == nil || p.repo == nil {
		return nil
	}
	return p.repo.Delete(ctx, p.kind, sessionID)
}
