package ui

import (
	"fmt"
	"sync"
	"time"

	"vornik.io/vornik/internal/config"
)

// reloadBound caps how long a config-save handler will wait for the live
// reload before returning "restart required". A normal reload is ~20ms; this
// is generous headroom while staying well under any reverse-proxy timeout.
const reloadBound = 3 * time.Second

// restartPendingFlag tracks whether a saved-but-not-applied config edit is
// waiting for a daemon restart. In-memory only (clears on boot).
type restartPendingFlag struct {
	mu     sync.RWMutex
	set    bool
	reason string
	since  time.Time
}

// mark records that a config edit needs a restart to apply. The first reason
// wins for the "since" timestamp; later edits refresh the reason text so the
// banner names the most recent change.
func (f *restartPendingFlag) mark(reason string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.set {
		f.since = now
	}
	f.set = true
	f.reason = reason
}

func (f *restartPendingFlag) snapshot() (bool, string, time.Time) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.set, f.reason, f.since
}

func (f *restartPendingFlag) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set = false
	f.reason = ""
	f.since = time.Time{}
}

// reloadStatusReader is the optional capability the banner uses to self-clear:
// once a reload cycle succeeds after the pending mark, the on-disk edit has
// been staged + activated, so the daemon is already current. The daemon's
// *config.ConfigReloader satisfies it.
type reloadStatusReader interface {
	Status() config.ReloadStatus
}

// RestartPending reports whether a config edit is saved but awaiting a daemon
// restart to take effect, with the most recent edit's reason and the time the
// pending state began. Rendered as a banner on every page.
func (s *Server) RestartPending() (bool, string, time.Time) {
	return s.restartPending.snapshot()
}

// reloadResult is the operator-facing outcome of applyConfigEdit, dropped into
// a handler's Success/Error render fields.
type reloadResult struct {
	Message string
	Level   string // "success" | "warning" | "error"
}

// applyConfigEdit applies a just-written config edit to the running daemon
// without ever blocking the request on the reloader. It is the single
// chokepoint every UI config-mutation handler calls after a successful write.
//
// reason is a short human phrase naming the edit (e.g. "project ibkr-trader
// config"); it is echoed in the restart-required banner.
func (s *Server) applyConfigEdit(reason string) reloadResult {
	if br, ok := s.configReloader.(boundedReloader); ok {
		outcome, err := br.TryReload(reloadBound)
		switch outcome {
		case config.ReloadApplied:
			return reloadResult{Message: "Configuration saved and applied live.", Level: "success"}
		case config.ReloadBlocked:
			s.markRestartPending(reason)
			return reloadResult{
				Message: "Configuration saved. It will apply once running tasks finish, or restart the daemon to apply it now.",
				Level:   "warning",
			}
		case config.ReloadDeferred:
			s.markRestartPending(reason)
			return reloadResult{
				Message: "Configuration saved to disk. A daemon restart is required to apply it (another reload is in progress or the reloader is busy).",
				Level:   "warning",
			}
		default: // ReloadFailed
			s.markRestartPending(reason)
			return reloadResult{
				Message: fmt.Sprintf("Configuration saved to disk, but live reload failed: %v. Restart the daemon to apply it.", err),
				Level:   "error",
			}
		}
	}

	// Reloader without the bounded capability (legacy/test fakes): call its
	// blocking Reload directly. Production always has boundedReloader, so
	// this path never runs against the wedge-prone real reloader.
	if s.configReloader != nil {
		if err := s.configReloader.Reload(); err != nil {
			s.markRestartPending(reason)
			return reloadResult{
				Message: fmt.Sprintf("Configuration saved to disk, but reload failed: %v. Restart the daemon to apply it.", err),
				Level:   "error",
			}
		}
		return reloadResult{Message: "Configuration saved and applied live.", Level: "success"}
	}

	// No reloader wired (test/smoke rigs): fall back to a direct registry
	// reload, which only re-reads YAML (no MCP re-sync, so it cannot wedge).
	if s.projectReg != nil {
		if err := s.projectReg.Load(s.configDir()); err != nil {
			s.markRestartPending(reason)
			return reloadResult{
				Message: fmt.Sprintf("Configuration saved to disk, but reload failed: %v. Restart the daemon to apply it.", err),
				Level:   "error",
			}
		}
	}
	return reloadResult{Message: "Configuration saved and applied live.", Level: "success"}
}

func (s *Server) markRestartPending(reason string) {
	s.restartPending.mark(reason, time.Now())
	// Observability: an edit was saved to disk but not applied live (reloader
	// busy/wedged/slow, or activation gated/failed). Correlates the banner
	// with the specific operation for debugging (companion review 2026-07-06).
	s.logger.Warn().Str("edit", reason).Msg("config saved but not applied live — restart required")
}

func (s *Server) clearRestartPending() {
	s.restartPending.clear()
}

// restartBannerView is the template-facing shape of the pending-restart
// state, exposed to the nav partial via the "restartPending" FuncMap entry.
type restartBannerView struct {
	Pending bool
	Reason  string
	Since   string // preformatted local time, empty when not pending
}

// restartBanner snapshots the pending-restart state for the persistent banner.
//
// It self-clears: if the reloader reports a reload cycle that succeeded after
// the pending mark (no staged-pending, not blocked), the on-disk edit has been
// applied — most commonly by the file-watcher reload the save itself triggers,
// or a later manual reload — so the banner is dropped. A wedged reloader never
// reports a fresh success, so the banner correctly persists until restart.
func (s *Server) restartBanner() restartBannerView {
	pending, reason, since := s.RestartPending()
	if pending {
		if sr, ok := s.configReloader.(reloadStatusReader); ok {
			st := sr.Status()
			if !st.PendingActivation && !st.Blocked && st.LastReload.After(since) {
				s.clearRestartPending()
				pending = false
			}
		}
	}
	v := restartBannerView{Pending: pending, Reason: reason}
	if pending {
		v.Since = since.Format("15:04 MST")
	}
	return v
}
