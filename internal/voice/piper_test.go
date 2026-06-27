package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeWAV builds a minimal 16-bit PCM RIFF/WAVE blob with the given
// sample rate and duration. Used by the table tests below to drive
// parseWAV + the runCmd fake stdout. Kept as a helper rather than a
// testdata fixture so the test stays hermetic — no chmod, no shell.
func makeWAV(sampleRate, durationSamples int) []byte {
	bytesPerSample := 2 // 16-bit PCM, mono
	dataBytes := durationSamples * bytesPerSample
	out := make([]byte, 0, 44+dataBytes)
	out = append(out, []byte("RIFF")...)
	out = appendU32LE(out, uint32(dataBytes+36))
	out = append(out, []byte("WAVE")...)
	out = append(out, []byte("fmt ")...)
	out = appendU32LE(out, 16)                   // PCM chunk size
	out = appendU16LE(out, 1)                    // audioFormat=1 (PCM)
	out = appendU16LE(out, 1)                    // channels
	out = appendU32LE(out, uint32(sampleRate))   // sample rate
	out = appendU32LE(out, uint32(sampleRate*2)) // byte rate
	out = appendU16LE(out, 2)                    // block align
	out = appendU16LE(out, 16)                   // bits per sample
	out = append(out, []byte("data")...)
	out = appendU32LE(out, uint32(dataBytes))
	out = append(out, make([]byte, dataBytes)...)
	return out
}

func appendU32LE(b []byte, v uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	return append(b, buf[:]...)
}

func appendU16LE(b []byte, v uint16) []byte {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	return append(b, buf[:]...)
}

// stubbedPiper builds a piperLocalTTS whose runCmd records every call
// and returns canned stdout/stderr/err per call index. The caller
// uses returned `calls` to assert on argv / stdin.
type recordedCall struct {
	bin   string
	args  []string
	stdin []byte
}

