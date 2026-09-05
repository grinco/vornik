package agentloop

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolvePath_SymlinkPrefix pins the resolve_path port against the three
// symlink shapes the design's parity table names (§3.1, resolve_path row):
// a link in an intermediate component of an EXISTING path, the same with a
// non-existent tail (CPython realpath(strict=False) keeps the tail lexical),
// and an intermediate link that leaves the workspace, which must refuse.
func TestResolvePath_SymlinkPrefix(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	outside := filepath.Join(tmp, "outside")
	mustWrite(t, filepath.Join(ws, "real", "sub", "file.txt"), "x")
	mustWrite(t, filepath.Join(outside, "x.txt"), "y")
	if err := os.Symlink("real", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	realWS := realpath(ws)

	got, err := resolvePath(ws, "link/sub/file.txt")
	if err != nil || got != filepath.Join(realWS, "real", "sub", "file.txt") {
		t.Errorf("intermediate link, existing tail: %q %v", got, err)
	}
	got, err = resolvePath(ws, "link/new/file.txt")
	if err != nil || got != filepath.Join(realWS, "real", "new", "file.txt") {
		t.Errorf("intermediate link, non-existent tail: %q %v", got, err)
	}
	if _, err = resolvePath(ws, "out/x.txt"); err == nil || err.Error() != "ERROR: path escapes workspace: out/x.txt" {
		t.Errorf("escaping link must refuse with the entrypoint's wording: %v", err)
	}
	if _, err = resolvePath(ws, "../outside/x.txt"); err == nil {
		t.Error("dot-dot escape must refuse")
	}
	if got, _ = resolvePath(ws, "/etc/passwd"); got != filepath.Join(realWS, "etc", "passwd") {
		t.Errorf("an absolute path outside the workspace is re-rooted under it: %q", got)
	}
}

// TestNoSubprocessExceptGit — the design's §5.6: the golden compares output
// and cannot see process shape, so this asserts by inspection that the only
// exec.Command argv the package ever builds starts with "git".
func TestNoSubprocessExceptGit(t *testing.T) {
	fset := token.NewFileSet()
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" || !strings.HasPrefix(sel.Sel.Name, "Command") {
				return true
			}
			if e.Name() != "git_tools.go" {
				t.Errorf("%s:%d spawns a subprocess; only git_tools.go may", e.Name(), fset.Position(call.Pos()).Line)
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Value != `"git"` {
				t.Errorf("%s:%d: the only argv[0] allowed is the literal \"git\"", e.Name(), fset.Position(call.Pos()).Line)
			}
			return true
		})
	}
}

func TestCurrentTime_InjectedClock(t *testing.T) {
	fixed := time.Date(2026, 9, 5, 0, 15, 30, 123456000, time.UTC)
	env := Env{Workspace: t.TempDir(), Now: func() time.Time { return fixed }}
	got := Dispatch(env, "current_time", json.RawMessage(`{"timezone":"Europe/Prague"}`))
	want := `{
  "timezone": "Europe/Prague",
  "date": "2026-09-05",
  "time": "02:15:30",
  "weekday": "Saturday",
  "rfc3339": "2026-09-05T02:15:30.123456+02:00",
  "utc": "2026-09-05T00:15:30.123456Z",
  "utc_offset": "+02:00",
  "is_dst": true,
  "unix": 1788567330
}`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := Dispatch(env, "current_time", json.RawMessage(`{"timezone":"Not/AZone"}`)); got != "ERROR: invalid timezone: Not/AZone" {
		t.Errorf("invalid zone: %q", got)
	}
	whole := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	env.Now = func() time.Time { return whole }
	if got := Dispatch(env, "current_time", nil); !strings.Contains(got, `"rfc3339": "2026-01-01T12:00:00+00:00"`) || !strings.Contains(got, `"utc": "2026-01-01T12:00:00Z"`) {
		t.Errorf("python isoformat omits microseconds when zero: %s", got)
	}
}

func TestFnmatch_PythonSemantics(t *testing.T) {
	cases := []struct {
		name, pat string
		want      bool
	}{
		{"sub/one.txt", "*.txt", true}, // * crosses "/" in fnmatch
		{"sub/one.txt", "*.md", false},
		{"one.txt", "one.???", true},
		{"a.txt", "[ab].txt", true},
		{"c.txt", "[!ab].txt", true},
		{"a.txt", "[!ab].txt", false},
		{"[x", "[x", true}, // an unterminated class is a literal "["
		{"x", "[]", false},
	}
	for _, c := range cases {
		if got := fnmatch(c.name, c.pat); got != c.want {
			t.Errorf("fnmatch(%q, %q) = %t, want %t", c.name, c.pat, got, c.want)
		}
	}
}

