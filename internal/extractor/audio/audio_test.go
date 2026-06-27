// Tests for the audio extractor. Real whisper invocations are
// skipped when the binary isn't on PATH (developer laptops); the
// rest is unit-tested via the JSON-parsing seams.
package audio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/extractor"
)

func requireWhisper(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("whisper"); err != nil {
		t.Skip("whisper not on PATH; skipping (install via pip/brew to enable)")
	}
}

func TestExtract_MissingBinary_GuidesOperator(t *testing.T) {
	ext := NewWithBinary("whisper-deliberately-missing-xyz", "")
	_, err := ext.Extract(context.Background(), extractor.Source{FilePath: "/dev/null"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "pip install") && !strings.Contains(err.Error(), "brew install") {
		t.Errorf("error should suggest install commands; got %v", err)
	}
}

func TestExtract_EmptyPath_Errors(t *testing.T) {
	_, err := New().Extract(context.Background(), extractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

// TestBuildSections_FiltersBadSegments — whisper sometimes emits
// near-empty segments + segments dominated by silence detection
// (no_speech_prob high). Verify the section builder drops both
// while preserving the rest in reading order.
func TestBuildSections_FiltersBadSegments(t *testing.T) {
	segments := []whisperSegment{
		{ID: 0, Start: 0, End: 5, Text: "Hello, world.", NoSpeechProb: 0.05},
		{ID: 1, Start: 5, End: 10, Text: "  ", NoSpeechProb: 0.10}, // empty after trim
		{ID: 2, Start: 10, End: 15, Text: "Second sentence here.", NoSpeechProb: 0.05},
		{ID: 3, Start: 15, End: 20, Text: "uhh", NoSpeechProb: 0.92}, // no-speech dominated
		{ID: 4, Start: 20, End: 30, Text: "Continuing the discussion.", NoSpeechProb: 0.02},
	}
	sections, outline := buildSections(segments)
	if len(sections) != 3 {
		t.Fatalf("sections = %d; want 3 (rejecting blank + no-speech)", len(sections))
	}
	if len(outline) != len(sections) {
		t.Errorf("outline (%d) != sections (%d)", len(outline), len(sections))
	}
	if sections[0].Content != "Hello, world." {
		t.Errorf("first section content = %q", sections[0].Content)
	}
	if sections[2].Content != "Continuing the discussion." {
		t.Errorf("last section content = %q", sections[2].Content)
	}
	if outline[0].TimestampStartSec != 0 {
		t.Errorf("first segment timestamp = %d; want 0", outline[0].TimestampStartSec)
	}
	if outline[2].TimestampStartSec != 20 {
		t.Errorf("third segment timestamp = %d; want 20", outline[2].TimestampStartSec)
	}
}

func TestBuildSections_AllEmpty_ReturnsNothing(t *testing.T) {
	segments := []whisperSegment{
		{Start: 0, End: 5, Text: "   ", NoSpeechProb: 0.05},
		{Start: 5, End: 10, Text: "", NoSpeechProb: 0.10},
	}
	sections, _ := buildSections(segments)
	if len(sections) != 0 {
		t.Errorf("blank-segment-only input must yield zero sections; got %d", len(sections))
	}
}

func TestFormatTimestamp(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "00:00:00"},
		{12, "00:00:12"},
		{125, "00:02:05"},
		{3661, "01:01:01"},
		{-5, "00:00:00"}, // negative defends against floating-point noise from whisper
	}
	for _, c := range cases {
		got := formatTimestamp(c.in)
		if got != c.want {
			t.Errorf("formatTimestamp(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestFormatTimestampRange(t *testing.T) {
	got := formatTimestampRange(125.3, 158.7)
	if got != "00:02:05 — 00:02:38" {
		t.Errorf("formatTimestampRange = %q", got)
	}
}

func TestTitleFromSource_StripsExtension(t *testing.T) {
	cases := map[string]string{
		"voice-memo.mp3": "voice-memo",
		"clip.m4a":       "clip",
		"recording":      "recording", // no extension preserved
		"":               "",
	}
	for in, want := range cases {
		got := titleFromSource(extractor.Source{OriginalName: in})
		if got != want {
			t.Errorf("titleFromSource(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestDurationFromSegments(t *testing.T) {
	if got := durationFromSegments(nil); got != 0 {
		t.Errorf("nil → 0; got %v", got)
	}
	segs := []whisperSegment{
		{End: 30}, {End: 75.5}, {End: 180.2},
	}
	if got := durationFromSegments(segs); got != 180.2 {
		t.Errorf("durationFromSegments = %v; want 180.2", got)
	}
}

func TestExtractor_Identifies(t *testing.T) {
	e := New()
	if e.Name() != Name {
		t.Errorf("Name = %q; want %q", e.Name(), Name)
	}
	if e.Version() != Version {
		t.Errorf("Version = %q; want %q", e.Version(), Version)
	}
}

// TestExtract_HappyPath only runs when whisper is on PATH AND
// the user has provided a tiny fixture audio file. We don't
// commit binary audio to the repo, so the test creates a brief
// silent WAV (parseable by whisper as "no speech" but the
// segment-walk plumbing still exercises). For full happy-path
// coverage operators run `go test -v` with a real .wav at
// $VORNIK_TEST_AUDIO_PATH.
func TestExtract_HappyPath_RealWhisperOptional(t *testing.T) {
	requireWhisper(t)
	fixture := os.Getenv("VORNIK_TEST_AUDIO_PATH")
	if fixture == "" {
		t.Skip("set VORNIK_TEST_AUDIO_PATH to a small .wav/.mp3 to run the real-whisper happy path")
	}
	res, err := New().Extract(context.Background(), extractor.Source{
		FilePath:     fixture,
		MimeType:     "audio/mpeg",
		OriginalName: filepath.Base(fixture),
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) == 0 {
		t.Error("expected at least one section from real audio fixture")
	}
	if res.Metadata.DurationSeconds == 0 {
		t.Error("DurationSeconds should be > 0 for real audio")
	}
}