func stubbedPiper(t *testing.T, cfg PiperConfig, responses []stubResp) (*piperLocalTTS, *[]recordedCall) {
	t.Helper()
	prov, err := NewPiperLocalTTS(cfg)
	if err != nil {
		t.Fatalf("NewPiperLocalTTS: %v", err)
	}
	p := prov.(*piperLocalTTS)
	calls := []recordedCall{}
	idx := 0
	p.runCmd = func(_ context.Context, name string, args []string, stdin []byte) ([]byte, []byte, error) {
		calls = append(calls, recordedCall{bin: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
		if idx >= len(responses) {
			return nil, nil, fmt.Errorf("test bug: runCmd called %d time(s), only %d response(s) queued", idx+1, len(responses))
		}
		r := responses[idx]
		idx++
		return r.stdout, r.stderr, r.err
	}
	return p, &calls
}

type stubResp struct {
	stdout []byte
	stderr []byte
	err    error
}

func TestPiperLocalTTS_New_RequiresModelPath(t *testing.T) {
	if _, err := NewPiperLocalTTS(PiperConfig{}); err == nil {
		t.Fatal("expected error when ModelPath is empty; got nil")
	}
}

func TestPiperLocalTTS_New_AppliesDefaults(t *testing.T) {
	prov, err := NewPiperLocalTTS(PiperConfig{ModelPath: "/tmp/voice.onnx"})
	if err != nil {
		t.Fatalf("NewPiperLocalTTS: %v", err)
	}
	p := prov.(*piperLocalTTS)
	if p.cfg.DefaultVoice != defaultPiperVoice {
		t.Errorf("DefaultVoice = %q, want %q", p.cfg.DefaultVoice, defaultPiperVoice)
	}
	if p.cfg.DefaultSpeed != defaultPiperSpeed {
		t.Errorf("DefaultSpeed = %v, want %v", p.cfg.DefaultSpeed, defaultPiperSpeed)
	}
	if p.cfg.MaxTextRunes != defaultPiperMaxRunes {
		t.Errorf("MaxTextRunes = %d, want %d", p.cfg.MaxTextRunes, defaultPiperMaxRunes)
	}
}

func TestPiperLocalTTS_Synthesize_EmptyText(t *testing.T) {
	p, _ := stubbedPiper(t, PiperConfig{ModelPath: "/m.onnx", BinaryPath: "/usr/bin/piper"}, nil)
	for _, in := range []string{"", "   ", "\t\n"} {
		_, err := p.Synthesize(context.Background(), in, TTSOptions{})
		if !errors.Is(err, ErrEmptyText) {
			t.Errorf("Synthesize(%q) err = %v, want ErrEmptyText", in, err)
		}
	}
}

func TestPiperLocalTTS_Synthesize_OversizeText(t *testing.T) {
	cfg := PiperConfig{ModelPath: "/m.onnx", BinaryPath: "/usr/bin/piper", MaxTextRunes: 10}
	p, _ := stubbedPiper(t, cfg, nil)
	_, err := p.Synthesize(context.Background(), strings.Repeat("a", 100), TTSOptions{})
	if !errors.Is(err, ErrOversizeText) {
		t.Errorf("err = %v, want ErrOversizeText", err)
	}
}

func TestPiperLocalTTS_Synthesize_MissingBinary(t *testing.T) {
	// Empty BinaryPath + a name that's vanishingly unlikely to be on
	// $PATH forces the exec.LookPath fallback to fail. We swap the
	// piper binary name by abusing the public API path: NewPiperLocalTTS
	// always uses "piper", so we set BinaryPath to a non-existent
	// absolute path and assert the runCmd never reaches a real exec
	// (the lookup branch only triggers when BinaryPath is empty).
	// Instead, exercise the empty-BinaryPath path by temporarily
	// emptying $PATH.
	t.Setenv("PATH", "")
	p, err := NewPiperLocalTTS(PiperConfig{ModelPath: "/m.onnx"})
	if err != nil {
		t.Fatalf("NewPiperLocalTTS: %v", err)
	}
	_, gotErr := p.Synthesize(context.Background(), "hi", TTSOptions{})
	if !errors.Is(gotErr, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", gotErr)
	}
}

func TestPiperLocalTTS_Synthesize_SubprocessNonZero(t *testing.T) {
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/usr/bin/piper",
	}, []stubResp{
		{stderr: []byte("Error: model not found\n"), err: errors.New("exit status 1")},
	})
	_, err := p.Synthesize(context.Background(), "hello", TTSOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "piper exec failed") {
		t.Errorf("err = %v, want it to mention 'piper exec failed'", err)
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("err = %v, want it to surface stderr 'model not found'", err)
	}
}

func TestPiperLocalTTS_Synthesize_EmptyOutput(t *testing.T) {
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/usr/bin/piper",
	}, []stubResp{{}}) // empty stdout, no error
	_, err := p.Synthesize(context.Background(), "hello", TTSOptions{})
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Errorf("err = %v, want empty-output message", err)
	}
}

func TestPiperLocalTTS_Synthesize_NotAWAV(t *testing.T) {
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/usr/bin/piper",
	}, []stubResp{{stdout: []byte("NOT-A-WAV-EVER")}})
	_, err := p.Synthesize(context.Background(), "hello", TTSOptions{})
	if err == nil || !strings.Contains(err.Error(), "WAV") {
		t.Errorf("err = %v, want parse-WAV message", err)
	}
}

func TestPiperLocalTTS_Synthesize_WAVOutput(t *testing.T) {
	wav := makeWAV(22050, 22050) // 1 second
	p, calls := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/usr/bin/piper",
	}, []stubResp{{stdout: wav}})

	audio, err := p.Synthesize(context.Background(), "hello world", TTSOptions{Format: "wav", Speed: 1.0})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if audio.MimeType != "audio/wav" {
		t.Errorf("MimeType = %q, want audio/wav", audio.MimeType)
	}
	if audio.SampleRateHz != 22050 {
		t.Errorf("SampleRateHz = %d, want 22050", audio.SampleRateHz)
	}
	if audio.DurationMs < 900 || audio.DurationMs > 1100 {
		t.Errorf("DurationMs = %d, want ~1000", audio.DurationMs)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.bin != "/usr/bin/piper" {
		t.Errorf("bin = %q, want /usr/bin/piper", got.bin)
	}
	if !sliceContains(got.args, "--model", "/m.onnx") {
		t.Errorf("args missing --model /m.onnx: %v", got.args)
	}
	if !sliceContains(got.args, "--length-scale", "1.0000") {
		t.Errorf("args missing --length-scale 1.0000 (speed=1.0): %v", got.args)
	}
	if string(got.stdin) != "hello world" {
		t.Errorf("stdin = %q, want %q", string(got.stdin), "hello world")
	}
}

