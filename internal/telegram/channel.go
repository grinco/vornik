package telegram

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// Channel adapts an existing *Bot to the conversation.Channel and
// conversation.StreamingChannel interfaces (slice 2 of the
// ConversationChannel rollout; see
// https://docs.vornik.io).
//
// Post-migration shape: the Channel is purely an OUTBOUND delegate.
// Bot drives inbound dispatch directly via SetReceiver +
// handleReceiverTurn — the receiver is set on the Bot itself, not
// through the Channel. Send / StreamingSend / ListSessions /
// ResolveSpeaker route through the wrapped *Bot.
type Channel struct {
	bot *Bot
}

// NewChannel wraps an existing *Bot. The returned Channel is an
// outbound-only delegate — see the type doc comment. Inbound
// dispatch is wired separately via Bot.SetReceiver.
func NewChannel(b *Bot) *Channel {
	return &Channel{bot: b}
}

// Name implements conversation.Channel.
func (c *Channel) Name() string { return "telegram" }

// Start satisfies the conversation.Channel interface. The receiver
// argument is unused — Bot drives dispatch via SetReceiver, not the
// Channel. Delegates to Bot.Start so daemons that lifecycle the bot
// through the Channel object still see the poll loop come up.
func (c *Channel) Start(ctx context.Context, _ conversation.Receiver) error {
	return c.bot.Start(ctx)
}

// Stop satisfies conversation.Channel by delegating to Bot.Stop.
func (c *Channel) Stop() error {
	return c.bot.Stop()
}

// Send delivers a one-shot outbound message. The SessionID is
// interpreted as a Telegram chat_id (decimal string). Returns the
// Telegram message_id as a decimal string so the dispatcher can
// correlate replies via InReplyTo on future inbound turns.
//
// Voice MVP slice 3: when the chat's most recent inbound was a voice
// message AND a TTS provider is wired, Send synthesises the text and
// posts via Telegram's sendVoice instead of sendMessage. On TTS
// failure (oversize text, missing model, etc.) the path falls back
// to a text-only sendMessage with the original body. The
// voice/text decision is owned by the channel adapter per the
// design doc; the dispatcher never knows.
func (c *Channel) Send(ctx context.Context, msg conversation.ChannelMessage) (string, error) {
	chatID, err := parseTelegramSessionID(msg.SessionID)
	if err != nil {
		return "", err
	}
	voiceMode := c.bot.shouldReplyAsVoice(chatID)
	c.bot.logger.Info().
		Int64("chat_id", chatID).
		Int("text_len", len(msg.Text)).
		Bool("voice_mode", voiceMode).
		Bool("tts_wired", c.bot.voice.TTS != nil).
		Bool("tracker_says_voice", c.bot.voiceTracker != nil && c.bot.voiceTracker.IsVoice(chatID)).
		Msg("telegram channel.Send: deciding reply mode")
	if voiceMode {
		sentID, ok, sendErr := c.bot.sendVoiceReply(ctx, chatID, msg.Text)
		if ok {
			c.bot.logger.Info().
				Int64("chat_id", chatID).
				Str("message_id", sentID).
				Msg("telegram channel.Send: voice reply delivered")
			return sentID, nil
		}
		if sendErr != nil {
			c.bot.logger.Warn().Err(sendErr).Int64("chat_id", chatID).
				Msg("voice: TTS reply failed; falling back to text")
		} else {
			c.bot.logger.Info().Int64("chat_id", chatID).
				Msg("voice: TTS reply not produced (e.g. length-cap, empty text); falling back to text")
		}
		// Fall through to text send.
	}
	id, err := c.bot.sendMessageGetID(ctx, chatID, msg.Text)
	if err != nil {
		return "", err
	}
	c.bot.logger.Info().
		Int64("chat_id", chatID).
		Int64("message_id", id).
		Msg("telegram channel.Send: text reply delivered")
	return strconv.FormatInt(id, 10), nil
}

// ListSessions returns a snapshot of chats the bot has seen at least
// one authorised message from. Reads the in-memory chatUsers map
// populated by HandleMessage's recordChatUser call; survives until a
// persistence-backed session listing replaces it.
func (c *Channel) ListSessions(_ context.Context) ([]conversation.Session, error) {
	c.bot.followupMu.Lock()
	ids := make([]int64, 0, len(c.bot.chatUsers))
	for chatID := range c.bot.chatUsers {
		ids = append(ids, chatID)
	}
	c.bot.followupMu.Unlock()

	out := make([]conversation.Session, 0, len(ids))
	for _, chatID := range ids {
		out = append(out, conversation.Session{
			ID:               strconv.FormatInt(chatID, 10),
			ParticipantCount: 1,
		})
	}
	return out, nil
}

