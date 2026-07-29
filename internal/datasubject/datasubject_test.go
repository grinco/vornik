package datasubject

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE coverage test, and the reason the two sets exist.
//
// Every personal-data-bearing table the ROPA inventories must be either
// LINKABLE (the subject axis can find rows in it) or explicitly UNCOVERED with
// a reason. A table in neither is a gap nobody can see: an Art 17 erasure would
// silently skip it while the report claimed success, which is precisely the
// documented-but-unenforced failure this design exists to remove.
//
// Reading the inventory out of the ROPA rather than hardcoding it means the
// test tracks the document that counsel and the operator actually maintain. If
// someone records a new table there, this fails until it is classified.
//
// see LLD § https://docs.vornik.io §4.2
func TestEveryROPATableIsClassified(t *testing.T) {
	inventory := ropaTableInventory(t)
	if len(inventory) < 5 {
		t.Fatalf("only %d tables parsed out of the ROPA — the parse is probably broken, "+
			"and a silently-empty inventory would make this test vacuous", len(inventory))
	}

	var unclassified []string
	for _, table := range inventory {
		_, linkable := linkableTables[LinkableTable(table)]
		_, uncovered := UncoveredTable[table]
		if !linkable && !uncovered {
			unclassified = append(unclassified, table)
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("these tables are named in docs/legal/ropa.md as holding personal data but are "+
			"neither in the linkable set nor listed as uncovered, so a subject erasure would "+
			"silently miss them: %v", unclassified)
	}
}

// A table cannot be both linkable and uncovered — that would leave the
// executor's behaviour ambiguous.
func TestNoTableIsBothLinkableAndUncovered(t *testing.T) {
	for table := range linkableTables {
		if reason, ok := UncoveredTable[string(table)]; ok {
			t.Errorf("%s is both linkable and listed as uncovered (%q) — pick one", table, reason)
		}
	}
}

// Every uncovered entry must carry a REASON, not just a name. An unexplained
// omission is indistinguishable from an oversight.
func TestUncoveredTablesStateAReason(t *testing.T) {
	for table, reason := range UncoveredTable {
		if len(strings.TrimSpace(reason)) < 30 {
			t.Errorf("%s: uncovered reason is too thin to audit (%q)", table, reason)
		}
		// The point of excluding a table is a legal ground, so name one.
		if !strings.Contains(reason, "Art") {
			t.Errorf("%s: uncovered reason should cite the provision it relies on: %q", table, reason)
		}
	}
}

// The AI Act evidence trail must never be linkable — it is not erasable on
// request, and the retention denylist already refuses to prune it. Listing it
// as linkable would invite an erasure path into it.
func TestDisclosureLogIsNotLinkable(t *testing.T) {
	if _, ok := linkableTables["channel_disclosure_log"]; ok {
		t.Fatal("channel_disclosure_log must not be linkable: it is Art 50/99 conformity evidence")
	}
	if err := ValidateTable("channel_disclosure_log"); err == nil {
		t.Fatal("ValidateTable must refuse the disclosure log")
	}
}

func TestValidateTable(t *testing.T) {
	if err := ValidateTable("project_memory_chunks"); err != nil {
		t.Errorf("a linkable table must validate: %v", err)
	}
	// An uncovered table is refused, and the error explains WHY rather than
	// just saying no — the operator needs to know it was a decision.
	err := ValidateTable("tool_audit_log")
	if err == nil {
		t.Fatal("an uncovered table must be refused")
	}
	if !strings.Contains(err.Error(), "deliberately not linkable") {
		t.Errorf("refusal should say the omission was deliberate: %v", err)
	}
	// An unknown table is refused with instructions.
	err = ValidateTable("some_new_table")
	if err == nil {
		t.Fatal("an unknown table must be refused")
	}
	if !strings.Contains(err.Error(), "add it to one") {
		t.Errorf("refusal should tell the developer what to do: %v", err)
	}
}

// A binder may DOWNGRADE its confidence but never upgrade it. Without this a
// KG extraction could claim `certain` and poison every downstream report — and
// the report is the entire product of this design.
func TestLinkValidate_ConfidenceCannotBeUpgraded(t *testing.T) {
	base := Link{Table: TableProjectMemoryChunks, RowID: "chunk-1", Exclusivity: SharedRow}

	l := base
	l.Source = SourceKGExtraction
	l.Confidence = ConfidenceCertain
	if err := l.Validate(); err == nil {
		t.Error("a kg_extraction link must not be allowed to claim certain")
	} else if !strings.Contains(err.Error(), "never upgrade") {
		t.Errorf("error should explain the rule: %v", err)
	}

	// Downgrading is fine: a binder that knows it is unsure may say so.
	l.Source = SourceAuthenticated
	l.Confidence = ConfidencePossible
	if err := l.Validate(); err != nil {
		t.Errorf("downgrading confidence must be allowed: %v", err)
	}

	// The natural pairing validates.
	l.Source = SourceKGExtraction
	l.Confidence = ConfidencePossible
	if err := l.Validate(); err != nil {
		t.Errorf("kg_extraction/possible must validate: %v", err)
	}
}

func TestLinkValidate_Rejects(t *testing.T) {
	ok := Link{Table: TableArtifacts, RowID: "a1", Source: SourceAuthenticated,
		Confidence: ConfidenceCertain, Exclusivity: ExclusiveRow}
	if err := ok.Validate(); err != nil {
		t.Fatalf("baseline link should validate: %v", err)
	}
	for name, mutate := range map[string]func(*Link){
		"empty row id":       func(l *Link) { l.RowID = " " },
		"unknown table":      func(l *Link) { l.Table = "nope" },
		"unknown source":     func(l *Link) { l.Source = "telepathy" },
		"missing confidence": func(l *Link) { l.Confidence = "" },
		"bad exclusivity":    func(l *Link) { l.Exclusivity = "maybe" },
		"uncovered table":    func(l *Link) { l.Table = "tool_audit_log" },
	} {
		l := ok
		mutate(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// Unknown exclusivity must be handled as SHARED. Assuming exclusivity would
// authorise deleting another person's data on a guess.
func TestExclusivity_UnknownIsTreatedAsShared(t *testing.T) {
	if !UnknownExclusivity.TreatAsShared() {
		t.Error("unknown exclusivity must be treated as shared")
	}
	if !SharedRow.TreatAsShared() {
		t.Error("shared is shared")
	}
	if ExclusiveRow.TreatAsShared() {
		t.Error("exclusive rows are the only ones safe to delete outright")
	}
}

func TestDefaultConfidence(t *testing.T) {
	for src, want := range map[Source]Confidence{
		SourceAuthenticated:    ConfidenceCertain,
		SourceOperatorLink:     ConfidenceCertain,
		SourceOperatorAsserted: ConfidenceCertain,
		SourceEmailEnvelope:    ConfidenceProbable,
		SourceKGExtraction:     ConfidencePossible,
	} {
		got, err := DefaultConfidence(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got != want {
			t.Errorf("%s default confidence = %q, want %q", src, got, want)
		}
	}
	if _, err := DefaultConfidence("invented"); err == nil {
		t.Error("an unknown source must error rather than default to certain")
	}
}

func TestLinkableTablesIsSorted(t *testing.T) {
	got := LinkableTables()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("LinkableTables must be sorted for stable diagnostics: %v", got)
		}
	}
}

// ropaTableInventory extracts backtick-quoted table names from the ROPA's
// "Categories of personal data" section — the operator-maintained inventory of
// where personal data lands.
func ropaTableInventory(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "legal", "ropa.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ROPA: %v", err)
	}
	body := string(data)
	start := strings.Index(body, "## 4. Categories of personal data")
	if start < 0 {
		t.Fatal("ROPA §4 not found — the inventory this test reads has moved")
	}
	end := strings.Index(body[start:], "\n## ")
	if end < 0 {
		end = len(body) - start
	}
	section := body[start : start+end]

	// Table names are backticked and snake_case; skip column references
	// (table.column) by taking the part before any dot.
	re := regexp.MustCompile("`([a-z][a-z0-9_]{4,})(?:\\.[a-z_]+)?`")
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(section, -1) {
		name := m[1]
		// Filter prose words that happen to be backticked in this section.
		if !strings.Contains(name, "_") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// The Art 14 notice template must carry every element Art 14(1)–(2) requires.
//
// A notice missing an element does not discharge the duty, and the gap is
// invisible on reading — nobody notices the absent paragraph. So the elements
// are asserted rather than trusted to review.
//
// It must ALSO stay honest about whose duty it is: Vornik is delivered as
// software an operator runs themselves, the project holds no deployment data,
// and a vendor page therefore cannot discharge an operator's Art 14 duty. The
// template is adopted and published BY the operator.
//
// see LLD § https://docs.vornik.io §4.13
func TestArt14TemplateCarriesRequiredElements(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "legal", "art14-notice-template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Art 14 template: %v", err)
	}
	body := strings.Join(strings.Fields(string(data)), " ")

	// One probe per Art 14(1)/(2) element.
	elements := map[string]string{
		"14(1)(a) controller identity": "[CONTROLLER LEGAL NAME]",
		"14(1)(b) DPO":                 "Data protection officer",
		"14(1)(c) purposes + basis":    "Article 6(1)(f)",
		"14(1)(d) categories":          "categories of data",
		"14(1)(e) recipients":          "Who else sees it",
		"14(1)(f) transfers":           "Standard Contractual Clauses",
		"14(2)(a) retention":           "How long it is kept",
		"14(2)(c) rights":              "Article 20",
		"14(2)(e) complaint":           "supervisory authority",
		"14(2)(f) source":              "Article 14(2)(f)",
		"14(2)(g) automated decisions": "Automated decisions",
	}
	for element, probe := range elements {
		if !strings.Contains(body, probe) {
			t.Errorf("template is missing %s (probe %q)", element, probe)
		}
	}

	// The relief must be described as CONDITIONAL on publication, since that is
	// the whole basis for using a page instead of individual notices.
	for _, want := range []string{"14(5)(b)", "conditional", "one month"} {
		if !strings.Contains(body, want) {
			t.Errorf("template should state the 14(5)(b) basis and its clock: missing %q", want)
		}
	}
	// And it must say where 14(5)(b) does NOT help, or an operator will assume
	// publishing covers the reachable case too.
	if !strings.Contains(body, "does NOT save you") {
		t.Error("template must state where 14(5)(b) relief does not apply (the reachable data subject)")
	}
}

// The public product page must not imply the project holds deployment data or
// can action a request — it cannot, and saying otherwise would suggest a
// controller relationship that does not exist.
func TestPublicPageDisclaimsVendorControllership(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "public", "ai-transparency.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transparency page: %v", err)
	}
	// Markdown hard-wraps prose, so collapse whitespace before matching —
	// otherwise any multi-word probe is hostage to where the line broke.
	body := strings.Join(strings.Fields(string(data)), " ")
	if !strings.Contains(body, "does not hold, store, or have access to any data from a customer's deployment") {
		t.Error("the public page must state plainly that the project holds no deployment data")
	}
	if !strings.Contains(body, "art14-notice-template.md") {
		t.Error("the public page should point operators at the template they must publish")
	}
}

