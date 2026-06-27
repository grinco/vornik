package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WhisperConfig configures the local whisper.cpp STT subprocess
// wrapper.
//
// Host dep matrix (slice-2 decisions):
//
//   - whisper.cpp `main` CLI binary
//     (https://github.com/ggerganov/whisper.cpp). The deployment host
//     needs `whisper-cpp` OR `main` on $PATH, OR an explicit
//     BinaryPath. The official build produces a binary named `main`;
//     some Linux distributions rename to `whisper-cpp` to avoid
//     PATH collisions. The wrapper probes both.
//   - one ggml-format model file (.bin). Recommended starting points:
//     ggml-base.en.bin (~150 MB, English-only, fast on CPU) and
//     ggml-medium.bin (~1.5 GB, multilingual, much slower). Operator
//     downloads via the upstream models/download-ggml-model.sh script
//     and points ModelPath at the .bin.
//   - ffmpeg, ALWAYS. Inbound audio from Telegram (OGG/Opus) and
//     Slack (MP4/M4A) needs normalisation to the 16-kHz mono PCM WAV
//     whisper.cpp accepts; the wrapper pipes every Transcribe call
//     through ffmpeg first. Documented here so the slice-2 commit
//     doesn't surprise operators on minimal containers.
//
// Why subprocess vs CGo vs Python (the slice-2 decision):
//
//   - whisper.cpp CLI via subprocess: no CGo (cross-compilation
//     trivially with `go build`), no Python runtime. Two binary
//     deps on the host but those install via the operator's package
//     manager.
//   - whisper.cpp via CGo: complicates cross-compile; pulling
//     C-toolchain headers into vornik's build pipeline raises the
//     dev-onboarding bar. Rejected.
//   - faster-whisper via Python: requires a Python runtime + pip
//     install + virtualenv management. Better accuracy on noisy
//     audio but the operational tax is heavy. Rejected for the MVP;
//     remains available as a slice-7 hosted-style fallback if an
//     operator wants it.
type WhisperConfig struct {
	// BinaryPath is the absolute path to the whisper.cpp main CLI
	// binary. Empty asks exec.LookPath("whisper-cpp"), falling back
	// to exec.LookPath("main") (the upstream-build name).
	BinaryPath string

	// ModelPath is the absolute path to the ggml model file
	// (e.g. /usr/local/share/whisper.cpp/ggml-base.en.bin). Required.
	ModelPath string

	// FFmpegPath is the absolute path to the ffmpeg binary. Empty
	// asks exec.LookPath("ffmpeg"). Always used — whisper.cpp can't
	// read OGG/Opus or MP4/M4A directly.
	FFmpegPath string

	// LanguageHint is an optional BCP-47 nudge for the recogniser.
	// Empty asks whisper.cpp to auto-detect.
	LanguageHint string

	// Threads pins the OMP thread count. Zero defers to whisper.cpp's
	// default (one per physical core).
	Threads int

	// TempDir is the directory under which the wrapper writes its
	// intermediate WAV. Empty falls back to os.TempDir(). Tests can
	// pin this so the artifact is asserted-on without race.
	TempDir string
}

// whisperLocalSTT wraps the whisper.cpp main CLI as an STTProvider.
// The subprocess flow is:
//
//  1. Read all of `audio` into memory (audio messages are short;
//     1 minute of OGG/Opus is ~120 KiB, far below the per-call cap
//     of any platform).
//  2. Spawn ffmpeg with stdin = audio bytes, stdout = 16 kHz mono
//     16-bit PCM WAV. Container detection is automatic; ffmpeg
//     probes the header.
//  3. Write the WAV to a temp file (whisper.cpp's `main` reads from
//     a file path, not stdin).
//  4. Spawn whisper.cpp with --output-json. The CLI writes
//     <temp>.json next to the input file.
//  5. Read and parse the JSON. Done.
//
// All four steps respect ctx cancellation; the temp WAV + JSON are
// cleaned up in a deferred best-effort sweep.
type whisperLocalSTT struct {
	cfg WhisperConfig

	// runCmd swaps in fakes for testing. Defaults to runRealCmd.
	runCmd func(ctx context.Context, name string, args []string, stdin []byte) (stdout []byte, stderr []byte, err error)

	// tempFileWriter is a seam for tests: real code uses
	// writeTempFile (which writes to disk + returns the path); tests
	// stub it to assert on the bytes without touching disk.
	tempFileWriter func(dir, namePrefix string, contents []byte) (path string, cleanup func(), err error)
}

