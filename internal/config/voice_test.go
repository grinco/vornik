package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestVoiceConfig_YAMLUnmarshal locks the YAML shape: the top-
// level `voice:` block populates Voice.STT / Voice.TTS with the
// fields the personal-assistant.yaml example documents.
func TestVoiceConfig_YAMLUnmarshal(t *testing.T) {
	const sample = `
voice:
  stt:
    provider: whisper-local
    model: /var/lib/vornik/voice/ggml-base.en.bin
    binary_path: /usr/local/bin/whisper-cpp
    ffmpeg_path: /usr/bin/ffmpeg
    language_hint: en
    threads: 4
  tts:
    provider: piper
    voice: /var/lib/vornik/voice/en_US-amy-medium.onnx
    binary_path: /usr/local/bin/piper
    ffmpeg_path: /usr/bin/ffmpeg
    speed: 1.1
    max_text_runes: 1500
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	stt := cfg.Voice.STT
	if stt.Provider != "whisper-local" {
		t.Errorf("STT.Provider = %q, want whisper-local", stt.Provider)
	}
	if stt.Model != "/var/lib/vornik/voice/ggml-base.en.bin" {
		t.Errorf("STT.Model = %q", stt.Model)
	}
	if stt.BinaryPath != "/usr/local/bin/whisper-cpp" {
		t.Errorf("STT.BinaryPath = %q", stt.BinaryPath)
	}
	if stt.FFmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("STT.FFmpegPath = %q", stt.FFmpegPath)
	}
	if stt.LanguageHint != "en" {
		t.Errorf("STT.LanguageHint = %q", stt.LanguageHint)
	}
	if stt.Threads != 4 {
		t.Errorf("STT.Threads = %d, want 4", stt.Threads)
	}

	tts := cfg.Voice.TTS
	if tts.Provider != "piper" {
		t.Errorf("TTS.Provider = %q, want piper", tts.Provider)
	}
	if tts.Voice != "/var/lib/vornik/voice/en_US-amy-medium.onnx" {
		t.Errorf("TTS.Voice = %q", tts.Voice)
	}
	if tts.BinaryPath != "/usr/local/bin/piper" {
		t.Errorf("TTS.BinaryPath = %q", tts.BinaryPath)
	}
	if tts.Speed != 1.1 {
		t.Errorf("TTS.Speed = %v, want 1.1", tts.Speed)
	}
	if tts.MaxTextRunes != 1500 {
		t.Errorf("TTS.MaxTextRunes = %d, want 1500", tts.MaxTextRunes)
	}
}

// TestVoiceConfig_DefaultsToZeroValues — a config without a
// voice block parses cleanly with empty providers (voice
// disabled).
func TestVoiceConfig_DefaultsToZeroValues(t *testing.T) {
	const sample = `server:
  address: ":8080"`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if cfg.Voice.STT.Provider != "" {
		t.Errorf("STT.Provider should default empty, got %q", cfg.Voice.STT.Provider)
	}
	if cfg.Voice.TTS.Provider != "" {
		t.Errorf("TTS.Provider should default empty, got %q", cfg.Voice.TTS.Provider)
	}
}

// TestVoiceConfig_PartialSTTOnly — operator wires STT but leaves
// TTS empty (transcribe-but-reply-in-text mode). Both sub-blocks
// are independent.
func TestVoiceConfig_PartialSTTOnly(t *testing.T) {
	const sample = `
voice:
  stt:
    provider: whisper-local
    model: /tmp/m.bin
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(sample), &cfg); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if cfg.Voice.STT.Provider != "whisper-local" {
		t.Errorf("STT.Provider = %q", cfg.Voice.STT.Provider)
	}
	if cfg.Voice.TTS.Provider != "" {
		t.Errorf("TTS.Provider should be empty, got %q", cfg.Voice.TTS.Provider)
	}
}