func TestPiperLocalTTS_Synthesize_SpeedPlumbing(t *testing.T) {
	wav := makeWAV(22050, 22050)
	cases := []struct {
		name        string
		speed       float64
		wantScale   string
		wantPresent bool
	}{
		{"natural", 1.0, "1.0000", true},
		{"fast", 2.0, "0.5000", true},
		{"slow", 0.5, "2.0000", true},
		{"zero-falls-back-to-default", 0.0, "1.0000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, calls := stubbedPiper(t, PiperConfig{
				ModelPath:  "/m.onnx",
				BinaryPath: "/p",
			}, []stubResp{{stdout: wav}})
			_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "wav", Speed: tc.speed})
			if err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			if !sliceContains((*calls)[0].args, "--length-scale", tc.wantScale) {
				t.Errorf("args missing --length-scale %s: %v", tc.wantScale, (*calls)[0].args)
			}
		})
	}
}

func TestPiperLocalTTS_Synthesize_OggOpus(t *testing.T) {
	wav := makeWAV(22050, 22050)
	oggBytes := []byte("OggS\x00\x02fake-ogg-payload")
	p, calls := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{
		{stdout: wav},      // piper call
		{stdout: oggBytes}, // ffmpeg call
	})
	audio, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "ogg-opus"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if audio.MimeType != "audio/ogg" {
		t.Errorf("MimeType = %q, want audio/ogg", audio.MimeType)
	}
	if string(audio.Bytes) != string(oggBytes) {
		t.Errorf("ogg bytes not propagated: got %q", audio.Bytes)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2 (piper + ffmpeg)", len(*calls))
	}
	if (*calls)[1].bin != "/ffmpeg" {
		t.Errorf("second call bin = %q, want /ffmpeg", (*calls)[1].bin)
	}
	if !sliceContains((*calls)[1].args, "libopus") {
		t.Errorf("ffmpeg args missing libopus: %v", (*calls)[1].args)
	}
	// stdin of ffmpeg call should be the WAV bytes verbatim
	if len((*calls)[1].stdin) != len(wav) {
		t.Errorf("ffmpeg stdin len = %d, want WAV len %d", len((*calls)[1].stdin), len(wav))
	}
}

func TestPiperLocalTTS_Synthesize_Mp4Aac(t *testing.T) {
	wav := makeWAV(22050, 22050)
	mp4Bytes := []byte("\x00\x00\x00\x20ftypiso5fake")
	p, calls := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{
		{stdout: wav},
		{stdout: mp4Bytes},
	})
	audio, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "mp4-aac"})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if audio.MimeType != "audio/mp4" {
		t.Errorf("MimeType = %q, want audio/mp4", audio.MimeType)
	}
	if !sliceContains((*calls)[1].args, "aac") {
		t.Errorf("ffmpeg args missing aac: %v", (*calls)[1].args)
	}
}

func TestPiperLocalTTS_Synthesize_UnknownFormat(t *testing.T) {
	wav := makeWAV(22050, 22050)
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
	}, []stubResp{{stdout: wav}})
	_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "flac"})
	if err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("err = %v, want unsupported-format error", err)
	}
}

func TestPiperLocalTTS_Synthesize_FfmpegMissing(t *testing.T) {
	wav := makeWAV(22050, 22050)
	t.Setenv("PATH", "")
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
		// FFmpegPath empty + empty PATH → exec.LookPath fails
	}, []stubResp{{stdout: wav}})
	_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "ogg-opus"})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Errorf("err = %v, want ErrProviderUnavailable", err)
	}
}

func TestPiperLocalTTS_Synthesize_FfmpegFails(t *testing.T) {
	wav := makeWAV(22050, 22050)
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{
		{stdout: wav},
		{stderr: []byte("Encoder libopus not found"), err: errors.New("exit status 1")},
	})
	_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "ogg-opus"})
	if err == nil || !strings.Contains(err.Error(), "ffmpeg transcode failed") {
		t.Errorf("err = %v, want ffmpeg transcode error", err)
	}
}