// NewWhisperLocalSTT constructs the wrapper. Returns an error only
// when the config is structurally broken (empty ModelPath). Missing
// binaries surface as ErrProviderUnavailable on the first Transcribe.
func NewWhisperLocalSTT(cfg WhisperConfig) (STTProvider, error) {
	if strings.TrimSpace(cfg.ModelPath) == "" {
		return nil, errors.New("voice: WhisperConfig.ModelPath is required")
	}
	return &whisperLocalSTT{
		cfg:            cfg,
		runCmd:         runRealCmd,
		tempFileWriter: writeTempFile,
	}, nil
}

// Transcribe is the STTProvider entry point. See the type comment for
// the four-step flow.
func (w *whisperLocalSTT) Transcribe(ctx context.Context, audio io.Reader, hint Hint) (Transcript, error) {
	if audio == nil {
		return Transcript{}, errors.New("voice: nil audio reader")
	}
	// Read the inbound payload into memory. 64 MiB is a defensive
	// ceiling — the design doc notes Telegram voice messages cap at
	// 1 minute (~120 KiB OGG/Opus) and Slack audio at 5 minutes
	// (~5 MiB MP4/AAC); 64 MiB is comfortable headroom.
	const maxInboundBytes = 64 * 1024 * 1024
	audioBytes, err := io.ReadAll(io.LimitReader(audio, maxInboundBytes+1))
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: read inbound audio: %w", err)
	}
	if int64(len(audioBytes)) > maxInboundBytes {
		return Transcript{}, fmt.Errorf("voice: inbound audio exceeds %d byte cap", maxInboundBytes)
	}
	if len(audioBytes) == 0 {
		return Transcript{}, errors.New("voice: empty audio input")
	}

	// Step 1: ffmpeg normalise to 16 kHz mono 16-bit PCM WAV. -y
	// would overwrite an output file, but we're piping to stdout
	// here. -ac 1 = mono; -ar 16000 = 16 kHz (whisper's native rate);
	// -f wav over stdout.
	ffmpegBin := w.cfg.FFmpegPath
	if ffmpegBin == "" {
		resolved, err := exec.LookPath("ffmpeg")
		if err != nil {
			return Transcript{}, fmt.Errorf("%w: ffmpeg binary not found: %v", ErrProviderUnavailable, err)
		}
		ffmpegBin = resolved
	}
	ffArgs := []string{
		"-loglevel", "error",
		"-i", "-",
		"-ac", "1",
		"-ar", "16000",
		"-acodec", "pcm_s16le",
		"-f", "wav",
		"-",
	}
	wavBytes, ffStderr, err := w.runCmd(ctx, ffmpegBin, ffArgs, audioBytes)
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: ffmpeg normalise failed: %w: %s", err, trimSpaces(string(ffStderr)))
	}
	if len(wavBytes) == 0 {
		return Transcript{}, errors.New("voice: ffmpeg produced empty WAV")
	}

	// Step 2: write to a temp file. whisper.cpp's `main` doesn't
	// accept stdin — it mmaps the audio file.
	tmpDir := w.cfg.TempDir
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	wavPath, cleanupWAV, err := w.tempFileWriter(tmpDir, "vornik-voice-*.wav", wavBytes)
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: write temp WAV: %w", err)
	}
	defer cleanupWAV()

	// Step 3: whisper.cpp subprocess. --output-json writes the
	// transcript to <input>.json (NOT stdout). --no-prints silences
	// the model-loading banner so stderr stays a usable error signal.
	whisperBin := w.cfg.BinaryPath
	if whisperBin == "" {
		resolved, lookupErr := lookupWhisperBinary()
		if lookupErr != nil {
			return Transcript{}, fmt.Errorf("%w: whisper.cpp binary not found: %v",
				ErrProviderUnavailable, lookupErr)
		}
		whisperBin = resolved
	}
	whisperArgs := []string{
		"--model", w.cfg.ModelPath,
		"--file", wavPath,
		"--output-json",
		"--no-prints",
	}
	if w.cfg.Threads > 0 {
		whisperArgs = append(whisperArgs, "--threads", fmt.Sprintf("%d", w.cfg.Threads))
	}
	lang := strings.TrimSpace(hint.LanguageHint)
	if lang == "" {
		lang = strings.TrimSpace(w.cfg.LanguageHint)
	}
	if lang != "" {
		// whisper.cpp accepts the language hint as a short code
		// ("en", "de"). BCP-47 like "en-US" → take the prefix.
		short := lang
		if idx := strings.IndexAny(short, "-_"); idx > 0 {
			short = short[:idx]
		}
		whisperArgs = append(whisperArgs, "--language", short)
	}

	_, whStderr, err := w.runCmd(ctx, whisperBin, whisperArgs, nil)
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: whisper.cpp failed: %w: %s",
			err, trimSpaces(string(whStderr)))
	}

	// Step 4: read & parse the JSON sidecar.
	jsonPath := wavPath + ".json"
	rawJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		return Transcript{}, fmt.Errorf("voice: read whisper JSON: %w", err)
	}
	defer func() { _ = os.Remove(jsonPath) }()
	return parseWhisperJSON(rawJSON)
}

