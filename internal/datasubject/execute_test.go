package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Increment 5, slice 5b — executing the decided plan (design §4.6 steps 2-3).
//
// 5b performs DELETIONS only: the artifact cascade for artifact rows, plain row
// deletes elsewhere. Redaction is 5c. The property that matters most here is that
// a redact action must be reported as NOT DONE rather than quietly passed over —
// an erasure report that counts an untouched row as erased is the compliance lie
// the whole design is written to avoid.

// --- fakes ---

type fakeRowDeleter struct {
	deleted []string // "table/rowID"
	failOn  map[string]error
}

func (f *fakeRowDeleter) DeleteRow(_ context.Context, table LinkableTable, rowID string) error {
	key := string(table) + "/" + rowID
	if err, ok := f.failOn[key]; ok {
		return err
	}
	f.deleted = append(f.deleted, key)
	return nil
}

type fakeArtifactEraser struct {
	erased  []string
	chunks  int
	derived ArtifactErasureCounts
	err     error
}

func (f *fakeArtifactEraser) EraseArtifact(_ context.Context, artifactID string) (ArtifactErasureCounts, error) {
	if f.err != nil {
		return ArtifactErasureCounts{}, f.err
	}
	f.erased = append(f.erased, artifactID)
	out := f.derived
	out.ChunksDeleted = f.chunks
	return out, nil
}

func planFor(t *testing.T, ground ErasureGround, items []Item) *ErasurePlan {
	t.Helper()
	plan, err := PlanErasure(verifiedErasure(ground), items, []string{"authenticated_identity"})
	if err != nil {
		t.Fatalf("PlanErasure: %v", err)
	}
	return plan
}

// --- the artifact cascade is composed, not reimplemented ---

// An artifact row must go through the cascade service, which already removes
// extractions, derived chunks and the on-disk storage directory with containment
// checked. Deleting the artifact row directly would orphan all three.
func TestExecuteErasure_ArtifactRowsGoThroughTheCascade(t *testing.T) {
	rows := &fakeRowDeleter{}
	arts := &fakeArtifactEraser{chunks: 7}
	ex := &Executor{Rows: rows, Artifacts: arts}

	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableArtifacts, RowID: "art-1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(arts.erased) != 1 || arts.erased[0] != "art-1" {
		t.Errorf("artifact cascade not invoked, got %v", arts.erased)
	}
	if len(rows.deleted) != 0 {
		t.Errorf("an artifact must NOT be deleted as a plain row — that orphans its extractions: %v", rows.deleted)
	}
	if res.DerivedChunksDeleted != 7 {
		t.Errorf("DerivedChunksDeleted = %d, want the cascade's 7", res.DerivedChunksDeleted)
	}
}

func TestExecuteErasure_NonArtifactRowsAreDeletedDirectly(t *testing.T) {
	rows := &fakeRowDeleter{}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}

	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
		{Table: TableOperatorProfile, RowID: "op-1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows.deleted) != 2 {
		t.Fatalf("expected 2 row deletes, got %v", rows.deleted)
	}
	if res.RowsDeleted != 2 {
		t.Errorf("RowsDeleted = %d, want 2", res.RowsDeleted)
	}
}

// --- the property that matters: redaction is NOT silently claimed ---

// 5b cannot redact. A redact action must be surfaced as deferred, with a reason,
// and must never be counted as erased.
func TestExecuteErasure_RedactActionsAreDeferredNotSilentlySkipped(t *testing.T) {
	rows := &fakeRowDeleter{}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}

	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: SharedRow},
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows.deleted) != 1 || rows.deleted[0] != "chat_audit_log/r1" {
		t.Errorf("only the exclusive row should be deleted, got %v", rows.deleted)
	}
	if len(res.Deferred) != 1 {
		t.Fatalf("the shared row must be reported as deferred, got %d deferred", len(res.Deferred))
	}
	d := res.Deferred[0]
	if d.RowID != "c1" || d.Disposition != DispositionRedact {
		t.Errorf("wrong deferred entry: %+v", d)
	}
	if strings.TrimSpace(d.Reason) == "" {
		t.Error("a deferred row must carry the reason it was not actioned")
	}
	if res.RowsDeleted != 1 {
		t.Errorf("RowsDeleted = %d — a deferred row must never be counted as erased", res.RowsDeleted)
	}
	// And the request must not be reportable as fully satisfied.
	if res.Complete() {
		t.Error("Complete() must be false while any row is deferred — the subject has not been fully erased")
	}
}

func TestExecuteErasure_CompleteOnlyWhenEverythingWasActioned(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{}}
	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Complete() {
		t.Error("an all-delete plan that fully succeeded must report Complete()")
	}
}

