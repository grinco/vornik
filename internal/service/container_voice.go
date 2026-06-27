package service

import (
	"os"
	"os/exec"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/voice"
)

// initVoice constructs the daemon-level speech-to-text and
// text-to-speech providers from config.Voice. Result lands on
// c.voiceSTT / c.voiceTTS; either may stay nil when the
// corresponding sub-block is empty or names an unsupported
// provider. Construction failures from a well-formed config
// (e.g. an unsupported provider name) log a warning and leave
// the provider nil — voice is opt-in scaffolding and a misconfig
// should not block daemon boot. Truly broken provider configs
// (e.g. whisper-local without a model path) surface as a returned
// error so the operator sees the typo loudly.
//
// Boot-time diagnostics: when a provider sub-block is configured,
// the function probes the resolved binary + model + ffmpeg paths
// and logs the outcome. Missing binaries / unreadable model files
// are reported as WARN so the operator sees the misconfig in the
// daemon's startup log instead of waiting for the first voice
// message to fail. Probing is best-effort — failures don't block
// boot (per the slice-1 design's "binaries probed lazily on first
// call" rule), but the diagnostic surface is what makes
// troubleshooting tractable when something's off.
func (c *Container) initVoice() error {
	// STT --------------------------------------------------------
	sttRaw := strings.TrimSpace(c.Config.Voice.STT.Provider)
	if sttRaw == "" {
		c.Logger.Debug().Msg("voice: STT disabled (no provider configured)")
	} else {
		c.Logger.Info().
			Str("provider", sttRaw).
			Str("binary_path", c.Config.Voice.STT.BinaryPath).
			Str("model", c.Config.Voice.STT.Model).
			Str("ffmpeg_path", c.Config.Voice.STT.FFmpegPath).
			Str("language_hint", c.Config.Voice.STT.LanguageHint).
			Msg("voice: configuring STT provider")
	}
	stt, err := buildSTTProvider(c.Config.Voice.STT)
	if err != nil {
		return err
	}
	if stt == nil && sttRaw != "" {
		c.Logger.Warn().
			Str("provider", sttRaw).
			Msg("voice: unsupported STT provider — voice inbound disabled (supported: whisper-local)")
	}
	if stt != nil {
		probeSTT(c, c.Config.Voice.STT)
	}
	c.voiceSTT = stt

	// TTS --------------------------------------------------------
	ttsRaw := strings.TrimSpace(c.Config.Voice.TTS.Provider)
	if ttsRaw == "" {
		c.Logger.Debug().Msg("voice: TTS disabled (no provider configured)")
	} else {
		c.Logger.Info().
			Str("provider", ttsRaw).
			Str("binary_path", c.Config.Voice.TTS.BinaryPath).
			Str("voice_model", c.Config.Voice.TTS.Voice).
			Str("ffmpeg_path", c.Config.Voice.TTS.FFmpegPath).
			Float64("speed", c.Config.Voice.TTS.Speed).
			Int("max_text_runes", c.Config.Voice.TTS.MaxTextRunes).
			Msg("voice: configuring TTS provider")
	}
	tts, err := buildTTSProvider(c.Config.Voice.TTS)
	if err != nil {
		return err
	}
	if tts == nil && ttsRaw != "" {
		c.Logger.Warn().
			Str("provider", ttsRaw).
			Msg("voice: unsupported TTS provider — voice outbound disabled (supported: piper)")
	}
	if tts != nil {
		probeTTS(c, c.Config.Voice.TTS)
	}
	c.voiceTTS = tts

	if c.voiceSTT != nil || c.voiceTTS != nil {
		c.Logger.Info().
			Bool("stt", c.voiceSTT != nil).
			Bool("tts", c.voiceTTS != nil).
			Msg("voice providers initialized")
	}
	return nil
}

func buildSTTProvider(cfg config.VoiceSTTConfig) (voice.STTProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return nil, nil
	case "whisper-local":
		return voice.NewWhisperLocalSTT(voice.WhisperConfig{
			BinaryPath:   strings.TrimSpace(cfg.BinaryPath),
			ModelPath:    strings.TrimSpace(cfg.Model),
			FFmpegPath:   strings.TrimSpace(cfg.FFmpegPath),
			LanguageHint: cfg.LanguageHint,
			Threads:      cfg.Threads,
		})
	default:
		return nil, nil
	}
}

