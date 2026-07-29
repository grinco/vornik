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
	EraseArtifact(ctx context.Context, artifactID string) (chunksDeleted int, err error)
}

// Executor performs a decided erasure plan.
type Executor struct {
	Rows      RowDeleter
	Artifacts ArtifactEraser
}

// DeferredAction is a planned action this slice could not perform.
type DeferredAction struct {
	Table       LinkableTable `json:"table"`
	RowID       string        `json:"row_id"`
	Disposition Disposition   `json:"disposition"`
	Reason      string        `json:"reason"`
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
	// ArtifactsErased counts source artifacts put through the cascade.
	ArtifactsErased int `json:"artifacts_erased"`
	// DerivedChunksDeleted counts memory chunks the cascade removed.
	DerivedChunksDeleted int `json:"derived_chunks_deleted"`

	// Deferred are rows this slice cannot yet action (redaction — slice 5c).
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

	for _, a := range plan.Actions {
		switch a.Disposition {
		case DispositionDelete:
			e.applyDelete(ctx, a, res)
		case DispositionRedact:
			// Slice 5c. Reported, never silently passed over.
			res.Deferred = append(res.Deferred, DeferredAction{
				Table: a.Table, RowID: a.RowID, Disposition: a.Disposition,
				Reason: "row also concerns other subjects and the recorded ground permits redaction, " +
					"but redact-and-re-embed is not yet implemented — this row has NOT been erased",
			})
		default:
			// An unknown disposition is a programming error, and treating it as
			// a no-op would silently drop a row from the erasure.
			res.Failed = append(res.Failed, FailedAction{
				Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("unknown disposition %q", a.Disposition),
			})
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
		chunks, err := e.Artifacts.EraseArtifact(ctx, a.RowID)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID, Err: err.Error()})
			return
		}
		res.ArtifactsErased++
		res.DerivedChunksDeleted += chunks
		return
	}
	if err := e.Rows.DeleteRow(ctx, a.Table, a.RowID); err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID, Err: err.Error()})
		return
	}
	res.RowsDeleted++
}
