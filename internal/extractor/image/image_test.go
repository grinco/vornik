// Tests for the image extractor. We generate fixture images
// inline (1×1 PNG / JPEG) so the happy path runs without
// committing binary blobs to the repo; OCR is exercised via
// a stub binary on operator-controlled PATH.
package image

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	swarmextractor "vornik.io/vornik/internal/extractor"
)

func writePNGFixture(t *testing.T, w, h int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.png")
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func writeJPEGFixture(t *testing.T, w, h int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jpg")
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 50}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtract_PNG_HappyPath_NoOCR(t *testing.T) {
	// Force OCR-unavailable by pointing at a missing binary.
	ext := NewWithTesseractBinary("tesseract-deliberately-missing-xyz")
	path := writePNGFixture(t, 64, 32)
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/png",
		OriginalName: "diagram.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(res.Sections) != 1 {
		t.Fatalf("sections = %d; want 1", len(res.Sections))
	}
	s := res.Sections[0]
	if !strings.Contains(s.Content, "Format: png") {
		t.Errorf("section content missing format: %q", s.Content)
	}
	if !strings.Contains(s.Content, "64 × 32 pixels") {
		t.Errorf("dimensions not surfaced: %q", s.Content)
	}
	if !strings.Contains(s.Content, "OCR not available") {
		t.Errorf("missing OCR-unavailable note: %q", s.Content)
	}
	if res.Metadata.Title != "diagram" {
		t.Errorf("Title = %q; want \"diagram\"", res.Metadata.Title)
	}
	if res.Metadata.Extra["format"] != "png" {
		t.Errorf("metadata.format = %q", res.Metadata.Extra["format"])
	}
	if res.Metadata.Extra["ocr_engine"] != "none (tesseract missing)" {
		t.Errorf("ocr_engine tag = %q", res.Metadata.Extra["ocr_engine"])
	}
}

func TestExtract_JPEG_DecodesDimensions(t *testing.T) {
	ext := NewWithTesseractBinary("tesseract-missing")
	path := writeJPEGFixture(t, 200, 100)
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/jpeg",
		OriginalName: "photo.jpg",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Metadata.Extra["format"] != "jpeg" {
		t.Errorf("format = %q; want jpeg", res.Metadata.Extra["format"])
	}
	if res.Metadata.Extra["width"] != "200" || res.Metadata.Extra["height"] != "100" {
		t.Errorf("dimensions metadata = %+v", res.Metadata.Extra)
	}
}

func TestExtract_NonImage_Errors(t *testing.T) {
	// Random bytes that aren't a valid image header.
	dir := t.TempDir()
	path := filepath.Join(dir, "junk.png")
	if err := os.WriteFile(path, []byte("this is not an image"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := NewWithTesseractBinary("missing").Extract(context.Background(), swarmextractor.Source{FilePath: path})
	if err == nil {
		t.Fatal("expected error on non-image input")
	}
}

func TestExtract_EmptyPath_Errors(t *testing.T) {
	_, err := New().Extract(context.Background(), swarmextractor.Source{})
	if err == nil {
		t.Fatal("expected error for empty FilePath")
	}
}

func TestExtract_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.png")
	if err := os.WriteFile(path, make([]byte, maxImageBytes+1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := New().Extract(context.Background(), swarmextractor.Source{FilePath: path})
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("expected cap-exceeded error; got %v", err)
	}
}

// TestExtract_OCR_StubBinary verifies that when a tesseract-like
// binary IS available, the extractor pipes its stdout into the
// section content. We use a tiny shell script as the stub so the
// test runs on every host regardless of tesseract installation.
func TestExtract_OCR_StubBinary(t *testing.T) {
	// Write a fake tesseract that ignores args and prints a
	// deterministic line on stdout. Bash isn't guaranteed but
	// /bin/sh is on every supported daemon host.
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fake-tesseract")
	body := "#!/bin/sh\necho 'WHITEBOARD: design sketch'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	path := writePNGFixture(t, 32, 32)
	ext := NewWithTesseractBinary(script)
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath:     path,
		MimeType:     "image/png",
		OriginalName: "whiteboard.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	body0 := res.Sections[0].Content
	if !strings.Contains(body0, "## Recognised text (tesseract OCR)") {
		t.Errorf("missing OCR section header: %q", body0)
	}
	if !strings.Contains(body0, "WHITEBOARD: design sketch") {
		t.Errorf("OCR stdout not folded into content: %q", body0)
	}
	if res.Metadata.Extra["ocr_engine"] != "tesseract" {
		t.Errorf("ocr_engine = %q", res.Metadata.Extra["ocr_engine"])
	}
}

// TestExtract_OCR_StubProducesEmpty — when tesseract runs but
// returns no text (small icons / blank images), the extractor
// must still produce a valid section; the OCR footer should
// indicate "no text recognised" rather than failing the whole
// extraction.
func TestExtract_OCR_StubProducesEmpty(t *testing.T) {
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "fake-empty-tesseract")
	body := "#!/bin/sh\necho ''\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	path := writePNGFixture(t, 16, 16)
	res, err := NewWithTesseractBinary(script).Extract(context.Background(), swarmextractor.Source{
		FilePath: path, OriginalName: "icon.png",
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(res.Sections[0].Content, "OCR ran but recognised no text") {
		t.Errorf("empty-OCR footer missing: %q", res.Sections[0].Content)
	}
	if res.Metadata.Extra["ocr_engine"] != "tesseract" {
		t.Errorf("ocr_engine = %q", res.Metadata.Extra["ocr_engine"])
	}
}

// TestExtract_OCR_PerPageTimeout — batch-3 ingress/untrusted-input:
// document-extraction hardening (d). A hanging OCR invocation must
// be bounded by a per-page deadline, not run until the whole
// extraction's (much larger) budget elapses. We stub tesseract with
// a script that sleeps far longer than the per-page timeout; the
// extractor must abort the OCR and surface a failure footer while
// still producing a valid metadata section. Pre-fix the OCR call
// uses the parent context only and would block for the full sleep.
func TestExtract_OCR_PerPageTimeout(t *testing.T) {
	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "slow-tesseract")
	// Sleep 30s — far beyond the tiny per-page timeout we inject.
	body := "#!/bin/sh\nsleep 30\necho 'too late'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	ext := NewWithTesseractBinary(script)
	ext.ocrPageTimeout = 200 * time.Millisecond // inject a tiny deadline

	path := writePNGFixture(t, 32, 32)
	start := time.Now()
	res, err := ext.Extract(context.Background(), swarmextractor.Source{
		FilePath: path, OriginalName: "slow.png",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Extract should degrade gracefully on OCR timeout, got: %v", err)
	}
	// Must have aborted well before the 30s sleep would finish.
	if elapsed > 5*time.Second {
		t.Fatalf("per-page OCR timeout not enforced; took %v", elapsed)
	}
	body0 := res.Sections[0].Content
	if !strings.Contains(body0, "OCR failed") {
		t.Errorf("expected OCR-failed footer after timeout; got: %q", body0)
	}
	if res.Metadata.Extra["ocr_engine"] != "failed" {
		t.Errorf("ocr_engine = %q; want failed", res.Metadata.Extra["ocr_engine"])
	}
}

func TestTitleFromSource(t *testing.T) {
	cases := map[string]string{
		"photo.jpg":      "photo",
		"WHITEBOARD.PNG": "WHITEBOARD",
		"":               "image",
	}
	for in, want := range cases {
		got := titleFromSource(swarmextractor.Source{OriginalName: in})
		if got != want {
			t.Errorf("titleFromSource(%q) = %q; want %q", in, got, want)
		}
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
