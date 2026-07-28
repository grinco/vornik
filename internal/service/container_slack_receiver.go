package service

import (
	"context"
	"strings"
	"sync"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/sessionstore"
	"vornik.io/vornik/internal/slack"
)

// slackSessionStore implements dispatcher.SessionStore for the Slack
// ConversationChannel. Mirrors emailSessionStore + githubSessionStore
// shape: one in-memory history map keyed on the channel's SessionID
// (the Slack thread root encoded as <team>/<channel>#<thread_ts>)
// plus a constant project resolver so every session stays inside the
// project that owns this channel.
//
// Per-project routing: one slackSessionStore per channel, each
// pinned to its own project. The container's lifecycle code in
// container.go constructs one store-and-receiver pair per
// SlackChannels[i].
//
// Daemon restart clears the in-memory history; the persisted truth
// is the Slack thread itself. Subsequent messages on the same thread
// start a fresh dispatcher turn — operators rely on the channel's
// thread metadata + the bot's reply text to carry context across
// restarts, not in-process state. Matches the email + github
// channels' "history is best-effort, the platform thread is the
// authoritative record" contract.
type slackSessionStore struct {
	registry         *registry.Registry
	projectID        string
	maxHistory       int
	maxHistoryTokens int

	mu      sync.Mutex
	history map[string][]chat.Message

	// persister (optional) DB-backs the session so a restart /
	// replica failover doesn't drop the in-flight conversation.
	// Nil = pre-feature in-memory-only behaviour.
	persister *sessionstore.Persister
}

// SetPersister wires the DB-backed persistence layer.
func (s *slackSessionStore) SetPersister(p *sessionstore.Persister) {
	s.persister = p
}

// newSlackSessionStore constructs a per-channel session store pinned
// to the supplied project. projectID may be empty in degenerate test
// wiring; production always supplies one because each Slack channel
// instance is one-project-per-workspace.
func newSlackSessionStore(reg *registry.Registry, projectID string) *slackSessionStore {
	return newSlackSessionStoreWithLimits(reg, projectID, 100, 0)
}

// newSlackSessionStoreWithLimits constructs a Slack store with the same
// message-count and token-budget policy used by Telegram conversations.
func newSlackSessionStoreWithLimits(
	reg *registry.Registry,
	projectID string,
	maxHistory int,
	maxHistoryTokens int,
) *slackSessionStore {
	return &slackSessionStore{
		registry:         reg,
		projectID:        projectID,
		maxHistory:       maxHistory,
		maxHistoryTokens: maxHistoryTokens,
		history:          make(map[string][]chat.Message),
	}
}

// boundedHistory applies chat.Conversation's whole-turn trimming semantics.
// It is used on both append and persisted-session hydration so an old
// unbounded channel_sessions row cannot be replayed wholesale after upgrade.
func (s *slackSessionStore) boundedHistory(messages []chat.Message) ([]chat.Message, int) {
	conv := chat.NewConversation("slack-session", s.maxHistory)
	conv.SetMaxTokens(s.maxHistoryTokens)
	for _, m := range messages {
		conv.AddMessage(m)
	}
	return conv.GetMessages(), conv.EstimateTokens()
}

// Load returns the per-session conversation snapshot for the
// dispatcher. History is copied (not aliased) so a concurrent
// Append on a different SessionID doesn't race the dispatcher.
//
// AllowedProjects is scoped to the single owning project so an
// inbound message from one Slack workspace can't accidentally route
// the dispatcher's create_task into another project — matches the
// email channel's per-project scoping.
func (s *slackSessionStore) Load(ctx context.Context, msg conversation.ChannelMessage) (dispatcher.Session, error) {
	s.mu.Lock()
	history := append([]chat.Message(nil), s.history[msg.SessionID]...)
	s.mu.Unlock()

	if len(history) == 0 && s.persister != nil {
		if persisted, _, found, err := s.persister.Load(ctx, msg.SessionID); err == nil && found && len(persisted) > 0 {
			history, _ = s.boundedHistory(persisted)
			s.mu.Lock()
			if current := s.history[msg.SessionID]; len(current) > 0 {
				// Append or another Load won the hydration race. Use its newer
				// snapshot and never overwrite persistence with stale history.
				history = append([]chat.Message(nil), current...)
				s.mu.Unlock()
			} else {
				s.history[msg.SessionID] = append([]chat.Message(nil), history...)
				// Keep hydration and legacy-row compaction ordered ahead of
				// Append. Append takes the same lock before publishing newer
				// history, then persists it after this save completes.
				_ = s.persister.Save(ctx, msg.SessionID, s.projectID, history)
				s.mu.Unlock()
			}
		}
	}

	sess := dispatcher.Session{
		History:       history,
		ActiveProject: s.projectID,
	}
	_, estimatedTokens := s.boundedHistory(history)
	estimatedTokens += len(msg.Text) / 4
	sess.ContextTier = chat.TierFromUsage(estimatedTokens, s.maxHistoryTokens)
	sess.ContextHeadroomPct = chat.HeadroomPct(estimatedTokens, s.maxHistoryTokens)
	if s.registry == nil || s.projectID == "" {
		return sess, nil
	}
	project := s.registry.GetProject(s.projectID)
	if project == nil {
		return sess, nil
	}
	sess.AvailableProjects = s.registry.ListProjects()
	sess.AllowedProjects = []string{s.projectID}

	if leadPrompt, _ := dispatcher.ResolveLeadPrompt(s.registry, s.projectID); leadPrompt != "" {
		swarm := s.registry.GetSwarm(project.SwarmID)
		sess.LeadSystemPrompt = dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, sess.AvailableProjects)
		sess.LeadSystemPrompt += s.threadDigestBlock(ctx, msg.SessionID)
	}
	return sess, nil
}

