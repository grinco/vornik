package imagemanifest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRecord(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "images.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodRecord = `{"version":2,"record_source":"registry","count":1,"images":[
  {"tag":"ghcr.io/grinco/vornik-agent:latest",
   "digests":{"amd64":"sha256:8b41a998f6080f06462866a2ae50ad40c1ca9bc11ae06f991044e5a6e6d24393"},
   "source_commit":"4b343821000000000000000000000000000000ab"}]}`

// Step 1 of the ordering: a missing file is ABSENT, which is the correct and
// silent description of a source install.
func TestLoadReleaseRecord_MissingFileIsAbsentNotCorrupt(t *testing.T) {
	_, err := LoadReleaseRecord(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, ErrRecordAbsent) {
		t.Fatalf("missing file must be ErrRecordAbsent, got %v", err)
	}
	if IsCorrupt(err) {
		t.Fatal("a missing file must never read as corrupt")
	}
}

func TestLoadReleaseRecord_Valid(t *testing.T) {
	rec, err := LoadReleaseRecord(writeRecord(t, goodRecord))
	if err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	got, ok := rec.Lookup(AgentImageTag)
	if !ok {
		t.Fatalf("agent image absent from parsed record")
	}
	if got.SourceCommit != "4b343821000000000000000000000000000000ab" {
		t.Errorf("source_commit round-trip wrong: %q", got.SourceCommit)
	}
}

// Step 2: unparseable JSON is CORRUPT, never absent. This is the truncation
// that breaks syntax.
func TestLoadReleaseRecord_TruncatedJSONIsCorrupt(t *testing.T) {
	body := goodRecord[:len(goodRecord)-12]
	_, err := LoadReleaseRecord(writeRecord(t, body))
	if !IsCorrupt(err) {
		t.Fatalf("truncated JSON must be corrupt, got %v", err)
	}
	if errors.Is(err, ErrRecordAbsent) {
		t.Fatal("corrupt must not also read as absent — that silently disables the check")
	}
}

// Step 3 is the one an implementation is most likely to skip: a truncation that
// leaves SYNTACTICALLY VALID JSON carrying a half-written digest. Without field
// validation this parses cleanly and compares false against everything.
func TestLoadReleaseRecord_ShortDigestIsCorruptNotAbsent(t *testing.T) {
	body := `{"version":2,"record_source":"registry","count":1,"images":[
	  {"tag":"ghcr.io/grinco/vornik-agent:latest","digests":{"amd64":"sha256:ab12"},
	   "source_commit":"4b343821000000000000000000000000000000ab"}]}`
	_, err := LoadReleaseRecord(writeRecord(t, body))
	if !IsCorrupt(err) {
		t.Fatalf("short digest must be corrupt, got %v", err)
	}
}

// The regex assumes a lowercase algorithm identifier, which is what podman
// emits. Uppercase must fail CLOSED (corrupt), never be silently accepted.
func TestLoadReleaseRecord_UppercaseAlgorithmIsCorrupt(t *testing.T) {
	body := strings.Replace(goodRecord, "sha256:", "SHA256:", 1)
	if _, err := LoadReleaseRecord(writeRecord(t, body)); !IsCorrupt(err) {
		t.Fatalf("uppercase algorithm must be corrupt, got %v", err)
	}
}

func TestLoadReleaseRecord_BadSourceCommitIsCorrupt(t *testing.T) {
	body := strings.Replace(goodRecord, "4b343821000000000000000000000000000000ab", "4b343", 1)
	if _, err := LoadReleaseRecord(writeRecord(t, body)); !IsCorrupt(err) {
		t.Fatalf("short source_commit must be corrupt, got %v", err)
	}
}

// A -dirty commit names a tree that exists on one machine and can never be
// verified by anyone. It is a release error, not a recorded value.
func TestLoadReleaseRecord_DirtyCommitIsCorrupt(t *testing.T) {
	body := strings.Replace(goodRecord,
		"4b343821000000000000000000000000000000ab",
		"4b343821000000000000000000000000000000ab-dirty", 1)
	if _, err := LoadReleaseRecord(writeRecord(t, body)); !IsCorrupt(err) {
		t.Fatalf("-dirty source_commit must be corrupt, got %v", err)
	}
}

// Step 4: truncation that drops entries but leaves valid JSON. count disagrees
// with the array length, which is the only thing that can catch it.
func TestLoadReleaseRecord_CountMismatchIsCorrupt(t *testing.T) {
	body := strings.Replace(goodRecord, `"count":1`, `"count":3`, 1)
	_, err := LoadReleaseRecord(writeRecord(t, body))
	if !IsCorrupt(err) {
		t.Fatalf("count/length disagreement must be corrupt, got %v", err)
	}
	if !strings.Contains(err.Error(), "count") {
		t.Errorf("the error should name the count mismatch, got %q", err)
	}
}

// An EMPTY record is a legitimately different statement from a missing one:
// "this release declares no images". It must parse, not error.
func TestLoadReleaseRecord_EmptyRecordIsValidNotAbsent(t *testing.T) {
	rec, err := LoadReleaseRecord(writeRecord(t, `{"version":2,"record_source":"registry","count":0,"images":[]}`))
	if err != nil {
		t.Fatalf("an empty record is valid, got %v", err)
	}
	if _, ok := rec.Lookup(AgentImageTag); ok {
		t.Error("empty record must not resolve any tag")
	}
}

func TestLoadReleaseRecord_UnknownVersionIsCorrupt(t *testing.T) {
	body := strings.Replace(goodRecord, `"version":2,"record_source":"registry"`, `"version":99`, 1)
	if _, err := LoadReleaseRecord(writeRecord(t, body)); !IsCorrupt(err) {
		t.Fatalf("an unknown schema version must be corrupt, got %v", err)
	}
}

// One record describes one build over one tree. Images disagreeing on the
// commit means it was assembled from several, which makes "the commit this
// release declares" undefined — so a record↔daemon comparison would be
// comparing against nothing in particular.
func TestLoadReleaseRecord_MixedSourceCommitsIsCorrupt(t *testing.T) {
	body := `{"version":2,"record_source":"registry","count":2,"images":[
	  {"tag":"ghcr.io/grinco/vornik-agent:latest",
	   "digests":{"amd64":"sha256:8b41a998f6080f06462866a2ae50ad40c1ca9bc11ae06f991044e5a6e6d24393"},
	   "source_commit":"4b343821000000000000000000000000000000ab"},
	  {"tag":"localhost/vornik-broker:latest",
	   "digests":{"amd64":"sha256:f8893b9d93093e9f5f7f97bf6ff17ff837cb4c1e6a4fabd27486f4febadbe266"},
	   "source_commit":"9f3c1a2000000000000000000000000000000cd0"}]}`
	_, err := LoadReleaseRecord(writeRecord(t, body))
	if !IsCorrupt(err) {
		t.Fatalf("mixed source commits must be corrupt, got %v", err)
	}
}

func TestReleaseRecord_SourceCommit(t *testing.T) {
	rec, err := LoadReleaseRecord(writeRecord(t, goodRecord))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rec.SourceCommit()
	if !ok || got != "4b343821000000000000000000000000000000ab" {
		t.Fatalf("SourceCommit = %q,%v", got, ok)
	}
	empty, err := LoadReleaseRecord(writeRecord(t, `{"version":2,"record_source":"registry","count":0,"images":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := empty.SourceCommit(); ok {
		t.Error("an empty record names no commit")
	}
}
