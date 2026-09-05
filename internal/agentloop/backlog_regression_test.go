package agentloop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// September 2026 backlog: capped output must not allocate the entire input.
func TestBacklogReadsBounded(t *testing.T) {
	for _, tool := range []string{"file_read", "read_many_files"} {
		t.Run(tool, func(t *testing.T) {
			ws := t.TempDir()
			f, err := os.Create(filepath.Join(ws, "large"))
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Truncate(16 << 20); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got := Dispatch(Env{Workspace: ws}, tool, json.RawMessage(`{"path":"large","paths":["large"]}`))
			runtime.ReadMemStats(&after)
			if !strings.Contains(got, "truncated at 30KB, total 16777216 bytes") {
				t.Fatalf("wrong trailer: %s", got[len(got)-min(100, len(got)):])
			}
			if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
				t.Fatalf("30KB-capped read allocated %d bytes", n)
			}
		})
	}
}

// September 2026 backlog: fnmatchRE treated each UTF-8 literal byte as a rune.
// Both user-facing tools share this translator, so exercise both dispatches.
func TestBacklogUnicodeGlobs(t *testing.T) {
	for _, c := range []struct{ name, pattern string }{
		{"café.txt", "café*"}, {"東京.go", "東京.*"}, {"📄.md", "📄.?d"},
		{"café.txt", "caf[é].txt"}, {"東京.go", "[東]京.*"}, {"café.txt", "caf[!a].txt"},
	} {
		t.Run(c.pattern, func(t *testing.T) {
			ws := t.TempDir()
			mustWrite(t, filepath.Join(ws, c.name), "needle")
			for _, tool := range []string{"glob", "grep"} {
				args := map[string]any{"pattern": c.pattern}
				if tool == "grep" {
					args = map[string]any{"pattern": "needle", "glob": c.pattern}
				}
				raw, _ := json.Marshal(args)
				got := Dispatch(Env{Workspace: ws}, tool, raw)
				if got != c.name {
					t.Errorf("%s: got %q, want %q", tool, got, c.name)
				}
			}
		})
	}
}

// September 2026 backlog: head_limit=-1 panicked at shown[:q.head]. Reject
// non-positive limits consistently even if the workspace has no matches.
func TestBacklogGrepLimitValidation(t *testing.T) {
	for _, mode := range []string{"files_with_matches", "content", "count"} {
		for _, limit := range []int{-1, 0, 1, 2, 200} {
			t.Run(fmt.Sprintf("%s/%d", mode, limit), func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("grep panicked instead of validating: %v", p)
					}
				}()
				ws := t.TempDir()
				mustWrite(t, filepath.Join(ws, "a"), "needle\nneedle\n")
				mustWrite(t, filepath.Join(ws, "b"), "needle\n")
				raw, _ := json.Marshal(map[string]any{"pattern": "needle", "output_mode": mode, "head_limit": limit})
				got := Dispatch(Env{Workspace: ws}, "grep", raw)
				if limit < 1 {
					if got != "ERROR: head_limit must be at least 1" {
						t.Fatalf("invalid limit: %q", got)
					}
					got = Dispatch(Env{Workspace: t.TempDir()}, "grep", raw)
					if got != "ERROR: head_limit must be at least 1" {
						t.Fatalf("empty workspace bypassed validation: %q", got)
					}
				} else if strings.HasPrefix(got, "ERROR:") || got == "(no matches)" {
					t.Fatalf("valid limit refused: %s", got)
				}
			})
		}
	}
}

func TestBacklogReadBoundaryCompatibility(t *testing.T) {
	for _, size := range []int{0, 29999, 30000, 30001, 40000} {
		for _, multibyte := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/%t", size, multibyte), func(t *testing.T) {
				content := strings.Repeat("x", size)
				if multibyte {
					content = strings.Repeat("é", size/2) + strings.Repeat("x", size%2)
				}
				ws := t.TempDir()
				mustWrite(t, filepath.Join(ws, "file"), content)
				got := Dispatch(Env{Workspace: ws}, "file_read", json.RawMessage(`{"path":"file"}`))
				want := content
				if size > 30000 {
					want = content[:30000] + fmt.Sprintf("\n\n[... truncated at 30KB, total %d bytes]", size)
				}
				if got != want {
					t.Errorf("file_read changed at boundary %d", size)
				}
				got = Dispatch(Env{Workspace: ws}, "read_many_files", json.RawMessage(`{"paths":["file"]}`))
				want = "===== FILE: file =====\n" + content
				// read_many_files' cap is CHARACTERS, not bytes (design §3.1): a
				// multibyte file of these byte sizes holds at most 20,000 runes
				// and is returned whole; only the ASCII sizes over the cap truncate,
				// and never mid-rune. (This table once encoded a byte-based cut
				// that split the last rune; see TestReadManyFiles_CapIsCharactersNeverSplitsARune.)
				if utf8.RuneCountInString(content) > 30000 {
					want = "===== FILE: file =====\n" + runeSlice(content, 30000) + fmt.Sprintf("\n[... truncated at 30KB, total %d bytes]", size)
				}
				if got != want {
					t.Errorf("read_many_files changed at boundary %d", size)
				}
			})
		}
	}
}

func TestReadFilePrefixErrorsAndSentinel(t *testing.T) {
	ws := t.TempDir()
	for _, path := range []string{filepath.Join(ws, "missing"), ws} {
		if _, _, err := readFilePrefix(path, 10); err == nil {
			t.Errorf("read of %q must fail", path)
		}
	}
	mustWrite(t, filepath.Join(ws, "file"), "0123456789abcdef")
	data, size, err := readFilePrefix(filepath.Join(ws, "file"), 10)
	if err != nil || string(data) != "0123456789a" || size != 16 {
		t.Fatalf("prefix=%q size=%d err=%v", data, size, err)
	}
}

func TestReadManyFilesErrorsAndTotalCap(t *testing.T) {
	ws := t.TempDir()
	env := Env{Workspace: ws}
	if got := Dispatch(env, "read_many_files", json.RawMessage(`{}`)); got != "ERROR: paths array is required" {
		t.Fatal(got)
	}
	got := Dispatch(env, "read_many_files", json.RawMessage(`{"paths":["missing","../outside"]}`))
	if !strings.Contains(got, "ERROR: file not found") || !strings.Contains(got, "ERROR: path escapes workspace") {
		t.Fatal(got)
	}
	mustWrite(t, filepath.Join(ws, "file"), strings.Repeat("x", 40000))
	got = Dispatch(env, "read_many_files", json.RawMessage(`{"paths":["file","file","file","file","file"]}`))
	if !strings.HasSuffix(got, "\n[... output truncated at 120KB]") {
		t.Fatal("total output cap missing")
	}
	if len(got) != 120000+len("\n[... output truncated at 120KB]") {
		t.Fatalf("unexpected capped length: %d", len(got))
	}
}

func TestGrepDefaultLimit(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a"), "needle")
	for _, mode := range []string{"files_with_matches", "content", "count"} {
		args := map[string]any{"pattern": "needle", "output_mode": mode}
		raw, _ := json.Marshal(args)
		got := Dispatch(Env{Workspace: ws}, "grep", raw)
		args["head_limit"] = 200
		raw, _ = json.Marshal(args)
		if explicit := Dispatch(Env{Workspace: ws}, "grep", raw); got != explicit {
			t.Errorf("%s default %q differs from 200: %q", mode, got, explicit)
		}
	}
}
