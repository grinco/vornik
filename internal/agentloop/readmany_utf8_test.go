package agentloop

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestReadManyFiles_CapIsCharactersNeverSplitsARune pins what §3.1 of the
// dispatch design promised and the port did not deliver: read_many_files' cap
// is 30,000 CHARACTERS (python sliced the decoded text), so a 40,000-byte file
// of 20,000 two-byte runes is under the cap and returned whole, and a file
// over the cap is cut at a rune boundary. Found validating the 2026-09-05
// audit fixes: the port read cap+1 BYTES and sliced there, which truncated
// multibyte files that python returned whole and ended the output with a
// lone 0xC3 — invalid UTF-8 handed to the model. Pre-existing in the port
// (01cbba7a); the bounded-read fix (29732200) kept it and its boundary test
// enshrined it.
func TestReadManyFiles_CapIsCharactersNeverSplitsARune(t *testing.T) {
	ws := t.TempDir()
	under := strings.Repeat("é", 20000) // 40,000 bytes, 20,000 runes: under the cap
	over := strings.Repeat("é", 30001)  // 60,002 bytes, 30,001 runes: one over
	mustWrite(t, filepath.Join(ws, "under.txt"), under)
	mustWrite(t, filepath.Join(ws, "over.txt"), over)

	got := Dispatch(Env{Workspace: ws}, "read_many_files", json.RawMessage(`{"paths":["under.txt"]}`))
	if !utf8.ValidString(got) {
		t.Fatalf("output is not valid UTF-8 (ends %x)", got[len(got)-8:])
	}
	if strings.Contains(got, "truncated") {
		t.Fatalf("a 20,000-character file is under the 30,000-character cap and must not be truncated:\n%s", got[len(got)-120:])
	}
	if want := "===== FILE: under.txt =====\n" + under; got != want {
		t.Fatalf("content changed (got %d runes, want %d)", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}

	got = Dispatch(Env{Workspace: ws}, "read_many_files", json.RawMessage(`{"paths":["over.txt"]}`))
	if !utf8.ValidString(got) {
		t.Fatalf("truncated output is not valid UTF-8")
	}
	want := "===== FILE: over.txt =====\n" + strings.Repeat("é", 30000) + "\n[... truncated at 30KB, total 60002 bytes]"
	if got != want {
		t.Fatalf("over-cap file: got %d runes ending %q, want the first 30,000 runes and the trailer", utf8.RuneCountInString(got), got[len(got)-60:])
	}
}
