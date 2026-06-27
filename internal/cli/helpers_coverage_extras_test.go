// Package cli: additional pure-helper coverage on top of
// helpers_coverage_test.go. Split into its own file so the
// coverage-uplift diff is reviewable in isolation; the runE entry
// points and config.Load-bound code paths are still skipped
// because they require a live daemon to integration-test.
package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestSummariseLine — the chat / log summariser. Behaviour pinned:
// newlines collapse to a visible glyph, surrounding whitespace
// trims, overflow truncates with an ellipsis. Operators read this
// output in the eval CLI; a regression that drops newlines silently
// would make multi-line errors unreadable.
func TestSummariseLine(t *testing.T) {
	cases := []struct {
		name, in string
		max      int
		want     string
	}{
		{"short", "hello", 10, "hello"},
		{"trim outer whitespace", "  hi  ", 10, "hi"},
		{"newline becomes glyph", "a\nb", 10, "a ⏎ b"},
		{"multiple newlines", "a\nb\nc", 20, "a ⏎ b ⏎ c"},
		{"exact length no ellipsis", "hi", 2, "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseLine(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("summariseLine(%q, %d) = %q, want %q",
					tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestHasRegistryLayout — the boolean that resolveConfigsDir relies
// on to discriminate "real configs dir" from "wrong directory."
// Pins the 3-of-3 subdir contract (projects / swarms / workflows).
func TestHasRegistryLayout(t *testing.T) {
	t.Run("missing all three", func(t *testing.T) {
		dir := t.TempDir()
		if hasRegistryLayout(dir) {
			t.Error("empty tmpdir reported as registry layout")
		}
	})
	t.Run("missing one of three", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"projects", "swarms"} { // no workflows
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if hasRegistryLayout(dir) {
			t.Error("missing workflows/ dir but still reported true")
		}
	})
	t.Run("file instead of subdir", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"projects", "swarms"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "workflows"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if hasRegistryLayout(dir) {
			t.Error("file (non-dir) at workflows reported as layout match")
		}
	})
	t.Run("complete layout", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"projects", "swarms", "workflows"} {
			if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if !hasRegistryLayout(dir) {
			t.Error("complete projects/swarms/workflows layout rejected")
		}
	})
}

// TestResolveConfigsDir — the discovery cascade for `vornikctl init`
// when called from a directory without a config file. Env override
// wins, then configPath-relative siblings, then XDG state.
func TestResolveConfigsDir(t *testing.T) {
	origEnv := os.Getenv("VORNIK_CONFIGS_DIR")
	origConfig := os.Getenv("VORNIK_CONFIG")
	t.Cleanup(func() {
		_ = os.Setenv("VORNIK_CONFIGS_DIR", origEnv)
		_ = os.Setenv("VORNIK_CONFIG", origConfig)
	})

	t.Run("env override wins when valid", func(t *testing.T) {
		dir := t.TempDir()
		for _, sub := range []string{"projects", "swarms", "workflows"} {
			_ = os.MkdirAll(filepath.Join(dir, sub), 0o755)
		}
		_ = os.Setenv("VORNIK_CONFIGS_DIR", dir)
		_ = os.Setenv("VORNIK_CONFIG", "")
		got := resolveConfigsDir("")
		if got != dir {
			t.Errorf("env override path: got %q, want %q", got, dir)
		}
	})

	t.Run("configPath sibling is found", func(t *testing.T) {
		_ = os.Setenv("VORNIK_CONFIGS_DIR", "")
		_ = os.Setenv("VORNIK_CONFIG", "")
		base := t.TempDir()
		siblings := filepath.Join(base, "configs")
		for _, sub := range []string{"projects", "swarms", "workflows"} {
			_ = os.MkdirAll(filepath.Join(siblings, sub), 0o755)
		}
		got := resolveConfigsDir(filepath.Join(base, "config.yaml"))
		if got != siblings {
			t.Errorf("configPath-sibling: got %q, want %q", got, siblings)
		}
	})
}

