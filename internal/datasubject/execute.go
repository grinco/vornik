package datasubject

import (
	"context"
	"errors"
	"fmt"
)

// Art 17 erasure — execution (design §4.6 steps 2-3), increment 5 slice 5b.
//
// PlanErasure decided; this performs. The split is deliberate: the decision is
// pure and exhaustively tested because it is the part that is irreversible if
// wrong, and this layer stays thin enough that its only real judgement is which
// store to route a row to.
//
// This slice performs DELETIONS ONLY. Redaction — removing one subject's data
// from a row that also concerns somebody else, and re-deriving the embedding —
// needs an LLM pass over the memory pipeline and is slice 5c. Until it lands, a
// redact action is reported as DEFERRED with its reason and is never counted as
// erased. That is the single most important property in this file: an erasure
// report that tallies an untouched row as erased tells a data subject their data
// is gone when it is not, which is worse than reporting honest partial progress.

// RowDeleter deletes one row of a linkable table. Table names come from the
// closed set, never from user input.
type RowDeleter interface {
	DeleteRow(ctx context.Context, table LinkableTable, rowID string) error
}

// ArtifactEraser runs the full artifact cascade — extraction rows, derived memory
// chunks, and the on-disk storage directory — returning how many chunks went.
//
// Artifacts are NOT deleted as plain rows. `extracted_documents` has no foreign
// key on `source_artifact_id`, and `project_memory_chunks.artifact_id` is
// ON DELETE SET NULL, so a plain row delete orphans the extraction, leaves the
// derived embedding in the vector store, and destroys the provenance that would
// let anyone find it later. internal/erasure exists for exactly this and has the
// containment and filesystem-before-rows properties already tested.
type ArtifactEraser interface {
	EraseArtifact(ctx context.Context, artifactID string) (ArtifactErasureCounts, error)
}

// ArtifactErasureCounts is what one artifact's cascade removed.
//
// It carries the DERIVED-graph counts alongside the chunk count because an
// erasure report that lists chunks and silently omits what those chunks derived
// is how 3,795 knowledge-graph entities accumulated in production behind
// erasures reported as complete. Once those rows are gone the report is the only
// evidence they were covered, so the subject-facing report has to be able to
// state them.
type ArtifactErasureCounts struct {
	ChunksDeleted int
	// GraphEntitiesDeleted are knowledge_entities no surviving chunk mentions.
	GraphEntitiesDeleted int
	// GraphEdgesDeleted are knowledge_edges left without evidence, plus those
	// removed with their entity by foreign key.
	GraphEdgesDeleted int
	// QuarantinedCopiesDeleted are project_memory_quarantine rows: the chunk's
	// full text, held because an ingest gate REJECTED it. Its foreign key is
	// ON DELETE SET NULL, so erasing the chunk alone left this copy behind.
	QuarantinedCopiesDeleted int
}

// Executor performs a decided erasure plan.
type Executor struct {
	Rows      RowDeleter
	Artifacts ArtifactEraser
	// Redact carries the stores needed for DispositionRedact (slice 5c). Nil on an
	// executor wired for deletion only, in which case a planned redaction is
	// DEFERRED with the capability-missing reason — never silently skipped, and
	// never treated as an error, because a missing capability is not a fault.
	Redact *RedactDeps
}

// DeferredAction is a planned action this slice could not perform.
type DeferredAction struct {
	Table       LinkableTable `json:"table"`
	RowID       string        `json:"row_id"`
	Disposition Disposition   `json:"disposition"`
	Reason      string        `json:"reason"`
}

// DeletedRecord identifies one row actually removed.
type DeletedRecord struct {
	Table LinkableTable `json:"table"`
	RowID string        `json:"row_id"`
}

// FailedAction is a planned action that was attempted and errored.
type FailedAction struct {
	Table LinkableTable `json:"table"`
	RowID string        `json:"row_id"`
	Err   string        `json:"error"`
}

