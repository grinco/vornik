package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReasoningBlock(t *testing.T) {
	in := "before <reasoning>private\r\n</reasoning> and after done"
	got := strings.TrimSpace(reasoningBlock.ReplaceAllString(in, ""))
	want := "before and after done"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func captureStdout(t *testing.T, fn func() error) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	if err := fn(); err != nil {
		_ = w.Close()
		t.Fatalf("buildUserContent: %v", err)
	}
	_ = w.Close()
	return <-done
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}

func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	fn()
}

func TestBuildUserContent_TextOnly(t *testing.T) {
	out := captureStdout(t, func() error {
		return buildUserContent([]string{"--text", "describe the plan"})
	})
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("expected JSON string, got %s: %v", out, err)
	}
	if s != "describe the plan" {
		t.Fatalf("got %q want %q", s, "describe the plan")
	}
}

func TestBuildUserContent_TextFromStdin(t *testing.T) {
	var out []byte
	withStdin(t, "prompt from stdin", func() {
		out = captureStdout(t, func() error {
			return buildUserContent([]string{"--text-file", "-"})
		})
	})

	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("expected JSON string, got %s: %v", out, err)
	}
	if s != "prompt from stdin" {
		t.Fatalf("got %q want %q", s, "prompt from stdin")
	}
}

func TestBuildUserContent_WithImage(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(imgPath, []byte{0xff, 0xd8, 0xff, 0xe0}, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() error {
		return buildUserContent([]string{"--text", "what is this", "--image", imgPath})
	})

	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal(out, &blocks); err != nil {
		t.Fatalf("expected JSON array, got %s: %v", out, err)
	}
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d: %s", len(blocks), out)
	}
	if blocks[0].Type != "text" || blocks[0].Text != "what is this" {
		t.Fatalf("text block: %+v", blocks[0])
	}
	if blocks[1].Type != "image_url" || blocks[1].ImageURL == nil {
		t.Fatalf("image block: %+v", blocks[1])
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(blocks[1].ImageURL.URL, prefix) {
		t.Fatalf("data URL prefix wrong: %s", blocks[1].ImageURL.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blocks[1].ImageURL.URL, prefix))
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !bytes.Equal(decoded, []byte{0xff, 0xd8, 0xff, 0xe0}) {
		t.Fatalf("payload bytes mismatch: %x", decoded)
	}
}

func TestBuildUserContent_InvalidImagesOnlyFallsBackToEmptyString(t *testing.T) {
	stderr := captureStderr(t, func() {
		out := captureStdout(t, func() error {
			return buildUserContent([]string{"--image", "/does/not/exist.jpg", "--image", "bad.bmp"})
		})
		var s string
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("expected text fallback, got %s: %v", out, err)
		}
		if s != "" {
			t.Fatalf("got %q want empty string fallback", s)
		}
	})
	if !strings.Contains(stderr, "skipping /does/not/exist.jpg") {
		t.Fatalf("missing missing-file warning: %q", stderr)
	}
	if !strings.Contains(stderr, "skipping bad.bmp") {
		t.Fatalf("missing unsupported-extension warning: %q", stderr)
	}
}

func TestBuildUserContent_TextWithOnlyInvalidImagesFallsBackToTextString(t *testing.T) {
	stderr := captureStderr(t, func() {
		out := captureStdout(t, func() error {
			return buildUserContent([]string{"--text", "keep this prompt", "--image", "bad.bmp", "--image", "/missing.png"})
		})
		var s string
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("expected JSON string fallback, got %s: %v", out, err)
		}
		if s != "keep this prompt" {
			t.Fatalf("got %q, want %q", s, "keep this prompt")
		}
	})
	if !strings.Contains(stderr, "skipping bad.bmp") {
		t.Fatalf("missing unsupported-extension warning: %q", stderr)
	}
	if !strings.Contains(stderr, "skipping /missing.png") {
		t.Fatalf("missing missing-file warning: %q", stderr)
	}
}