func TestPiperLocalTTS_Synthesize_FfmpegEmptyOutput(t *testing.T) {
	wav := makeWAV(22050, 22050)
	p, _ := stubbedPiper(t, PiperConfig{
		ModelPath:  "/m.onnx",
		BinaryPath: "/p",
		FFmpegPath: "/ffmpeg",
	}, []stubResp{
		{stdout: wav},
		{}, // empty stdout, no error
	})
	_, err := p.Synthesize(context.Background(), "hi", TTSOptions{Format: "ogg-opus"})
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Errorf("err = %v, want empty-output error", err)
	}
}

func TestParseWAV_TooShort(t *testing.T) {
	if _, _, err := parseWAV([]byte("RIFF")); err == nil {
		t.Errorf("expected error on truncated header")
	}
}

func TestParseWAV_BadMagic(t *testing.T) {
	in := make([]byte, 44)
	copy(in, []byte("NOTAWAVE0000WAVE"))
	if _, _, err := parseWAV(in); err == nil {
		t.Errorf("expected error on bad magic")
	}
}

func TestParseWAV_OK(t *testing.T) {
	in := makeWAV(48000, 24000) // 0.5 s @ 48 kHz
	sr, dur, err := parseWAV(in)
	if err != nil {
		t.Fatalf("parseWAV: %v", err)
	}
	if sr != 48000 {
		t.Errorf("sampleRate = %d, want 48000", sr)
	}
	if dur < 480 || dur > 520 {
		t.Errorf("durationMs = %d, want ~500", dur)
	}
}

// TestRunRealCmd_RoundTrip exercises the production runRealCmd via
// /bin/sh -c so we don't take a hard dep on the actual piper binary
// in CI but still cover the exec.CommandContext path (stdin pipe,
// stdout capture, non-zero exit). Skipped on non-Unix hosts where
// /bin/sh isn't present.
func TestRunRealCmd_RoundTrip(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	stdout, stderr, err := runRealCmd(context.Background(), "/bin/sh", []string{"-c", "cat"}, []byte("hello"))
	if err != nil {
		t.Fatalf("runRealCmd: %v (stderr=%s)", err, stderr)
	}
	if string(stdout) != "hello" {
		t.Errorf("stdout = %q, want %q", stdout, "hello")
	}
}

func TestRunRealCmd_NonZeroExit(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh available")
	}
	_, stderr, err := runRealCmd(context.Background(), "/bin/sh", []string{"-c", "echo boom >&2; exit 7"}, nil)
	if err == nil {
		t.Fatalf("expected error on exit 7")
	}
	if !strings.Contains(string(stderr), "boom") {
		t.Errorf("stderr = %q, want 'boom'", stderr)
	}
}

func TestRunRealCmd_BinaryNotFound(t *testing.T) {
	// Use an absolute path that can't exist so we don't depend on
	// PATH ordering across CI environments. tmpdir + a clearly-fake
	// name avoids any cleanup question.
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "definitely-not-a-binary")
	_, _, err := runRealCmd(context.Background(), missing, nil, nil)
	if err == nil {
		t.Fatal("expected error when binary missing")
	}
}

func TestRunesIn(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hi", 2},
		{"héllo", 5},
		{"日本語", 3},
	}
	for _, tc := range cases {
		if got := runesIn(tc.in); got != tc.want {
			t.Errorf("runesIn(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTrimSpaces(t *testing.T) {
	if got := trimSpaces("  hello   world  \n\t"); got != "hello world" {
		t.Errorf("trimSpaces = %q, want %q", got, "hello world")
	}
}

// sliceContains reports whether `args` contains `flag` immediately
// followed by `value`. Used to assert on subprocess argv shape.
func sliceContains(args []string, pair ...string) bool {
	if len(pair) == 0 {
		return false
	}
	if len(pair) == 1 {
		for _, a := range args {
			if a == pair[0] {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args)-len(pair)+1; i++ {
		ok := true
		for j, p := range pair {
			if args[i+j] != p {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// keep io referenced — parseWAV's io use may evolve.
var _ = io.Discard
