package telegram

// SessionStore implements dispatcher.SessionStore for Telegram —
// slice 2 of the ConversationChannel rollout. Wraps an existing
// *Bot so the legacy in-memory conversations + activeProjects maps
// remain the authoritative state during the migration window;
// the store is a thin translation layer between the receiver's
// ChannelMessage shape and the Bot's per-chat getters.
//
// Two callers:
//   - dispatcher.ChannelReceiver consumes Load to build a
//     Request and Append to persist the post-turn state.
//   - Bot itself can keep reading b.conversations directly for
//     non-dispatcher flows (slash commands, forum routing) —
//     this store does not replace those.
//
// Speaker-rejection mirrors the GitHub adapter: when the Bot's
// allowlist denies the user, Load returns
// conversation.ErrSpeakerUnknown without touching the dispatcher,
// so unauthorised callers can't burn LLM budget.
//
// Project-access revocation is NOT handled here — the Bot's
// HandleMessage layer detects the mid-flight revocation case
// (operator narrowed the allowlist after a chat pinned a project)
// and clears the pin before invoking the receiver, so this store
// can trust that any non-empty activeProjects[chatID] is still
// permitted.

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
)

const approximateCharsPerToken = 4

// artifactChatSender is the per-chat dispatcher.FileSender adapter. It binds a
// chat id to the Bot so the dispatcher's file tools (send_artifact,
// render_document) deliver without knowing Telegram's chat-id model — the
// channel-agnostic seam that lets non-Telegram channels (email) implement file
// delivery too. Delegates to the Bot's streaming upload.
type artifactChatSender struct {
	bot    *Bot
	chatID int64
}

func (a artifactChatSender) SendArtifactFile(ctx context.Context, fileName string, content io.Reader, caption string) error {
	return a.bot.SendDocumentReader(ctx, a.chatID, content, fileName, caption)
}

// SessionStore is the dispatcher-facing session adapter for
// Telegram. Holds a reference to the Bot whose state it wraps and
// the registry needed to resolve lead-system prompts.
type SessionStore struct {
	bot      *Bot
	registry *registry.Registry
}

// NewSessionStore builds a Telegram SessionStore over the given
// Bot. Registry is optional — when nil (or when no project is
// pinned), LeadSystemPrompt stays empty and the dispatcher renders
// its default system prompt.
func NewSessionStore(bot *Bot, reg *registry.Registry) *SessionStore {
	return &SessionStore{bot: bot, registry: reg}
}

