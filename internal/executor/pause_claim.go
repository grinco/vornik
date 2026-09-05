package executor

// Pause claims — see https://docs.vornik.io
// ownership-design.md.
//
// THE RULE (design §4): a pause is claimed in memory before it is written to
// the database, and the claim — not the goroutine's liveness — is what
// authorises every write the pause makes. No other writer may erase a claimed
// pause.
//
// Why a claim is needed at all. Setting e.shuttingDown is also the signal every
// on_fail guard in the workflow loop waits for. The instant Shutdown flips it,
// the in-flight goroutine stops routing, returns through runExecution's
// isShuttingDown arm, and its deferred cleanupExecution DELETES ITS OWN HANDLE
// from activeExecutions. pauseWithReason, which resolved its authority to write
// from that map, then found nothing and returned ErrNoActiveExecution — so a
// graceful shutdown wrote no pause at all, and the next daemon start orphaned
// the RUNNING row instead of resuming its checkpoint. Measured at 9 failures in
// 6000 runs of TestShutdown_SystemStepOnFailDoesNotMarkFailedOnShutdown before
// the fix; presence in activeExecutions is a liveness fact, not an authority to
// write, and it expires precisely because the pause asked the goroutine to stop.

// LOCKING. Claims have their own mutex rather than reusing e.mu, and the order
// is always e.mu → claimMu, never the reverse. claimPause is called while e.mu
// is held (that is the point — the claim must be taken in the same critical
// section that decides to pause); stampPauseClaim is called from
// saveExecutionState, which Resume reaches WITH e.mu held. Sharing one mutex
// would deadlock that path.

// INVARIANT, and future maintainers depend on it: a pause reason is stamped
// onto an execution's snapshot ONLY while a claim is held — by the pause path
// itself, or by stampPauseClaim on its behalf. Two consequences the code relies
// on: a second pause cannot stamp while a first claim stands (claimPause
// refuses it), and abandonRefusedPause can therefore clear a reason it wrote
// without racing anyone. Add a writer that stamps without a claim and both stop
// being true.

// claimPause records that taskID is being paused for the given reason, and
// reports whether this caller took the claim. Call it from the same critical
// section that decides to pause — a claim taken after the decision could be
// lost to the very race it exists to close.
//
// A second claim on a task that already has one returns false and leaves the
// first reason standing: the first pause to decide owns the outcome.
func (e *Executor) claimPause(taskID, reason string) bool {
	if e == nil || taskID == "" {
		return false
	}
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	if e.pauseClaims == nil {
		e.pauseClaims = make(map[string]string)
	}
	if _, exists := e.pauseClaims[taskID]; exists {
		return false
	}
	e.pauseClaims[taskID] = reason
	return true
}

// pauseClaim returns the reason claimed for taskID, if any.
func (e *Executor) pauseClaim(taskID string) (string, bool) {
	if e == nil || taskID == "" {
		return "", false
	}
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	reason, ok := e.pauseClaims[taskID]
	return reason, ok
}

// releasePauseClaim drops taskID's claim. Called when the pause did not take
// (the conditional status write was refused because the execution had already
// finished, been cancelled, or was already paused), when Resume lifts the
// pause, and when a fresh execution is dispatched for the task — a
// re-dispatched task is not paused, and a claim left behind would re-stamp a
// pause reason onto every later checkpoint.
func (e *Executor) releasePauseClaim(taskID string) {
	if e == nil || taskID == "" {
		return
	}
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	delete(e.pauseClaims, taskID)
}

// stampPauseClaim applies a claimed pause reason to a state snapshot on its way
// to the database (design §5.4). Every snapshot write in this package funnels
// through saveExecutionState, so whoever writes — the workflow loop's
// checkpoint, markStepInFlight, the retry-counter reset — a claimed pause
// survives it.
//
// An explicit reason already in hand always wins, which is what lets Resume
// clear the field: it releases the claim BEFORE writing the cleared state.
//
// Note what this deliberately does NOT do: suppress the write. A step that
// genuinely finished during the SIGTERM drain should have its result
// checkpointed — freezing the snapshot at the pause point would re-run that
// step on resume, paying for it twice and repeating its side effects.
func (e *Executor) stampPauseClaim(taskID string, state *executionState) {
	if e == nil || state == nil || state.PausedReason != "" {
		return
	}
	if reason, ok := e.pauseClaim(taskID); ok {
		state.PausedReason = reason
	}
}
