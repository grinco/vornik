package executor

import (
	"context"
	"errors"

	"vornik.io/vornik/internal/persistence"
)

// ancestorOrEnd loads a lineage ancestor by id, reporting a row that is not
// there as the END OF THE CHAIN rather than as an error.
//
// WHY THIS EXISTS. Four walks climb task.ParentTaskID — countCheckpointDepth,
// countRouteDepth, countDelegationDepth and ancestorIDs — and every one of them
// was written as:
//
//	anc, err := e.taskRepo.Get(ctx, pid)
//	if err != nil { return depth, err }   // fatal
//	if anc == nil { return depth, nil }   // chain ends here
//
// The intent is in the second branch and is right: an ancestor row that is gone
// (pruned by retention, deleted with a project) ends the lineage, it does not
// break the guard. But `TaskRepository.Get` is MissErrNotFound — production
// returns (nil, persistence.ErrNotFound) for an absent row, never (nil, nil).
// So the terminating branch was DEAD against the real database, and the fatal
// one is what a missing ancestor actually took: a checkpoint whose depth could
// not be counted, and a delegation refused with "delegation cycle check
// failed: not found".
//
// It stayed invisible because every test double in this package answered a miss
// with (nil, nil) — looser than production, and so certifying the miss path
// without ever exercising it. Found 2026-08-22 by tightening those doubles
// (https://docs.vornik.io).
//
// One primitive rather than the same three lines four times: this is the fourth
// occurrence of the class, which is where a repeated fix becomes a seam.
func (e *Executor) ancestorOrEnd(ctx context.Context, id string) (*persistence.Task, error) {
	anc, err := e.taskRepo.Get(ctx, id)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		// A real failure — connection lost, context cancelled. Distinct from
		// absence, and it must NOT be read as "the lineage ends here": that
		// would silently under-count a depth guard.
		return nil, err
	}
	return anc, nil
}
