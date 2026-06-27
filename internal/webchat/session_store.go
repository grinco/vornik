package webchat

import (
	"context"
	"sync"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/sessionstore"
)

// SessionStore implements dispatcher.SessionStore for the web
// chat channel. Each cookie-identified browser session has its
// own history slice; the daemon's lifetime bounds the history.
// On daemon restart the next prompt starts a fresh dispatcher
// turn — no DB persistence, matching the GitHub channel.
//
// The store is bound to a single project per browser pageview.
// The project comes from the URL (/ui/projects/<id>/chat), not
// the session — so a SessionStore is project-scoped and the UI
// handler resolves AvailableProjects / LeadSystemPrompt off the
// registry at Load time.
type SessionStore struct {
	registry  *registry.Registry
	projectID string

	mu      sync.Mutex
	history map[string][]chat.Message

	// HistoryCap caps how many turns per session the store keeps.
	// Zero means unbounded; production wires a positive value so a
	// chatty session can't exhaust process memory. 200 is the
	// default used by NewSessionStore — comfortably above any
	// reasonable conversation depth, well under any GC concern.
	HistoryCap int

	// ContextBudget is the token-window size against which the
	// per-turn context-budget tier is computed. Zero disables the
	// tier surface (Session.ContextTier stays at TierPeak — "no
	// signal"). When set, Load estimates the conversation's token
	// usage with chars/4 and divides by ContextBudget to produce
	// the PEAK / GOOD / DEGRADING / POOR band the dispatcher uses
	// to force tool deferral and the UI renders as a colored
	// badge.
	ContextBudget int

	// persister (optional) write-through-persists session state
	// to Postgres so a daemon restart or replica failover doesn't
	// drop conversations. Nil keeps the pre-feature in-memory-only
	// behaviour for tests + opt-out deployments. Set via
	// SetPersister at wire time.
	persister *sessionstore.Persister
}

// SetPersister wires the read-through / write-through DB layer.
// Called once at container-wiring time; the channel kind is
// "webchat" so the (kind, session_id) composite PK keeps cookie
// hashes isolated from other channels' session ids.
func (s *SessionStore) SetPersister(p *sessionstore.Persister) {
	s.persister = p
}

// NewSessionStore returns an empty store bound to projectID. nil
// registry is allowed (the store then yields empty
// AvailableProjects / LeadSystemPrompt — useful in tests).
func NewSessionStore(reg *registry.Registry, projectID string) *SessionStore {
	return &SessionStore{
		registry:   reg,
		projectID:  projectID,
		history:    make(map[string][]chat.Message),
		HistoryCap: 200,
	}
}

// Load returns the dispatcher.Session for the inbound message.
// History is copied (not aliased) so a concurrent Append on a
// different SessionID can't race the dispatcher's read.
//
// LeadSystemPrompt is resolved from the project's swarm lead role
// when the registry is wired; channels without a lead prompt fall
// back to the dispatcher's default system prompt.
func (s *SessionStore) Load(ctx context.Context, msg conversation.ChannelMessage) (dispatcher.Session, error) {
	history := s.historyWithReadThrough(ctx, msg.SessionID)

	sess := dispatcher.Session{
		History:       history,
		ActiveProject: s.projectID,
	}
	// Tier computation rides at Load time so the dispatcher and the
	// UI see the same band for this turn. Token estimate matches the
	// chat.Conversation.EstimateTokens chars/4 heuristic plus the
	// inbound message's own chars/4 — the dispatcher will see this
	// estimate's cost on the next LLM call. Zero ContextBudget means
	// the deployment opted out; tier stays at PEAK ("no signal").
	if s.ContextBudget > 0 {
		used := estimateTokens(history) + len(msg.Text)/charsPerTokenEstimate
		sess.ContextTier = chat.TierFromUsage(used, s.ContextBudget)
		sess.ContextHeadroomPct = chat.HeadroomPct(used, s.ContextBudget)
	}
	if s.registry == nil || s.projectID == "" {
		return sess, nil
	}

	project := s.registry.GetProject(s.projectID)
	if project == nil {
		return sess, nil
	}
	sess.AvailableProjects = s.registry.ListProjects()
	// Web chat is project-pinned at the URL; restrict dispatcher
	// tools to the URL's project so a chatty prompt can't
	// accidentally route create_task into a sibling project the
	// operator wasn't looking at.
	sess.AllowedProjects = []string{s.projectID}

	if leadPrompt, _ := dispatcher.ResolveLeadPrompt(s.registry, s.projectID); leadPrompt != "" {
		swarm := s.registry.GetSwarm(project.SwarmID)
		sess.LeadSystemPrompt = dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, sess.AvailableProjects)
	}
	return sess, nil
}

