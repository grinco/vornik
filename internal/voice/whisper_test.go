package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubbedWhisper builds a whisperLocalSTT with controllable runCmd
// and tempFileWriter seams. The returned `calls` slice records every
// subprocess invocation in order; the returned `lastWAVPath` mutates
// each time the writer is called.
func stubbedWhisper(t *testing.T, cfg WhisperConfig, responses []stubResp) (*whisperLocalSTT, *[]recordedCall, *string) {
	t.Helper()
	prov, err := NewWhisperLocalSTT(cfg)
	if err != nil {
		t.Fatalf("NewWhisperLocalSTT: %v", err)
	}
	w := prov.(*whisperLocalSTT)
	calls := []recordedCall{}
	lastPath := ""
	idx := 0
	w.runCmd = func(_ context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		calls = append(calls, recordedCall{bin: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
		if idx >= len(responses) {
			return nil, nil, fmt.Errorf("test bug: runCmd called %d time(s), only %d response(s) queued", idx+1, len(responses))
		}
		r := responses[idx]
		idx++
		return r.stdout, r.stderr, r.err
	}
	w.tempFileWriter = func(dir, prefix string, contents []byte) (string, func(), error) {
		// Use Go's os.CreateTemp to keep the on-disk semantics
		// realistic but record the path so the test can drop the
		// expected .json sidecar.
		f, err := os.CreateTemp(dir, prefix)
		if err != nil {
			return "", func() {}, err
		}
		if _, err := f.Write(contents); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return "", func() {}, err
		}
		_ = f.Close()
		lastPath = f.Name()
		return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
	}
	return w, &calls, &lastPath
}

func TestWhisperLocalSTT_New_RequiresModelPath(t *testing.T) {
	if _, err := NewWhisperLocalSTT(WhisperConfig{}); err == nil {
		t.Fatal("expected error when ModelPath is empty")
	}
}

func TestWhisperLocalSTT_Transcribe_NilReader(t *testing.T) {
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, nil)
	if _, err := w.Transcribe(context.Background(), nil, Hint{}); err == nil {
		t.Fatal("expected error on nil audio")
	}
}

func TestWhisperLocalSTT_Transcribe_EmptyAudio(t *testing.T) {
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, nil)
	_, err := w.Transcribe(context.Background(), bytes.NewReader(nil), Hint{})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want empty-audio message", err)
	}
}

