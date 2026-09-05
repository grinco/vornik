package membench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ClearResult reports what a clear removed, per table. Returned rather than
// logged so the caller can print it: an operator who runs a benchmark deserves
// to see that the store was actually reset, and a silent clear is how the
// absent one went unnoticed for so long.
type ClearResult struct {
	Mentions int64
	Edges    int64
	Entities int64
	Chunks   int64
}

// Total is the row count across every table the clear touched.
func (r ClearResult) Total() int64 { return r.Mentions + r.Edges + r.Entities + r.Chunks }

// ClearBenchmarkStore removes a benchmark project's retrievable memory so a run
// starts from a known-empty store.
//
// WHY THIS EXISTS. The design said a benchmark run "writes and clears bulk
// memory", the flag is named --i-know-this-wipes, CheckDestructiveTarget's
// refusal text promised the run "will bulk-write and clear", and
// VornikSystem.Teardown's comment said cleanup happened "once per run at the
// database level". Four statements agreeing — and nothing deleted anything.
// The authorisation was built and the action it authorises never was.
//
// Measured cost of the absence, three runs of the identical 120 items:
// admitted deposits 426 -> 426 -> 209 as the store filled, until two real
// dataset items lost their entire haystack to dedup_hash and the run was
// stamped untrustworthy. Accuracy moved 0.692 -> 0.750 on a manual wipe with
// nothing else changed, so the missing clear did not merely dirty a run — it
// moved the score, in the direction that UNDERSTATES the system, because a
// deduped item loses the haystack it is scored on.
//
// CHUNKS ARE NOT THE WHOLE SURFACE. Deleting chunks cascades entity_mentions by
// FK and stops. knowledge_entities and knowledge_edges survive, and they are on
// the retrieval path — internal/memory/query_expander.go seeds expansion from
// knowledge_entities, and internal/memory/graph/searcher.go searches entities,
// edges and mentions. A chunk-only wipe of the real bench database left 3,545
// entities and 2,392 edges behind, all still queryable. So the clear covers the
// graph too, in dependency order.
//
// WHOLESALE IS RIGHT HERE AND WRONG IN PRODUCTION. A benchmark project holds
// nothing worth keeping between runs. Erasing one subject in production must
// NOT drop a project's graph — that case is per-derived-row and is tracked
// separately (https://docs.vornik.io, Art 17 erasure).
//
// CALL ORDER. Only after CheckDestructiveTarget and VerifyWriteTarget both
// pass: those are what prove the target is neither production nor a database
// the operator did not name. Clearing before them would make this function the
// most dangerous thing in the package.
//
// scopePrefix narrows the CHUNK delete to the harness's own namespace (the
// memory harness passes "membench/%", so it cannot clear a project that merely
// starts with the same letters). Empty means project-wide, which is what the
// agent harness needs: its contamination is the bench project's own memory
// whatever scope it was written under.
//
// IT RETRIES ON DEADLOCK, and the reason is worth stating because the orders
// genuinely conflict. This clear deletes mentions -> edges -> entities ->
// chunks; the graph pipeline inserts the ENTITY first and then the MENTION
// referencing it and its chunk, taking FOR KEY SHARE on both parents. So the
// clear holds mention/entity rows while wanting chunks, and an in-flight
// extraction holds an entity while wanting the same rows. Postgres detects the
// cycle and kills one side — usually the clear, which is the whole run.
//
// It fires exactly when you would hit it: the daemon starts, finds unextracted
// chunks from the previous run, and begins working through them; the operator
// then starts a benchmark whose first action is this clear. See
// clearDeadlockAttempts for why a retry is the right size of fix, and the final
// error for what an operator does when it is not enough — "deadlock detected"
// alone tells them nothing about which two things collided.
//
// Design of record: 2026-08-10-memory-benchmark-harness-design.md §5.8 and
// 2026-08-13-agent-quality-benchmark-design.md §5.2a.
func ClearBenchmarkStore(ctx context.Context, db *sql.DB, projectID, scopePrefix string) (ClearResult, error) {
	var lastErr error
	for attempt := 1; attempt <= clearDeadlockAttempts; attempt++ {
		res, err := clearOnce(ctx, db, projectID, scopePrefix)
		if err == nil {
			return res, nil
		}
		if !isDeadlock(err) {
			return ClearResult{}, err
		}
		lastErr = err
		if attempt == clearDeadlockAttempts {
			break
		}
		// Back off before retrying: the extractor holding the other side of the
		// cycle needs to finish its statement, and retrying instantly just
		// re-enters the same deadlock.
		select {
		case <-ctx.Done():
			return ClearResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * clearDeadlockBackoff):
		}
	}
	return ClearResult{}, fmt.Errorf(
		"clear for project %q deadlocked on all %d attempts: the daemon is concurrently "+
			"extracting a graph backlog, which writes entity-then-mention while this clear "+
			"deletes mention-then-entity, so Postgres kills one side. Quiesce the ingest "+
			"worker for the duration (stop the bench daemon, clear, restart) or wait for the "+
			"backlog to drain: %w",
		strings.TrimSpace(projectID), clearDeadlockAttempts, lastErr)
}