// Append records the dispatcher's reply turn. Replaces history
// rather than appending — same contract as Result.Messages
// (`the full updated conversation history`).
//
// HistoryCap trim happens here: if the post-turn history exceeds
// the cap, the oldest entries are discarded keeping the most
// recent ones. The dispatcher's own history-trim runs upstream
// for in-flight context windows; this cap is a memory guard, not
// a context-budget guard.
func (s *SessionStore) Append(ctx context.Context, msg conversation.ChannelMessage, result dispatcher.Result) error {
	if len(result.Messages) == 0 {
		// Defensive: an empty Messages slice would wipe history on
		// a dispatcher error path. Skip rather than clobber.
		return nil
	}
	s.mu.Lock()
	updated := append([]chat.Message(nil), result.Messages...)
	if s.HistoryCap > 0 && len(updated) > s.HistoryCap {
		updated = updated[len(updated)-s.HistoryCap:]
	}
	s.history[msg.SessionID] = updated
	s.mu.Unlock()

	// Write-through persistence. Soft-fails on DB error: the
	// in-memory cache already has the post-turn state, so the
	// user's current conversation is unaffected. The next
	// successful Save catches up.
	if s.persister != nil {
		_ = s.persister.Save(ctx, msg.SessionID, s.projectID, updated)
	}
	return nil
}

// History returns a copy of the stored history for sessionID, with the
// same persister read-through as Load. Used by the UI handler to render
// the message list on a GET page load — without the read-through, a
// cold cache (daemon restart, replica takeover, or simply the first
// visit) rendered an EMPTY thread on load and the conversation only
// reappeared after the first message warmed the cache via Load. Empty
// slice for unknown ids.
func (s *SessionStore) History(sessionID string) []chat.Message {
	// Page render shouldn't hang on a slow DB; bound the read-through.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.historyWithReadThrough(ctx, sessionID)
}

// historyWithReadThrough returns the cached history for sessionID,
// hydrating from the persister on a cache miss and filling the cache.
// Shared by Load (turn dispatch) and History (UI render) so both
// surfaces see persisted conversations — the read-through used to live
// only in Load, which is why a fresh page load showed no history.
func (s *SessionStore) historyWithReadThrough(ctx context.Context, sessionID string) []chat.Message {
	s.mu.Lock()
	history := append([]chat.Message(nil), s.history[sessionID]...)
	s.mu.Unlock()

	if len(history) == 0 && s.persister != nil && sessionID != "" {
		if persisted, _, found, err := s.persister.Load(ctx, sessionID); err == nil && found && len(persisted) > 0 {
			history = persisted
			s.mu.Lock()
			s.history[sessionID] = append([]chat.Message(nil), persisted...)
			s.mu.Unlock()
		}
	}
	return history
}

// Reset drops a session's history. Used by the UI's "clear chat"
// affordance; not auto-fired on logout because daemon-side state
// already has no auth tie. Also wipes the persisted row so a
// replica can't replay the cleared conversation.
func (s *SessionStore) Reset(sessionID string) {
	s.mu.Lock()
	delete(s.history, sessionID)
	s.mu.Unlock()
	if s.persister != nil {
		_ = s.persister.Delete(context.Background(), sessionID)
	}
}

// charsPerTokenEstimate matches the heuristic in
// chat.Conversation.EstimateTokens — a rough "1 token ≈ 4 chars"
// approximation. Good enough for tier-banding (10-20% error doesn't
// move the four-tier verdict) and stays close to the dispatcher's
// own estimate.
const charsPerTokenEstimate = 4

// estimateTokens returns the chars/4 token approximation for a
// message slice. Mirrors chat.Conversation.EstimateTokens's logic
// but stateless — the webchat store doesn't own a Conversation
// object so we sum directly off the dispatcher.Result.Messages
// shape it stores.
func estimateTokens(msgs []chat.Message) int {
	var n int
	for _, m := range msgs {
		n += len(m.Content) / charsPerTokenEstimate
	}
	return n
}

// Compile-time guard.
var _ dispatcher.SessionStore = (*SessionStore)(nil)
