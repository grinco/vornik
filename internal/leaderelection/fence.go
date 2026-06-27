package leaderelection

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"vornik.io/vornik/internal/persistence"
)

// VerifyEpoch re-reads the authoritative lock row and reports whether
// this elector still holds the CURRENT epoch. It returns:
//   - ok=true  + the current epoch, when this holder still owns the lock
//     at the epoch it last held (safe to act);
//   - ok=false + the current epoch, when a different holder owns the lock
//     OR the stored epoch has advanced past the epoch this elector last
//     held (a new leader has taken over — this caller is a stale leader,
//     must NOT act);
//   - a non-nil error if the lock row can't be read (caller should treat
//     as "do not act" — fail closed).
func (e *Elector) VerifyEpoch(ctx context.Context) (ok bool, current int64, err error) {
	row, err := e.repo.Get(ctx, e.workerID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	current = row.Epoch
	if row.HolderID != e.holderID {
		return false, current, nil
	}
	if row.Epoch > e.epoch.Load() {
		// A takeover bumped the epoch past what we last held.
		return false, current, nil
	}
	return true, current, nil
}

// EpochVerifier is the optional capability a leader gate may expose to support
// epoch fencing. *Elector implements it. Gates that only expose IsLeader() do
// NOT implement it and are unaffected (pre-fence behaviour preserved).
type EpochVerifier interface {
	VerifyEpoch(ctx context.Context) (ok bool, current int64, err error)
}

// DangerousWriteAllowed reports whether a leader-gated dangerous action may
// proceed. gate may be nil (single-process → always proceed) or a plain
// IsLeader-only gate (→ proceed; cheap-gating only, pre-fence behaviour).
// When gate is an EpochVerifier, a superseded epoch OR a lock-read error
// returns false (fail closed) so a resumed stale leader cannot act (review B1).
func DangerousWriteAllowed(ctx context.Context, gate any) (proceed bool, reason string) {
	ev, ok := gate.(EpochVerifier)
	if !ok {
		return true, ""
	}
	leaderOK, _, err := ev.VerifyEpoch(ctx)
	if err != nil {
		return false, "leader epoch read failed (fencing closed)"
	}
	if !leaderOK {
		return false, "superseded by a newer leader epoch"
	}
	return true, ""
}

// fenceRejections counts dangerous writes refused by the epoch fence, by the
// worker that attempted the write. A non-zero rate means a stale (resumed)
// leader is being held back by a newer epoch — the fence doing its job
// against review finding B1.
//
// It is nil until the daemon wires it via RegisterFenceMetrics against the
// registry actually served at /metrics; LeaderFenceRejected is a no-op until
// then (single-process / tests). Callers across packages (autonomy, telegram)
// increment it through LeaderFenceRejected without threading a *Metrics handle.
var fenceRejections *prometheus.CounterVec

// RegisterFenceMetrics registers the leader-fence counter against reg (the
// registry served at /metrics). Call exactly once during daemon metric wiring.
// Safe with a nil reg (falls back to the default registerer for single-process).
func RegisterFenceMetrics(reg prometheus.Registerer) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	fenceRejections = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik",
		Subsystem: "leader",
		Name:      "fence_rejections_total",
		Help:      "Dangerous leader-gated writes refused by the epoch fence, by worker.",
	}, []string{"worker"})
}

// LeaderFenceRejected increments the fence-rejection counter for the named
// worker (e.g. "autonomy_manager", "telegram_poller"). Call it on the
// fail-closed path immediately after DangerousWriteAllowed returns proceed=false.
// No-op if RegisterFenceMetrics has not been called (single-process / tests).
func LeaderFenceRejected(worker string) {
	if fenceRejections != nil {
		fenceRejections.WithLabelValues(worker).Inc()
	}
}