func TestWhisperLocalSTT_Transcribe_MissingFFmpeg(t *testing.T) {
	t.Setenv("PATH", "")
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		// FFmpegPath empty + PATH empty → LookPath fails
	}, nil)
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("OggS")), Hint{})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestWhisperLocalSTT_Transcribe_FFmpegFails(t *testing.T) {
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{
		{stderr: []byte("Invalid data found when processing input"), err: errors.New("exit status 1")},
	})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("garbage")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "ffmpeg normalise failed") {
		t.Errorf("err = %v, want ffmpeg-normalise error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_FFmpegEmpty(t *testing.T) {
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{{}}) // empty stdout, no error
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("OggS\x00")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "empty WAV") {
		t.Errorf("err = %v, want empty-WAV error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_HappyPath(t *testing.T) {
	wav := makeWAV(16000, 16000)
	tmp := t.TempDir()
	w, calls, lastPath := stubbedWhisper(t, WhisperConfig{
		ModelPath:    "/models/ggml-base.bin",
		BinaryPath:   "/usr/local/bin/whisper-cpp",
		FFmpegPath:   "/usr/bin/ffmpeg",
		TempDir:      tmp,
		LanguageHint: "en",
		Threads:      4,
	}, []stubResp{
		{stdout: wav}, // ffmpeg
		{},            // whisper.cpp writes JSON sidecar; stdout unused
	})

	// Run Transcribe in a goroutine because we have to drop the
	// JSON sidecar AFTER tempFileWriter is called by the first
	// subprocess step but BEFORE the JSON read. Simpler: we know
	// runCmd is called twice (ffmpeg, whisper); after the second
	// stub responds, the code reads the sidecar. We pre-create the
	// expected JSON by hooking the SECOND runCmd response.
	jsonBody := `{
		"result": {"language": "en"},
		"params": {"language": "en"},
		"transcription": [
			{"text": " Hello, world.", "offsets": {"from": 0, "to": 1200},
			 "no_speech_prob": 0.05}
		]
	}`
	// Replace the second response's runCmd so it writes the sidecar
	// before returning.
	w.runCmd = func(_ context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		*calls = append(*calls, recordedCall{bin: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
		switch len(*calls) {
		case 1:
			return wav, nil, nil
		case 2:
			if *lastPath == "" {
				return nil, nil, errors.New("test bug: tempFileWriter never called")
			}
			if err := os.WriteFile(*lastPath+".json", []byte(jsonBody), 0o644); err != nil {
				return nil, nil, fmt.Errorf("test bug: write sidecar: %w", err)
			}
			return nil, nil, nil
		}
		return nil, nil, errors.New("test bug: third runCmd call")
	}

	tr, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("OggS-mock-payload")), Hint{LanguageHint: "en-US", MimeType: "audio/ogg"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "Hello, world." {
		t.Errorf("Text = %q, want %q", tr.Text, "Hello, world.")
	}
	if tr.Language != "en" {
		t.Errorf("Language = %q, want en", tr.Language)
	}
	if tr.DurationMs != 1200 {
		t.Errorf("DurationMs = %d, want 1200", tr.DurationMs)
	}
	if tr.Confidence < 0.94 || tr.Confidence > 0.96 {
		t.Errorf("Confidence = %v, want ~0.95", tr.Confidence)
	}

	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(*calls))
	}
	// ffmpeg call shape
	if (*calls)[0].bin != "/usr/bin/ffmpeg" {
		t.Errorf("ffmpeg call bin = %q", (*calls)[0].bin)
	}
	if !sliceContains((*calls)[0].args, "-ar", "16000") {
		t.Errorf("ffmpeg args missing -ar 16000: %v", (*calls)[0].args)
	}
	if !sliceContains((*calls)[0].args, "-ac", "1") {
		t.Errorf("ffmpeg args missing -ac 1: %v", (*calls)[0].args)
	}
	if string((*calls)[0].stdin[:4]) != "OggS" {
		t.Errorf("ffmpeg stdin doesn't start with the inbound audio bytes: %q", (*calls)[0].stdin[:4])
	}
	// whisper.cpp call shape
	if (*calls)[1].bin != "/usr/local/bin/whisper-cpp" {
		t.Errorf("whisper call bin = %q", (*calls)[1].bin)
	}
	if !sliceContains((*calls)[1].args, "--model", "/models/ggml-base.bin") {
		t.Errorf("whisper args missing --model: %v", (*calls)[1].args)
	}
	if !sliceContains((*calls)[1].args, "--language", "en") {
		t.Errorf("whisper args missing --language en (Hint.LanguageHint=en-US should map to en): %v", (*calls)[1].args)
	}
	if !sliceContains((*calls)[1].args, "--threads", "4") {
		t.Errorf("whisper args missing --threads 4: %v", (*calls)[1].args)
	}
	if !sliceContains((*calls)[1].args, "--output-json") {
		t.Errorf("whisper args missing --output-json: %v", (*calls)[1].args)
	}
}

func TestWhisperLocalSTT_Transcribe_WhisperBinaryMissing(t *testing.T) {
	t.Setenv("PATH", "")
	wav := makeWAV(16000, 16000)
	w, calls, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		FFmpegPath: "/ffmpeg",
		// BinaryPath empty + PATH empty → lookupWhisperBinary fails
	}, []stubResp{{stdout: wav}})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("Oggs")), Hint{})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(*calls) != 1 {
		t.Errorf("expected 1 call (ffmpeg), got %d", len(*calls))
	}
}