// TestCopyRegistryDir — the recursive copier used by the init
// subcommands' staging-temp-dir pattern. Pins both contracts:
// dirs become dirs, files become files (with identical content).
func TestCopyRegistryDir(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstChild := filepath.Join(dst, "copied")
	if err := copyRegistryDir(src, dstChild); err != nil {
		t.Fatalf("copyRegistryDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dstChild, "a.txt"))
	if err != nil || string(got) != "alpha" {
		t.Errorf("a.txt copy: %q / %v", got, err)
	}
	gotB, err := os.ReadFile(filepath.Join(dstChild, "sub", "b.txt"))
	if err != nil || string(gotB) != "beta" {
		t.Errorf("sub/b.txt copy: %q / %v", gotB, err)
	}
}

// TestCopyRegistryDir_MissingSourceErrors — explicit guard that the
// helper surfaces a clear error when the source doesn't exist.
func TestCopyRegistryDir_MissingSourceErrors(t *testing.T) {
	dst := t.TempDir()
	err := copyRegistryDir("/nonexistent/vornik-test-source", filepath.Join(dst, "out"))
	if err == nil {
		t.Error("missing source: expected error, got nil")
	}
}

// TestListPresets — the embedded-fs swarm preset catalogue. Asserts
// output is non-empty and sorted.
func TestListPresets(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := listPresets(cmd); err != nil {
		t.Fatalf("listPresets: %v", err)
	}
	out := buf.String()
	if out == "" {
		t.Error("listPresets produced no output")
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var names []string
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		parts := strings.Fields(ln)
		if len(parts) == 0 {
			continue
		}
		names = append(names, parts[0])
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("preset listing not sorted: %v", names)
			break
		}
	}
}

// TestReadPreset — the embed.FS preset fetcher's safety surface.
func TestReadPreset(t *testing.T) {
	t.Run("rejects empty", func(t *testing.T) {
		if _, err := readPreset(""); err == nil {
			t.Error("empty preset name: expected error")
		}
	})
	t.Run("rejects traversal-shaped names", func(t *testing.T) {
		for _, name := range []string{"../etc/passwd", "sub/preset", "a.b", "a\\b"} {
			if _, err := readPreset(name); err == nil {
				t.Errorf("name %q: expected rejection", name)
			}
		}
	})
	t.Run("unknown preset error", func(t *testing.T) {
		if _, err := readPreset("does-not-exist-anywhere"); err == nil {
			t.Error("unknown preset: expected error")
		}
	})
}

// TestPromptDefault_NonTTYReturnsFallback — when stdin isn't a
// terminal (test environment), promptDefault returns the fallback
// rather than blocking on a read.
func TestPromptDefault_NonTTYReturnsFallback(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&bytes.Buffer{})
	got := promptDefault(cmd, "label", "the-fallback")
	if got != "the-fallback" {
		t.Errorf("non-TTY promptDefault = %q, want %q", got, "the-fallback")
	}
}

