package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// PiperConfig configures the local Piper TTS subprocess wrapper.
//
// Host dep matrix (slice-1 decisions):
//
//   - Piper CLI binary (https://github.com/rhasspy/piper). The
//     deployment host needs `piper` somewhere on $PATH OR an explicit
//     BinaryPath. Tested with piper 2023.x; the binary's CLI surface
//     has been stable since 1.0.
//   - At least one voice model (.onnx + .onnx.json pair). The
//     `en_US-amy-medium` model is the documented default; operators
//     point ModelPath at the .onnx file and Piper auto-discovers the
//     adjacent .json.
//   - ffmpeg, when callers ask for Format other than "wav" (Piper
//     emits WAV natively; the transcode step turns it into ogg-opus
//     for Telegram or mp4-aac for Slack). The fallback decision is
//     "use ffmpeg" — universally available, avoids a Go-side encoder
//     dependency. Documented here so the slice-1 commit doesn't
//     surprise operators on minimal containers.
//
// All three are runtime-probed lazily: a missing binary surfaces as
// ErrProviderUnavailable on the first Synthesize call, NOT at
// construction. This lets the daemon boot in environments where voice
// is opt-in and the operator hasn't yet installed the deps.
type PiperConfig struct {
	// BinaryPath is the absolute path to the piper CLI binary. Empty
	// asks exec.LookPath("piper"), so operators can install via their
	// package manager and leave the field unset.
	BinaryPath string

	// ModelPath is the absolute path to the voice model's .onnx file.
	// Required — Piper has no implicit default model.
	ModelPath string

	// FFmpegPath is the absolute path to the ffmpeg binary used for
	// the WAV→ogg-opus / WAV→mp4-aac transcode. Empty asks
	// exec.LookPath("ffmpeg").
	FFmpegPath string

	// DefaultVoice is the fallback when TTSOptions.VoiceID is empty.
	// Piper's CLI doesn't actually use a voice name — the voice IS
	// the .onnx model — but we keep the field for symmetry with
	// hosted providers (slice 7) and to surface it in audit logs.
	DefaultVoice string

	// DefaultSpeed is the fallback when TTSOptions.Speed is 0.
	// Piper's CLI flag --length-scale takes the INVERSE of speed
	// (length 0.5 = 2x faster); the wrapper does that translation.
	DefaultSpeed float64

	// MaxTextRunes caps one synthesis call. Defends against an LLM
	// reply that's wider than the platform's 1-minute / 5-minute
	// voice envelope. Zero falls back to defaultPiperMaxRunes
	// (1500 runes ~ 90 seconds at conversational pace).
	MaxTextRunes int
}

const (
	defaultPiperVoice    = "en_US-amy-medium"
	defaultPiperSpeed    = 1.0
	defaultPiperMaxRunes = 1500
)

// piperLocalTTS wraps the Piper CLI binary as a TTSProvider. The
// subprocess flow is:
//
//  1. spawn piper with --model <ModelPath> --output_raw (Piper emits
//     a raw WAV on stdout when --output_raw is unset; we pipe stdout
//     into either ffmpeg or back to the caller depending on Format).
//  2. write the input text on the subprocess's stdin and close it
//     (Piper waits for EOF before emitting).
//  3. read stdout bytes.
//  4. transcode if Format != "wav".
//
// All four steps happen under a context-aware exec.Cmd so cancellation
// propagates to the OS process.
type piperLocalTTS struct {
	cfg PiperConfig

	// runCmd swaps in fakes for testing. Defaults to runRealCmd; tests
	// stub this to return canned WAV / fail at specific stages.
	runCmd func(ctx context.Context, name string, args []string, stdin []byte) (stdout []byte, stderr []byte, err error)
}

// NewPiperLocalTTS constructs the Piper subprocess wrapper. Returns
// an error only when the config is structurally broken (empty
// ModelPath). Missing binaries are NOT a construction failure —
// they surface as ErrProviderUnavailable on the first call.
func NewPiperLocalTTS(cfg PiperConfig) (TTSProvider, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, errors.New("voice: PiperConfig.ModelPath is required")
	}
	if cfg.DefaultVoice == "" {
		cfg.DefaultVoice = defaultPiperVoice
	}
	if cfg.DefaultSpeed <= 0 {
		cfg.DefaultSpeed = defaultPiperSpeed
	}
	if cfg.MaxTextRunes <= 0 {
		cfg.MaxTextRunes = defaultPiperMaxRunes
	}
	return &piperLocalTTS{cfg: cfg, runCmd: runRealCmd}, nil
}