func TestWhisperLocalSTT_Transcribe_WhisperFails(t *testing.T) {
	wav := makeWAV(16000, 16000)
	tmp := t.TempDir()
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
		TempDir:    tmp,
	}, []stubResp{
		{stdout: wav},
		{stderr: []byte("model load failed"), err: errors.New("exit status 2")},
	})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("Oggs")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "whisper.cpp failed") {
		t.Errorf("err = %v, want whisper.cpp-failed error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_MissingJSONSidecar(t *testing.T) {
	wav := makeWAV(16000, 16000)
	tmp := t.TempDir()
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
		TempDir:    tmp,
	}, []stubResp{
		{stdout: wav},
		{}, // whisper exits 0 but never writes the sidecar
	})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("Oggs")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "read whisper JSON") {
		t.Errorf("err = %v, want read-JSON error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_MalformedJSON(t *testing.T) {
	wav := makeWAV(16000, 16000)
	tmp := t.TempDir()
	w, calls, lastPath := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
		TempDir:    tmp,
	}, nil)
	w.runCmd = func(_ context.Context, name string, _ []string, _ []byte) ([]byte, []byte, error) {
		*calls = append(*calls, recordedCall{bin: name})
		switch len(*calls) {
		case 1:
			return wav, nil, nil
		case 2:
			if err := os.WriteFile(*lastPath+".json", []byte("not-json"), 0o644); err != nil {
				return nil, nil, err
			}
			return nil, nil, nil
		}
		return nil, nil, errors.New("unexpected call")
	}
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("Oggs")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "whisper JSON parse") {
		t.Errorf("err = %v, want JSON-parse error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_TempFileWriterFails(t *testing.T) {
	wav := makeWAV(16000, 16000)
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{{stdout: wav}})
	w.tempFileWriter = func(dir, prefix string, contents []byte) (string, func(), error) {
		return "", func() {}, errors.New("disk full")
	}
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("Oggs")), Hint{})
	if err == nil || !strings.Contains(err.Error(), "write temp WAV") {
		t.Errorf("err = %v, want temp-WAV error", err)
	}
}

