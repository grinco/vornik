package dispatcher

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/registry"
)

// ingestedMarker is the exact phrase three other components match on:
// prompt.go's INBOUND ATTACHMENTS branch (a), agent.go, and tools.go. It is
// a protocol string, not prose, so it is asserted verbatim.
const ingestedMarker = "↳ ingested into project memory"

func msgWithAttachment(name, mime string, ext *conversation.ExtractionSummary) conversation.ChannelMessage {
	return conversation.ChannelMessage{
		Text: "what is this?",
		Attachments: []conversation.Attachment{{
			Name:       name,
			MimeType:   mime,
			SizeBytes:  218203,
			ArtifactID: "artifact_x",
			Extraction: ext,
		}},
	}
}

func sampleExtraction() *conversation.ExtractionSummary {
	return &conversation.ExtractionSummary{
		Title:               "photo",
		SectionCount:        1,
		ChunksIngested:      1,
		ExtractedDocumentID: "extdoc_x",
	}
}

// Regression for D4 (2026-07-29). "↳ ingested into project memory" tells the
// lead, via prompt.go:596 branch (a), that "the work is done ... stop".
// vornik-extract-image ALWAYS produces an extraction, so a photo tripped that
// directive and the dispatcher was instructed to report the file as ingested
// and schedule nothing — suppressing the whole vision handover.
//
// Live on email today, which is the one channel that populates Attachments.
//
// see LLD § https://docs.vornik.io §4.5b
func TestEnrichUserContent_ImageExtractionIsNotClaimedSufficient(t *testing.T) {
	got := enrichUserContent(msgWithAttachment("photo.jpg", "image/jpeg", sampleExtraction()))

	if strings.Contains(got, ingestedMarker) {
		t.Errorf("an image extraction must NOT claim the work is done; trailer was:\n%s", got)
	}
	// Positive assertion, not just the absence: a bare "metadata only"
	// reads as a status rather than a denial, and would let a future
	// rewording re-open D4 by degrees.
	if !strings.Contains(got, "has NOT been interpreted") {
		t.Errorf("the media trailer must explicitly deny interpretation; trailer was:\n%s", got)
	}
	// The extraction is still surfaced — the lead may legitimately use the
	// OCR text; it just cannot treat it as the whole job.
	if !strings.Contains(got, "extdoc_x") {
		t.Errorf("the extracted_document_id must still be available to the lead:\n%s", got)
	}
}

// The other half of the protocol: documents keep the exact phrasing, because
// prompt.go, agent.go, and tools.go all match on it. A golden assertion, not
// a substring-ish one.
func TestEnrichUserContent_DocumentKeepsIngestedMarkerVerbatim(t *testing.T) {
	got := enrichUserContent(msgWithAttachment("book.epub", "application/epub+zip", &conversation.ExtractionSummary{
		Title:               "Book Title",
		Author:              "Author",
		SectionCount:        18,
		ChunksIngested:      412,
		ExtractedDocumentID: "extdoc_xyz",
	}))
	want := "    ↳ ingested into project memory (Book Title by Author; 18 sections, 412 chunks; extracted_document_id=extdoc_xyz)"
	if !strings.Contains(got, want) {
		t.Errorf("document trailer drifted from the protocol string.\nwant line: %q\ngot:\n%s", want, got)
	}
}

func TestEnrichUserContent_AudioAndVideoAlsoDenyInterpretation(t *testing.T) {
	for _, tc := range []struct{ name, mime string }{
		{"voice.opus", "audio/ogg"},
		{"clip.mp4", "video/mp4"},
	} {
		got := enrichUserContent(msgWithAttachment(tc.name, tc.mime, sampleExtraction()))
		if strings.Contains(got, ingestedMarker) {
			t.Errorf("%s: must not claim sufficiency:\n%s", tc.name, got)
		}
		if !strings.Contains(got, "has NOT been") {
			t.Errorf("%s: must deny interpretation explicitly:\n%s", tc.name, got)
		}
	}
}

// An attachment with no extraction is case (b) and must stay exactly as it
// was — neither marker appears, so the lead passes input_files as before.
func TestEnrichUserContent_NoExtractionUnchanged(t *testing.T) {
	got := enrichUserContent(msgWithAttachment("photo.jpg", "image/jpeg", nil))
	if strings.Contains(got, ingestedMarker) || strings.Contains(got, "metadata only") {
		t.Errorf("an attachment with no extraction must carry neither marker:\n%s", got)
	}
	if !strings.Contains(got, "photo.jpg") {
		t.Errorf("attachment must still be listed:\n%s", got)
	}
}

// Unclassifiable inputs are treated as media (conservative): we cannot prove
// the extraction is sufficient, so we do not let the lead assume it.
func TestEnrichUserContent_UnknownKindDeniesInterpretation(t *testing.T) {
	got := enrichUserContent(msgWithAttachment("mystery.xyz", "", sampleExtraction()))
	if strings.Contains(got, ingestedMarker) {
		t.Errorf("an unclassifiable extraction must not claim sufficiency:\n%s", got)
	}
}

// The lead prompt must carry the case-(c) branch, and the phrase it keys on
// must be the one the trailer actually emits. If these drift the directive
// silently stops matching — the failure mode that made D4 invisible.
func TestLeadPrompt_HasMediaCaseMatchingTheTrailer(t *testing.T) {
	prompt := BuildLeadSystemPrompt(&registry.Project{ID: "assistant"}, nil, "", nil)
	for _, want := range []string{
		"has NOT been interpreted",
		"'vision' workflow",
		"NEVER describe an image you cannot see",
		"Three cases to recognise",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("lead system prompt is missing %q", want)
		}
	}
	// The trailer's own denial phrase must appear in the directive, or the
	// branch cannot fire on the text the trailer produces.
	trailer := enrichUserContent(msgWithAttachment("photo.jpg", "image/jpeg", sampleExtraction()))
	if !strings.Contains(trailer, "has NOT been interpreted") {
		t.Fatal("trailer no longer emits the phrase the prompt branches on")
	}
}
