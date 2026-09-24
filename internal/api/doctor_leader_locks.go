package api

// Doctor check that surfaces the health of every
// daemon_leader_locks row. Pairs with the leaderelection
// primitive shipped in 8f1ab86 + 81adf49 — operators see at a
// glance which singleton workers are running (active lease),
// which are about to expire (no daemon renewing), and which are
// expired entirely (no current leader).
//
// Severity table:
//
//   OK      — every row has expires_at in the future + a
//             renewed_at within one TTL of now.
//   WARNING — at least one row is stale (renewed_at older than
//             one TTL but lease still valid).
//   ERROR   — at least one row is fully expired (expires_at in
//             the past); no daemon is currently the leader for
//             that worker.
//
// Special case: empty table reports OK with a note. Fresh
// deployments + SQLite (whose stub returns nil) land here.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// leaderLockStaleAfter is the renewed_at age past which we flag
// a row as "stale" — the holder appears to have stopped
// renewing without releasing. Set to leaderelection.DefaultTTL
// so any row whose renewed_at is older than one full lease has
// missed at least two renew cycles (renew cadence is TTL/3).
var leaderLockStaleAfter = leaderelection.DefaultTTL

// checkLeaderLocksHealth enumerates every row in
// daemon_leader_locks + classifies each into ACTIVE / STALE /
// EXPIRED. Reports the aggregate as the doctor row's status +
// per-worker detail in Items.
func (h *DoctorHandlers) checkLeaderLocksHealth() DoctorCheck {
	name := "daemon_leader_locks_health"
	if h.leaderLockRepo == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "leader-election repo not wired (single-process or pre-migration-57 deployment)"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := h.leaderLockRepo.List(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "failed to enumerate leader-lock rows: " + err.Error()}
	}
	if len(rows) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no leader-lock rows yet (fresh deployment or no singleton workers wired)"}
	}

	now := time.Now()
	active := 0
	stale := 0
	expired := 0
	orphaned := 0
	// The wiring set is resolved ONCE per check run rather than per row, but
	// still at run time — see DoctorHandlers.wiredWorkerIDs for why it is a
	// func. A nil accessor leaves `wired` nil, and knownWiring false keeps the
	// pre-#60 severity.
	var wired map[string]bool
	knownWiring := h.wiredWorkerIDs != nil
	if knownWiring {
		ids := h.wiredWorkerIDs()
		wired = make(map[string]bool, len(ids))
		for _, id := range ids {
			wired[id] = true
		}
	}
	items := make([]string, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		classification, detail := classifyLeaderLock(r, now)
		// ORPHANED is a severity qualifier on an already-classified row, not a
		// fourth classification: the row IS expired or stale: the question is
		// whether anything in this deployment could ever renew it again.
		isOrphan := knownWiring && !wired[r.WorkerID] && classification != "ACTIVE"
		label := classification
		if isOrphan {
			label = classification + "/ORPHANED"
			detail += " — no elector for this worker in this build/config"
		}
		items = append(items, fmt.Sprintf("[%s] %s: %s", label, r.WorkerID, detail))
		switch {
		case isOrphan:
			orphaned++
		case classification == "ACTIVE":
			active++
		case classification == "STALE":
			stale++
		case classification == "EXPIRED":
			expired++
		}
	}
	sort.Strings(items)

	switch {
	case expired > 0:
		return DoctorCheck{
			Name:   name,
			Status: "ERROR",
			Message: fmt.Sprintf(
				"%d leader-lock row(s) past expires_at — no current leader. %d active, %d stale.",
				expired, active, stale,
			),
			Items: items,
		}
	case stale > 0 || orphaned > 0:
		// WARNING, not ERROR, for a row nothing in this deployment will ever
		// renew — and the remediation is IN the message, because a check with
		// no reachable green state is one the operator stops reading (#60).
		msg := fmt.Sprintf(
			"%d leader-lock row(s) stale (holder hasn't renewed within one TTL). %d active.",
			stale, active,
		)
		if orphaned > 0 {
			msg = fmt.Sprintf(
				"%d leader-lock row(s) ORPHANED — no elector for that worker in this build/config, "+
					"so nothing will ever renew them (Enterprise-only subsystem on a Community build, "+
					"a disabled feature, or a deleted project). Clear them with "+
					"`vornikctl leader-lock release <worker>` or `--all-orphaned`. "+
					"%d stale, %d active.",
				orphaned, stale, active,
			)
		}
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: msg,
			Items:   items,
		}
	default:
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%d worker(s) holding fresh leader locks.", active),
			Items:   items,
		}
	}
}

// classifyLeaderLock returns the row's category + a short
// operator-facing detail string. Pure — testable without DB.
func classifyLeaderLock(r *persistence.DaemonLeaderLock, now time.Time) (string, string) {
	expiresIn := r.ExpiresAt.Sub(now)
	renewedAgo := now.Sub(r.RenewedAt)
	switch {
	case expiresIn <= 0:
		return "EXPIRED", fmt.Sprintf(
			"holder=%s expired %s ago (last renewed %s ago)",
			r.HolderID, humanLeaderLockDuration(-expiresIn), humanLeaderLockDuration(renewedAgo),
		)
	case renewedAgo > leaderLockStaleAfter:
		return "STALE", fmt.Sprintf(
			"holder=%s last renewed %s ago (lease valid for another %s)",
			r.HolderID, humanLeaderLockDuration(renewedAgo), humanLeaderLockDuration(expiresIn),
		)
	default:
		return "ACTIVE", fmt.Sprintf(
			"holder=%s renewed %s ago (lease valid for another %s)",
			r.HolderID, humanLeaderLockDuration(renewedAgo), humanLeaderLockDuration(expiresIn),
		)
	}
}

// humanLeaderLockDuration renders a coarse "12s" / "3m 5s" /
// "1h 24m" / "2d 4h" — same convention as the admin CPC page's
// humanDuration helper. Kept here rather than reaching across
// packages so the doctor check has no UI-package coupling.
func humanLeaderLockDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm %ds", m, s)
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) - days*24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd %dh", days, h)
}
