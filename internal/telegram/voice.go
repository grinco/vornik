package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"sync"

	"vornik.io/vornik/internal/voice"
)

// TelegramVoice represents Telegram's inbound voice attachment. The
// Bot API delivers this as Message.voice — OGG with Opus, mono. We
// inline the parse here rather than extending the existing Update
// struct in bot.go so the voice MVP is reviewable as a slice.
type TelegramVoice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"` // seconds
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

// TelegramAudio represents Telegram's inbound audio attachment.
// Audio files (music, podcasts) arrive via Message.audio with a
// MIME like audio/mpeg or audio/mp4. The bot treats both `voice`
// and `audio` as transcribable inputs in the MVP.
type TelegramAudio struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	FileName string `json:"file_name,omitempty"`
}

// VoiceProviders bundles the STT + TTS implementations the bot uses
// for voice messages. Either or both may be nil to disable a
// direction. Nil STT skips inbound transcription (voice attachments
// drop through to the legacy "I've attached a file." path); nil TTS
// makes outbound replies stay text even when the inbound was voice.
//
// Mirrors the slice-3-decision noted in the design doc: voice is a
// behaviour of the channel adapter, not a new ConversationChannel
// method. The two interfaces are the integration shim.
type VoiceProviders struct {
	STT voice.STTProvider
	TTS voice.TTSProvider

	// MaxOutboundDuration caps synthesised voice messages at the
	// platform's UX limit. Telegram's seekable voice-bubble UI works
	// well up to ~60 s; longer replies still play but the operator
	// experience degrades. Zero falls back to 60 s.
	MaxOutboundDuration int64 // milliseconds
}

const (
	// telegramVoiceMaxDurationMs is the platform-default voice
	// length cap. Documented in the design doc §"Length cap".
	telegramVoiceMaxDurationMs int64 = 60_000
)

// WithVoiceProviders wires the STT + TTS pair onto the bot. Nil
// providers are allowed: a nil STT keeps inbound voice as a regular
// audio attachment (existing handle-document path), a nil TTS keeps
// outbound replies as text.
//
// Calling this without at least one non-nil provider is harmless —
// the bot's voice paths are gated on the field, so leaving everything
// nil restores the pre-voice behaviour exactly.
func WithVoiceProviders(p VoiceProviders) BotOption {
	return func(b *Bot) {
		b.voice = p
		if b.voice.MaxOutboundDuration <= 0 {
			b.voice.MaxOutboundDuration = telegramVoiceMaxDurationMs
		}
		if b.voiceTracker == nil {
			b.voiceTracker = newVoiceInboundTracker()
		}
	}
}

// voiceInboundTracker remembers per-chat which session's most recent
// inbound was a voice message. The channel adapter consults it on
// Send to decide voice-vs-text reply. State is in-memory only — a
// daemon restart resets every chat to "text-mode"; the next inbound
// voice flips it back. That's the right tradeoff for the MVP: the
// alternative would persist a 1-bit-per-chat flag into the DB which
// is far more code than the rare-case improvement justifies.
//
// Cleared on any text-only inbound on the same chat (Telegram users
// frequently send a typed correction right after a voice message;
// the reply to the correction should be text, not voice).
type voiceInboundTracker struct {
	mu  sync.RWMutex
	set map[int64]bool
}

func newVoiceInboundTracker() *voiceInboundTracker {
	return &voiceInboundTracker{set: make(map[int64]bool)}
}

func (t *voiceInboundTracker) set_(chatID int64, isVoice bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if isVoice {
		t.set[chatID] = true
	} else {
		delete(t.set, chatID)
	}
}

// MarkVoice marks a chat as having received a voice inbound. Exported
// for tests; production code calls it from the inbound voice handler.
func (t *voiceInboundTracker) MarkVoice(chatID int64) { t.set_(chatID, true) }

// MarkText clears the voice mark for a chat. Exported for tests;
// production code calls it from HandleMessage when the inbound has
// non-empty Text and no voice attachment.
func (t *voiceInboundTracker) MarkText(chatID int64) { t.set_(chatID, false) }