// threadDigestBlock renders the "what was discussed in this channel's threads"
// system-prompt block for a CHANNEL-scoped turn, or "" for anything else.
//
// Only channel-scoped sessions get it: inside a thread the lead already has
// that thread's own history, and injecting sibling threads there would be noise
// plus a cross-thread bleed for no benefit.
//
// Best-effort throughout — a digest is an enrichment, so a persistence error
// degrades to "no block" rather than failing the user's turn.
func (s *slackSessionStore) threadDigestBlock(ctx context.Context, sessionID string) string {
	if threadKeyFromSessionID(sessionID) != slack.ChannelSessionThreadRoot {
		return ""
	}
	prefix := channelPrefixFromSessionID(sessionID)
	if prefix == "" {
		return ""
	}

	// Durable view first. Fetch more than we render so that excluding the
	// caller and non-thread siblings can't starve the block below the cap.
	siblings, err := s.persister.ListByPrefix(ctx, prefix, maxDigestThreads*3)
	if err != nil {
		return ""
	}
	if len(siblings) == 0 {
		// No persistence (SQLite / unwired) or nothing stored yet — fall back
		// to whatever this process has seen. Without this, single-process
		// deployments would get no digests at all.
		siblings = s.inMemorySiblings(prefix)
	}
	return renderThreadDigests(digestsForChannel(siblings, sessionID))
}

// inMemorySiblings projects the in-process history map into the same shape as
// the persisted listing. UpdatedAt is unknown here, so ordering falls back to
// the stable sort in renderThreadDigests rather than being fabricated.
func (s *slackSessionStore) inMemorySiblings(prefix string) []sessionstore.SiblingSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sessionstore.SiblingSession, 0, len(s.history))
	for id, h := range s.history {
		if !strings.HasPrefix(id, prefix) || len(h) == 0 {
			continue
		}
		out = append(out, sessionstore.SiblingSession{
			SessionID: id,
			History:   append([]chat.Message(nil), h...),
		})
	}
	return out
}

// Append replaces the session's history with the dispatcher's
// post-turn Messages slice. Mirrors emailSessionStore.Append's
// reasoning — Result.Messages is documented as "full updated
// conversation history" so a replace is the right operation. An
// empty post-turn slice means the dispatcher errored before
// producing anything; skip rather than wipe the in-memory state.
func (s *slackSessionStore) Append(ctx context.Context, msg conversation.ChannelMessage, result dispatcher.Result) error {
	if len(result.Messages) == 0 {
		return nil
	}
	s.mu.Lock()
	updated, _ := s.boundedHistory(result.Messages)
	s.history[msg.SessionID] = updated
	s.mu.Unlock()

	if s.persister != nil {
		_ = s.persister.Save(ctx, msg.SessionID, s.projectID, updated)
	}
	return nil
}

// snapshotHistory returns a copy of the stored history for a
// session. Test seam; production code reads through Load.
func (s *slackSessionStore) snapshotHistory(sessionID string) []chat.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]chat.Message(nil), s.history[sessionID]...)
}

// Compile-time guard: slackSessionStore satisfies the dispatcher
// SessionStore contract.
var _ dispatcher.SessionStore = (*slackSessionStore)(nil)

// ReadThread implements dispatcher.ChannelThreadReader over this channel's
// session state, backing the get_channel_thread tool.
//
// Reads the in-memory map first, then the durable row, so a thread whose
// session predates the current process is still readable — which is the point,
// since the whole feature exists because people follow up on conversations from
// days ago.
//
// Access scoping is NOT enforced here: the tool resolves the requested thread
// against the caller's own container prefix before calling this. Keeping the
// check in one place (the tool) avoids two implementations disagreeing about
// what "same channel" means.
func (s *slackSessionStore) ReadThread(ctx context.Context, sessionID string) ([]chat.Message, error) {
	s.mu.Lock()
	if h, ok := s.history[sessionID]; ok && len(h) > 0 {
		out := append([]chat.Message(nil), h...)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	history, _, found, err := s.persister.Load(ctx, sessionID)
	if err != nil || !found {
		return nil, err
	}
	return history, nil
}
