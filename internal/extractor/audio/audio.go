// Package audio implements the audio extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 4).
//
// Approach: shell out to OpenAI's whisper CLI. The model bundles
// well on Linux (homebrew / pip), and the operator host already
// has it installed (verified via `which whisper`). For per-
// segment timestamp + avg_logprob quality signals we use
// --output_format json which gives us a stable JSON shape.
//
// One section per whisper segment. Segments are ~30s by default;
// for an hour of audio that yields ~120 sections — chunky but
// still navigable via document_get_outline. Aggregating
// adjacent segments into 5-minute "chapters" is a Phase-4b
// quality lift; the v1 here keeps the contract simple.
//
// Why shell-out vs Go binding: faster-whisper-rs / whisper.cpp-go
// exist but lag the upstream model releases and bring CGO
// dependencies that conflict with the daemon's CGO_ENABLED=0
// posture. Shelling out matches what we do for PDF (poppler)
// and keeps the binary surface small.
package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
)

const (
	Name    = "vornik-extract-audio"
	Version = "0.1.0"

	// maxAudioSegments caps the per-document segment count. A
	// 24-hour audiobook at 30s per segment would produce ~2880
	// segments — manageable but the chunker downstream would
	// also have to ingest that many. 10000 is a generous cap;
	// real-world podcasts / meetings sit well below.
	maxAudioSegments = 10000

	// minSegmentChars filters out whisper's empty/near-empty
	// segments (silence detection noise). 4 chars covers single
	// utterances like "um" / "yes" while dropping pure-whitespace
	// segments.
	minSegmentChars = 4

	// noSpeechRejectThreshold is whisper's recommended cutoff for
	// "this segment is probably not speech". Above this, the
	// transcript is unreliable enough that including it pollutes
	// memory_search hits.
	noSpeechRejectThreshold = 0.6
)

// New returns the default extractor (uses whisper on PATH with
// the "turbo" model — fast, good-enough quality, no GPU required).
func New() *Extractor { return &Extractor{} }

// NewWithBinary lets tests inject a different binary path. Set
// modelOverride to use a non-default whisper model (e.g. "tiny"
// for tests, "large-v3" for production accuracy).
func NewWithBinary(binary, modelOverride string) *Extractor {
	return &Extractor{binaryPath: binary, model: modelOverride}
}

// Extractor implements extractor.Extractor for audio files via
// the whisper CLI. Stateless across calls.
type Extractor struct {
	binaryPath string // empty = look up "whisper" on PATH
	model      string // empty = "turbo" (whisper's default)
}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract runs whisper over src.FilePath and parses the JSON
// transcript into sections. Each whisper segment becomes one
// extractor.Section; the OutlineEntry carries the segment's
// timestamp so retrieval can cite "from the recording at
// 00:43:21" (matching the Phase-4 design's quoted retrieval
// shape).
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("audio: source file path is empty")
	}

	binary := e.binaryPath
	if binary == "" {
		binary = "whisper"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: %s not found on PATH (install via `pip install -U openai-whisper` or `brew install openai-whisper`): %w", binary, err)
	}

	// whisper writes its output files into --output_dir alongside
	// the source. Use a fresh temp dir so the output filename is
	// predictable (basename without extension + .json).
	outDir, err := os.MkdirTemp("", "vornik-whisper-*")
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(outDir) }()

	model := e.model
	if model == "" {
		model = "turbo"
	}

	args := []string{
		src.FilePath,
		"--model", model,
		"--output_format", "json",
		"--output_dir", outDir,
		// --verbose False suppresses the progress bar so we
		// don't spam the daemon log with per-segment text.
		"--verbose", "False",
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return extractor.Result{}, fmt.Errorf("audio: whisper failed: %s", msg)
	}

	// whisper writes <basename>.json into --output_dir.
	base := filepath.Base(src.FilePath)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	jsonPath := filepath.Join(outDir, base+".json")
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: read whisper JSON %q: %w (stderr: %s)", jsonPath, err, strings.TrimSpace(stderr.String()))
	}

	var out whisperOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return extractor.Result{}, fmt.Errorf("audio: parse whisper JSON: %w", err)
	}
	if len(out.Segments) > maxAudioSegments {
		return extractor.Result{}, fmt.Errorf("audio: %d segments exceeds cap %d", len(out.Segments), maxAudioSegments)
	}

	sections, outline := buildSections(out.Segments)
	if len(sections) == 0 {
		return extractor.Result{}, ErrNoSpeech
	}

	metadata := extractor.Metadata{
		Title:    titleFromSource(src),
		Language: strings.TrimSpace(out.Language),
	}
	if d := durationFromSegments(out.Segments); d > 0 {
		metadata.DurationSeconds = int(d)
	}

	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// ErrNoSpeech is returned when whisper produced zero usable
