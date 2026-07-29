package datasubject

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