// ResolveSpeaker maps a Telegram user_id (decimal string) to a
// Speaker. Returns ErrSpeakerUnknown when the bot is configured with
// an allowlist and the user is not on it. Dev-mode (empty allowlist)
// passes through unconditionally to match Bot.IsAllowed semantics.
func (c *Channel) ResolveSpeaker(_ context.Context, channelSpeakerID string) (conversation.Speaker, error) {
	userID, err := strconv.ParseInt(channelSpeakerID, 10, 64)
	if err != nil {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	if !c.bot.IsAllowed(userID) {
		return conversation.Speaker{}, conversation.ErrSpeakerUnknown
	}
	return conversation.Speaker{
		ID:          "telegram:" + channelSpeakerID,
		DisplayName: "Telegram user " + channelSpeakerID,
	}, nil
}

// defaultTelegramEditInterval is the floor between successive
// editMessageText calls in a single Stream. Telegram's documented
// "edits per second" secondary limit kicks in around 1/s per chat;
// 750ms keeps us safely below.
const defaultTelegramEditInterval = 750 * time.Millisecond

// StreamingSend implements conversation.StreamingChannel. The
// returned Stream sends a single-character placeholder, then
// coalesces Append calls into editMessageText edits respecting
// defaultTelegramEditInterval. On error the dispatcher falls back
// to Channel.Send per the ConversationChannel design.
//
// Voice short-circuit: when the chat is in voice-reply mode (last
// inbound was a voice message and TTS is wired), we refuse to
// stream and return ErrVoiceModeNoStream. Telegram has no way to
// progressively update an audio bubble — TTS needs the complete
// text to synthesise a single OGG/Opus file. The dispatcher's
// existing fallback ("on StreamingSend error, route through
// Channel.Send") then takes the one-shot path, where the voice
// branch lives. Without this guard, streaming bypasses the voice
// path entirely and voice replies silently degrade to text edits.
func (c *Channel) StreamingSend(ctx context.Context, sessionID string) (conversation.Stream, error) {
	chatID, err := parseTelegramSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	if c.bot.shouldReplyAsVoice(chatID) {
		c.bot.logger.Info().
			Int64("chat_id", chatID).
			Msg("telegram: refusing streaming for voice-mode chat — dispatcher will fall back to Channel.Send")
		return nil, ErrVoiceModeNoStream
	}
	msgID, err := c.bot.sendMessageGetID(ctx, chatID, telegramStreamPlaceholder)
	if err != nil {
		return nil, err
	}
	return &telegramStream{
		bot:          c.bot,
		ctx:          ctx,
		chatID:       chatID,
		messageID:    msgID,
		minEditEvery: defaultTelegramEditInterval,
	}, nil
}

// ErrVoiceModeNoStream is the sentinel StreamingSend returns when
// the chat's most-recent inbound was a voice message and the bot
// has TTS wired. The dispatcher reacts to any StreamingSend error
// by falling back to a one-shot Channel.Send, which is where the
// voice-reply path lives.
var ErrVoiceModeNoStream = streamConstError("telegram: voice mode active; use Channel.Send for one-shot voice reply")

// telegramStreamPlaceholder is the initial body of a streaming
// outbound. Telegram rejects sending an empty-string message, so we
// post a single bullet that the first Append edit replaces.
const telegramStreamPlaceholder = "…"

// telegramStream implements conversation.Stream over
// Bot.editMessageText. Internally rate-limited so callers don't
// throttle.
type telegramStream struct {
	bot          *Bot
	ctx          context.Context
	chatID       int64
	messageID    int64
	minEditEvery time.Duration

	mu         sync.Mutex
	buf        string
	lastEdit   time.Time
	terminated error
}

// Append appends text to the accumulated stream body. Edits the
// upstream message at most once per minEditEvery; intermediate
// appends are buffered and flushed on the next edit or on Close.
// Returns a terminal error if the upstream edit fails or if the
// stream was already closed.
func (s *telegramStream) Append(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminated != nil {
		return s.terminated
	}
	s.buf += text
	if time.Since(s.lastEdit) < s.minEditEvery {
		return nil
	}
	if err := s.bot.editMessageText(s.ctx, s.chatID, s.messageID, s.buf); err != nil {
		s.terminated = err
		return err
	}
	s.lastEdit = time.Now()
	return nil
}

// Close flushes any buffered Append delta as a final edit and marks
// the stream terminated. Returns the Telegram message_id of the
// edited message as a decimal string, matching Channel.Send.
// Idempotent: a second Close returns ErrStreamClosed.
func (s *telegramStream) Close() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminated != nil {
		return "", s.terminated
	}
	if s.buf != "" {
		if err := s.bot.editMessageText(s.ctx, s.chatID, s.messageID, s.buf); err != nil {
			s.terminated = err
			return "", err
		}
	}
	s.terminated = ErrStreamClosed
	return strconv.FormatInt(s.messageID, 10), nil
}

