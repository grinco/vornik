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
// RESTART SURVIVAL, corrected 2026-07-30. This comment used to say a
// daemon restart clears the history and that operators should rely on
// the Slack thread itself to carry context. That predates the
// persister and is no longer true: when a channel-session repo is
// wired (SetPersister, Postgres in production), Load rehydrates from
// the durable row and the conversation survives a restart —
// TestSlackSessionStore_ConversationSurvivesARestart pins it.
//
// It matters because it is a correctness property, not a nicety: a
// customer asked about a job Vornik had scheduled and got "I don't
// know anything about it" (2026-07-30). Only on SQLite, whose repo is
// a documented no-op, does the in-memory map remain the whole truth.
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

	// A thread hanging off a message WE posted is a continuation of the conversation it
	// hangs off, not a new one. Without this, opening a thread under a channel-level
	// answer produced an EMPTY session and the bot could not see the exchange the person
	// was visibly replying to — operator report 2026-07-30, quoting the bot: "I don't
	// have a recent thread context to anchor on."
	if len(history) == 0 {
		history = s.inheritedChannelHistory(ctx, msg)
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
		sess.LeadSystemPrompt += slackFormattingBlock
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
	//
	// Nil persister (SQLite, or unwired) has nothing durable to list and must not be
	// dereferenced — same latent panic as ReadThread had.
	var siblings []sessionstore.SiblingSession
	if s.persister != nil {
		var err error
		siblings, err = s.persister.ListByPrefix(ctx, prefix, maxDigestThreads*3)
		if err != nil {
			return ""
		}
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

	// A store without persistence (SQLite, or unwired test/degenerate config) has
	// nothing durable to consult. Dereferencing it unconditionally crashed the daemon
	// on the first in-memory miss.
	if s.persister == nil {
		return nil, nil
	}

	history, _, found, err := s.persister.Load(ctx, sessionID)
	if err != nil || !found {
		return nil, err
	}
	return history, nil
}

// ThreadEngaged implements slack.ThreadEngagementChecker: it reports whether this store
// holds conversation history for a Slack session id.
//
// It is what lets a mention-less reply inside a thread start a turn (incident
// 2026-07-30 — three follow-ups in a thread the bot was actively holding went
// unanswered). Reading through ReadThread is deliberate: it consults the durable row as
// well as the in-memory map, so engagement survives a daemon restart, which is the
// whole point — people follow up on a thread hours later.
//
// A lookup error reads as NOT engaged. The failure mode of a false negative is one
// missed reply the user can retrieve by tagging the bot; a false positive would have
// it answering a conversation between colleagues.
func (s *slackSessionStore) ThreadEngaged(ctx context.Context, sessionID string) bool {
	history, err := s.ReadThread(ctx, sessionID)
	return err == nil && len(history) > 0
}

// ThreadParentIsBotKey is the ChannelSpecific flag the Slack channel sets when an
// inbound thread reply's parent_user_id is our own bot user id — i.e. the thread hangs
// off a message Vornik posted.
const ThreadParentIsBotKey = "thread_parent_is_bot"

// inheritedChannelHistory returns the channel-scoped conversation a thread should start
// from, or nil.
//
// WHY. The 2026-07-28 continuity change keys a top-level channel message on the CHANNEL
// (`<team>/<channel>#main`) and the bot answers at channel level. Slack lets a person
// open a thread under that answer, which produces a session keyed on the thread root — a
// brand-new empty conversation. The person is visibly replying to an exchange the bot
// then cannot see, and says so: "I don't have a recent thread context to anchor on."
//
// SCOPE, deliberately narrow on both axes:
//
//   - Only when the thread is rooted on OUR OWN message. Two colleagues opening a thread
//     under each other's message and tagging the bot get help with that thread, not a
//     replay of an unrelated channel conversation.
//   - Only to SEED an empty thread. Once the thread has its own exchanges those are the
//     truth; re-seeding every turn would replay the channel history and grow the prompt
//     without bound.
//
// The prefix is derived from the caller's own session id, so a thread in one channel can
// never be seeded from another channel's conversation.
func (s *slackSessionStore) inheritedChannelHistory(
	ctx context.Context,
	msg conversation.ChannelMessage,
) []chat.Message {
	if msg.ChannelSpecific[ThreadParentIsBotKey] != "true" {
		return nil
	}
	// At channel level the parent is the channel session itself; inheriting would mean
	// loading ourselves.
	if threadKeyFromSessionID(msg.SessionID) == slack.ChannelSessionThreadRoot {
		return nil
	}
	prefix := channelPrefixFromSessionID(msg.SessionID)
	if prefix == "" {
		return nil
	}

	inherited, err := s.ReadThread(ctx, prefix+slack.ChannelSessionThreadRoot)
	if err != nil || len(inherited) == 0 {
		return nil
	}
	bounded, _ := s.boundedHistory(inherited)
	return bounded
}

// slackFormattingBlock teaches the lead Slack's own markup.
//
// OPERATOR REQUEST 2026-07-30: use rich text in chat — hyperlinks on words, emoji,
// emphasis. Most outbound text is written by the model, not composed by the daemon, so
// the daemon-side helpers only get half the job done; the model has to know the
// conventions for the surface it is speaking on.
//
// The mrkdwn traps are worth naming explicitly because Slack is NOT Markdown and the
// models default to Markdown: `**bold**` renders with visible asterisks, and
// `[label](url)` renders as literal brackets. Both look like a bug to the reader.
const slackFormattingBlock = `

SLACK FORMATTING — you are writing into Slack, which uses mrkdwn, NOT Markdown.
- Links: <https://example.com|the words to click>. NEVER paste a bare URL and never use
  [label](url) — Slack shows the brackets literally. Long URLs are unreadable in a
  channel, so always anchor them on words.
- Bold is *single asterisks*, italic is _underscores_, strikethrough is ~tildes~.
  **Double** asterisks are wrong here and render visibly.
- ` + "`code`" + ` and triple-backtick blocks work as expected; use them for ids, paths and
  commands so they can be copied.
- Emoji are welcome and useful as status at a glance (:white_check_mark: done, :x: failed,
  :warning: caution). Use them sparingly — one per message, not decoration.
- Bulleted lists: start lines with • or -. Headings do not exist in mrkdwn; use *bold* on
  its own line instead.
- Mentioning a person: <@USER_ID>. Never invent an id; use one you were given.
Formatting is for reading. When a reply is delivered as a voice note it is converted to
plain speech automatically, so write naturally and do not avoid formatting on that account.
`