// whisperJSONShape mirrors the subset of whisper.cpp's --output-json
// envelope we consume. The full schema is documented at
// https://github.com/ggerganov/whisper.cpp/blob/master/examples/main/main.cpp
// — we lift the language verdict, the global duration, and the
// per-segment text + confidence (when present).
//
// Layout (abbreviated):
//
//	{
//	  "result": { "language": "en" },
//	  "params": { "model": "ggml-base.en.bin", "language": "en" },
//	  "transcription": [
//	    { "text": "...", "offsets": { "from": 0, "to": 5000 } },
//	    ...
//	  ]
//	}
//
// Newer whisper.cpp builds emit a token-level "confidence" or
// "no_speech_prob" — we honour `no_speech_prob` when present
// (treating 1 - no_speech_prob as the segment confidence) and fall
// back to 0 (unknown).
type whisperJSONShape struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Params struct {
		Language string `json:"language"`
	} `json:"params"`
	Transcription []struct {
		Text    string `json:"text"`
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		NoSpeechProb *float64 `json:"no_speech_prob,omitempty"`
		Confidence   *float64 `json:"confidence,omitempty"`
	} `json:"transcription"`
}

// parseWhisperJSON folds the JSON sidecar into a Transcript. Combines
// the per-segment texts (space-separated), takes the max-To offset as
// the duration, and averages the per-segment confidence (when any
// segment reported one).
func parseWhisperJSON(raw []byte) (Transcript, error) {
	var doc whisperJSONShape
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Transcript{}, fmt.Errorf("voice: whisper JSON parse: %w", err)
	}
	parts := make([]string, 0, len(doc.Transcription))
	var totalConf float64
	var confSamples int
	var maxTo int64
	for _, seg := range doc.Transcription {
		txt := strings.TrimSpace(seg.Text)
		if txt != "" {
			parts = append(parts, txt)
		}
		if seg.Offsets.To > maxTo {
			maxTo = seg.Offsets.To
		}
		switch {
		case seg.Confidence != nil:
			totalConf += *seg.Confidence
			confSamples++
		case seg.NoSpeechProb != nil:
			totalConf += 1.0 - *seg.NoSpeechProb
			confSamples++
		}
	}
	out := Transcript{
		Text:       strings.Join(parts, " "),
		DurationMs: maxTo,
	}
	if doc.Result.Language != "" {
		out.Language = doc.Result.Language
	} else {
		out.Language = doc.Params.Language
	}
	if confSamples > 0 {
		out.Confidence = totalConf / float64(confSamples)
	}
	return out, nil
}

// lookupWhisperBinary probes the known names whisper.cpp's main CLI
// ships under: distros rename to "whisper-cpp", upstream's 2024+
// builds publish "whisper-cli" (homebrew uses this name), and the
// historical default was "main". Returns the first hit. The error
// surface is the LAST exec.LookPath error, which is the more
// actionable one (it includes $PATH context).
func lookupWhisperBinary() (string, error) {
	candidates := []string{"whisper-cpp", "whisper-cli", "main"}
	var lastErr error
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no whisper.cpp candidate on PATH")
	}
	return "", lastErr
}

// writeTempFile writes contents to a fresh file under dir with the
// given prefix pattern (os.CreateTemp shape), returning the path and a
// best-effort cleanup callback. The seam exists so tests can stub the
// disk-touching path without permission gymnastics.
func writeTempFile(dir, namePrefix string, contents []byte) (string, func(), error) {
	f, err := os.CreateTemp(dir, namePrefix)
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.Write(contents); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	path := f.Name()
	return path, func() { _ = os.Remove(path) }, nil
}

// Compile-time guard: whisperLocalSTT satisfies STTProvider.
var _ STTProvider = (*whisperLocalSTT)(nil)

// Ensure filepath remains referenced — Transcribe may grow filepath
// safety logic on follow-up slices.
var _ = filepath.Base