func TestBuildUserContent_TextFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(p, []byte("from file"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error {
		return buildUserContent([]string{"--text-file", p})
	})
	var s string
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s != "from file" {
		t.Fatalf("got %q", s)
	}
}

func TestMimeFromExt(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr string
	}{
		{name: "jpeg", path: "test.jpg", want: "image/jpeg"},
		{name: "jpeg uppercase ext", path: "test.JPEG", want: "image/jpeg"},
		{name: "png", path: "test.png", want: "image/png"},
		{name: "gif", path: "test.gif", want: "image/gif"},
		{name: "webp", path: "test.webp", want: "image/webp"},
		{name: "unsupported", path: "test.bmp", wantErr: "unsupported image extension"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := mimeFromExt(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func runHelperMain(t *testing.T, args ...string) (string, string, int) {
	return runHelperMainWithInput(t, "", args...)
}

func runHelperMainWithInput(t *testing.T, input string, args ...string) (string, string, int) {
	t.Helper()
	cmdArgs := append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run helper main: %v", err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestMainNowSeconds(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "now-seconds")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	got := strings.TrimSpace(stdout)
	ts, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("stdout %q is not unix seconds: %v", got, err)
	}
	now := time.Now().Unix()
	if ts < now-5 || ts > now+5 {
		t.Fatalf("timestamp %d not near now %d", ts, now)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestMainUsageWhenMissingCommand(t *testing.T) {
	stdout, stderr, code := runHelperMain(t)
	if code != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "usage: vornik-agent-helper") {
		t.Fatalf("missing usage in stderr: %q", stderr)
	}
}

func TestMainUnknownCommandShowsUsage(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "unknown-cmd")
	if code != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "usage: vornik-agent-helper") {
		t.Fatalf("missing usage in stderr: %q", stderr)
	}
}

func TestMainDurationSecondsRequiresStartArg(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "duration-seconds")
	if code != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "duration-seconds requires start unix seconds") {
		t.Fatalf("missing validation message in stderr: %q", stderr)
	}
}

func TestMainDurationSecondsInvalidStartArg(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "duration-seconds", "not-a-number")
	if code != 2 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "invalid syntax") {
		t.Fatalf("missing parse error in stderr: %q", stderr)
	}
}

func TestMainBuildUserContentInvalidFlagExitsOne(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "build-user-content", "--does-not-exist")
	if code != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("missing flag parse error in stderr: %q", stderr)
	}
}

func TestStringList(t *testing.T) {
	var s stringList
	if err := s.Set("one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("two"); err != nil {
		t.Fatal(err)
	}
	if got := s.String(); got != "one,two" {
		t.Fatalf("got %q, want %q", got, "one,two")
	}
}

func TestBuildUserContent_MixedWithInvalidImages(t *testing.T) {
	// Exported function: buildUserContent() handling mixed valid/invalid images.
	// Should emit valid content with stderr warnings for invalid images.
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(imgPath, []byte{0xff, 0xd8, 0xff, 0xe0}, 0o644); err != nil {
		t.Fatal(err)
	}

	stderr := captureStderr(t, func() {
		out := captureStdout(t, func() error {
			return buildUserContent([]string{"--text", "vision prompt", "--image", imgPath, "--image", "/nonexistent.bmp", "--image", "missing.gif"})
		})
		var blocks []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url,omitempty"`
		}
		if err := json.Unmarshal(out, &blocks); err != nil {
			t.Fatalf("expected JSON array, got %s: %v", out, err)
		}
		// Expect text + image blocks despite invalid images being logged
		if len(blocks) != 2 {
			t.Fatalf("expected 2 blocks (text + valid image), got %d: %s", len(blocks), out)
		}
		if blocks[0].Type != "text" || blocks[0].Text != "vision prompt" {
			t.Fatalf("text block %+v", blocks[0])
		}
		if blocks[1].Type != "image_url" {
			t.Fatalf("expected image_url block, got %+v", blocks[1])
		}
	})
	// Verify stderr reports the invalid images
	if !strings.Contains(stderr, "skipping /nonexistent.bmp") {
		t.Fatalf("missing missing-file warning: %q", stderr)
	}
	if !strings.Contains(stderr, "skipping missing.gif") {
		t.Fatalf("missing unsupported-extension warning: %q", stderr)
	}
}

