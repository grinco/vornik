package datasubject

import "fmt"

// What the data subject is told, per class of record.
//
// The design calls this the highest-priority finding of round 1, and the reason is
// that the obvious sentence — "your personal data has been removed" — is false for a
// redacted record. VerifyRedaction proves the subject's KNOWN IDENTIFIERS are gone.
// It does not, and cannot, prove that no combination of remaining details could be
// associated with them: pronouns, role references and Recital 26 quasi-identifiers
// are all outside what any substring check reaches.
//
// So the narrative states the mechanical guarantee and disclaims the inferential one.
// That boundary is exactly the one VerifyRedaction's tests pin, which is what keeps
// this text honest rather than aspirational.
//
// see LLD § https://docs.vornik.io §7

// The subject-facing sentences. Constants rather than inline strings because these
// are compliance assertions: they are pinned by tests, quoted in the DPA, and must
// not drift with a refactor.
const (
	// NarrativeDeleted — the unambiguous case.
	NarrativeDeleted = "This record was removed in full."

	// NarrativeRedacted — the load-bearing one. States what was verified AND what
	// could not be, in that order, because a subject who reads only the first
	// sentence should not come away with a stronger belief than the truth supports.
	NarrativeRedacted = "Your known identifiers were removed from this record and the " +
		"record was re-derived. It was kept rather than deleted because it also concerns " +
		"other people, whose data we are required to preserve. We verified mechanically " +
		"that none of your identifiers remains. We cannot guarantee that no combination " +
		"of the remaining details could still be associated with you."

	// NarrativeCollisionDeleted — a deletion, but for a different reason than an
	// ordinary one, and the subject is entitled to the actual reason.
	NarrativeCollisionDeleted = "This record was removed in full; once your data was " +
		"removed from it, it duplicated another record we already hold."

	// NarrativeDeferred — named, never hidden. A subject told "erased" while records
	// remain has been misled, which is worse than being told the work is unfinished.
	NarrativeDeferred = "We could not safely remove your data from this record " +
		"automatically. It has NOT been changed, and an operator is handling it."

	// NarrativeFailed — distinct from deferred: something went wrong rather than
	// something being undecidable.
	NarrativeFailed = "An error prevented us from removing your data from this record. " +
		"It has NOT been changed, and an operator is handling it."
)

// NarrativeEntry is one record's outcome in subject-facing terms.
type NarrativeEntry struct {
	Table LinkableTable `json:"table"`
	RowID string        `json:"row_id"`
	// What describes the record in plain language, so the subject is not asked to
	// interpret a table name.
	What string `json:"what"`
	// Outcome is the sentence the subject reads.
	Outcome string `json:"outcome"`
	// Detail carries the operator-facing reason for a deferral or failure, so the
	// report is also the handover note.
	Detail string `json:"detail,omitempty"`
}

// SubjectNarrative is the subject-facing view of an execution.
type SubjectNarrative struct {
	// Summary is a single honest sentence about completeness.
	Summary string `json:"summary"`
	// Entries covers every record the plan touched or failed to touch.
	Entries []NarrativeEntry `json:"entries"`
	// Retained maps a kept data category to its legal ground. Not erasure failures —
	// Art 17(3)(b) and Art 99 require them, and the subject is entitled to know
	// which records remain and on what basis.
	Retained map[string]string `json:"retained,omitempty"`
	// Limitations are the honest bounds of the whole exercise (backups, replicas,
	// inferential identification).
	Limitations []string `json:"limitations,omitempty"`
}

// Narrative renders the execution in the language of §7.
//
// Built from the RESULT, never from the plan: the plan is what was intended and the
// result is what happened, and every sentence here is a claim about what happened.
func (r *ErasureResult) Narrative(plan *ErasurePlan) SubjectNarrative {
	n := SubjectNarrative{Summary: r.summarySentence()}
	if plan != nil {
		n.Retained = plan.RetainedCategories
		n.Limitations = plan.Limitations
	}

	for _, a := range r.Deleted {
		n.Entries = append(n.Entries, NarrativeEntry{
			Table: a.Table, RowID: a.RowID, What: describeTable(a.Table),
			Outcome: NarrativeDeleted,
		})
	}
	for _, a := range r.Redacted {
		n.Entries = append(n.Entries, NarrativeEntry{
			Table: a.Table, RowID: a.RowID, What: describeTable(a.Table),
			Outcome: NarrativeRedacted,
		})
	}
	for _, a := range r.CollisionDeleted {
		n.Entries = append(n.Entries, NarrativeEntry{
			Table: a.Table, RowID: a.RowID, What: describeTable(a.Table),
			Outcome: NarrativeCollisionDeleted,
		})
	}
	for _, a := range r.Deferred {
		n.Entries = append(n.Entries, NarrativeEntry{
			Table: a.Table, RowID: a.RowID, What: describeTable(a.Table),
			Outcome: NarrativeDeferred, Detail: a.Reason,
		})
	}
	for _, a := range r.Failed {
		n.Entries = append(n.Entries, NarrativeEntry{
			Table: a.Table, RowID: a.RowID, What: describeTable(a.Table),
			Outcome: NarrativeFailed, Detail: a.Err,
		})
	}
	return n
}

// summarySentence refuses to claim completion that did not happen.
func (r *ErasureResult) summarySentence() string {
	if r.Complete() {
		return fmt.Sprintf("Every record we identified was actioned: %d removed in full, "+
			"%d rewritten to remove your data while preserving other people's, and %d removed "+
			"because they duplicated an existing record once your data was taken out.",
			r.RowsDeleted, len(r.Redacted), len(r.CollisionDeleted))
	}
	return fmt.Sprintf("This request is NOT yet complete. %d record(s) were removed in full "+
		"and %d were rewritten, but %d could not be actioned automatically and %d failed with "+
		"an error. Those records still contain your data and an operator is handling them.",
		r.RowsDeleted, len(r.Redacted), len(r.Deferred), len(r.Failed))
}

// describeTable renders a table name in terms a subject can act on, falling back to
// the raw name rather than inventing a description for an unknown table.
func describeTable(t LinkableTable) string {
	if d, ok := linkableTables[t]; ok {
		return d
	}
	return string(t)
}