// The legal documents mix three kinds of statement, and the mixture is the
// dangerous part: a software fact an operator may rely on, the vendor's own
// worked answer, and a value the operator must supply. An operator who mistook
// the second for the third would publish an Art 30 record that is false for
// them — worse than an absent one under audit.
//
// This asserts the markers survive editing. It is a documentation test on
// purpose: the failure mode is silent erosion during an unrelated edit, which
// review does not reliably catch.
//
// see LLD § https://docs.vornik.io §4.11
func TestLegalDocsMarkTemplateVersusExample(t *testing.T) {
	ropa := readDoc(t, filepath.Join("..", "..", "docs", "legal", "ropa.md"))

	// The legend itself must be present, or the per-section markers are
	// uninterpretable.
	for _, want := range []string{"[SOFTWARE FACT]", "[EXAMPLE]", "[TEMPLATE — REPLACE]"} {
		if !strings.Contains(ropa, want) {
			t.Errorf("ROPA is missing the %s legend entry", want)
		}
	}

	// Every numbered section must carry a marker. An unmarked section is one an
	// operator has to guess about.
	for _, section := range []string{
		"## 1. Controller / processor identity",
		"## 2. Processing activities at a glance",
		"## 3. Categories of data subject",
		"## 4. Categories of personal data",
		"## 5. Recipients",
		"## 6. Third-country transfers",
		"## 7. Retention",
		"## 8. Security measures",
		"## 9. Known gaps",
	} {
		idx := strings.Index(ropa, section)
		if idx < 0 {
			t.Errorf("ROPA section %q not found — heading changed without updating this test", section)
			continue
		}
		// The marker rides on the heading line itself.
		line := ropa[idx:]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		if !strings.Contains(line, "[SOFTWARE FACT]") &&
			!strings.Contains(line, "[TEMPLATE — REPLACE]") &&
			!strings.Contains(line, "[EXAMPLE]") {
			t.Errorf("ROPA section %q carries no template/example/fact marker", section)
		}
	}

	// Every vendor-specific legal document must disclaim that it speaks for a
	// customer deployment. Naming the vendor's own answer without that
	// disclaimer is the error this audit exists to remove.
	for _, doc := range []string{"ropa.md", "editorial-responsibility.md", "art14-notice-template.md"} {
		body := readDoc(t, filepath.Join("..", "..", "docs", "legal", doc))
		if !strings.Contains(body, "delivered, not operated on") {
			t.Errorf("%s should state that the software is delivered, not operated on the operator's behalf, "+
				"so a reader cannot mistake a vendor answer for their own position", doc)
		}
	}
}

// readDoc reads a doc with whitespace collapsed, since markdown hard-wraps and
// a multi-word probe would otherwise depend on where the line broke.
func readDoc(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}