// Under a discretion-removing ground the shared row is planned as a DELETE, so
// 5b can execute it — nothing is deferred.
func TestExecuteErasure_DiscretionRemovingGroundLeavesNothingDeferred(t *testing.T) {
	rows := &fakeRowDeleter{}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}

	plan := planFor(t, GroundUnlawfulProcessing, []Item{
		{Table: TableProjectMemoryChunks, RowID: "c1", Exclusivity: SharedRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Deferred) != 0 {
		t.Errorf("under %s the shared row is deleted, not deferred: %+v", GroundUnlawfulProcessing, res.Deferred)
	}
	if len(rows.deleted) != 1 {
		t.Errorf("expected the shared row deleted, got %v", rows.deleted)
	}
	if !res.Complete() {
		t.Error("all planned rows were actioned, so the result is complete")
	}
}

// --- failure handling ---

// One failing row must not abandon the rest. Stopping at the first error leaves
// MORE of the subject's data behind than continuing does — but the failure has to
// be reported, and the result must not read as success.
func TestExecuteErasure_ContinuesPastAFailureAndReportsIt(t *testing.T) {
	boom := errors.New("row locked")
	rows := &fakeRowDeleter{failOn: map[string]error{"chat_audit_log/r1": boom}}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}

	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
		{Table: TableChatAuditLog, RowID: "r2", Exclusivity: ExclusiveRow},
		{Table: TableOperatorProfile, RowID: "op-1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("a failed row must surface as an error, so the caller cannot mark the request actioned")
	}
	if res == nil {
		t.Fatal("the result must still be returned alongside the error — the caller needs to know what DID happen")
	}
	if len(res.Failed) != 1 || res.Failed[0].RowID != "r1" {
		t.Errorf("expected r1 recorded as failed, got %+v", res.Failed)
	}
	if res.RowsDeleted != 2 {
		t.Errorf("RowsDeleted = %d, want 2 — the other rows must still have been erased", res.RowsDeleted)
	}
	if res.Complete() {
		t.Error("Complete() must be false when a row failed")
	}
}

func TestExecuteErasure_CascadeFailureIsRecordedNotSwallowed(t *testing.T) {
	arts := &fakeArtifactEraser{err: errors.New("storage root missing")}
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: arts}

	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableArtifacts, RowID: "art-1", Exclusivity: ExclusiveRow},
	})
	res, err := ex.Execute(context.Background(), plan)
	if err == nil {
		t.Fatal("a cascade failure must surface")
	}
	if len(res.Failed) != 1 || res.Failed[0].Table != TableArtifacts {
		t.Errorf("expected the artifact recorded as failed, got %+v", res.Failed)
	}
}

// --- wiring guards ---

// Missing dependencies must refuse before anything is touched. A nil deleter with
// a plan full of deletes would otherwise panic partway through an irreversible
// operation.
func TestExecuteErasure_RefusesWithoutItsStores(t *testing.T) {
	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableChatAuditLog, RowID: "r1", Exclusivity: ExclusiveRow},
	})
	for name, ex := range map[string]*Executor{
		"no row deleter":     {Artifacts: &fakeArtifactEraser{}},
		"no artifact eraser": {Rows: &fakeRowDeleter{}},
		"nothing wired":      {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ex.Execute(context.Background(), plan); err == nil {
				t.Error("an unwired executor must refuse rather than partially erase")
			}
		})
	}
}

func TestExecuteErasure_RefusesNilPlan(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{}}
	if _, err := ex.Execute(context.Background(), nil); err == nil {
		t.Error("a nil plan must refuse")
	}
}

// An empty plan is a valid no-op, and it is complete: there was nothing to erase.
func TestExecuteErasure_EmptyPlanIsACompleteNoOp(t *testing.T) {
	rows := &fakeRowDeleter{}
	ex := &Executor{Rows: rows, Artifacts: &fakeArtifactEraser{}}
	res, err := ex.Execute(context.Background(), planFor(t, GroundConsentWithdrawn, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows.deleted) != 0 || res.RowsDeleted != 0 {
		t.Error("nothing should have been deleted")
	}
	if !res.Complete() {
		t.Error("an empty plan is complete — there was nothing to do")
	}
}

// The result must carry the plan's identity so the ledger entry and the report
// cannot be attributed to the wrong request.
func TestExecuteErasure_ResultCarriesRequestIdentity(t *testing.T) {
	ex := &Executor{Rows: &fakeRowDeleter{}, Artifacts: &fakeArtifactEraser{}}
	res, err := ex.Execute(context.Background(), planFor(t, GroundConsentWithdrawn, nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.RequestID != "req-1" || res.SubjectID != "subj-1" {
		t.Errorf("result identity = %s/%s, want req-1/subj-1", res.RequestID, res.SubjectID)
	}
}

// The derived-graph counts must reach the erasure result.
//
// An erasure report that lists chunks and silently omits what those chunks
// DERIVED is how 3,795 knowledge-graph entities accumulated in production
// behind erasures reported as complete (design §4.14). Once the rows are gone
// the report is the only evidence they were covered, so a count that stops at
// the executor is the defect one level along.
func TestExecute_reportsDerivedGraphCounts(t *testing.T) {
	eraser := &fakeArtifactEraser{
		chunks: 7,
		derived: ArtifactErasureCounts{
			GraphEntitiesDeleted:     4,
			GraphEdgesDeleted:        6,
			QuarantinedCopiesDeleted: 2,
		},
	}
	plan := planFor(t, GroundConsentWithdrawn, []Item{
		{Table: TableArtifacts, RowID: "artifact-1", Exclusivity: ExclusiveRow},
	})
	exec := &Executor{Rows: &fakeRowDeleter{}, Artifacts: eraser}

	res, err := exec.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.DerivedGraphEntitiesDeleted != 4 {
		t.Errorf("DerivedGraphEntitiesDeleted = %d, want 4", res.DerivedGraphEntitiesDeleted)
	}
	if res.DerivedGraphEdgesDeleted != 6 {
		t.Errorf("DerivedGraphEdgesDeleted = %d, want 6", res.DerivedGraphEdgesDeleted)
	}
	if res.QuarantinedCopiesDeleted != 2 {
		t.Errorf("QuarantinedCopiesDeleted = %d, want 2 — the quarantine table holds the "+
			"chunk's full text and its FK is SET NULL, so it outlives a chunk-only erasure",
			res.QuarantinedCopiesDeleted)
	}
}
