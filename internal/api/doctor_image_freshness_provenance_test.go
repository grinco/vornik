package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/imagemanifest"
)

// Contract C5's doctor half (design §S2.4): the host says how it obtained each
// image, because nothing about the image itself can tell you afterwards.

func TestProvenanceSummaryCountsBothMethods(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obtained.json")
	t.Setenv(imagemanifest.ObtainedPathEnv, path)

	rec, _ := imagemanifest.LoadObtained(path)
	rec.Note(imagemanifest.ObtainedImage{Tag: imagemanifest.AgentImageTag, Method: imagemanifest.MethodPulled})
	rec.Note(imagemanifest.ObtainedImage{Tag: "localhost/vornik-broker:latest", Method: imagemanifest.MethodBuilt})
	rec.Note(imagemanifest.ObtainedImage{Tag: "localhost/vornik-scraper:latest", Method: imagemanifest.MethodBuilt})
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}

	got := obtainProvenanceSummary()
	if !strings.Contains(got, "1 pulled") || !strings.Contains(got, "2 built") {
		t.Errorf("summary = %q, want 1 pulled and 2 built", got)
	}
}

// A host with no record is NORMAL — every install predating Stage 2 — and must
// not be reported as a fault or guessed at.
func TestNoProvenanceRecordIsSilent(t *testing.T) {
	t.Setenv(imagemanifest.ObtainedPathEnv, filepath.Join(t.TempDir(), "absent.json"))

	if got := obtainProvenanceSummary(); got != "" {
		t.Errorf("summary = %q, want empty — absent is a normal state, not a finding", got)
	}
}

// Corrupt is NOT absent, and must be said out loud: the host believes it is
// recording provenance and is not.
func TestCorruptProvenanceIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obtained.json")
	t.Setenv(imagemanifest.ObtainedPathEnv, path)
	if err := writeCorrupt(path); err != nil {
		t.Fatal(err)
	}

	got := obtainProvenanceSummary()
	if !strings.Contains(got, "unreadable") {
		t.Errorf("summary = %q, want it to name the unreadable record — silence here is the "+
			"'examined and clean' vs 'never examined' conflation", got)
	}
}

func writeCorrupt(path string) error {
	return os.WriteFile(path, []byte("{not json"), 0o644)
}