func TestWhisperLocalSTT_Transcribe_OversizeAudio(t *testing.T) {
	w, _, _ := stubbedWhisper(t, WhisperConfig{
		ModelPath:  "/m.bin",
		BinaryPath: "/whisper",
		FFmpegPath: "/ffmpeg",
	}, nil)
	huge := make([]byte, 64*1024*1024+10)
	_, err := w.Transcribe(context.Background(), bytes.NewReader(huge), Hint{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want oversize-audio error", err)
	}
}

func TestParseWhisperJSON_MultiSegment(t *testing.T) {
	body := `{
		"result": {"language": "de"},
		"params": {"language": ""},
		"transcription": [
			{"text": " Guten ", "offsets": {"from": 0, "to": 500}, "no_speech_prob": 0.1},
			{"text": "Tag.", "offsets": {"from": 500, "to": 1500}, "no_speech_prob": 0.2}
		]
	}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Text != "Guten Tag." {
		t.Errorf("Text = %q, want %q", tr.Text, "Guten Tag.")
	}
	if tr.Language != "de" {
		t.Errorf("Language = %q, want de", tr.Language)
	}
	if tr.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", tr.DurationMs)
	}
	// confidence avg = (0.9 + 0.8) / 2 = 0.85
	if tr.Confidence < 0.84 || tr.Confidence > 0.86 {
		t.Errorf("Confidence = %v, want ~0.85", tr.Confidence)
	}
}

func TestParseWhisperJSON_ConfidenceExplicit(t *testing.T) {
	body := `{
		"result": {"language": "en"},
		"transcription": [
			{"text": "Hi", "offsets": {"from": 0, "to": 300}, "confidence": 0.42}
		]
	}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Confidence < 0.41 || tr.Confidence > 0.43 {
		t.Errorf("Confidence = %v, want ~0.42", tr.Confidence)
	}
}

func TestParseWhisperJSON_NoConfidenceReturnsZero(t *testing.T) {
	body := `{"transcription":[{"text":"Hi","offsets":{"from":0,"to":100}}]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Confidence != 0 {
		t.Errorf("Confidence = %v, want 0 (no signal)", tr.Confidence)
	}
}

func TestParseWhisperJSON_EmptyTranscription(t *testing.T) {
	body := `{"result":{"language":"en"},"transcription":[]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Text != "" {
		t.Errorf("Text = %q, want empty (silent audio)", tr.Text)
	}
	if tr.Language != "en" {
		t.Errorf("Language = %q, want en (from result)", tr.Language)
	}
}

func TestParseWhisperJSON_FallbackToParamsLanguage(t *testing.T) {
	body := `{"params":{"language":"fr"},"transcription":[{"text":"Bonjour","offsets":{"from":0,"to":600}}]}`
	tr, err := parseWhisperJSON([]byte(body))
	if err != nil {
		t.Fatalf("parseWhisperJSON: %v", err)
	}
	if tr.Language != "fr" {
		t.Errorf("Language = %q, want fr (from params fallback)", tr.Language)
	}
}

func TestParseWhisperJSON_BadJSON(t *testing.T) {
	if _, err := parseWhisperJSON([]byte("not-json")); err == nil {
		t.Error("expected error on bad JSON")
	}
}

func TestLookupWhisperBinary_BothMissing(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := lookupWhisperBinary(); err == nil {
		t.Error("expected error when neither candidate on PATH")
	}
}

func TestLookupWhisperBinary_Found(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "main")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("PATH", tmp)
	got, err := lookupWhisperBinary()
	if err != nil {
		t.Fatalf("lookupWhisperBinary: %v", err)
	}
	if filepath.Base(got) != "main" {
		t.Errorf("found %q, want path ending in 'main'", got)
	}
}

// TestLookupWhisperBinary_FindsWhisperCli covers the 2024+ upstream
// rename that homebrew and recent source builds ship as "whisper-cli"
// (instead of "main" or the distro-renamed "whisper-cpp").
func TestLookupWhisperBinary_FindsWhisperCli(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "whisper-cli")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	t.Setenv("PATH", tmp)
	got, err := lookupWhisperBinary()
	if err != nil {
		t.Fatalf("lookupWhisperBinary: %v", err)
	}
	if filepath.Base(got) != "whisper-cli" {
		t.Errorf("found %q, want path ending in 'whisper-cli'", got)
	}
}

// TestLookupWhisperBinary_PrefersDistroName — when both whisper-cpp
// and whisper-cli exist, the distro-rename takes precedence so a
// system-managed install wins over a homebrew side-channel.
func TestLookupWhisperBinary_PrefersDistroName(t *testing.T) {
	tmp := t.TempDir()
	for _, n := range []string{"whisper-cpp", "whisper-cli", "main"} {
		if err := os.WriteFile(filepath.Join(tmp, n), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	t.Setenv("PATH", tmp)
	got, err := lookupWhisperBinary()
	if err != nil {
		t.Fatalf("lookupWhisperBinary: %v", err)
	}
	if filepath.Base(got) != "whisper-cpp" {
		t.Errorf("found %q, want whisper-cpp to win", got)
	}
}

func TestWriteTempFile_WriteAndCleanup(t *testing.T) {
	tmp := t.TempDir()
	path, cleanup, err := writeTempFile(tmp, "vt-*.wav", []byte("hi"))
	if err != nil {
		t.Fatalf("writeTempFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup didn't remove %s: stat err = %v", path, err)
	}
}

func TestWriteTempFile_BadDir(t *testing.T) {
	_, _, err := writeTempFile("/nonexistent/vornik-test-dir-zzz", "vt-*.wav", []byte("hi"))
	if err == nil {
		t.Error("expected error on bad dir")
	}
}

// Keep io referenced — Transcribe's io.LimitReader may evolve.
var _ = io.EOF