func buildTTSProvider(cfg config.VoiceTTSConfig) (voice.TTSProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "":
		return nil, nil
	case "piper":
		return voice.NewPiperLocalTTS(voice.PiperConfig{
			BinaryPath:   strings.TrimSpace(cfg.BinaryPath),
			ModelPath:    strings.TrimSpace(cfg.Voice),
			FFmpegPath:   strings.TrimSpace(cfg.FFmpegPath),
			DefaultSpeed: cfg.Speed,
			MaxTextRunes: cfg.MaxTextRunes,
		})
	default:
		return nil, nil
	}
}

// probeSTT runs best-effort accessibility checks on the resolved
// STT config and logs the outcomes. Doesn't fail boot — the
// provider wrapper probes lazily on first call by design, but the
// operator sees the warnings here first so they can fix the
// install before sending a voice message and getting a humane
// error reply.
func probeSTT(c *Container, cfg config.VoiceSTTConfig) {
	probeBinary(c, "whisper", cfg.BinaryPath, []string{"whisper-cpp", "whisper-cli", "main"})
	probeModel(c, "whisper", cfg.Model)
	probeBinary(c, "ffmpeg", cfg.FFmpegPath, []string{"ffmpeg"})
}

func probeTTS(c *Container, cfg config.VoiceTTSConfig) {
	probeBinary(c, "piper", cfg.BinaryPath, []string{"piper"})
	probeModel(c, "piper", cfg.Voice)
	probeBinary(c, "ffmpeg", cfg.FFmpegPath, []string{"ffmpeg"})
}

// probeBinary resolves a binary path either from the explicit
// config value or by walking $PATH using the provided candidates,
// then stat's the result. Logs INFO with the resolved path, or
// WARN when nothing's reachable. The label parameter ("whisper",
// "piper", "ffmpeg") is the field-tag the operator searches for
// in their logs.
func probeBinary(c *Container, label, configured string, pathCandidates []string) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		info, err := os.Stat(configured)
		switch {
		case err == nil && info.Mode()&0o111 == 0:
			c.Logger.Warn().
				Str("path", configured).
				Str("mode", info.Mode().String()).
				Msgf("voice: %s binary exists but is not executable — chmod +x or fix binary_path", label)
		case err == nil:
			c.Logger.Info().
				Str("path", configured).
				Msgf("voice: %s binary OK", label)
		case os.IsNotExist(err):
			c.Logger.Warn().
				Str("path", configured).
				Msgf("voice: %s binary not found at configured path — first voice call will fail", label)
		default:
			c.Logger.Warn().
				Err(err).
				Str("path", configured).
				Msgf("voice: %s binary stat failed", label)
		}
		return
	}
	// No explicit path — fall back to $PATH lookup.
	for _, name := range pathCandidates {
		if p, err := exec.LookPath(name); err == nil {
			c.Logger.Info().
				Str("path", p).
				Str("name", name).
				Msgf("voice: %s binary resolved from $PATH", label)
			return
		}
	}
	c.Logger.Warn().
		Strs("tried", pathCandidates).
		Msgf("voice: %s binary not on $PATH (set binary_path in config to point at the install) — first voice call will fail", label)
}

// probeModel stat's the model file and logs its size. Empty path
// is a configuration error (provider construction would have
// failed already) — the wrapper still reports it for symmetry.
func probeModel(c *Container, label, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		c.Logger.Warn().
			Msgf("voice: %s model path is empty", label)
		return
	}
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		c.Logger.Warn().
			Str("path", path).
			Msgf("voice: %s model path is a directory, not a file", label)
	case err == nil:
		c.Logger.Info().
			Str("path", path).
			Int64("size_bytes", info.Size()).
			Msgf("voice: %s model OK", label)
	case os.IsNotExist(err):
		c.Logger.Warn().
			Str("path", path).
			Msgf("voice: %s model file not found — first voice call will fail", label)
	default:
		c.Logger.Warn().
			Err(err).
			Str("path", path).
			Msgf("voice: %s model stat failed", label)
	}
}