// IsVoice reports whether the chat's most-recent inbound was a voice
// message. Used by Channel.Send to decide voice-vs-text reply.
// Nil-receiver safe so callers don't have to guard around the lazy
// allocation in WithVoiceProviders.
func (t *voiceInboundTracker) IsVoice(chatID int64) bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.set[chatID]
}

// handleVoiceAttachment fetches the inbound voice file via the
// existing getFile + downloadFile path, hands the bytes to the STT
// provider, and rewrites msg.Text to the transcript. Tags the
// voice-inbound flag on the chat tracker so the next outbound
// Channel.Send routes through TTS.
//
// On STT failure the function returns a humane error message string;
// HandleMessage uses that to short-circuit the dispatcher and reply
// directly to the user, per the design doc §"Failure mode".
//
// The audio attachment is NOT removed from the inbound — the audit
// surface keeps the original blob. The transcript also surfaces on
// ChannelSpecific["voice.transcript_text"] (alongside language /
// duration / confidence) so downstream consumers that want the raw
// transcription have it without parsing the Text field.
func (b *Bot) handleVoiceAttachment(ctx context.Context, msg *Message, hint voice.Hint) (humaneError string, ok bool) {
	if b.voice.STT == nil {
		return "", false
	}
	if msg.FileID == "" {
		return "", false
	}
	telegramPath, err := b.getFile(ctx, msg.FileID)
	if err != nil {
		b.logger.Warn().Err(err).Str("file_id", msg.FileID).Msg("voice: getFile failed")
		return "I couldn't fetch your voice message from Telegram. Can you try again or type it?", false
	}
	audioBytes, err := b.fetchTelegramBytes(ctx, telegramPath)
	if err != nil {
		b.logger.Warn().Err(err).Msg("voice: file download failed")
		return "I couldn't fetch your voice message from Telegram. Can you try again or type it?", false
	}
	tr, err := b.voice.STT.Transcribe(ctx, bytes.NewReader(audioBytes), hint)
	if err != nil {
		b.logger.Warn().Err(err).Msg("voice: STT.Transcribe failed")
		return "I couldn't make out the voice message — can you try again or type it?", false
	}
	msg.Text = tr.Text
	msg.VoiceTranscript = voiceTranscript{
		Text:       tr.Text,
		Language:   tr.Language,
		DurationMs: tr.DurationMs,
		Confidence: tr.Confidence,
	}
	if b.voiceTracker == nil {
		b.voiceTracker = newVoiceInboundTracker()
	}
	b.voiceTracker.MarkVoice(msg.ChatID)
	b.logger.Info().
		Int64("chat_id", msg.ChatID).
		Int("text_len", len(tr.Text)).
		Int64("audio_ms", tr.DurationMs).
		Float64("confidence", tr.Confidence).
		Msg("voice: inbound transcribed")
	return "", true
}

// fetchTelegramBytes downloads a Telegram file by its server-side
// path (the one getFile returns) and reads the bytes into memory.
// Honors maxTelegramDownloadBytes so a misbehaving server can't OOM
// the daemon.
func (b *Bot) fetchTelegramBytes(ctx context.Context, telegramPath string) ([]byte, error) {
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", b.config.Token, telegramPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voice: file download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("voice: file download status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTelegramDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxTelegramDownloadBytes {
		return nil, fmt.Errorf("voice: file exceeds %d byte limit", maxTelegramDownloadBytes)
	}
	return body, nil
}