// Load implements dispatcher.SessionStore. Resolves the chat_id +
// user_id from the inbound message, checks the speaker against the
// Bot's allowlist, and assembles the per-chat Session snapshot the
// receiver hands the dispatcher.
func (s *SessionStore) Load(ctx context.Context, msg conversation.ChannelMessage) (dispatcher.Session, error) {
	chatID, err := strconv.ParseInt(msg.SessionID, 10, 64)
	if err != nil {
		return dispatcher.Session{}, fmt.Errorf("telegram: SessionID %q is not a chat_id: %w", msg.SessionID, err)
	}

	// Restart / replica-failover rehydrate. The first inbound on a
	// chat after a process boot finds an empty in-memory cache;
	// pull the persisted history (if any) and populate the bot's
	// conversations + activeProjects maps before getConversation
	// runs. Errors are logged inside the persister and ignored
	// here — a transient DB blip just degrades us to "fresh
	// session" for this turn, same as the pre-feature behaviour.
	if persister := s.bot.sessionPersisterRef(); persister != nil && !s.bot.hasInMemorySession(chatID) {
		if hist, ap, found, err := persister.Load(ctx, msg.SessionID); err == nil && found {
			s.bot.hydrateSession(chatID, hist, ap)
		}
	}
	// SpeakerID is optional on synthetic / system inbounds. Only
	// parse + check when it's present so server-internal callers
	// (autonomy notifications, retro replays) don't trip the
	// allowlist gate.
	var userID int64
	if msg.SpeakerID != "" {
		userID, err = strconv.ParseInt(msg.SpeakerID, 10, 64)
		if err != nil {
			return dispatcher.Session{}, fmt.Errorf("telegram: SpeakerID %q is not a user_id: %w", msg.SpeakerID, err)
		}
		if !s.bot.IsAllowed(userID) {
			return dispatcher.Session{}, conversation.ErrSpeakerUnknown
		}
	}

	conv := s.bot.getConversation(chatID)
	// MessagesForLLM applies read-path compaction when a compactor is wired
	// (overflow turns become a topic gist); without one it returns the same
	// payload GetMessages() would. Persistence (Append) still stores raw
	// history — compaction is a send-time view only.
	history := conv.MessagesForLLM()
	activeProject := s.bot.getActiveProject(chatID)
	estimatedTokens := conv.EstimateTokens() + len(msg.Text)/approximateCharsPerToken

	sess := dispatcher.Session{
		History:            history,
		ActiveProject:      activeProject,
		ChatID:             chatID,
		FileSender:         artifactChatSender{bot: s.bot, chatID: chatID},
		AllowedProjects:    s.bot.AllowedProjectsForUser(userID),
		ContextTier:        chat.TierFromUsage(estimatedTokens, s.bot.config.MaxHistoryTokens),
		ContextHeadroomPct: chat.HeadroomPct(estimatedTokens, s.bot.config.MaxHistoryTokens),
	}

	// Lead-system-prompt resolution mirrors GitHub: only when both
	// a registry and an active project are wired AND the project
	// has a lead role configured. Absent any piece, the dispatcher
	// falls back to its default prompt (still functional, just
	// without the project's persona).
	if s.registry != nil && activeProject != "" {
		project := s.registry.GetProject(activeProject)
		if project != nil {
			sess.AvailableProjects = s.bot.getProjectListForUser(userID)
			if leadPrompt, _ := dispatcher.ResolveLeadPrompt(s.registry, activeProject); leadPrompt != "" {
				swarm := s.registry.GetSwarm(project.SwarmID)
				sess.LeadSystemPrompt = dispatcher.BuildLeadSystemPrompt(project, swarm, leadPrompt, sess.AvailableProjects)
			}
		}
	}

	return sess, nil
}

// Append implements dispatcher.SessionStore. Mirrors the legacy
// waiter goroutine's post-turn side effects: switch_project flips
// the chat's pin; the dispatcher's full post-turn message slice
// replaces the chat's conversation history (matches the
// "dispatcher.Result.Messages is authoritative" contract that the
// GitHub store also follows).
func (s *SessionStore) Append(ctx context.Context, msg conversation.ChannelMessage, result dispatcher.Result) error {
	if len(result.Messages) == 0 {
		// Defensive: empty post-turn Messages would wipe history.
		// Skip rather than discard state on a dispatcher error path
		// (mirrors githubSessionStore.Append).
		return nil
	}
	chatID, err := strconv.ParseInt(msg.SessionID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: Append SessionID %q is not a chat_id: %w", msg.SessionID, err)
	}

	if result.NewProject != "" && result.NewProject != s.bot.getActiveProject(chatID) {
		s.bot.setActiveProject(chatID, result.NewProject)
	}

	s.bot.resetConversation(chatID)
	conv := s.bot.getConversation(chatID)
	for _, m := range result.Messages {
		conv.AddMessage(m)
	}

	// Write-through to Postgres so a daemon restart or replica
	// failover doesn't drop this conversation. The DB row is
	// post-turn-authoritative: full history + active project.
	// Note: resetConversation above already fired a Delete on the
	// persisted row; the Save below replaces it with the
	// post-turn slice — net effect is one upserted row per turn.
	if persister := s.bot.sessionPersisterRef(); persister != nil {
		_ = persister.Save(ctx, msg.SessionID, s.bot.getActiveProject(chatID), result.Messages)
	}
	return nil
}

// Compile-time guard: *SessionStore satisfies the dispatcher
// contract.
var _ dispatcher.SessionStore = (*SessionStore)(nil)
