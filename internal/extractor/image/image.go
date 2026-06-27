// Package image implements the image extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 5).
//
// Approach: pure-Go header decode for dimensions + format (uses
// stdlib image package which already supports jpeg/png/gif), then
// optionally shell out to tesseract for OCR when the binary is on
// PATH. Tesseract is the long-standing open-source OCR engine;
// when missing, the extractor degrades gracefully — operators
// still get an image-with-metadata entry indexed into memory,
// they just don't get the recognised text layer.
//
// Why no EXIF in v1: reading EXIF requires a separate dependency
// (stdlib's image/jpeg skips APP1 segments entirely). Most useful
// image content the operator uploads is whiteboards / screenshots
// / diagrams; for those, OCR text is far more valuable than camera
// metadata. EXIF is a Phase-5b lift.
//
// Why no vision-model captions: those require an LLM call per
// image. The design doc puts that behind a per-project opt-in
// budget (§11 Phase 5 bullet 3); Phase-5 ships the deterministic
// floor only.
package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // stdlib decoder side-effect import
	_ "image/jpeg" // stdlib decoder side-effect import
	_ "image/png"  // stdlib decoder side-effect import
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vornik.io/vornik/internal/extractor"
)

const (
	Name    = "vornik-extract-image"
	Version = "0.1.0"

	// maxImageBytes caps the per-image read. 32 MiB covers any
	// reasonable scanned/photographed input; a multi-GB raw image
	// from a DSLR is rejected here so the daemon doesn't OOM on
	// stdlib image.DecodeConfig (which fully reads the header
	// before returning size).
	maxImageBytes = 32 << 20

	// defaultOCRPageTimeout bounds a SINGLE tesseract invocation
	// (one image == one OCR "page"). Independent of the parent
	// extraction context so a per-page hang can't consume the whole
	// extraction budget — an attacker crafting an image that makes
	// tesseract spin is capped here, per page, not per document.
	// batch-3 ingress/untrusted-input: document-extraction hardening (d).
	// 120s is generous for a single dense scanned page; real OCR of
	// a screenshot/whiteboard finishes in well under a second.
	defaultOCRPageTimeout = 120 * time.Second
)

// New returns the default extractor. OCR is attempted when
// tesseract is on PATH; absence is a non-fatal degradation
// (extraction still produces a metadata-only section).
func New() *Extractor { return &Extractor{} }

// NewWithTesseractBinary lets tests inject a different binary
// path / a stub script that fakes the OCR contract.
func NewWithTesseractBinary(path string) *Extractor {
	return &Extractor{tesseractPath: path}
}

// Extractor implements extractor.Extractor for image files.
type Extractor struct {
	// tesseractPath overrides the PATH lookup for tesseract.
	// Empty means "look up `tesseract` on PATH at extract time".
	tesseractPath string

	// ocrPageTimeout bounds a single OCR invocation. Zero falls back
	// to defaultOCRPageTimeout. Tests inject a tiny value to exercise
	// the per-page-deadline guard without sleeping.
	ocrPageTimeout time.Duration
}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract reads the image at src.FilePath, decodes its dimensions
// via stdlib (jpeg/png/gif), and optionally OCRs it with
// tesseract. Returns a single-section Result whose content is
// human-readable text the chunker + embedder both work cleanly
// with.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("image: source file path is empty")
	}

	f, err := os.Open(src.FilePath)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Stat first so an enormous file fails fast instead of
	// streaming gigabytes through image.DecodeConfig.
	st, err := f.Stat()
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: stat: %w", err)
	}
	if st.Size() > maxImageBytes {
		return extractor.Result{}, fmt.Errorf("image: file size %d exceeds cap %d", st.Size(), maxImageBytes)
	}

	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return extractor.Result{}, fmt.Errorf("image: decode header (unsupported format or corrupt file): %w", err)
	}

	title := titleFromSource(src)
	body := strings.Builder{}
	fmt.Fprintf(&body, "Image: %s\n", title)
	fmt.Fprintf(&body, "Format: %s\n", format)
	fmt.Fprintf(&body, "Dimensions: %d × %d pixels\n", cfg.Width, cfg.Height)
	fmt.Fprintf(&body, "File size: %d bytes\n", st.Size())

	// OCR pass — best-effort. tesseract missing is logged at
	// the wrapper level (the registry's pre-flight check); the
	// extractor itself just appends an OCR section when text
	// comes back.
	ocrText, ocrErr := e.runOCR(ctx, src.FilePath)
	switch {
	case ocrErr == nil && ocrText != "":
		body.WriteString("\n## Recognised text (tesseract OCR)\n\n")
		body.WriteString(ocrText)
	case errors.Is(ocrErr, errOCRUnavailable):
		body.WriteString("\n(OCR not available: tesseract is not installed on the daemon host)")
	case ocrErr != nil:
		fmt.Fprintf(&body, "\n(OCR failed: %s)", ocrErr.Error())
	default:
		// ocrErr == nil && ocrText == "" — OCR ran but produced
		// no text. Common for icon-like images.
		body.WriteString("\n(OCR ran but recognised no text)")
	}

	content := strings.TrimSpace(body.String())
	section := extractor.Section{
		SectionID: "001-image",
		Title:     title,
		Content:   content,
	}
	outline := extractor.OutlineEntry{
		SectionID: section.SectionID,
		Title:     title,
		Depth:     0,
		TextBytes: len(content),
	}

	return extractor.Result{
		Metadata: extractor.Metadata{
			Title: title,
			Extra: map[string]string{
				"format":     format,
				"width":      fmt.Sprintf("%d", cfg.Width),
				"height":     fmt.Sprintf("%d", cfg.Height),
				"size_bytes": fmt.Sprintf("%d", st.Size()),
				"ocr_engine": ocrEngineLabel(ocrErr),
			},
		},
		Outline:  []extractor.OutlineEntry{outline},
		Sections: []extractor.Section{section},
	}, nil
}