// sendVoice POSTs an OGG/Opus blob to Telegram's sendVoice API. Used
// by the Channel.Send path when the chat's inbound was a voice
// message. Returns the upstream message_id so the outbound persists
// with a correlatable id.
func (b *Bot) sendVoice(ctx context.Context, chatID int64, audio voice.Audio) (int64, error) {
	if len(audio.Bytes) == 0 {
		return 0, fmt.Errorf("voice: sendVoice with empty audio")
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		defer func() { _ = pw.Close() }()
		defer func() { _ = writer.Close() }()
		if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if audio.DurationMs > 0 {
			if err := writer.WriteField("duration", strconv.FormatInt(audio.DurationMs/1000, 10)); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		part, err := writer.CreateFormFile("voice", "voice.ogg")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := part.Write(audio.Bytes); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()
	url := fmt.Sprintf("%s/sendVoice", b.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		_ = pr.Close()
		return 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("voice: sendVoice request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0, fmt.Errorf("voice: sendVoice read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("voice: sendVoice HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed SendMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("voice: sendVoice parse: %w", err)
	}
	if !parsed.OK {
		return 0, fmt.Errorf("voice: sendVoice error: %s", parsed.Description)
	}
	return parsed.Result.MessageID, nil
}

// sendVoiceReply is the integration helper Channel.Send calls when
// the chat tracker reports a voice inbound. Decides whether the
// reply fits the platform's 1-minute UX cap (synthesise + sendVoice)
// or whether it should fall back to text-only (chosen-decision per
// slice 3 — see commit message).
//
// Decision rationale (flagged in the commit):
//
//   - "send text" (the chosen path here) preserves the full reply
//     fidelity. Multi-shot voice splits make the bot feel chatty;
//     truncate+intro hides content the user might need.
//   - "split into multiple voices" would require chunking on
//     sentence boundaries + rate-limit serialisation; deferred to a
//     future slice if operators report demand.
//   - "truncate with voice intro" is a partial answer at best —
//     better to keep voice symmetry only when full content fits.
//
// Returns the upstream message id as a decimal string and an empty
// errstring on success, OR (zero id, non-empty errstring) when TTS
// fails for a reason the caller should escalate. The caller falls
// back to text-Send in that case.
func (b *Bot) sendVoiceReply(ctx context.Context, chatID int64, text string) (sentID string, voiceUsed bool, err error) {
	if b.voice.TTS == nil {
		return "", false, nil
	}
	audio, err := b.voice.TTS.Synthesize(ctx, text, voice.TTSOptions{Format: "ogg-opus"})
	if err != nil {
		// Oversize-text or empty-text: caller-side budget exceeded.
		// Caller falls back to text.
		return "", false, err
	}
	if audio.DurationMs > b.voice.MaxOutboundDuration {
		b.logger.Info().
			Int64("chat_id", chatID).
			Int64("audio_ms", audio.DurationMs).
			Int64("cap_ms", b.voice.MaxOutboundDuration).
			Msg("voice: synthesised audio exceeds platform cap; falling back to text")
		return "", false, nil
	}
	mid, err := b.sendVoice(ctx, chatID, audio)
	if err != nil {
		return "", false, err
	}
	return strconv.FormatInt(mid, 10), true, nil
}

// shouldReplyAsVoice consults the tracker for the channel's
// voice-vs-text decision. Returns true when the chat's most-recent
// inbound was a voice message AND a TTS provider is wired.
func (b *Bot) shouldReplyAsVoice(chatID int64) bool {
	if b.voiceTracker == nil || b.voice.TTS == nil {
		return false
	}
	return b.voiceTracker.IsVoice(chatID)
}

// detectVoiceAttachment returns the file_id + hint when the inbound
// Telegram Update.Message carries a voice or audio attachment. Empty
// FileID means no voice/audio was present and the caller should fall
// through to the existing document/photo path. Returned by the
// extended Update parser in HandleUpdate (slice 3).
func detectVoiceAttachment(voiceField *TelegramVoice, audioField *TelegramAudio) (fileID string, fileName string, hint voice.Hint) {
	if voiceField != nil && voiceField.FileID != "" {
		mime := voiceField.MimeType
		if mime == "" {
			mime = "audio/ogg"
		}
		return voiceField.FileID, "voice.ogg", voice.Hint{MimeType: mime}
	}
	if audioField != nil && audioField.FileID != "" {
		mime := audioField.MimeType
		if mime == "" {
			mime = "audio/mpeg"
		}
		name := audioField.FileName
		if name == "" {
			name = "audio"
		}
		return audioField.FileID, name, voice.Hint{MimeType: mime}
	}
	return "", "", voice.Hint{}
}

// voiceImportHint widens this package's local voiceHint into a
// voice.Hint for the STT provider call. Kept as a free function so
// handlers.go doesn't import the voice package directly — the
// import-boundary discipline matches the bot's existing style of
// keeping handlers.go provider-free.
func voiceImportHint(h voiceHint) voice.Hint {
	return voice.Hint{
		MimeType:     h.MimeType,
		SampleRateHz: h.SampleRateHz,
	}
}
