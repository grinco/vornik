// Package voice owns vornik's speech-to-text and text-to-speech
// provider plumbing. The channel adapters (internal/telegram,
// internal/slack) call these interfaces to transcribe inbound voice
// messages and synthesise outbound replies; the dispatcher never sees
// audio.
//
// Design doc: https://docs.vornik.io
//
// The package is intentionally tiny — two interfaces and four value
// types. The integration glue (when to transcribe, when to reply with
// voice, length caps, error reporting) lives in the channel adapters,
// not here. That keeps this package re-usable for future channels
// (web chat, GitHub) without dragging Telegram/Slack semantics into
// the provider layer.
//
// Build sequence (per the design doc):
//
//	Slice 1 — interfaces + Piper TTS (this file + piper.go)
//	Slice 2 — Whisper STT (whisper.go)
//	Slice 3 — Telegram voice round-trip (internal/telegram)
//	Slice 4 — Slack audio round-trip (internal/slack)
//	Slices 5–7 — caches, /voice on, hosted providers (out of MVP scope)
package voice

import (
	"context"
	"errors"
	"io"
)

// STTProvider is the speech-to-text contract. Implementations
// transcribe a single audio payload to text + metadata. Returning an
// error means the channel adapter falls back to a humane "I couldn't
// make out that voice message" reply; an empty Transcript.Text is NOT
// an error (Whisper occasionally returns an empty transcript on
// genuinely silent audio).
//
// Implementations MUST honour ctx cancellation — voice handling sits
// on a chat hot path and a wedged subprocess would back up the
// channel's inbound goroutine.
type STTProvider interface {
	Transcribe(ctx context.Context, audio io.Reader, hint Hint) (Transcript, error)
}

// TTSProvider is the text-to-speech contract. Implementations
// synthesise text to a bytes payload + metadata. The channel adapter
// decides what to do with the bytes (upload via Telegram's sendVoice,
// Slack's files.upload_v2, persist as an artifact, etc.).
//
// Implementations MUST honour ctx cancellation.
type TTSProvider interface {
	Synthesize(ctx context.Context, text string, opts TTSOptions) (Audio, error)
}

// Transcript is the result of STTProvider.Transcribe. Text is the
// primary product; the other fields surface in audit logs and
// optionally on ChannelMessage.ChannelSpecific so operators can debug
// "the bot misheard me".
type Transcript struct {
	// Text is the transcribed body. May be empty when the audio is
	// silent or the model couldn't extract any words.
	Text string

	// Language is the detected speaker language as a BCP-47 tag
	// (e.g. "en-US", "de-DE"). Empty when the provider doesn't
	// expose detection.
	Language string

	// DurationMs is the original audio duration in milliseconds.
	// Useful for cost rows on hosted providers (Deepgram bills per
	// audio-second) and for the voice.duration_ms ChannelSpecific
	// tag.
	DurationMs int64

	// Confidence is a 0..1 quality estimate the model exposes.
	// 0 when the provider doesn't expose it; do NOT treat 0 as
	// "definitely wrong" — many local models simply don't report.
	Confidence float64
}

// Hint guides the STT provider without being a hard constraint. All
// fields are optional; an empty Hint asks the provider to auto-detect
// everything it can.
type Hint struct {
	// LanguageHint is an optional BCP-47 nudge (e.g. "en-US") the
	// caller passes when it has a strong prior on the speaker
	// language. The provider may ignore it.
	LanguageHint string

	// MimeType is the inbound audio container's declared type
	// (e.g. "audio/ogg", "audio/mp4"). Providers that need to
	// transcode use this to pick the right ffmpeg input flag.
	MimeType string

	// SampleRateHz is the audio's sample rate when the caller can
	// extract it cheaply. 0 means unknown; the provider should
	// probe / normalise.
	SampleRateHz int
}

// Audio is the result of TTSProvider.Synthesize. Bytes is the wire
// payload the channel adapter ships upstream as-is — Telegram wants
// OGG/Opus, Slack wants MP4/AAC, the WAV format is the lingua-franca
// fallback for callers that will transcode themselves.
type Audio struct {
	// Bytes is the encoded audio payload. May be large (~16 kB per
	// second of OGG/Opus at the Piper-default bitrate); callers
	// stream it to the upstream API rather than holding it long.
	Bytes []byte

	// MimeType is the wire content-type (e.g. "audio/ogg",
	// "audio/mp4"). Channel adapters set the upload's Content-Type
	// from this field.
	MimeType string

	// DurationMs is the synthesised audio duration in milliseconds.
	// Drives the platform length-cap check (Telegram 60s, Slack
	// 300s) without re-decoding the audio.
	DurationMs int64

	// SampleRateHz is the audio's sample rate. Telegram's sendVoice
	// expects 48 kHz; Slack's audio clip is more permissive.
	SampleRateHz int
}

// TTSOptions tune one Synthesize call. Empty fields take per-provider
// defaults — the Piper implementation honours VoiceID, Speed, and
// Format; Language is auto-derived from the voice model.
type TTSOptions struct {
	// VoiceID is the implementation-specific voice identifier.
	// For Piper this is the voice-model name (e.g.
	// "en_US-amy-medium"). Empty falls back to the provider's
	// configured default.
	VoiceID string

	// Language is a BCP-47 tag. Honored by providers that support
	// multilingual voices; ignored by single-language voice models
	// (most Piper voices). Empty leaves the voice's native language.
	Language string

	// Speed is the playback speed (1.0 = natural). Range 0.5–2.0
	// is the practical safe band; values outside that may degrade
	// quality. 0 falls back to 1.0.
	Speed float64

	// Format is the desired output container — "ogg-opus" for
	// Telegram, "mp4-aac" for Slack, "wav" as a fallback. Empty
	// falls back to the provider's native output (WAV for Piper).
	Format string
}

// ErrEmptyText is returned by TTSProvider.Synthesize when the text
// argument trims to empty. The channel adapter MUST guard against
// this upstream — calling Synthesize with no text is a logic error,
// not a runtime failure.
var ErrEmptyText = errors.New("voice: empty text")

// ErrOversizeText is returned by TTSProvider.Synthesize when the text
// length exceeds the provider's per-call cap. The channel adapter
// reacts by falling back to a text reply (matching the design doc's
// length-cap behaviour).
var ErrOversizeText = errors.New("voice: text exceeds provider cap")

// ErrProviderUnavailable is returned when the provider's underlying
// binary / model isn't installed. Surfaced verbatim so operators see
// "set PIPER_BIN" rather than a generic 500.
var ErrProviderUnavailable = errors.New("voice: provider unavailable")