// ErasureResult is what an execution actually did — as distinct from what the
// plan intended, which is the distinction a subject-facing report lives or dies
// on.
type ErasureResult struct {
	SubjectID string `json:"subject_id"`
	RequestID string `json:"request_id"`

	// RowsDeleted counts rows actually removed. Deferred and failed rows are
	// never included.
	RowsDeleted int `json:"rows_deleted"`
	// Deleted lists them. The count alone cannot produce a per-record
	// subject-facing narrative, and the subject is entitled to know WHICH records
	// were removed, not just how many.
	Deleted []DeletedRecord `json:"deleted,omitempty"`
	// ArtifactsErased counts source artifacts put through the cascade.
	ArtifactsErased int `json:"artifacts_erased"`
	// DerivedChunksDeleted counts memory chunks the cascade removed.
	DerivedChunksDeleted int `json:"derived_chunks_deleted"`
	// DerivedGraphRowsDeleted counts what those chunks had DERIVED: knowledge
	// entities and edges, and the pre-ingest copies in the quarantine table.
	// Separate from the chunk count because they are a different claim — the
	// chunks were the data, these are what the system built out of it, and an
	// erasure that removed one and not the other was still reported as
	// complete until 2026-08-21.
	DerivedGraphEntitiesDeleted int `json:"derived_graph_entities_deleted"`
	DerivedGraphEdgesDeleted    int `json:"derived_graph_edges_deleted"`
	QuarantinedCopiesDeleted    int `json:"quarantined_copies_deleted"`

	// Redacted are shared rows rewritten to remove the subject while preserving
	// the other subjects the row concerns.
	Redacted []RedactedAction `json:"redacted,omitempty"`
	// CollisionDeleted are rows deleted because their redacted form duplicated an
	// existing row. Separate from RowsDeleted because the subject is told a
	// different thing about them.
	CollisionDeleted []CollisionDeletion `json:"collision_deleted,omitempty"`

	// Deferred are rows that could not be actioned — a failed verification, a
	// declined review, a fired version guard, or a missing capability.
	Deferred []DeferredAction `json:"deferred,omitempty"`
	// Failed are rows that were attempted and errored.
	Failed []FailedAction `json:"failed,omitempty"`
}

// Complete reports whether every planned action was actually carried out.
//
// The caller MUST consult this before moving a request to StateActioned. A
// request marked actioned while rows are deferred or failed would put a false
// completion in the accountability ledger — and the ledger is the evidence that
// the right was honoured.
func (r *ErasureResult) Complete() bool {
	return len(r.Deferred) == 0 && len(r.Failed) == 0
}

// ErrPartialErasure signals that one or more planned rows ERRORED. It does not
// fire for a deferred redaction, which is a missing capability rather than a
// fault — see Execute.
var ErrPartialErasure = errors.New("datasubject: erasure failed for one or more planned rows")

