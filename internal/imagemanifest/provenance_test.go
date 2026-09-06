package imagemanifest

import (
	"os"
	"path/filepath"
	"testing"
)

// Contract C5's check (design §S2.4): a host records whether it pulled or
// built, because neither RepoDigests nor the revision label can tell you
// afterwards.

func TestObtainedRecordRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "obtained-images.json")

	rec, err := LoadObtained(path)
	if err != nil {
		t.Fatalf("a missing record is an empty one, not an error: %v", err)
	}
	if len(rec.Images) != 0 {
		t.Fatalf("got %d images from a missing record", len(rec.Images))
	}

	rec.Note(ObtainedImage{
		Tag: AgentImageTag, Method: MethodPulled,
		Reference: AgentImageTag + "@" + digestA, ResolvedFrom: "ghcr.io/grinco/vornik-agent:sha-abc123abc123",
	})
	rec.Note(ObtainedImage{Tag: "localhost/vornik-broker:latest", Method: MethodBuilt, Reference: eeHead})
	if err := rec.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := LoadObtained(path)
	if err != nil {
		t.Fatalf("LoadObtained: %v", err)
	}
	got, ok := back.MethodFor(AgentImageTag)
	if !ok {
		t.Fatal("the agent image was not recorded")
	}
	if got.Method != MethodPulled {
		t.Errorf("Method = %q, want %q", got.Method, MethodPulled)
	}
	if got.ResolvedFrom == "" {
		t.Error("ResolvedFrom must survive: keeping what we resolved next to what we got is " +
			"what makes a moved GHCR tag detectable afterwards (§S2.2)")
	}
	if got.At == "" {
		t.Error("At must be stamped")
	}
}

// The whole point of C5: pulled and built are distinguishable, which no
// inspection of the image can tell you.
func TestPulledAndBuiltAreDistinguishable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obtained.json")
	rec, _ := LoadObtained(path)

	rec.Note(ObtainedImage{Tag: AgentImageTag, Method: MethodBuilt, Reference: eeHead})
	if got, _ := rec.MethodFor(AgentImageTag); got.Method != MethodBuilt {
		t.Fatalf("Method = %q, want built", got.Method)
	}

	// The same tag, later pulled. The record must move, not accumulate.
	rec.Note(ObtainedImage{Tag: AgentImageTag, Method: MethodPulled, Reference: AgentImageTag + "@" + digestA})
	if len(rec.Images) != 1 {
		t.Errorf("got %d entries for one tag, want 1 — the latest observation is the true one", len(rec.Images))
	}
	if got, _ := rec.MethodFor(AgentImageTag); got.Method != MethodPulled {
		t.Errorf("Method = %q, want pulled", got.Method)
	}
}

// Corrupt is not absent. A host whose record cannot be parsed must not be told
// it simply has none — that is the conflation ErrRecordAbsent exists to prevent
// for the release record, and it is the same mistake here.
func TestCorruptObtainedRecordIsNotAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obtained.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadObtained(path); err == nil {
		t.Fatal("a corrupt record read as success")
	} else if !IsCorrupt(err) {
		t.Errorf("err = %v, want a corrupt-record error so callers cannot handle it "+
			"through the absent path", err)
	}
}

func TestUnknownObtainedVersionIsCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obtained.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"images":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObtained(path); !IsCorrupt(err) {
		t.Errorf("err = %v, want corrupt — a newer schema may mean something different by the "+
			"same field names, and guessing is how a check starts comparing the wrong things", err)
	}
}

func TestObtainedPathHonoursTheEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv(ObtainedPathEnv, want)
	if got := DefaultObtainedPath(); got != want {
		t.Errorf("DefaultObtainedPath() = %q, want %q — the CE quickstart and the EE package "+
			"do not share a data directory, and a record written where nothing reads it is "+
			"worse than none", got, want)
	}
}

func TestCommitTagShape(t *testing.T) {
	got := CommitTag(AgentImageTag, "a8324f170a0f2016c7af94b876f23d7cfd6f607f")
	want := "ghcr.io/grinco/vornik-agent:sha-a8324f170a0f"
	if got != want {
		t.Errorf("CommitTag = %q, want %q — this must match publish-agent-image.yml's "+
			"${GITHUB_SHA::12}, or resolution finds nothing and every update builds", got, want)
	}
}