func TestPyJSON_MatchesJsonDumps(t *testing.T) {
	got := pyJSON(pyObject{{"s", "é <tag> \"q\" \\ \n"}, {"n", 3}, {"b", true}, {"list", []pyObject{}}, {"astral", "😀"}})
	want := "{\n  \"s\": \"\\u00e9 <tag> \\\"q\\\" \\\\ \\n\",\n  \"n\": 3,\n  \"b\": true,\n  \"list\": [],\n  \"astral\": \"\\ud83d\\ude00\"\n}"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// The four divergences, each pinned so the difference stays chosen
// (agent-tool dispatch design §3.1).
func TestDivergences_ArePinned(t *testing.T) {
	ws := t.TempDir()
	env := Env{Workspace: ws}

	// D1: an invalid UTF-8 byte survives file_edit (python replaced it with U+FFFD).
	mustWrite(t, filepath.Join(ws, "u.txt"), "caf\xc3\xa9 and \xff here\n")
	out := Dispatch(env, "file_edit", json.RawMessage(`{"path":"u.txt","old_string":"and","new_string":"AND"}`))
	if !strings.HasPrefix(out, "OK: replaced 1 occurrence(s)") || !strings.HasSuffix(out, "(16 bytes)") {
		t.Errorf("D1 edit: %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "u.txt")); string(data) != "caf\xc3\xa9 AND \xff here\n" {
		t.Errorf("D1: the invalid byte must be preserved, got %q", data)
	}

	// D2: RE2 rejects a backreference with the invalid-regex line.
	if out := Dispatch(env, "grep", json.RawMessage(`{"pattern":"(a)\\1"}`)); !strings.HasPrefix(out, "ERROR: invalid regex: ") {
		t.Errorf("D2: %q", out)
	}

	// D4: git output is cut at 30 000 BYTES on a rune boundary and the trailer counts bytes.
	wide := strings.Repeat("é", 20000)
	if got := capGitOutput(wide); !strings.HasSuffix(got, "[... truncated at 30KB, total 40000 bytes]") || len(strings.TrimSuffix(got, "\n\n[... truncated at 30KB, total 40000 bytes]")) != 30000 {
		t.Errorf("D4: %d bytes, tail %q", len(got), got[len(got)-50:])
	}
}

func TestFileRead_CapAndToolResultsRefusal(t *testing.T) {
	ws := t.TempDir()
	env := Env{Workspace: ws}
	mustWrite(t, filepath.Join(ws, "big.bin"), strings.Repeat("a", 40000))
	mustWrite(t, filepath.Join(ws, ".tool_results", "s.txt"), "spill")
	out := Dispatch(env, "file_read", json.RawMessage(`{"path":"big.bin"}`))
	if len(out) != 30000+len("\n\n[... truncated at 30KB, total 40000 bytes]") || !strings.HasSuffix(out, "total 40000 bytes]") {
		t.Errorf("cap: %d bytes", len(out))
	}
	if out := Dispatch(env, "file_read", json.RawMessage(`{"path":".tool_results/s.txt"}`)); out != "ERROR: .tool_results is only readable through tool_result_read" {
		t.Errorf("spill refusal: %q", out)
	}
	if out := Dispatch(env, "file_read", json.RawMessage(`{}`)); out != "ERROR: path is required" {
		t.Errorf("no path: %q", out)
	}
}

func TestArgs_JQReadings(t *testing.T) {
	a := decodeArgs(json.RawMessage(`{"s":"x","f":false,"t":true,"n":5,"z":null,"arr":["a",1]}`))
	if a.str("s", "d") != "x" || a.str("f", "d") != "d" || a.str("t", "d") != "true" || a.str("n", "d") != "5" || a.str("z", "d") != "d" || a.str("missing", "d") != "d" {
		t.Errorf("str readings: %+v", a)
	}
	if a.intOr("n", 9) != 5 || a.intOr("s", 9) != 9 || a.intOr("missing", 9) != 9 {
		t.Error("intOr")
	}
	if got := a.strList("arr"); len(got) != 2 || got[0] != "a" || got[1] != "1" {
		t.Errorf("strList: %v", got)
	}
	if a.boolFlag("t") != true || a.boolFlag("f") || a.boolFlag("s") {
		t.Error("boolFlag")
	}
}

// TestDispatch_RefusesEmptyWorkspace pins the 2026-09-05 incident: the
// entrypoint did not export WORKSPACE, the helper read "" and resolved every
// path against "." — so "artifacts/out/ingestion.md" was reported as escaping
// the workspace and the easeit-companion ingest step hit the tool cap three
// times. An empty workspace must be refused by name, not mis-described as an
// escape: the escape message sent the model into retrying a path that was fine.
func TestDispatch_RefusesEmptyWorkspace(t *testing.T) {
	for _, tool := range []string{"file_write", "file_read", "glob"} {
		got := Dispatch(Env{Workspace: ""}, tool, json.RawMessage(`{"path":"artifacts/out/ingestion.md","pattern":"*"}`))
		if !strings.Contains(got, "WORKSPACE") || strings.Contains(got, "escapes workspace") {
			t.Errorf("%s with empty workspace: got %q, want a refusal naming WORKSPACE (not an escape)", tool, got)
		}
	}
}
