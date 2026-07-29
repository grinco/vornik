// Package video implements the video extractor for the document pipeline.
//
// Video needs no video-capable model, because it decomposes into the two
// modalities the system already handles: sampled keyframes are images, and
// the audio track is speech that whisper turns into text. That is the whole
// design — one mechanism, reused, rather than a third special case.
//
// Approach: shell out to ffprobe (metadata) and ffmpeg (frame sampling),
// matching the audio extractor's whisper shell-out and the PDF extractor's
// poppler shell-out. A CGO binding would conflict with the daemon's
// CGO_ENABLED=0 posture and lag upstream releases. Neither binary is a new
// deployment dependency: voice already requires ffmpeg
// (voice.stt.ffmpeg_path), and ffprobe ships in the same package.
//
// Frame sampling is uniform-interval, not scene-change detection. Uniform is
// predictable and cheap, and adequate for "what is this video about"; the
// cost is that anything shorter than the interval may appear in no frame,
// which is why the frame-manifest section says so in as many words rather
// than leaving a reader to assume the frames are exhaustive.
//
// see LLD § https://docs.vornik.io §4.6
package video

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/extractor"
)

// Name identifies this extractor on every extracted_documents row.
const Name = "vornik-extract-video"

// Version is bumped when the extraction logic changes enough to justify
// re-running over historical artifacts.
const Version = "0.1.0"

const (

	// defaultMaxFrames / defaultMinIntervalSeconds bound a sampling run.
	// Eight frames covers a short clip's arc without turning a one-hour
	// recording into a flipbook; the interval floor stops a 10-second clip
	// from producing eight near-identical frames.
	defaultMaxFrames          = 8
	defaultMinIntervalSeconds = 5

	sectionMetadata = "001-metadata"
	sectionFrames   = "002-frames"
)

// Extractor implements extractor.Extractor for video files. Stateless
// across calls.
type Extractor struct {
	ffmpegPath  string // empty = look up "ffmpeg" on PATH
	ffprobePath string // empty = look up "ffprobe" on PATH
	maxFrames   int
	minInterval int
}

// New returns the default extractor: ffmpeg/ffprobe from PATH, built-in
// sampling bounds.
func New() *Extractor { return &Extractor{} }

// NewWithOptions lets the operator (and tests) override the binaries and
// the sampling bounds. Zero values fall back to the defaults.
func NewWithOptions(ffmpegPath, ffprobePath string, maxFrames, minIntervalSeconds int) *Extractor {
	return &Extractor{
		ffmpegPath:  ffmpegPath,
		ffprobePath: ffprobePath,
		maxFrames:   maxFrames,
		minInterval: minIntervalSeconds,
	}
}

// Name implements extractor.Extractor.
func (*Extractor) Name() string { return Name }

// Version implements extractor.Extractor.
func (*Extractor) Version() string { return Version }

