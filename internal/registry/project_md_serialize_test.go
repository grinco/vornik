package registry

import (
	"strings"
	"testing"
)

// TestSerializeProjectBrief_HappyPath — every field round-trips
// to a clean PROJECT.md that the parser accepts. This is the
// load-bearing test for the brief editor: anything the editor
// produces must be parseable on the next reload.
func TestSerializeProjectBrief_HappyPath(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "demo",
		DisplayName:     "Demo Project",
		Description:     "A short intro paragraph for the homepage hero.",
		Goal:            "Process incoming requests.",
		Audience:        "Operator-facing chat users.",
		SuccessCriteria: "Replies cite sources.",
		OutOfScope:      "Code review.",
		RiskCadence:     "Low-risk conversational cadence.",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	round, err := ParseProjectMarkdown(out, "round.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown on serializer output: %v\nbody:\n%s", err, string(out))
	}
	if round.ProjectID != brief.ProjectID {
		t.Errorf("ProjectID round-trip: got %q want %q", round.ProjectID, brief.ProjectID)
	}
	if round.DisplayName != brief.DisplayName {
		t.Errorf("DisplayName round-trip: got %q want %q", round.DisplayName, brief.DisplayName)
	}
	if round.Description != brief.Description {
		t.Errorf("Description round-trip: got %q want %q", round.Description, brief.Description)
	}
	if round.Goal != brief.Goal {
		t.Errorf("Goal round-trip: got %q want %q", round.Goal, brief.Goal)
	}
	if round.Audience != brief.Audience {
		t.Errorf("Audience round-trip: got %q want %q", round.Audience, brief.Audience)
	}
	if round.SuccessCriteria != brief.SuccessCriteria {
		t.Errorf("SuccessCriteria round-trip: got %q want %q", round.SuccessCriteria, brief.SuccessCriteria)
	}
	if round.OutOfScope != brief.OutOfScope {
		t.Errorf("OutOfScope round-trip: got %q want %q", round.OutOfScope, brief.OutOfScope)
	}
	if round.RiskCadence != brief.RiskCadence {
		t.Errorf("RiskCadence round-trip: got %q want %q", round.RiskCadence, brief.RiskCadence)
	}
}

// TestSerializeProjectBrief_OptionalSectionsOmitted — empty
// OutOfScope / RiskCadence don't produce empty section
// headings; the rendered file stays tight. Required sections
// still emit even when empty (the parser rejects them, but the
// serializer doesn't pre-validate — that's the caller's job).
func TestSerializeProjectBrief_OptionalSectionsOmitted(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "minimal",
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	body := string(out)
	if strings.Contains(body, "## Out of scope") {
		t.Errorf("empty OutOfScope still emitted a section heading. body:\n%s", body)
	}
	if strings.Contains(body, "## Risk & cadence") {
		t.Errorf("empty RiskCadence still emitted a section heading. body:\n%s", body)
	}
}

// TestSerializeProjectBrief_NoDisplayName — DisplayName empty
// means "inherit from project.yaml"; the serializer must omit
// the key from frontmatter rather than writing
// `displayName: ""` (which would override the YAML's value with
// an empty string on the next load).
func TestSerializeProjectBrief_NoDisplayName(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "demo",
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	if strings.Contains(string(out), "displayName") {
		t.Errorf("empty DisplayName should not be written. body:\n%s", string(out))
	}
}

// TestSerializeProjectBrief_NoPreamble — Description empty
// means "no preamble"; the serializer must go from the
// frontmatter directly to the first `## Goal` section without
// blank lines that signal an empty paragraph.
func TestSerializeProjectBrief_NoPreamble(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "demo",
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	round, err := ParseProjectMarkdown(out, "round.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v\nbody:\n%s", err, string(out))
	}
	if round.Description != "" {
		t.Errorf("Description round-trip on empty: got %q want empty", round.Description)
	}
}

