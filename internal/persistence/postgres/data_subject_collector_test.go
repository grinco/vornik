package postgres

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// Every table the subject axis may link to must have a collector, or an Art 15
// export would silently omit that table's content while still listing the item
// — the subject would be told data exists and shown nothing, with no indication
// that the gap is ours rather than theirs.
//
// CollectItems refuses an uncollectable table at runtime too; this fails at
// build time instead, on the commit that widens the closed set.
func TestEveryLinkableTableHasACollector(t *testing.T) {
	var missing []string
	for _, name := range datasubject.LinkableTables() {
		if _, ok := collectorSpecs[datasubject.LinkableTable(name)]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("linkable tables with no collector spec: %v", missing)
	}
}

// A collector must not name a table outside the closed set: the table name is
// interpolated into SQL, so the closed set is also the injection boundary.
func TestNoCollectorForAnUnlinkableTable(t *testing.T) {
	allowed := map[string]bool{}
	for _, name := range datasubject.LinkableTables() {
		allowed[name] = true
	}
	for table := range collectorSpecs {
		if !allowed[string(table)] {
			t.Errorf("collector spec for %q, which is not a linkable table", table)
		}
	}
}

// Each spec must actually be able to read something, and name where the data
// came from — Art 14(2)(f) entitles a subject to the source.
func TestCollectorSpecsAreWellFormed(t *testing.T) {
	for table, spec := range collectorSpecs {
		if spec.idCol == "" {
			t.Errorf("%s: no id column", table)
		}
		if len(spec.contentCols) == 0 {
			t.Errorf("%s: no content columns — the export would list the item with nothing in it", table)
		}
		if len(spec.originNote) < 15 {
			t.Errorf("%s: origin note too thin to answer Art 14(2)(f): %q", table, spec.originNote)
		}
	}
}

// --- erasure execution (increment 5, slice 5b) ---

// DeleteRow interpolates the table and id column into SQL, so both must come
// from the same closed map the collector uses. Reusing collectorSpecs rather than
// adding a parallel eraserSpecs map means TestEveryLinkableTableHasACollector
// already guards erasure coverage too — one source of truth, one coverage test.
func TestDeleteRow_UsesTheClosedCollectorSpecSet(t *testing.T) {
	for _, name := range datasubject.LinkableTables() {
		spec, ok := collectorSpecs[datasubject.LinkableTable(name)]
		if !ok {
			t.Errorf("%s has no spec, so DeleteRow could not erase it", name)
			continue
		}
		if strings.TrimSpace(spec.idCol) == "" {
			t.Errorf("%s has no id column, so DeleteRow has no WHERE clause to target", name)
		}
	}
}

// Artifacts must never be deleted as a plain row: extracted_documents has no FK
// on source_artifact_id and project_memory_chunks.artifact_id is ON DELETE SET
// NULL, so a row delete orphans the extraction and leaves the derived embedding
// in the vector store. The cascade in internal/erasure is the only correct path,
// and this is the defence-in-depth guard in case a caller routes it wrongly.
func TestDeleteRow_RefusesArtifactsToProtectTheCascade(t *testing.T) {
	r := &DataSubjectRepository{}
	err := r.DeleteRow(context.Background(), datasubject.TableArtifacts, "art-1")
	if err == nil {
		t.Fatal("DeleteRow must refuse artifacts — they require the cascade")
	}
	if !strings.Contains(err.Error(), "cascade") {
		t.Errorf("the refusal should point at the cascade, got %v", err)
	}
}

func TestDeleteRow_RefusesATableOutsideTheClosedSet(t *testing.T) {
	r := &DataSubjectRepository{}
	if err := r.DeleteRow(context.Background(), datasubject.LinkableTable("tool_audit_log"), "r1"); err == nil {
		t.Fatal("DeleteRow must refuse a table with no spec rather than build SQL from it")
	}
}

func TestDeleteRow_RefusesAnEmptyRowID(t *testing.T) {
	r := &DataSubjectRepository{}
	if err := r.DeleteRow(context.Background(), datasubject.TableChatAuditLog, "  "); err == nil {
		t.Fatal("an empty row id must be refused — the WHERE clause would be meaningless")
	}
}

// The accountability chain must not record a hash of a document nobody kept.
//
// Found by the 5b code review chasing a different question: report_hash was
// written to the ledger unconditionally while the report itself was saved only if
// the operator happened to pass --out. A fingerprint of an unsaved document proves
// nothing, and it is the evidence Art 5(2) accountability rests on — so the
// report is stored WITH the hash, in one call, or not at all.
func TestSaveRequestReport_RequiresBothTheReportAndItsHash(t *testing.T) {
	r := &DataSubjectRepository{}
	ctx := context.Background()
	if err := r.SaveRequestReport(ctx, "req-1", "", "sha256:abc"); err == nil {
		t.Error("a hash with no report body must be refused — it would attest to nothing")
	}
	if err := r.SaveRequestReport(ctx, "req-1", `{"plan":{}}`, ""); err == nil {
		t.Error("a report with no hash must be refused — nothing would pin it")
	}
	if err := r.SaveRequestReport(ctx, "", `{"plan":{}}`, "sha256:abc"); err == nil {
		t.Error("an empty request id must be refused")
	}
}
