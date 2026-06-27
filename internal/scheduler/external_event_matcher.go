package scheduler

// Phase 30 — webhook event matcher (LLD §7.1). DEFERRED.
//
// The scheduled-deadline tick (external_wait.go) covers re-
// execution trigger #4 ("expected_by reached"). The event-match
// trigger (#5: webhook payload matches a per-task predicate) is
// a follow-up:
//
//   - Operators emit `event_match` JSON inside outcome=external_wait.
//     The shape is opaque here (stored in task_messages metadata).
//   - On each accepted webhook event the matcher would scan
//     active AWAITING_EXTERNAL tasks, evaluate predicates, and
//     re-queue matches.
//
// Predicate language v1 (per LLD §7.1):
//
//   field op literal           subject == "quote ready"
//   field contains literal     body contains "invoice"
//   field in [lit, lit]        from in ["a@x", "b@x"]
//   <expr> AND <expr>          composition
//
// Implementation hooks:
//
//   - extend webhook_events ingest in internal/api/handlers.go to
//     publish accepted events on a channel.
//   - parse task_messages.metadata->event_match into an AST.
//   - evaluate per-event against active tasks (limit 1000); on
//     match call TransitionConditional + Wake.
//
// Skipped here so the scheduled-deadline behaviour can ship now
// without dragging the parser + AST eval surface in. The hook
// stub keeps the package importable from container.go without a
// missing-symbol error and serves as a single grep-anchor when
// the time comes to implement it.

// ExternalEventMatcher is a placeholder type. Future
// implementation: see this file's package comment.
type ExternalEventMatcher struct{}
