// Package persistence — trading subsystem interfaces.
//
// Broker→daemon audit channel: TradingOrder + TradingSafetyEvent + TradingFill + TradingPositionsSnapshot. All four surface via the broker-MCP audit writer; the daemon consumes via these repos for soak-panel + UI surfaces.
// Split from interfaces.go on 2026-05-28 to keep each domain in
// its own file. Same package; no API change — pure file-org.
package persistence

import (
	"context"
	"time"
)

// TradingOrderRepository persists order events streamed from
// the broker MCP via the audit channel. UPSERT semantics
// keyed on (project_id, idempotency_key) — the writer's retry
// loop can POST the same row repeatedly under transient
// daemon outages and the row lands once.
type TradingOrderRepository interface {
	// Record upserts one order row. PRIMARY KEY collisions on
	// id are also handled (the broker writes a deterministic id
	// per call), so the same order can be re-posted with no
	// harm. Returns nil on both insert and no-op-on-conflict
	// paths so the broker's writer treats them identically.
	Record(ctx context.Context, order *TradingOrder) error
	// List returns rows matching the filter, newest-first.
	// Caller iterates the slice; the soak-panel tiles use this
	// to count orders / sum volume over a window.
	List(ctx context.Context, filter TradingOrderFilter) ([]*TradingOrder, error)
	// Count returns the total row count under filter — for
	// pagination headers + headline tiles.
	Count(ctx context.Context, filter TradingOrderFilter) (int64, error)
}

// TradingSafetyEventRepository persists broker-side safety
// decisions streamed from the broker MCP via the audit
// channel. INSERT-only — events are append-only history; no
// updates after the fact. PRIMARY KEY on id keeps retries
// idempotent (broker's writer can re-post the same row under
// transient daemon outages and the row lands once).
type TradingSafetyEventRepository interface {
	// Record inserts one safety event row. ON CONFLICT (id) DO
	// NOTHING so retries are silent no-ops.
	Record(ctx context.Context, event *TradingSafetyEvent) error
	// List returns events matching the filter, newest-first.
	// Powers the project-page safety timeline.
	List(ctx context.Context, filter TradingSafetyEventFilter) ([]*TradingSafetyEvent, error)
	// Count returns the total row count under filter.
	Count(ctx context.Context, filter TradingSafetyEventFilter) (int64, error)
}

// TradingFillRepository persists fill events streamed from the
// broker MCP's poll loop. INSERT-only — fills are append-only
// history. PRIMARY KEY on id keeps retries idempotent (broker's
// writer uses a deterministic id per (order_id, filled_at) pair).
type TradingFillRepository interface {
	// Record inserts one fill row. ON CONFLICT (id) DO
	// NOTHING so retries are silent no-ops. Writes the four
	// exec-keyed columns (exec_id, account_id, source,
	// source_detail) introduced in the fill-reconciliation arc.
	Record(ctx context.Context, fill *TradingFill) error
	// List returns fills matching the filter, newest-first.
	// Powers the per-symbol P&L rollup + soak-panel volume
	// (precise once fills land vs. the trading_orders-based
	// estimate today).
	List(ctx context.Context, filter TradingFillFilter) ([]*TradingFill, error)
	// SumVolume returns SUM(qty * price) over fills matching the
	// filter (typically a project + time window). Powers the
	// soak panel's "volume today / 7d" tile with precise
	// realised volume — replaces the trading_orders LMT-price
	// estimate that historically inflated cancelled-but-non-
	// filled flow into the metric.
	SumVolume(ctx context.Context, filter TradingFillFilter) (float64, error)
	// MaxFilledAt returns the newest filled_at timestamp for the
	// given project — the exec-reconcile cursor seed. Returns
	// the Unix epoch (to_timestamp(0)) when the project has no
	// fills yet, so the caller can use it directly as a "since"
	// bound without a nil check.
	MaxFilledAt(ctx context.Context, projectID string) (time.Time, error)
	// PatchCommission fills commission_usd only when the column is
	// currently NULL, so a late commissionReportEvent cannot
	// overwrite a sweep-populated zero with a stale value. The
	// idempotency contract: calling twice with the same value is
	// safe — the second call is a no-op because the first write
	// makes the WHERE clause false.
	PatchCommission(ctx context.Context, id string, commissionUSD float64) error
	// RecordShadow inserts a fill into trading_fills_shadow for
	// shadow-mode reconciliation comparison. The shadow table has
	// no FK to trading_orders, so fills can be recorded before
	// the corresponding order is ingested. Idempotent on id via
	// ON CONFLICT (id) DO NOTHING.
	RecordShadow(ctx context.Context, fill *TradingFill) error
	// ListNullCommission returns fills whose commission_usd IS NULL and
	// whose filled_at is older than olderThan. Used by the commission
	// backfill sweep (SweepMissingCommissions) to find fills that never
	// received a commissionReportEvent from the broker. Returns enough
	// fields to match back to an execution: id, exec_id, account_id,
	// project_id, symbol, filled_at.
	ListNullCommission(ctx context.Context, olderThan time.Time) ([]*TradingFill, error)
}

// TradingPositionsSnapshotRepository writes and reads the
// equity/cash/unrealised-PL time series. The daemon's sampler
// goroutine writes one row per project per cadence (5 min default);
// the project-detail UI's soak panel reads back to compute Sharpe
// and max drawdown.
type TradingPositionsSnapshotRepository interface {
	// Record inserts one snapshot row. ID is generated by the
	// caller; RecordedAt defaults to NOW() when zero.
	Record(ctx context.Context, snap *TradingPositionsSnapshot) error
	// ListSince returns snapshots for one project recorded at or
	// after `since`, oldest first so callers can iterate the
	// time series without re-sorting. Capped at limit; pass 0
	// for the repository's default (10000 — enough for 30+ days
	// at the default 5-min cadence).
	ListSince(ctx context.Context, projectID string, since time.Time, limit int) ([]*TradingPositionsSnapshot, error)
}
