package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DurabilityReport mirrors featuredoctor.DurabilityReport without importing
// it (storage is a leaf the doctor's adapter wraps).
type DurabilityReport struct {
	Driver string
	OK     bool
	Detail string
}

// ProbeDurability reports whether the active store honours the durable
// commit contract the config-apply journal relies on (2026-09-13
// config-assistant review R6, plan §7g):
//
//   - SQLite: PRAGMA synchronous must be FULL (2) or EXTRA (3). With WAL,
//     NORMAL (1) can lose the last transactions on power loss, which turns
//     a PREPARED journal row into a record of an apply that then did not
//     happen — the opposite of a recovery record.
//   - Postgres: synchronous_commit must be on, remote_write, or
//     remote_apply for the daemon's session. off/local acknowledge a
//     commit before it is durable on disk / on the replica.
//
// The probe reads the LIVE setting on a fresh connection from the pool so
// an override (a per-role ALTER, a DSN pragma) is what gets reported, not
// what the daemon believes it configured.
func ProbeDurability(ctx context.Context, driver string, db *sql.DB) DurabilityReport {
	if db == nil {
		return DurabilityReport{Driver: driver, OK: false, Detail: "no database handle"}
	}
	switch driver {
	case "sqlite":
		var v int
		if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&v); err != nil {
			return DurabilityReport{Driver: driver, OK: false, Detail: "PRAGMA synchronous: " + err.Error()}
		}
		name := map[int]string{0: "OFF", 1: "NORMAL", 2: "FULL", 3: "EXTRA"}[v]
		if name == "" {
			name = fmt.Sprintf("%d", v)
		}
		return DurabilityReport{Driver: driver, OK: v >= 2, Detail: "sqlite synchronous=" + name}
	case "postgres":
		var v string
		if err := db.QueryRowContext(ctx, `SHOW synchronous_commit`).Scan(&v); err != nil {
			return DurabilityReport{Driver: driver, OK: false, Detail: "SHOW synchronous_commit: " + err.Error()}
		}
		v = strings.ToLower(strings.TrimSpace(v))
		ok := v == "on" || v == "remote_write" || v == "remote_apply"
		return DurabilityReport{Driver: driver, OK: ok, Detail: "postgres synchronous_commit=" + v}
	default:
		return DurabilityReport{Driver: driver, OK: false, Detail: "unknown driver " + driver}
	}
}

// ProbeDurability is the Backend-bound form.
func (b *Backend) ProbeDurability(ctx context.Context) DurabilityReport {
	if b == nil {
		return DurabilityReport{OK: false, Detail: "no storage backend"}
	}
	return ProbeDurability(ctx, b.Driver, b.DB)
}
