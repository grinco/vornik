package agentloop

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// INCIDENT 2026-09-16 — task_20260916163809_c16163e002783786, execution
// exec_20260916164618_4c53e9deafd4e8e6, dead in 8s on:
//
//	Missing prerequisite: file_read of
//	"/app/workspace/app/workspace/artifacts/in/2026-09-16-retrieval-recency-design.md"
//	returned not-found twice.
//
// The doubled path is not a daemon rewrite. The audit ledger for that
// execution has the model calling file_read with three different spellings of
// the same file in one step — "artifacts/in/<base>",
// "/app/workspace/artifacts/in/<base>", and
// "app/workspace/artifacts/in/<base>". The third has lost its leading slash,
// and resolvePath joins a relative path onto the workspace, so the path it
// opened restated the workspace root twice. The repeat-miss guard then killed
// the step over a file that was sitting right there.
//
// resolvePath is ALREADY tolerant in the mirror-image case: an absolute path
// outside the workspace is re-rooted under it rather than refused. A relative
// path that spells out the workspace root is the same class of near-miss and
// gets the same treatment. Confinement is unchanged — the result is under the
// workspace by construction either way.
func TestResolvePath_RelativePathRestatingWorkspaceRoot(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "app", "workspace")
	mustWrite(t, filepath.Join(ws, "artifacts", "in", "design.md"), "the design")
	realWS := realpath(ws)
	deslashed := strings.TrimPrefix(realWS, "/")

	got, err := resolvePath(ws, deslashed+"/artifacts/in/design.md")
	if err != nil {
		t.Fatalf("de-slashed absolute path must resolve, not refuse: %v", err)
	}
	if want := filepath.Join(realWS, "artifacts", "in", "design.md"); got != want {
		t.Errorf("de-slashed absolute path resolved to %q, want %q", got, want)
	}

	// The workspace root itself, with the slash dropped.
	if got, err = resolvePath(ws, deslashed); err != nil || got != realWS {
		t.Errorf("de-slashed workspace root: got %q err %v, want %q", got, err, realWS)
	}
}

// The tolerance must not swallow an ordinary relative path that merely starts
// with a similarly-named component. Only the workspace root's own spelling,
// on a component boundary, counts.
func TestResolvePath_RelativePathNotRestatingWorkspaceRootIsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "app", "workspace")
	mustWrite(t, filepath.Join(ws, "notes.md"), "x")
	realWS := realpath(ws)
	deslashed := strings.TrimPrefix(realWS, "/")

	got, err := resolvePath(ws, "notes.md")
	if err != nil || got != filepath.Join(realWS, "notes.md") {
		t.Errorf("plain relative path: got %q err %v", got, err)
	}
	// A sibling whose name merely has the workspace spelling as a string
	// prefix — no component boundary — must NOT be rewritten.
	got, err = resolvePath(ws, deslashed+"-other/notes.md")
	if err != nil || got != filepath.Join(realWS, deslashed+"-other", "notes.md") {
		t.Errorf("prefix-without-boundary must be joined verbatim: got %q err %v", got, err)
	}
}

// End to end through the tool, which is where the incident was felt: the read
// must return the file's content, not "file not found" on a doubled path.
func TestFileRead_DeSlashedAbsolutePathReachesTheFile(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "app", "workspace")
	mustWrite(t, filepath.Join(ws, "artifacts", "in", "design.md"), "the design body")
	deslashed := strings.TrimPrefix(realpath(ws), "/")

	args, err := json.Marshal(map[string]string{"path": deslashed + "/artifacts/in/design.md"})
	if err != nil {
		t.Fatal(err)
	}
	out := Dispatch(Env{Workspace: ws}, "file_read", args)
	if out != "the design body" {
		t.Errorf("file_read on a de-slashed absolute path returned %q", out)
	}
}

// The bash twin and this one computed the de-slashed root differently, so the
// parity they depend on held only while WORKSPACE was canonical — true of the
// container image and of nothing else (review-20260916-2c6c, finding 1). The
// bash side now normpaths first; this pins the Go side against the same three
// shapes so the pair is asserted rather than assumed.
func TestResolvePath_NonCanonicalWorkspaceResolvesIdentically(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "app", "workspace")
	mustWrite(t, filepath.Join(ws, "artifacts", "in", "design.md"), "x")
	realWS := realpath(ws)
	deslashed := strings.TrimPrefix(realWS, "/")
	want := filepath.Join(realWS, "artifacts", "in", "design.md")

	for _, odd := range []string{ws + "/", ws + "//", ws + "/."} {
		got, err := resolvePath(odd, deslashed+"/artifacts/in/design.md")
		if err != nil {
			t.Errorf("workspace %q refused: %v", odd, err)
			continue
		}
		if got != want {
			t.Errorf("workspace %q resolved to %q, want %q", odd, got, want)
		}
	}
}
