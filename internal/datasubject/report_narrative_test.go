package datasubject

import (
	"strings"
	"testing"
)

// Slice 5c §7 — what the data subject is told. These are compliance assertions, not
// cosmetics: the sentences below are what a regulator reads and what the DPA quotes,
// so they are pinned rather than left to drift.

// THE LOAD-BEARING ASSERTION. A redacted record's entry must carry BOTH halves: the
// mechanical guarantee that identifiers are gone, AND the disclaimer that inferential
// association cannot be ruled out. Either alone misleads — the first overstates, and
// omitting the first understates work that was actually done and verified.
func TestNarrative_RedactedEntryStatesTheGuaranteeAndTheDisclaimer(t *testing.T) {
	res := &ErasureResult{
		Redacted: []RedactedAction{{
			Table: TableProjectMemoryChunks, RowID: "c1",
			BeforeHash: "h1", AfterHash: "h2", Model: "m", Verified: true,
		}},
	}
	n := res.Narrative(nil)
	if len(n.Entries) != 1 {
		t.Fatalf("expected one entry, got %+v", n.Entries)
	}
	got := n.Entries[0].Outcome

	// The guarantee.
	if !strings.Contains(got, "verified mechanically") {
		t.Error("the entry must state the mechanical guarantee that was actually proved")
	}
	// The disclaimer, which is what keeps the guarantee honest.
	if !strings.Contains(got, "cannot guarantee") {
		t.Error("the entry MUST disclaim inferential identification — VerifyRedaction cannot " +
			"catch pronouns, role references or Recital 26 quasi-identifiers, so claiming " +
			"full removal would be false")
	}
	// And why the record still exists at all.
	if !strings.Contains(got, "other people") {
		t.Error("the entry must explain why the record was kept, or the subject reads a " +
			"partial erasure as a refusal")
	}
	// The one thing it must NOT say.
	if strings.Contains(strings.ToLower(got), "your personal data has been removed") {
		t.Error("this is exactly the false claim §7 exists to prevent")
	}
}

// A deleted record gets the unambiguous sentence, and each class is distinguishable —
// a subject must be able to tell a full removal from a rewrite.
func TestNarrative_EachOutcomeClassIsDistinct(t *testing.T) {
	res := &ErasureResult{
		RowsDeleted: 1,
		Deleted:     []DeletedRecord{{Table: TableChatAuditLog, RowID: "d1"}},
		Redacted:    []RedactedAction{{Table: TableProjectMemoryChunks, RowID: "r1"}},
		CollisionDeleted: []CollisionDeletion{
			{Table: TableProjectMemoryChunks, RowID: "x1", SurvivorID: "s1"}},
		Deferred: []DeferredAction{{Table: TableProjectMemoryChunks, RowID: "f1",
			Disposition: DispositionRedact, Reason: "verification failed"}},
		Failed: []FailedAction{{Table: TableChatAuditLog, RowID: "e1", Err: "deadlock"}},
	}
	n := res.Narrative(nil)
	if len(n.Entries) != 5 {
		t.Fatalf("every record must appear exactly once, got %d: %+v", len(n.Entries), n.Entries)
	}
	seen := map[string]string{}
	for _, e := range n.Entries {
		seen[e.RowID] = e.Outcome
	}
	if seen["d1"] != NarrativeDeleted {
		t.Errorf("a plain deletion must read as a full removal, got %q", seen["d1"])
	}
	if seen["x1"] == NarrativeDeleted {
		t.Error("a collision deletion must be distinguishable from an ordinary one — the " +
			"subject is entitled to the actual reason")
	}
	if !strings.Contains(seen["x1"], "duplicated") {
		t.Errorf("the collision entry must say why, got %q", seen["x1"])
	}
	if seen["r1"] != NarrativeRedacted {
		t.Errorf("a redaction must read as a rewrite, got %q", seen["r1"])
	}
	for _, id := range []string{"f1", "e1"} {
		if !strings.Contains(seen[id], "NOT been changed") {
			t.Errorf("%s must state plainly that the record is unchanged, got %q", id, seen[id])
		}
	}
	// Deferred and failed must not read identically — one is undecidable, the other
	// is a fault, and the operator handling them needs to know which.
	if seen["f1"] == seen["e1"] {
		t.Error("a deferral and an error must be distinguishable")
	}
}