// Synthesize is the TTSProvider entry point. Validates inputs,
// invokes the Piper subprocess, transcodes if needed, and returns
// the encoded audio + metadata. Honors ctx cancellation.
func (p *piperLocalTTS) Synthesize(ctx context.Context, text string, opts TTSOptions) (Audio, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Audio{}, ErrEmptyText
	}
	if runesIn(trimmed) > p.cfg.MaxTextRunes {
		return Audio{}, fmt.Errorf("%w: %d runes > %d cap",
			ErrOversizeText, runesIn(trimmed), p.cfg.MaxTextRunes)
	}

	bin := p.cfg.BinaryPath
	if bin == "" {
		resolved, err := exec.LookPath("piper")
		if err != nil {
			return Audio{}, fmt.Errorf("%w: piper binary not found: %v", ErrProviderUnavailable, err)
		}
		bin = resolved
	}

	speed := opts.Speed
	if speed <= 0 {
		speed = p.cfg.DefaultSpeed
	}
	// Piper's --length-scale is the inverse of speed: shorter scale =
	// faster speech.
	lengthScale := 1.0 / speed

	args := []string{
		"--model", p.cfg.ModelPath,
		"--length-scale", formatFloat(lengthScale),
		"--output_file", "-",
	}
	stdout, stderr, err := p.runCmd(ctx, bin, args, []byte(trimmed))
	if err != nil {
		// Subprocess exited non-zero or couldn't spawn. Surface
		// stderr verbatim — Piper's error messages are short and
		// actionable ("model not found", "invalid voice", ...).
		return Audio{}, fmt.Errorf("voice: piper exec failed: %w: %s", err, trimSpaces(string(stderr)))
	}
	if len(stdout) == 0 {
		return Audio{}, errors.New("voice: piper produced empty output")
	}

	// Piper writes a RIFF WAV header followed by PCM samples. The
	// parser here is defensive: it only consults the header to set
	// SampleRateHz and DurationMs; the bytes themselves pass through
	// unchanged to the transcode step (or the caller, when
	// Format=="wav").
	sampleRate, durationMs, parseErr := parseWAV(stdout)
	if parseErr != nil {
		return Audio{}, fmt.Errorf("voice: piper output not a parseable WAV: %w", parseErr)
	}

	format := opts.Format
	if format == "" {
		format = "wav"
	}

	switch format {
	case "wav":
		return Audio{
			Bytes:        stdout,
			MimeType:     "audio/wav",
			DurationMs:   durationMs,
			SampleRateHz: sampleRate,
		}, nil
	case "ogg-opus":
		out, err := p.transcode(ctx, stdout, format)
		if err != nil {
			return Audio{}, err
		}
		return Audio{
			Bytes:        out,
			MimeType:     "audio/ogg",
			DurationMs:   durationMs,
			SampleRateHz: sampleRate,
		}, nil
	case "mp4-aac":
		out, err := p.transcode(ctx, stdout, format)
		if err != nil {
			return Audio{}, err
		}
		return Audio{
			Bytes:        out,
			MimeType:     "audio/mp4",
			DurationMs:   durationMs,
			SampleRateHz: sampleRate,
		}, nil
	default:
		return Audio{}, fmt.Errorf("voice: unsupported format %q (want wav | ogg-opus | mp4-aac)", format)
	}
}