// Execute carries out a decided plan.
//
// FAILURE POLICY: a failing row does NOT abandon the rest. Stopping at the first
// error would leave more of the subject's data in place than continuing does, and
// the subject's interest is in maximal erasure. But every failure is recorded and
// the call returns an error, so a caller cannot mistake partial progress for
// success — the result is returned ALONGSIDE the error, because "what did happen"
// is exactly what the operator needs when an erasure half-completes.
func (e *Executor) Execute(ctx context.Context, plan *ErasurePlan) (*ErasureResult, error) {
	if plan == nil {
		return nil, errors.New("datasubject: no erasure plan to execute")
	}
	// Refuse before touching anything: a half-wired executor part-way through an
	// irreversible operation is the worst possible time to discover the wiring.
	if e == nil || e.Rows == nil || e.Artifacts == nil {
		return nil, errors.New("datasubject: executor is missing required stores " +
			"(row deleter and artifact eraser) — refusing to begin an irreversible erasure")
	}

	res := &ErasureResult{SubjectID: plan.SubjectID, RequestID: plan.RequestID}

	// Identifiers are re-queried HERE, at execution, not carried from planning
	// (§5 step 2): a plan-time snapshot can be stale, and verifying a rewrite
	// against a stale set would report success while a newly-recorded identifier
	// survives in the text. Read once for the whole plan — the set belongs to the
	// request and is stable for its duration.
	var (
		ids     []string
		idErr   error
		retries []collisionRetry
	)
	if e.Redact != nil && e.Redact.Identifiers != nil {
		ids, idErr = e.Redact.Identifiers.Identifiers(ctx, plan.SubjectID)
	} else if e.Redact != nil {
		idErr = errors.New("no identifier source is wired")
	}

	for _, a := range plan.Actions {
		switch a.Disposition {
		case DispositionDelete:
			e.applyDelete(ctx, a, res)
		case DispositionRedact:
			if e.Redact == nil || e.Redact.Redactor == nil || e.Redact.Rewriter == nil {
				// Reported, never silently passed over.
				res.Deferred = append(res.Deferred, DeferredAction{
					Table: a.Table, RowID: a.RowID, Disposition: a.Disposition,
					Reason: "row also concerns other subjects and the recorded ground permits " +
						"redaction, but this executor has no redaction capability wired — this " +
						"row has NOT been erased",
				})
				break
			}
			if idErr != nil {
				// Without the subject's identifiers there is nothing to verify a
				// rewrite against, so proceeding would mean writing a generated
				// replacement on an unverifiable basis. Defer the whole redaction set.
				res.Deferred = append(res.Deferred, DeferredAction{
					Table: a.Table, RowID: a.RowID, Disposition: a.Disposition,
					Reason: fmt.Sprintf("the subject's identifiers could not be read (%v), so no "+
						"rewrite could be verified; this row has NOT been changed", idErr),
				})
				break
			}
			if retry := e.applyRedact(ctx, a, ids, plan, res); retry != nil {
				retries = append(retries, *retry)
			}
		default:
			// An unknown disposition is a programming error, and treating it as
			// a no-op would silently drop a row from the erasure.
			res.Failed = append(res.Failed, FailedAction{
				Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("unknown disposition %q", a.Disposition),
			})
		}
	}

	// Second pass for collision deferrals (§4.2). A chunk whose redacted form
	// duplicated a chunk that was ITSELF awaiting redaction could not be resolved on
	// the first pass: deleting into that survivor would have preserved a chunk still
	// containing the subject. By now the survivor has been redacted or has failed, so
	// the collision can be re-evaluated once. Once only — a chunk still unresolved
	// after this is reported deferred and needs a human, not an unbounded retry loop.
	for _, r := range retries {
		if next := e.applyRedact(ctx, r.action, ids, plan, res); next != nil {
			e.deferRedaction(r.action, res, fmt.Sprintf(
				"after redaction this record duplicated record %s, which is itself still "+
					"unresolved, so it was left untouched and needs manual handling",
				next.survivorID))
		}
	}

	// An error means something WENT WRONG. A deferred redaction did not go wrong
	// — it is a capability this slice does not have yet, and erroring on it would
	// make every shared-row erasure look like a malfunction. The guard against
	// mistaking it for success is Complete(), which the caller must consult
	// before moving the request to StateActioned.
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%w: %d of %d planned row(s) failed",
			ErrPartialErasure, len(res.Failed), len(plan.Actions))
	}
	return res, nil
}

// applyDelete routes one delete to the right store and records the outcome.
func (e *Executor) applyDelete(ctx context.Context, a Action, res *ErasureResult) {
	if a.Table == TableArtifacts {
		counts, err := e.Artifacts.EraseArtifact(ctx, a.RowID)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID, Err: err.Error()})
			return
		}
		res.ArtifactsErased++
		res.DerivedChunksDeleted += counts.ChunksDeleted
		res.DerivedGraphEntitiesDeleted += counts.GraphEntitiesDeleted
		res.DerivedGraphEdgesDeleted += counts.GraphEdgesDeleted
		res.QuarantinedCopiesDeleted += counts.QuarantinedCopiesDeleted
		res.Deleted = append(res.Deleted, DeletedRecord{Table: a.Table, RowID: a.RowID})
		return
	}
	if err := e.Rows.DeleteRow(ctx, a.Table, a.RowID); err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID, Err: err.Error()})
		return
	}
	res.RowsDeleted++
	res.Deleted = append(res.Deleted, DeletedRecord{Table: a.Table, RowID: a.RowID})
}