// TestSerializeProjectBrief_ExtraSectionsAtTail — Extra
// sections captured by the parser are written at the end, in
// source order, so unknown content survives the editor round-
// trip even though the form doesn't surface those fields.
func TestSerializeProjectBrief_ExtraSectionsAtTail(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "demo",
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
		Extra: []ProjectBriefSection{
			{Heading: "References", Body: "ref body 1"},
			{Heading: "Notes", Body: "note body 2"},
		},
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	body := string(out)

	refIdx := strings.Index(body, "## References")
	notesIdx := strings.Index(body, "## Notes")
	successIdx := strings.Index(body, "## Success criteria")
	if refIdx < 0 || notesIdx < 0 {
		t.Fatalf("Extra section headings missing. body:\n%s", body)
	}
	if successIdx >= refIdx || refIdx >= notesIdx {
		t.Errorf("Extra sections not at tail in source order. successIdx=%d refIdx=%d notesIdx=%d",
			successIdx, refIdx, notesIdx)
	}

	round, err := ParseProjectMarkdown(out, "extras.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v", err)
	}
	if len(round.Extra) != 2 {
		t.Fatalf("Extra count round-trip: got %d want 2", len(round.Extra))
	}
	if round.Extra[0].Heading != "References" || round.Extra[1].Heading != "Notes" {
		t.Errorf("Extra heading order round-trip: got %v", round.Extra)
	}
	if !strings.Contains(round.Extra[0].Body, "ref body 1") {
		t.Errorf("Extra[0].Body round-trip: got %q", round.Extra[0].Body)
	}
}

// TestSerializeProjectBrief_MultiLineBodies — long prose bodies
// (typical for Goal) survive verbatim through the round trip
// without escape sequences or trailing-whitespace surprises.
func TestSerializeProjectBrief_MultiLineBodies(t *testing.T) {
	multi := "Line one.\nLine two with **bold**.\n\n- bullet one\n- bullet two\n"
	brief := &ProjectBrief{
		ProjectID:       "demo",
		Goal:            multi,
		Audience:        "a",
		SuccessCriteria: "s",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	round, err := ParseProjectMarkdown(out, "multi.md")
	if err != nil {
		t.Fatalf("ParseProjectMarkdown: %v\nbody:\n%s", err, string(out))
	}
	want := strings.TrimSpace(multi)
	got := strings.TrimSpace(round.Goal)
	if got != want {
		t.Errorf("multi-line Goal round-trip mismatch.\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// TestSerializeProjectBrief_RequiresProjectID — empty
// ProjectID is operator error; the serializer refuses rather
// than producing a file the parser will reject. Surfaces the
// problem at the form-save call site.
func TestSerializeProjectBrief_RequiresProjectID(t *testing.T) {
	brief := &ProjectBrief{
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
	}
	_, err := SerializeProjectBrief(brief)
	if err == nil || !strings.Contains(err.Error(), "projectId") {
		t.Errorf("err = %v, want missing-projectId rejection", err)
	}
}

// TestSerializeProjectBrief_FixedSectionOrder — Goal precedes
// Audience precedes Success criteria; Out of scope and Risk &
// cadence follow when present. A serializer that emitted
// sections in struct-field order or alphabetical order would
// confuse operators reading the file. Pinning the order pins
// the diff shape.
func TestSerializeProjectBrief_FixedSectionOrder(t *testing.T) {
	brief := &ProjectBrief{
		ProjectID:       "demo",
		Goal:            "g",
		Audience:        "a",
		SuccessCriteria: "s",
		OutOfScope:      "o",
		RiskCadence:     "r",
	}
	out, err := SerializeProjectBrief(brief)
	if err != nil {
		t.Fatalf("SerializeProjectBrief: %v", err)
	}
	body := string(out)
	headings := []string{
		"## Goal",
		"## Audience",
		"## Success criteria",
		"## Out of scope",
		"## Risk & cadence",
	}
	last := -1
	for _, h := range headings {
		idx := strings.Index(body, h)
		if idx < 0 {
			t.Fatalf("heading %q missing. body:\n%s", h, body)
		}
		if idx <= last {
			t.Errorf("heading %q appeared before its predecessor (last=%d, this=%d)", h, last, idx)
		}
		last = idx
	}
}