// ffprobeOutput is the subset of `ffprobe -print_format json` we read.
type ffprobeOutput struct {
	Format struct {
		Duration   string `json:"duration"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		RFrameRate string `json:"r_frame_rate"`
	} `json:"streams"`
}

// Extract probes the video, samples keyframes, and returns metadata plus a
// frame manifest.
//
// It deliberately does NOT fail when a step degrades: a video whose frames
// could not be sampled still yields its metadata, because a partial answer
// an operator can see beats a failed task that explains nothing. What it
// will not do is imply coverage it does not have — the manifest states the
// interval and that visual timing cannot be cited precisely.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("video: source file path is empty")
	}
	ffprobe, err := e.resolve(e.ffprobePath, "ffprobe")
	if err != nil {
		return extractor.Result{}, err
	}

	probe, probeErr := runFFprobe(ctx, ffprobe, src.FilePath)
	if probeErr != nil {
		return extractor.Result{}, probeErr
	}

	meta, duration, hasAudio := metadataFromProbe(src, probe)

	interval, wanted := e.samplingPlan(duration)
	frames, frameErr := e.sampleFrames(ctx, src.FilePath, interval, wanted)
	if frameErr != nil {
		meta.Extra["frame_sampling_error"] = frameErr.Error()
	}
	meta.Extra["frames_sampled"] = strconv.Itoa(len(frames))
	meta.Extra["frame_interval_seconds"] = strconv.Itoa(interval)

	sections := []extractor.Section{{
		SectionID: sectionMetadata,
		Title:     meta.Title,
		Content:   renderMetadata(meta, hasAudio),
	}, {
		SectionID: sectionFrames,
		Title:     "Sampled frames",
		Content:   renderFrameManifest(frames, interval, frameErr),
	}}
	outline := []extractor.OutlineEntry{
		{SectionID: sectionMetadata, Title: meta.Title, Depth: 0, TextBytes: len(sections[0].Content)},
		{SectionID: sectionFrames, Title: "Sampled frames", Depth: 0, TextBytes: len(sections[1].Content)},
	}

	files := make([]extractor.ProducedFile, 0, len(frames))
	for _, f := range frames {
		files = append(files, extractor.ProducedFile{RelPath: f.name, Content: f.content})
	}

	return extractor.Result{
		Metadata: meta,
		Outline:  outline,
		Sections: sections,
		Files:    files,
	}, nil
}

// resolve looks up a binary, returning an error that tells the operator how
// to install it rather than a bare "not found".
func (e *Extractor) resolve(configured, fallback string) (string, error) {
	binary := configured
	if binary == "" {
		binary = fallback
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("video: %s not found on PATH (install ffmpeg — the voice subsystem already requires it): %w", binary, err)
	}
	return resolved, nil
}

// samplingPlan converts a duration into (interval, frame count), honouring
// the interval floor so a short clip does not yield near-identical frames.
func (e *Extractor) samplingPlan(durationSeconds float64) (interval, frames int) {
	maxFrames := e.maxFrames
	if maxFrames <= 0 {
		maxFrames = defaultMaxFrames
	}
	minInterval := e.minInterval
	if minInterval <= 0 {
		minInterval = defaultMinIntervalSeconds
	}
	if durationSeconds <= 0 {
		// Unknown duration: take the floor interval and let the frame cap
		// bound the run.
		return minInterval, maxFrames
	}
	interval = int(durationSeconds) / maxFrames
	if interval < minInterval {
		interval = minInterval
	}
	frames = int(durationSeconds)/interval + 1
	if frames > maxFrames {
		frames = maxFrames
	}
	if frames < 1 {
		frames = 1
	}
	return interval, frames
}

type frame struct {
	name    string
	content []byte
}

// sampleFrames runs one ffmpeg pass writing JPEG frames at a fixed rate
// into a temp dir, then reads them back as bytes for the Runner to persist.
func (e *Extractor) sampleFrames(ctx context.Context, path string, interval, wanted int) ([]frame, error) {
	ffmpeg, err := e.resolve(e.ffmpegPath, "ffmpeg")
	if err != nil {
		return nil, err
	}
	outDir, err := os.MkdirTemp("", "vornik-video-frames-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(outDir) }()

	args := []string{
		"-nostdin", "-loglevel", "error",
		"-i", path,
		"-vf", fmt.Sprintf("fps=1/%d", interval),
		"-frames:v", strconv.Itoa(wanted),
		"-q:v", "3",
		filepath.Join(outDir, "frame-%03d.jpg"),
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("ffmpeg frame sampling failed: %s", msg)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil {
		return nil, fmt.Errorf("read frame dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".jpg") {
			names = append(names, ent.Name())
		}
	}
	// Deterministic order: the manifest's frame numbering must match the
	// timestamps it claims.
	sort.Strings(names)

	out := make([]frame, 0, len(names))
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(outDir, n))
		if err != nil {
			continue
		}
		out = append(out, frame{name: n, content: data})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no frames")
	}
	return out, nil
}

func renderMetadata(meta extractor.Metadata, hasAudio bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Video: %s\n", meta.Title)
	keys := make([]string, 0, len(meta.Extra))
	for k := range meta.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\n", k, meta.Extra[k])
	}
	if !hasAudio {
		b.WriteString("\nThis video has no audio track, so there is no speech to transcribe.\n")
	}
	return b.String()
}

// renderFrameManifest lists the frames and states the limits of what they
// cover.
//
// The honesty is the feature. An agent asked "what happens at the end of
// this video" will otherwise answer from the last SAMPLED frame as though it
// were the last frame — the same confident-but-unfounded shape as describing
// an image nobody looked at.
func renderFrameManifest(frames []frame, interval int, sampleErr error) string {
	var b strings.Builder
	if len(frames) == 0 {
		b.WriteString("No frames were sampled from this video")
		if sampleErr != nil {
			fmt.Fprintf(&b, " (%s)", sampleErr.Error())
		}
		b.WriteString(".\nNothing about the video's visual content has been established.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d frames sampled at a uniform %d-second interval, stored under files/:\n\n", len(frames), interval)
	for i, f := range frames {
		fmt.Fprintf(&b, "- files/%s — approximately %s into the video\n", f.name, formatOffset(i*interval))
	}
	fmt.Fprintf(&b, `
IMPORTANT — what these frames do and do not show. Sampling is UNIFORM, not
scene-aware: anything shorter than %d seconds may appear in no frame at all,
and visual timing cannot be cited more precisely than the interval. Do not
describe the video's ending from the last sampled frame, and do not state
that something never appears merely because no frame shows it. The audio
track (transcribed separately when present) carries speech timing; the
frames do not carry visual timing.
`, interval)
	return b.String()
}

func formatOffset(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
}

func runFFprobe(ctx context.Context, ffprobe, path string) (*ffprobeOutput, error) {
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format", "-show_streams",
		path,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("video: ffprobe failed: %s", msg)
	}
	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("video: parse ffprobe JSON: %w", err)
	}
	return &out, nil
}

func parseSeconds(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

// parseFrameRate converts ffprobe's "30000/1001" rational into fps.
func parseFrameRate(s string) float64 {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return 0
	}
	num, err1 := strconv.ParseFloat(parts[0], 64)
	den, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || den == 0 {
		return 0
	}
	return num / den
}

func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" {
		name = filepath.Base(src.FilePath)
	}
	if ext := filepath.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	if name == "" {
		return "video"
	}
	return name
}

// metadataFromProbe folds an ffprobe result into extractor.Metadata,
// returning the duration and whether an audio track exists — both of which
// drive decisions the caller makes (sampling plan, and whether to say there
// is no speech to transcribe).
func metadataFromProbe(src extractor.Source, probe *ffprobeOutput) (extractor.Metadata, float64, bool) {
	meta := extractor.Metadata{
		Title: titleFromSource(src),
		Extra: map[string]string{},
	}
	duration := parseSeconds(probe.Format.Duration)
	if duration > 0 {
		meta.DurationSeconds = int(duration)
		meta.Extra["duration_seconds"] = strconv.Itoa(int(duration))
	}
	if probe.Format.FormatName != "" {
		meta.Extra["format"] = probe.Format.FormatName
	}
	var hasAudio bool
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if s.Width > 0 && s.Height > 0 {
				meta.Extra["width"] = strconv.Itoa(s.Width)
				meta.Extra["height"] = strconv.Itoa(s.Height)
			}
			if s.CodecName != "" {
				meta.Extra["video_codec"] = s.CodecName
			}
			if fps := parseFrameRate(s.RFrameRate); fps > 0 {
				meta.Extra["fps"] = strconv.FormatFloat(fps, 'f', -1, 64)
			}
		case "audio":
			hasAudio = true
			if s.CodecName != "" {
				meta.Extra["audio_codec"] = s.CodecName
			}
		}
	}
	if hasAudio {
		meta.Extra["has_audio_track"] = "true"
	}
	return meta, duration, hasAudio
}