// The operator-facing reason rides along with the deferral, so the report doubles as
// the handover note rather than requiring a separate log dig.
func TestNarrative_DeferralCarriesTheOperatorReason(t *testing.T) {
	res := &ErasureResult{Deferred: []DeferredAction{{
		Table: TableProjectMemoryChunks, RowID: "f1",
		Disposition: DispositionRedact,
		Reason:      "the proposed rewrite did not verify clean",
	}}}
	n := res.Narrative(nil)
	if n.Entries[0].Detail != "the proposed rewrite did not verify clean" {
		t.Errorf("the reason must survive into the report, got %q", n.Entries[0].Detail)
	}
}

// THE SUMMARY MUST NOT CLAIM COMPLETION IT DID NOT ACHIEVE. This is the sentence a
// subject reads first, and an incomplete erasure announced as complete is the exact
// failure the whole increment is built to avoid.
func TestNarrative_SummaryRefusesToClaimAnIncompleteErasureIsDone(t *testing.T) {
	incomplete := &ErasureResult{
		RowsDeleted: 2,
		Deleted:     []DeletedRecord{{RowID: "a"}, {RowID: "b"}},
		Deferred:    []DeferredAction{{RowID: "c", Reason: "verification failed"}},
	}
	s := incomplete.Narrative(nil).Summary
	if !strings.Contains(s, "NOT yet complete") {
		t.Errorf("an incomplete erasure must say so first, got %q", s)
	}
	if !strings.Contains(s, "still contain your data") {
		t.Errorf("and must say the records still hold the subject's data, got %q", s)
	}

	complete := &ErasureResult{
		RowsDeleted: 1,
		Deleted:     []DeletedRecord{{RowID: "a"}},
		Redacted:    []RedactedAction{{RowID: "b"}},
	}
	cs := complete.Narrative(nil).Summary
	if strings.Contains(cs, "NOT yet complete") {
		t.Errorf("a complete erasure must not be hedged, got %q", cs)
	}
	if !strings.Contains(cs, "Every record") {
		t.Errorf("a complete erasure should say so plainly, got %q", cs)
	}
}

// Retained categories and limitations come from the plan and must reach the report —
// they are the difference between "we erased everything" and the truth.
func TestNarrative_CarriesRetainedCategoriesAndLimitations(t *testing.T) {
	plan := &ErasurePlan{
		RetainedCategories: map[string]string{
			"chat_audit_log": "Retained under Art 17(3)(b) / Art 99",
		},
		Limitations: []string{"backups are not reached"},
	}
	n := (&ErasureResult{}).Narrative(plan)
	if n.Retained["chat_audit_log"] == "" {
		t.Error("retained categories must reach the subject with their legal ground")
	}
	if len(n.Limitations) != 1 {
		t.Error("limitations must reach the subject — backups and replicas are not reached")
	}
}

// A nil plan must not panic: the report is written even for an execution that failed
// early, and that is exactly when the report matters most.
func TestNarrative_NilPlanIsSafe(t *testing.T) {
	n := (&ErasureResult{}).Narrative(nil)
	if n.Summary == "" {
		t.Error("even an empty result must produce a summary sentence")
	}
}

// The subject should not have to interpret a database table name.
func TestNarrative_DescribesRecordsInPlainLanguage(t *testing.T) {
	res := &ErasureResult{Redacted: []RedactedAction{
		{Table: TableProjectMemoryChunks, RowID: "c1"}}}
	what := res.Narrative(nil).Entries[0].What
	if what == string(TableProjectMemoryChunks) {
		t.Errorf("a known table should be described, not named raw: %q", what)
	}
	// An unknown table falls back to its name rather than inventing a description.
	unknown := &ErasureResult{Redacted: []RedactedAction{
		{Table: LinkableTable("some_future_table"), RowID: "c2"}}}
	if got := unknown.Narrative(nil).Entries[0].What; got != "some_future_table" {
		t.Errorf("an unknown table must fall back to its own name, got %q", got)
	}
}