// segments — typically a silent file or one where every segment
// scored above the no_speech threshold. Callers may route to
// re-extraction with a different model or surface "no speech
// detected" to the operator.
var ErrNoSpeech = errors.New("audio: no speech segments above the quality threshold")

// whisperOutput is the subset of whisper's JSON schema we read.
// Fields we don't use are tolerated by encoding/json (default
// behaviour: ignore unknown).
type whisperOutput struct {
	Text     string           `json:"text"`
	Segments []whisperSegment `json:"segments"`
	Language string           `json:"language"`
}

// whisperSegment carries the per-segment metadata whisper emits.
// AvgLogProb / NoSpeechProb are the two quality signals we filter
// on; CompressionRatio is exposed by whisper but more useful for
// hallucination detection (Phase-4b work).
type whisperSegment struct {
	ID           int     `json:"id"`
	Start        float64 `json:"start"`
	End          float64 `json:"end"`
	Text         string  `json:"text"`
	AvgLogProb   float64 `json:"avg_logprob"`
	NoSpeechProb float64 `json:"no_speech_prob"`
}

// buildSections turns whisper segments into the extractor's
// section + outline shape. Filters out segments that fail the
// quality bar (empty text, dominated by non-speech) so the
// indexed text stays clean.
func buildSections(segments []whisperSegment) ([]extractor.Section, []extractor.OutlineEntry) {
	sections := make([]extractor.Section, 0, len(segments))
	outline := make([]extractor.OutlineEntry, 0, len(segments))
	for i, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if len(text) < minSegmentChars {
			continue
		}
		if seg.NoSpeechProb > noSpeechRejectThreshold {
			continue
		}
		sectionID := fmt.Sprintf("segment-%04d", i+1)
		title := formatTimestampRange(seg.Start, seg.End)
		sections = append(sections, extractor.Section{
			SectionID: sectionID,
			Title:     title,
			Content:   text,
		})
		outline = append(outline, extractor.OutlineEntry{
			SectionID:         sectionID,
			Title:             title,
			Depth:             0,
			TimestampStartSec: int(seg.Start),
			TextBytes:         len(text),
		})
	}
	return sections, outline
}

// durationFromSegments returns the end-timestamp of the last
// segment as the document's total duration. Whisper's JSON
// doesn't carry an explicit "duration" field at the top level,
// but segment.End on the last segment is reliably the total.
func durationFromSegments(segments []whisperSegment) float64 {
	if len(segments) == 0 {
		return 0
	}
	return segments[len(segments)-1].End
}

// formatTimestampRange renders a section title like "00:00:12 —
// 00:00:42". Operator-readable + memory-search-friendly: a query
// for "what was said around 12 minutes" can land on a section
// whose title carries the exact second.
func formatTimestampRange(startSec, endSec float64) string {
	return fmt.Sprintf("%s — %s", formatTimestamp(startSec), formatTimestamp(endSec))
}

func formatTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// titleFromSource derives the document title from the original
// filename, stripping the extension. Whisper doesn't expose
// embedded ID3 / metadata; pulling those would require a
// separate ffprobe pass we don't want in the v1 path.
func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" && src.FilePath != "" {
		name = filepath.Base(src.FilePath)
	}
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}