func TestBuildUserContent_NoTextNoValidImagesFallsBackToEmptyString(t *testing.T) {
	stderr := captureStderr(t, func() {
		out := captureStdout(t, func() error {
			return buildUserContent([]string{"--image", "/does/not/exist.png", "--image", "bad.tiff"})
		})
		var s string
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("expected JSON string, got %s: %v", out, err)
		}
		if s != "" {
			t.Fatalf("got %q want empty string fallback", s)
		}
	})
	if !strings.Contains(stderr, "skipping /does/not/exist.png") {
		t.Fatalf("missing missing-file warning: %q", stderr)
	}
	if !strings.Contains(stderr, "skipping bad.tiff") {
		t.Fatalf("missing unsupported-extension warning: %q", stderr)
	}
}

func TestBuildUserContent_TextFileDoesNotExist(t *testing.T) {
	stdout, stderr, code := runHelperMain(t, "build-user-content", "--text-file", "/path/to/nonexistent-file.txt")
	if code != 1 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stdout != "" {
		t.Fatalf("unexpected stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "no such file or directory") {
		t.Fatalf("missing error message in stderr: %q", stderr)
	}
}

func TestDurationSeconds(t *testing.T) {
	// Test Exported behavior: duration-seconds command line subcommand.
	// Start at a known time before test execution; compute duration difference.
	start := time.Now().Unix() - 100 // 100 seconds ago
	stdout, stderr, code := runHelperMain(t, "duration-seconds", strconv.FormatInt(start, 10))
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	got := strings.TrimSpace(stdout)
	ts, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("stdout %q is not unix seconds: %v", got, err)
	}
	expectedDiff := int64(100)
	if ts < expectedDiff-5 || ts > expectedDiff+5 {
		t.Fatalf("duration %d not near expected %d (within 5 seconds)", ts, expectedDiff)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestNowMillis(t *testing.T) {
	// Test Exported behavior: now-ms command line subcommand.
	// Both commands should report timestamps within reasonable tolerance of Unix time.
	stdout, stderr, code := runHelperMain(t, "now-ms")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	got := strings.TrimSpace(stdout)
	ms, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("stdout %q is not milliseconds: %v", got, err)
	}
	seconds := time.Now().Unix()
	// Milliseconds should be seconds * 1000 + fraction
	if ms < seconds*1000-1000 || ms > seconds*1000+1000 {
		t.Fatalf("ms %d not near seconds*1000 (%d)", ms, seconds*1000)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestStripReasoning(t *testing.T) {
	const input = "Before <reasoning>secret</reasoning> and after"
	const expected = "Before and after"
	stdout, stderr, code := runHelperMainWithInput(t, input, "strip-reasoning")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	got := strings.TrimSpace(stdout)
	if got != expected {
		t.Fatalf("got %q, want %q", got, expected)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
}

func TestMainBuildUserContentWithoutPromptEmitsImageArray(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "only.png")
	imgBytes := []byte{0x89, 'P', 'N', 'G'}
	if err := os.WriteFile(imgPath, imgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runHelperMain(t, "build-user-content", "--image", imgPath)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}

	var blocks []struct {
		Type     string `json:"type"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	if err := json.Unmarshal([]byte(stdout), &blocks); err != nil {
		t.Fatalf("expected JSON array, got %s: %v", stdout, err)
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 image block, got %d: %s", len(blocks), stdout)
	}
	if blocks[0].Type != "image_url" || blocks[0].ImageURL == nil {
		t.Fatalf("unexpected block: %+v", blocks[0])
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(blocks[0].ImageURL.URL, prefix) {
		t.Fatalf("data URL prefix wrong: %s", blocks[0].ImageURL.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blocks[0].ImageURL.URL, prefix))
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !bytes.Equal(decoded, imgBytes) {
		t.Fatalf("payload bytes mismatch: %x", decoded)
	}
}
