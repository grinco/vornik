package service

// Wires per-worker leader-election Electors into the daemon
// lifecycle. Each singleton background worker (autonomy
// manager, memory consolidate, title/classify backfill,
// KG extraction, equity sampler, watchdog, archive sweeper,
// retention sweeper, external_wait monitor, cpc_timeout
// scanner, reminders runner) gets its own elector so a
// multi-replica deployment can roll individual workers between
// replicas independently.
//
// The archive sweeper's elector is wired in
// container_archive_sweeper.go (it's tied to the sweeper's
// construction). This file owns the rest.
//
// See https://docs.vornik.io → "Horizontal scaling — multi-instance
// deployment" + https://docs.vornik.io

import (
	"context"

	"vornik.io/vornik/internal/leaderelection"
)

// workerIDPrefix lets operators distinguish leader-lock rows
// owned by the daemon from any future external owner. Keeps
// the `daemon_leader_locks` table self-describing without an
// extra "source" column.
const workerIDPrefix = ""

// initWorkerElector constructs an Elector for one named worker.
// Returns nil when the lock repo isn't wired (SQLite branch in
// v1 — single-process deployments skip election entirely).
//
// Holder ID comes from c.daemonHolderID() (hostname:pid:nonce)
// so every elector this daemon owns reports the same holder
// string. Lets the future doctor check join rows to "this
// daemon owns these worker locks" without per-elector
// bookkeeping.
// InitWorkerElector is the exported wrapper for initWorkerElector, allowing
// subsystems that have moved to internal/enterprise to create a leader elector
// via the Container reference in BuildDeps without accessing the unexported method.
func (c *Container) InitWorkerElector(workerID string) *leaderelection.Elector {
	return c.initWorkerElector(workerID)
}

func (c *Container) initWorkerElector(workerID string) *leaderelection.Elector {
	if !c.capabilities().RunWorkers {
		c.Logger.Debug().Str("worker_id", workerID).
			Msg("node profile: run_workers=false; skipping singleton worker elector")
		return nil
	}
	if c.repos == nil || c.repos.LeaderLocks == nil {
		return nil
	}
	return leaderelection.New(
		c.repos.LeaderLocks,
		workerIDPrefix+workerID,
		c.daemonHolderID(),
		leaderelection.DefaultTTL,
		c.Logger.With().Str("component", "leader-election").Logger(),
	)
}

// allElectors returns every elector this container constructed,
// in no particular order. Skips nil entries so callers don't
// need per-elector nil checks. Used by the shutdown sequence to
// release leases before DB close.
func (c *Container) allElectors() []*leaderelection.Elector {
	if c == nil {
		return nil
	}
	candidates := []*leaderelection.Elector{
		c.archiveSweeperElector,
		c.autonomyElector,
		c.titleBackfillElector,
		c.classifyBackfillElector,
		c.consolidateElector,
		c.llmConsolidateElector,
		c.instinctElector,
		c.kgExtractElector,
		c.watchdogElector,
		c.telegramPollerElector,
		c.retentionElector,
		c.externalWaitElector,
		c.cpcTimeoutElector,
		c.remindersElector,
		c.ratelimitCounterSweepElector,
		// clusterNodePrunerElector + clusterMonitorElector moved to
		// internal/enterprise/clustering (Phase 2c); they register via
		// RegisterExtraElector and are released through extraElectors below.
	}
	out := make([]*leaderelection.Elector, 0, len(candidates))
	for _, e := range candidates {
		if e != nil {
			out = append(out, e)
		}
	}
	// Fold in extras minted by Subsystem.Start that don't have a
	// dedicated named field (per-project email IMAP electors).
	// Without this, releaseAllLeaderLeases() would skip them on
	// drain — peer replicas wait the full TTL before claiming
	// the per-project email lock.
	c.extraElectorsMu.Lock()
	for _, e := range c.extraElectors {
		if e != nil {
			out = append(out, e)
		}
	}
	c.extraElectorsMu.Unlock()
	return out
}

// releaseAllLeaderLeases best-effort releases every leader lease
// this daemon holds. Called from the drain sequence before any
// DB-closing shutdown phase fires, so the DELETE statements
// reach the database. Without this, peer replicas would wait
// out the TTL (typically minutes) before claiming the leases.
//
// Per-elector errors are logged but never returned — the TTL
// expiry is the safety net. The caller continues into the rest
// of shutdown regardless.
func (c *Container) releaseAllLeaderLeases(ctx context.Context) {
	for _, e := range c.allElectors() {
		_ = e.Release(ctx)
	}
}
