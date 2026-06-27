package service

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

// TestBuildSTTProvider_EmptyProviderReturnsNil — operator hasn't
// wired STT; helper returns (nil, nil) so initVoice leaves
// c.voiceSTT nil and the channel adapters stay on the
// text-only path.
func TestBuildSTTProvider_EmptyProviderReturnsNil(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{})
	if err != nil {
		t.Fatalf("buildSTTProvider(empty): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildSTTProvider_UnknownProviderReturnsNil — an unsupported
// provider name parses without error but yields a nil provider;
// initVoice surfaces this via a warn-level log rather than failing
// boot, since voice is opt-in scaffolding.
func TestBuildSTTProvider_UnknownProviderReturnsNil(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{Provider: "deepgram-cloud"})
	if err != nil {
		t.Fatalf("buildSTTProvider(unknown): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider for unknown name, got %T", p)
	}
}

// TestBuildSTTProvider_WhisperLocalRequiresModel — the well-formed-
// provider path bubbles up provider-side validation errors so the
// operator sees the typo loudly at boot.
func TestBuildSTTProvider_WhisperLocalRequiresModel(t *testing.T) {
	_, err := buildSTTProvider(config.VoiceSTTConfig{Provider: "whisper-local"})
	if err == nil {
		t.Fatal("expected error from whisper-local with empty model, got nil")
	}
	if !strings.Contains(err.Error(), "ModelPath") {
		t.Errorf("error %q should mention ModelPath", err.Error())
	}
}

// TestBuildSTTProvider_WhisperLocalHappyPath — well-formed config
// constructs a non-nil provider. Missing binaries surface at
// Transcribe time, not here.
func TestBuildSTTProvider_WhisperLocalHappyPath(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{
		Provider: "whisper-local",
		Model:    "/tmp/fake.ggml.bin",
	})
	if err != nil {
		t.Fatalf("buildSTTProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestBuildSTTProvider_CaseInsensitiveProviderName — guards
// against operator-side capitalisation typos in YAML.
func TestBuildSTTProvider_CaseInsensitiveProviderName(t *testing.T) {
	p, err := buildSTTProvider(config.VoiceSTTConfig{
		Provider: "Whisper-Local",
		Model:    "/tmp/fake.ggml.bin",
	})
	if err != nil {
		t.Fatalf("buildSTTProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestBuildTTSProvider_EmptyProviderReturnsNil — symmetric to STT.
func TestBuildTTSProvider_EmptyProviderReturnsNil(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{})
	if err != nil {
		t.Fatalf("buildTTSProvider(empty): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildTTSProvider_UnknownProviderReturnsNil — unsupported
// names parse without error.
func TestBuildTTSProvider_UnknownProviderReturnsNil(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{Provider: "elevenlabs"})
	if err != nil {
		t.Fatalf("buildTTSProvider(unknown): %v", err)
	}
	if p != nil {
		t.Errorf("expected nil provider, got %T", p)
	}
}

// TestBuildTTSProvider_PiperRequiresVoice — provider validation
// errors surface to the operator at boot.
func TestBuildTTSProvider_PiperRequiresVoice(t *testing.T) {
	_, err := buildTTSProvider(config.VoiceTTSConfig{Provider: "piper"})
	if err == nil {
		t.Fatal("expected error from piper with empty voice, got nil")
	}
	if !strings.Contains(err.Error(), "ModelPath") {
		t.Errorf("error %q should mention ModelPath", err.Error())
	}
}

// TestBuildTTSProvider_PiperHappyPath — well-formed config returns
// a non-nil provider.
func TestBuildTTSProvider_PiperHappyPath(t *testing.T) {
	p, err := buildTTSProvider(config.VoiceTTSConfig{
		Provider:     "piper",
		Voice:        "/tmp/voice.onnx",
		Speed:        1.0,
		MaxTextRunes: 1500,
	})
	if err != nil {
		t.Fatalf("buildTTSProvider: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

// TestInitVoice_EmptyConfigSucceedsAndLeavesBothNil — daemon boots
// without a voice block; initVoice succeeds, both providers stay
// nil, and the channel adapters fall back to their text-only paths.
func TestInitVoice_EmptyConfigSucceedsAndLeavesBothNil(t *testing.T) {
	c := &Container{
		Config: &config.Config{},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT != nil {
		t.Errorf("voiceSTT should be nil, got %T", c.voiceSTT)
	}
	if c.voiceTTS != nil {
		t.Errorf("voiceTTS should be nil, got %T", c.voiceTTS)
	}
}

// TestInitVoice_UnsupportedProviderLogsAndLeavesNil — unknown
// provider names don't fail the daemon; the warn-log is the
// signal the operator sees.
func TestInitVoice_UnsupportedProviderLogsAndLeavesNil(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "deepgram-cloud"},
				TTS: config.VoiceTTSConfig{Provider: "elevenlabs"},
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT != nil {
		t.Errorf("voiceSTT should be nil for unsupported provider, got %T", c.voiceSTT)
	}
	if c.voiceTTS != nil {
		t.Errorf("voiceTTS should be nil for unsupported provider, got %T", c.voiceTTS)
	}
}

// TestInitVoice_MalformedSupportedProviderFailsBoot — a supported
// provider with a structurally broken config (no model path) is
// loud-fail at boot, not silent.
func TestInitVoice_MalformedSupportedProviderFailsBoot(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "whisper-local"}, // missing Model
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err == nil {
		t.Fatal("expected error for whisper-local without model, got nil")
	}
}

// TestInitVoice_HappyPathWiresBoth — well-formed STT + TTS land on
// the container so downstream channel adapters can pick them up.
func TestInitVoice_HappyPathWiresBoth(t *testing.T) {
	c := &Container{
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{Provider: "whisper-local", Model: "/tmp/m.bin"},
				TTS: config.VoiceTTSConfig{Provider: "piper", Voice: "/tmp/v.onnx"},
			},
		},
		Logger: zerolog.Nop(),
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}
	if c.voiceSTT == nil {
		t.Error("voiceSTT should be set")
	}
	if c.voiceTTS == nil {
		t.Error("voiceTTS should be set")
	}
}

// captureLogger returns a zerolog.Logger that writes JSON records
// into the supplied buffer so tests can assert on the log surface
// produced by initVoice's probes.
func captureLogger(buf *bytes.Buffer) zerolog.Logger {
	return zerolog.New(buf)
}

// logRecords parses the captured log buffer (one JSON object per
// line) into a slice of decoded maps. Test helper for asserting on
// the diagnostic surface.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findLog returns the first log record whose "message" field
// contains the substring needle; nil when no match.
func findLog(records []map[string]any, needle string) map[string]any {
	for _, r := range records {
		if msg, ok := r["message"].(string); ok && strings.Contains(msg, needle) {
			return r
		}
	}
	return nil
}

// TestProbeBinary_ConfiguredPathMissing — operator pointed at a
// path that doesn't exist; we want a WARN with the bad path so the
// diagnostic is greppable.
func TestProbeBinary_ConfiguredPathMissing(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeBinary(c, "whisper", "/does/not/exist/whisper-cli", []string{"whisper-cpp"})

	rec := findLog(logRecords(t, &buf), "whisper binary not found at configured path")
	if rec == nil {
		t.Fatalf("expected 'not found at configured path' warning; got %q", buf.String())
	}
	if rec["level"] != "warn" {
		t.Errorf("level = %v, want warn", rec["level"])
	}
	if rec["path"] != "/does/not/exist/whisper-cli" {
		t.Errorf("path = %v, want /does/not/exist/whisper-cli", rec["path"])
	}
}

// TestProbeBinary_ConfiguredPathOK — explicit path that exists
// and is executable logs INFO with the resolved path.
func TestProbeBinary_ConfiguredPathOK(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "whisper-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeBinary(c, "whisper", fake, []string{"whisper-cpp"})

	rec := findLog(logRecords(t, &buf), "whisper binary OK")
	if rec == nil {
		t.Fatalf("expected 'whisper binary OK' info; got %q", buf.String())
	}
	if rec["level"] != "info" {
		t.Errorf("level = %v, want info", rec["level"])
	}
}

// TestProbeBinary_ConfiguredPathNotExecutable — a common operator
// foot-gun: the binary file exists but isn't chmod +x. Surface a
// distinct warning so they can fix it before sending voice.
func TestProbeBinary_ConfiguredPathNotExecutable(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "whisper-cli")
	if err := os.WriteFile(fake, []byte("data"), 0o644); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeBinary(c, "whisper", fake, []string{"whisper-cpp"})

	rec := findLog(logRecords(t, &buf), "not executable")
	if rec == nil {
		t.Fatalf("expected 'not executable' warning; got %q", buf.String())
	}
	if rec["level"] != "warn" {
		t.Errorf("level = %v, want warn", rec["level"])
	}
}

// TestProbeBinary_FallbackToPath — config is empty, $PATH lookup
// succeeds, INFO logs the resolved location.
func TestProbeBinary_FallbackToPath(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "whisper-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("PATH", tmp)
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeBinary(c, "whisper", "", []string{"whisper-cpp", "whisper-cli", "main"})

	rec := findLog(logRecords(t, &buf), "resolved from $PATH")
	if rec == nil {
		t.Fatalf("expected 'resolved from $PATH' info; got %q", buf.String())
	}
	if rec["name"] != "whisper-cli" {
		t.Errorf("name = %v, want whisper-cli", rec["name"])
	}
}

// TestProbeBinary_FallbackToPathFails — empty config + nothing on
// PATH yields the loud catch-all WARN listing every candidate the
// operator could install.
func TestProbeBinary_FallbackToPathFails(t *testing.T) {
	t.Setenv("PATH", "")
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeBinary(c, "whisper", "", []string{"whisper-cpp", "whisper-cli", "main"})

	rec := findLog(logRecords(t, &buf), "not on $PATH")
	if rec == nil {
		t.Fatalf("expected 'not on $PATH' warning; got %q", buf.String())
	}
	if rec["level"] != "warn" {
		t.Errorf("level = %v, want warn", rec["level"])
	}
}

// TestProbeModel_MissingFile — operator config points at a model
// file that isn't there; warn so they download it.
func TestProbeModel_MissingFile(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", "/does/not/exist/ggml-base.en.bin")

	rec := findLog(logRecords(t, &buf), "model file not found")
	if rec == nil {
		t.Fatalf("expected 'model file not found' warning; got %q", buf.String())
	}
	if rec["level"] != "warn" {
		t.Errorf("level = %v, want warn", rec["level"])
	}
}

// TestProbeModel_HappyPath — model file exists; INFO with the
// size so the operator can sanity-check the download didn't get
// truncated.
func TestProbeModel_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	model := filepath.Join(tmp, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("ggml-fake-bytes"), 0o644); err != nil {
		t.Fatalf("write fake model: %v", err)
	}
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", model)

	rec := findLog(logRecords(t, &buf), "model OK")
	if rec == nil {
		t.Fatalf("expected 'model OK' info; got %q", buf.String())
	}
	if got, want := int64(rec["size_bytes"].(float64)), int64(len("ggml-fake-bytes")); got != want {
		t.Errorf("size_bytes = %d, want %d", got, want)
	}
}

// TestProbeModel_IsDirectory — common config typo: operator
// points at a directory containing the model instead of the file
// itself. Surface a distinct warning.
func TestProbeModel_IsDirectory(t *testing.T) {
	tmp := t.TempDir()
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", tmp)

	rec := findLog(logRecords(t, &buf), "is a directory, not a file")
	if rec == nil {
		t.Fatalf("expected 'is a directory' warning; got %q", buf.String())
	}
}

// TestProbeModel_EmptyPath — defensive: nil config snuck through
// to the probe. Should warn rather than crash.
func TestProbeModel_EmptyPath(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: captureLogger(&buf)}
	probeModel(c, "whisper", "")

	rec := findLog(logRecords(t, &buf), "model path is empty")
	if rec == nil {
		t.Fatalf("expected 'model path is empty' warning; got %q", buf.String())
	}
}

// TestInitVoice_LogsResolvedConfigForSTT — when the operator
// wires STT, the boot-time config dump exposes the resolved
// binary + model paths so they're greppable on first failure.
func TestInitVoice_LogsResolvedConfigForSTT(t *testing.T) {
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "whisper-cli")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	model := filepath.Join(tmp, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("data"), 0o644); err != nil {
		t.Fatalf("write model: %v", err)
	}
	ffmpeg := filepath.Join(tmp, "ffmpeg")
	if err := os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write ffmpeg: %v", err)
	}

	var buf bytes.Buffer
	c := &Container{
		Logger: captureLogger(&buf),
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{
					Provider:   "whisper-local",
					Model:      model,
					BinaryPath: binary,
					FFmpegPath: ffmpeg,
				},
			},
		},
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}

	records := logRecords(t, &buf)
	if rec := findLog(records, "configuring STT provider"); rec == nil {
		t.Errorf("missing config-dump log; got %q", buf.String())
	}
	if rec := findLog(records, "whisper binary OK"); rec == nil {
		t.Errorf("missing whisper-OK log; got %q", buf.String())
	}
	if rec := findLog(records, "whisper model OK"); rec == nil {
		t.Errorf("missing model-OK log; got %q", buf.String())
	}
	if rec := findLog(records, "ffmpeg binary OK"); rec == nil {
		t.Errorf("missing ffmpeg-OK log; got %q", buf.String())
	}
	if rec := findLog(records, "voice providers initialized"); rec == nil {
		t.Errorf("missing summary log; got %q", buf.String())
	}
}

// TestInitVoice_WarnsOnEachMisconfig — well-formed Provider but
// every dependency is broken: the operator sees one WARN per
// missing component so they can fix them in order.
func TestInitVoice_WarnsOnEachMisconfig(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: captureLogger(&buf),
		Config: &config.Config{
			Voice: config.VoiceConfig{
				STT: config.VoiceSTTConfig{
					Provider:   "whisper-local",
					Model:      "/no/such/model.bin",
					BinaryPath: "/no/such/whisper-cli",
					FFmpegPath: "/no/such/ffmpeg",
				},
			},
		},
	}
	if err := c.initVoice(); err != nil {
		t.Fatalf("initVoice: %v", err)
	}

	records := logRecords(t, &buf)
	for _, want := range []string{
		"whisper binary not found at configured path",
		"whisper model file not found",
		"ffmpeg binary not found at configured path",
	} {
		if findLog(records, want) == nil {
			t.Errorf("missing warning %q; got %q", want, buf.String())
		}
	}
}