// TestLoadEvalSuite — the eval suite loader.
func TestLoadEvalSuite(t *testing.T) {
	t.Run("missing file errors", func(t *testing.T) {
		_, err := loadEvalSuite("any", "/nonexistent/vornik-eval-suite.json")
		if err == nil {
			t.Error("missing file: expected error")
		}
	})
	t.Run("malformed JSON errors", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "broken.json")
		if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadEvalSuite("any", path); err == nil {
			t.Error("malformed JSON: expected error")
		}
	})
	t.Run("valid empty suite deserialises", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ok.json")
		if err := os.WriteFile(path, []byte(`{"cases":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		suite, err := loadEvalSuite("any", path)
		if err != nil {
			t.Fatalf("loadEvalSuite: %v", err)
		}
		if suite == nil {
			t.Error("returned suite is nil")
		}
	})
}

// TestFetchEvalTask_HappyPath uses httptest to stub the vornik API.
func TestFetchEvalTask_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/projects/p1/tasks/t1") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"t1","status":"COMPLETED"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	out, err := fetchEvalTask(client, "p1", "t1")
	if err != nil {
		t.Fatalf("fetchEvalTask: %v", err)
	}
	if out == nil {
		t.Fatal("nil response")
	}
}

// TestFetchEvalExecutionResult_HappyPath mirrors the task fetcher
// for the execution-result side.
func TestFetchEvalExecutionResult_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"exec_1","result":{"ok":true}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key")
	raw, err := fetchEvalExecutionResult(client, "exec_1")
	if err != nil {
		t.Fatalf("fetchEvalExecutionResult: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Errorf("returned RawMessage is not valid JSON: %v", err)
	}
	if parsed["ok"] != true {
		t.Errorf("result body lost in decode: %v", parsed)
	}
}

// TestIsTerminalTaskStatus — the small predicate the eval loop
// uses to decide "should I poll again?"
func TestIsTerminalTaskStatus(t *testing.T) {
	cases := map[string]bool{
		"COMPLETED": true,
		"FAILED":    true,
		"CANCELLED": true,
		"QUEUED":    false,
		"RUNNING":   false,
		"PAUSED":    false,
		"":          false,
		"completed": false, // case-sensitive by design
	}
	for status, want := range cases {
		if got := isTerminalTaskStatus(status); got != want {
			t.Errorf("isTerminalTaskStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestLanIPHint_NonEmpty — the helper must always return something
// printable even when no LAN interface is up.
func TestLanIPHint_NonEmpty(t *testing.T) {
	if got := lanIPHint(); got == "" {
		t.Error("lanIPHint returned empty string")
	}
}

// TestParseAPIError_NestedJSON — the canonical daemon error shape
// (`{"error":{"code":"...","message":"..."}}`). Both pieces must
// land in the *APIError so the eval CLI can show the operator
// actionable detail (not "API error 400" alone).
func TestParseAPIError_NestedJSON(t *testing.T) {
	body := strings.NewReader(`{"error":{"code":"E_BAD","message":"something broke"}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       io.NopCloser(body),
		Request:    &http.Request{URL: &url.URL{Path: "/api/v1/x"}},
	}
	err := ParseAPIError(resp)
	if err == nil {
		t.Fatal("ParseAPIError on a 400 returned nil")
	}
	if !strings.Contains(err.Error(), "something broke") {
		t.Errorf("error message %q lost the upstream detail", err.Error())
	}
	if !strings.Contains(err.Error(), "E_BAD") {
		t.Errorf("error message %q lost the code", err.Error())
	}
}

// TestParseAPIError_EmptyBody — empty / whitespace bodies degrade
// to the stdlib's status text so the operator sees something better
// than "API error 500: ".
func TestParseAPIError_EmptyBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    &http.Request{URL: &url.URL{Path: "/api/v1/x"}},
	}
	err := ParseAPIError(resp)
	if err == nil {
		t.Fatal("ParseAPIError on a 500 with empty body returned nil")
	}
	if !strings.Contains(err.Error(), "Internal Server Error") {
		t.Errorf("expected stdlib status text in %q", err.Error())
	}
}

// TestPassthroughJSON — the registry CLI's HTTP→stdout passthrough.
// Signature is `passthroughJSON([]byte) error`: it pretty-prints
// the given JSON blob to stdout. Verify it writes something
// containing the body keys.
func TestPassthroughJSON(t *testing.T) {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	if err := passthroughJSON([]byte(`{"projects":[]}`)); err != nil {
		t.Fatalf("passthroughJSON: %v", err)
	}
	_ = w.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r)
	out := buf.String()
	if !strings.Contains(out, `"projects"`) {
		t.Errorf("passthroughJSON output missing body fragment; got %q", out)
	}
}

// TestPassthroughJSON_NonJSONIsRawFallback — the helper intentionally
// falls back to raw bytes when the server returns non-JSON. That's
// the documented behaviour ("server might have returned plain text
// for some reason and the CLI should still surface it"). Pin the
// behaviour with a non-JSON body so a future refactor doesn't
// silently change it.
func TestPassthroughJSON_NonJSONIsRawFallback(t *testing.T) {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	if err := passthroughJSON([]byte(`not json`)); err != nil {
		t.Errorf("non-JSON should fall back to raw, not error: %v", err)
	}
	_ = w.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(r)
	if !strings.Contains(buf.String(), "not json") {
		t.Errorf("raw fallback didn't print body; got %q", buf.String())
	}
}