// errOCRUnavailable signals that tesseract isn't installed. The
// extractor uses it to switch on a "OCR not available" footer in
// the section content without dropping the rest of the metadata.
var errOCRUnavailable = errors.New("tesseract not available")

// runOCR shells out to tesseract and returns the recognised text.
// Returns (errOCRUnavailable, "") when the binary is missing —
// caller treats that as a non-fatal degradation.
func (e *Extractor) runOCR(ctx context.Context, imagePath string) (string, error) {
	binary := e.tesseractPath
	if binary == "" {
		binary = "tesseract"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return "", errOCRUnavailable
	}

	// Per-page OCR deadline (hardening (d)): bound this single
	// invocation independently of the parent extraction context so a
	// hanging/spinning tesseract can't run until the whole-extraction
	// budget elapses. CommandContext kills the process when ctx
	// fires.
	timeout := e.ocrPageTimeout
	if timeout <= 0 {
		timeout = defaultOCRPageTimeout
	}
	ocrCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// tesseract <input> stdout — writes recognised text to stdout
	// when the second arg is the literal "stdout". --psm 3 is the
	// default page-segmentation mode (fully automatic, no OSD); we
	// keep it explicit so a future per-project tuning surface has
	// a clean knob.
	cmd := exec.CommandContext(ocrCtx, resolved, imagePath, "stdout", "--psm", "3")
	// Run tesseract in its own process group and, on context fire,
	// SIGKILL the whole group — otherwise a child (e.g. a wrapper
	// shell's `sleep`) survives and keeps the stdout pipe open,
	// blocking Wait forever and defeating the per-page deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Negative pid = signal the entire process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Belt-and-suspenders: even if a grandchild keeps the pipe open,
	// don't block Wait more than a moment past the kill.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Distinguish a per-page-deadline kill from a genuine
		// tesseract error so the operator-visible footer is honest
		// about which guard fired.
		if ocrCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("tesseract: per-page OCR timeout exceeded (%s)", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tesseract: %s", msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ocrEngineLabel renders a small "ocr_engine" metadata tag the
// document-detail UI can surface. Lets the operator see at a
// glance which images got OCR vs which didn't, without scrolling
// to find the "(OCR not available)" footer.
func ocrEngineLabel(ocrErr error) string {
	switch {
	case errors.Is(ocrErr, errOCRUnavailable):
		return "none (tesseract missing)"
	case ocrErr != nil:
		return "failed"
	default:
		return "tesseract"
	}
}

// titleFromSource derives the image title from the
// operator-visible filename, extension stripped. Falls back to
// "image" when no filename is available.
func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" && src.FilePath != "" {
		name = filepath.Base(src.FilePath)
	}
	if name == "" {
		return "image"
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}