// transcode runs ffmpeg to convert Piper's WAV output to either
// ogg-opus (Telegram-native) or mp4-aac (Slack-native). Driven by a
// canned arg list per target format so the call site stays small.
func (p *piperLocalTTS) transcode(ctx context.Context, wavBytes []byte, format string) ([]byte, error) {
	bin := p.cfg.FFmpegPath
	if bin == "" {
		resolved, err := exec.LookPath("ffmpeg")
		if err != nil {
			return nil, fmt.Errorf("%w: ffmpeg binary not found: %v", ErrProviderUnavailable, err)
		}
		bin = resolved
	}
	var args []string
	switch format {
	case "ogg-opus":
		// Telegram's sendVoice expects OGG with a single Opus stream.
		// 48 kHz is the only Opus-native rate; ffmpeg auto-resamples
		// Piper's 22.05 kHz output. -application voip biases the
		// encoder for speech latency over music fidelity.
		args = []string{
			"-loglevel", "error",
			"-f", "wav",
			"-i", "-",
			"-c:a", "libopus",
			"-b:a", "32k",
			"-application", "voip",
			"-ar", "48000",
			"-ac", "1",
			"-f", "ogg",
			"-",
		}
	case "mp4-aac":
		// Slack's audio clip UI renders MP4/AAC inline. -movflags
		// frag_keyframe+empty_moov lets ffmpeg write the MP4 to a
		// non-seekable stdout (the default MP4 writer rewinds, which
		// breaks pipes).
		args = []string{
			"-loglevel", "error",
			"-f", "wav",
			"-i", "-",
			"-c:a", "aac",
			"-b:a", "64k",
			"-ar", "44100",
			"-ac", "1",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4",
			"-",
		}
	default:
		return nil, fmt.Errorf("voice: transcode: unsupported format %q", format)
	}
	stdout, stderr, err := p.runCmd(ctx, bin, args, wavBytes)
	if err != nil {
		return nil, fmt.Errorf("voice: ffmpeg transcode failed: %w: %s",
			err, trimSpaces(string(stderr)))
	}
	if len(stdout) == 0 {
		return nil, errors.New("voice: ffmpeg transcode produced empty output")
	}
	return stdout, nil
}

// runRealCmd is the production runCmd implementation. Pipes stdin in,
// collects stdout + stderr separately, and honours ctx cancellation
// via exec.CommandContext (SIGKILL on ctx.Done()).
func runRealCmd(ctx context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return outBuf.Bytes(), errBuf.Bytes(), err
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// parseWAV reads the minimal RIFF/WAVE header to extract sample rate
// and duration. Doesn't validate every chunk — we trust Piper to emit
// a sane file and only need the two numbers for downstream metadata.
// The parser is defensive against truncated headers; returns an error
// when the file is too short to be a WAV at all.
//
// Wire layout (little-endian):
//
//	offset 0:  "RIFF"
//	offset 8:  "WAVE"
//	offset 12: "fmt "
//	offset 16: chunk size (4 bytes)
//	offset 20: audio format (2 bytes, 1 = PCM)
//	offset 22: num channels (2 bytes)
//	offset 24: sample rate (4 bytes)
//	offset 28: byte rate (4 bytes)
//	...
//	offset 36: "data"
//	offset 40: data chunk size (4 bytes)
//	offset 44: PCM samples
func parseWAV(b []byte) (sampleRate int, durationMs int64, err error) {
	if len(b) < 44 {
		return 0, 0, fmt.Errorf("wav too short: %d bytes", len(b))
	}
	if string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, errors.New("not a RIFF/WAVE file")
	}
	// numChannels = b[22:24] LE
	channels := int(binary.LittleEndian.Uint16(b[22:24]))
	if channels <= 0 {
		channels = 1
	}
	sampleRate = int(binary.LittleEndian.Uint32(b[24:28]))
	byteRate := int(binary.LittleEndian.Uint32(b[28:32]))
	dataSize := int(binary.LittleEndian.Uint32(b[40:44]))
	if byteRate <= 0 {
		// Fall back to (sampleRate * channels * 16-bit). Piper always
		// emits 16-bit PCM; we don't read the bits-per-sample field
		// explicitly because the byte rate already encodes it.
		byteRate = sampleRate * channels * 2
	}
	if byteRate <= 0 {
		return sampleRate, 0, nil
	}
	durationMs = int64(dataSize) * 1000 / int64(byteRate)
	return sampleRate, durationMs, nil
}

// runesIn counts UTF-8 rune length without iterating twice.
func runesIn(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// formatFloat renders a float as a short decimal for CLI args. Avoids
// the scientific-notation forms strconv.FormatFloat emits for small
// values (Piper's flag parser doesn't accept "1e-2").
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', 4, 64)
}

// trimSpaces collapses repeated whitespace runs in stderr so error
// messages stay greppable. Stderr from Piper/ffmpeg often contains
// long ANSI runs and progress bars.
func trimSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Compile-time guard: piperLocalTTS satisfies TTSProvider.
var _ TTSProvider = (*piperLocalTTS)(nil)

// ensure io is used (parseWAV may evolve to streaming).
var _ = io.Discard
