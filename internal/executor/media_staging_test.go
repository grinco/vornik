package executor

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression for T-1df3 (task_20260729005109_669509a3c07a1df3, 2026-07-29).
//
// A user sent a photo over Telegram and asked what was in it. The image
// extractor ran, produced 115 bytes of OCR-plus-EXIF metadata, and recorded
// an inputExtractions entry — which made the staging skip drop the 218 KB of
// actual pixels. The agent runtime's image detection reads staged paths, so
// with nothing staged it could never fire, and the task returned an honest
// "no vision-capable tool is available and the raw pixels are not staged".
//
// Extraction is only a substitute for the original when the original is a
// document. For media it is a lossy derivative and the raw bytes must ALSO
// be staged.
//
// see LLD § https://docs.vornik.io §4.2
func TestExtractTaskInputArtifacts_MediaStagedDespiteExtraction(t *testing.T) {
	// T-1df3's payload shape, verbatim in structure.
	payload := []byte(`{"context":{
		"inputFiles":["/var/lib/vornik/artifacts/assistant/inputs/artifact_x/photo.jpg"],
		"inputExtractions":[{"extracted_document_id":"extdoc_x","section_count":1,"chunks_ingested":1}]
	}}`)
	got := extractTaskInputArtifacts(payload, 0, false, 0)
	if len(got) != 1 {
		t.Fatalf("an extracted IMAGE must still be staged (pixels are not in the extraction); got %v", got)
	}
	if got[0]["name"] != "photo.jpg" {
		t.Errorf("staged name = %q, want photo.jpg", got[0]["name"])
	}
}

// The other half of the contract, and the reason the original guard exists:
// staging a 32 MB EPUB next to its extraction let a worker file_read the
// binary and blow the chat-proxy cap (2026-05-21, T-fa9e / T-7f98 / T-8889).
// Documents must stay extraction-only. If this test ever fails, the fix
// above has reopened that incident.
func TestExtractTaskInputArtifacts_DocumentsStayExtractionOnly(t *testing.T) {
	for _, name := range []string{"book.epub", "report.pdf", "notes.md", "page.html", "sheet.xlsx"} {
		payload := []byte(`{"context":{
			"inputFiles":["/tmp/` + name + `"],
			"inputExtractions":[{"extracted_document_id":"doc-1"}]
		}}`)
		if got := extractTaskInputArtifacts(payload, 0, false, 0); got != nil {
			t.Errorf("%s: extracted document must NOT be staged, got %v", name, got)
		}
	}
}

func TestExtractTaskInputArtifacts_AudioAndVideoStagedDespiteExtraction(t *testing.T) {
	for _, name := range []string{"voice.opus", "meeting.mp3", "clip.mp4"} {
		payload := []byte(`{"context":{
			"inputFiles":["/tmp/` + name + `"],
			"inputExtractions":[{"extracted_document_id":"doc-1"}]
		}}`)
		got := extractTaskInputArtifacts(payload, 0, false, 0)
		if len(got) != 1 {
			t.Errorf("%s: extracted media must still be staged, got %v", name, got)
		}
	}
}

// An unclassifiable input keeps today's behaviour of being on disk: we
// cannot prove its extraction is sufficient, so we do not assume it.
func TestExtractTaskInputArtifacts_UnknownKindStaged(t *testing.T) {
	payload := []byte(`{"context":{
		"inputFiles":["/tmp/mystery.xyz"],
		"inputExtractions":[{"extracted_document_id":"doc-1"}]
	}}`)
	if got := extractTaskInputArtifacts(payload, 0, false, 0); len(got) != 1 {
		t.Fatalf("unclassifiable input should be staged, got %v", got)
	}
}

// The count-mismatch branch flags every basename as extracted. That must
// still hold for documents, and must NOT suppress media — the mismatch says
// nothing about whether pixels survived extraction.
func TestExtractTaskInputArtifacts_CountMismatchStillStagesMedia(t *testing.T) {
	payload := []byte(`{"context":{
		"inputFiles":["/tmp/a.pdf","/tmp/photo.png"],
		"inputExtractions":[{"extracted_document_id":"doc-1"}]
	}}`)
	got := extractTaskInputArtifacts(payload, 0, false, 0)
	if len(got) != 1 {
		t.Fatalf("expected only the image staged, got %v", got)
	}
	if got[0]["name"] != "photo.png" {
		t.Errorf("staged %q, want photo.png (the pdf must stay extraction-only)", got[0]["name"])
	}
}

// Over the cap the raw media file is skipped and the extraction stands
// alone. Unlike a document skip this is a degradation, so it is bounded by
// an explicit operator-set size rather than happening silently at any size.
func TestExtractTaskInputArtifacts_MediaOverCapSkipped(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "huge.jpg")
	if err := os.WriteFile(big, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "small.jpg")
	if err := os.WriteFile(small, make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"context":{
		"inputFiles":["` + big + `","` + small + `"],
		"inputExtractions":[{"extracted_document_id":"d1"},{"extracted_document_id":"d2"}]
	}}`)
	got := extractTaskInputArtifacts(payload, 1024, false, 0)
	if len(got) != 1 {
		t.Fatalf("expected only the under-cap image staged, got %v", got)
	}
	if got[0]["name"] != "small.jpg" {
		t.Errorf("staged %q, want small.jpg", got[0]["name"])
	}
}

// A cap of zero means unbounded — the config default must not accidentally
// suppress every image on a deployment that never set the key.
func TestExtractTaskInputArtifacts_ZeroCapIsUnbounded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(p, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"context":{
		"inputFiles":["` + p + `"],
		"inputExtractions":[{"extracted_document_id":"d1"}]
	}}`)
	if got := extractTaskInputArtifacts(payload, 0, false, 0); len(got) != 1 {
		t.Fatalf("zero cap must not suppress staging, got %v", got)
	}
}

// An unstattable media path is staged anyway: stageInputArtifacts is the
// component that actually reads bytes and already skips what it cannot
// read, so refusing here would drop a readable file over a transient stat
// error on a path this function only inspects by name.
func TestExtractTaskInputArtifacts_UnstattableMediaStillStaged(t *testing.T) {
	payload := []byte(`{"context":{
		"inputFiles":["/nonexistent/dir/photo.jpg"],
		"inputExtractions":[{"extracted_document_id":"d1"}]
	}}`)
	if got := extractTaskInputArtifacts(payload, 1024, false, 0); len(got) != 1 {
		t.Fatalf("stat failure must not drop the artifact, got %v", got)
	}
}

// Traversal safety property §4.2 relies on: the staged name is a sanitised
// basename, never a path that could escape artifacts/in/. Enforced here by
// filepath.Base and again by safepath in stageInputArtifacts.
func TestExtractTaskInputArtifacts_TraversalNameReducedToBasename(t *testing.T) {
	payload := []byte(`{"context":{"inputFiles":["/tmp/../../etc/passwd"]}}`)
	got := extractTaskInputArtifacts(payload, 0, false, 0)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %v", got)
	}
	if got[0]["name"] != "passwd" {
		t.Errorf("staged name = %q, want the sanitised basename %q", got[0]["name"], "passwd")
	}
}