// ErrStreamClosed is the terminal sentinel returned by Append and
// Close once a stream has finalised. Exported so callers / tests
// can branch on it via errors.Is.
var ErrStreamClosed = streamConstError("telegram: stream closed")

type streamConstError string

func (e streamConstError) Error() string { return string(e) }

// parseTelegramSessionID parses a SessionID string into a Telegram
// chat_id. Returns a descriptive error rather than silently coercing
// so dispatcher bugs surface as routing errors not silent drops.
func parseTelegramSessionID(sessionID string) (int64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("telegram: empty SessionID")
	}
	id, err := strconv.ParseInt(sessionID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("telegram: SessionID %q is not a chat_id: %w", sessionID, err)
	}
	return id, nil
}

// MessageToChannelMessage translates a *Message into the generic
// ChannelMessage envelope. Forum thread IDs and reply-to-message IDs
// move into ChannelSpecific / InReplyTo so the dispatcher can reason
// about them without importing this package.
//
// UserID == 0 is treated as a synthetic / server-internal turn
// (auto-resume after a watched task completes). SpeakerID is left
// empty in that case so the SessionStore's allowlist check skips —
// auto-resume is not user-authenticated, it's server-driven.
func MessageToChannelMessage(msg *Message) conversation.ChannelMessage {
	cs := map[string]string{}
	if msg.MessageThreadID != 0 {
		cs["telegram_thread_id"] = strconv.FormatInt(msg.MessageThreadID, 10)
	}
	if msg.Username != "" {
		cs["telegram_username"] = msg.Username
	}
	if msg.FileID != "" {
		cs["telegram_file_id"] = msg.FileID
	}
	if msg.FileName != "" {
		cs["telegram_file_name"] = msg.FileName
	}
	// Voice MVP slice 3: surface the inbound voice metadata onto
	// ChannelSpecific so downstream consumers can branch without
	// having to peek inside the telegram package. The dispatcher
	// ignores these keys; future audit / session-store integrations
	// (slice 6) read them for spend attribution and replay.
	if msg.IsVoice {
		cs["voice.inbound"] = "true"
		if msg.VoiceTranscript.DurationMs > 0 {
			cs["voice.duration_ms"] = strconv.FormatInt(msg.VoiceTranscript.DurationMs, 10)
		}
		if msg.VoiceTranscript.Confidence > 0 {
			cs["voice.transcript_confidence"] = strconv.FormatFloat(msg.VoiceTranscript.Confidence, 'f', 4, 64)
		}
		if msg.VoiceTranscript.Language != "" {
			cs["voice.language"] = msg.VoiceTranscript.Language
		}
	}
	var inReplyTo string
	if msg.ReplyToMessageID != 0 {
		inReplyTo = strconv.FormatInt(msg.ReplyToMessageID, 10)
	}
	var threadID string
	if msg.MessageThreadID != 0 {
		threadID = strconv.FormatInt(msg.MessageThreadID, 10)
	}
	var speakerID string
	if msg.UserID != 0 {
		speakerID = strconv.FormatInt(msg.UserID, 10)
	}
	return conversation.ChannelMessage{
		Source:          "telegram",
		ID:              strconv.FormatInt(msg.ID, 10),
		SessionID:       strconv.FormatInt(msg.ChatID, 10),
		SpeakerID:       speakerID,
		Text:            msg.Text,
		InReplyTo:       inReplyTo,
		ThreadID:        threadID,
		Timestamp:       time.Now(),
		ChannelSpecific: cs,
	}
}

// Compile-time guarantees: *Channel satisfies the conversation
// contract. Breaks the build first if the interface drifts.
var (
	_ conversation.Channel          = (*Channel)(nil)
	_ conversation.StreamingChannel = (*Channel)(nil)
	_ conversation.Stream           = (*telegramStream)(nil)
)