// clearDeadlockAttempts / clearDeadlockBackoff bound the retry.
//
// The clear is ONE transaction and idempotent, so a retry is safe: nothing it
// deleted survives a rollback, and the extraction worker that won the deadlock
// moves on. Three attempts with a linear backoff covers the measured case — a
// 1,897-chunk backlog on 2026-08-21, where the clear failed on its FIRST attempt
// and took the whole run with it — without letting a permanently busy daemon
// stall a run indefinitely.
const (
	clearDeadlockAttempts = 3
	clearDeadlockBackoff  = 250 * time.Millisecond
)

// isDeadlock reports whether err is Postgres SQLSTATE 40P01 (deadlock_detected).
//
// Matched on the SQLSTATE via the driver's own error type where possible, and on
// the code string otherwise — the sqlite lane wraps errors differently and a
// string check there costs nothing, whereas importing a driver into this package
// to type-assert would be worse than the check it replaces.
func isDeadlock(err error) bool {
	if err == nil {
		return false
	}
	var pqErr interface{ SQLState() string }
	if errors.As(err, &pqErr) {
		return pqErr.SQLState() == deadlockSQLState
	}
	return strings.Contains(err.Error(), "deadlock detected") ||
		strings.Contains(err.Error(), deadlockSQLState)
}

// deadlockSQLState is Postgres deadlock_detected.
const deadlockSQLState = "40P01"

// clearOnce is one attempt. Split out so the retry above wraps a whole
// transaction rather than a statement: a deadlock aborts the transaction, so
// retrying anything smaller would resume inside a dead one.
func clearOnce(ctx context.Context, db *sql.DB, projectID, scopePrefix string) (ClearResult, error) {
	var res ClearResult

	project := strings.TrimSpace(projectID)
	if project == "" {
		return res, fmt.Errorf("refusing to clear: no project named. An empty project id " +
			"would make every predicate match nothing or everything depending on the " +
			"statement, and a clear that silently removes nothing is the defect this fixes")
	}
	if db == nil {
		return res, fmt.Errorf("refusing to clear: no database handle")
	}

	// One transaction: a half-cleared store reads as reset and is not, which is
	// the same shape as the absent clear.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("clear: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// Dependency order: mentions reference entities, so they go first. Chunks
	// go last — they are the cheapest thing to re-derive if anything fails.
	steps := []struct {
		name  string
		query string
		args  []any
		count *int64
	}{
		{
			name: "entity_mentions",
			query: `DELETE FROM entity_mentions em USING knowledge_entities ke
			         WHERE em.entity_id = ke.id AND ke.project_id = $1`,
			args: []any{project}, count: &res.Mentions,
		},
		{
			name:  "knowledge_edges",
			query: `DELETE FROM knowledge_edges WHERE project_id = $1`,
			args:  []any{project}, count: &res.Edges,
		},
		{
			name:  "knowledge_entities",
			query: `DELETE FROM knowledge_entities WHERE project_id = $1`,
			args:  []any{project}, count: &res.Entities,
		},
	}

	chunkQuery := `DELETE FROM project_memory_chunks WHERE project_id = $1`
	chunkArgs := []any{project}
	if p := strings.TrimSpace(scopePrefix); p != "" {
		// BOTH predicates. A bare prefix would clear a project someone named
		// "membench-something"; a bare project id would clear rows this
		// harness does not own.
		chunkQuery += ` AND repo_scope LIKE $2`
		chunkArgs = append(chunkArgs, p)
	}
	steps = append(steps, struct {
		name  string
		query string
		args  []any
		count *int64
	}{name: "project_memory_chunks", query: chunkQuery, args: chunkArgs, count: &res.Chunks})

	for _, s := range steps {
		out, err := tx.ExecContext(ctx, s.query, s.args...)
		if err != nil {
			return ClearResult{}, fmt.Errorf("clear %s for project %q: %w", s.name, project, err)
		}
		if n, err := out.RowsAffected(); err == nil {
			*s.count = n
		}
	}

	if err := tx.Commit(); err != nil {
		return ClearResult{}, fmt.Errorf("clear: commit: %w", err)
	}
	return res, nil
}
