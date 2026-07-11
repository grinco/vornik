package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

var errFakeReload = errors.New("fake reload failure")

// fakeBoundedReloader is a ui.ConfigReloader whose TryReload returns a
// preset outcome, so applyConfigEdit's outcome→UX mapping can be tested
// without a real reload cycle.
type fakeBoundedReloader struct {
	outcome config.ReloadOutcome
	err     error
}

func (f fakeBoundedReloader) Reload() error { return f.err }
func (f fakeBoundedReloader) TryReload(time.Duration) (config.ReloadOutcome, error) {
	return f.outcome, f.err
}

func TestApplyConfigEdit_Applied_NoRestartPending(t *testing.T) {
	s := NewServer(WithConfigReloader(fakeBoundedReloader{outcome: config.ReloadApplied}))

	res := s.applyConfigEdit("project foo config")

	if res.Level != "success" {
		t.Fatalf("Level = %q, want success", res.Level)
	}
	if pending, _, _ := s.RestartPending(); pending {
		t.Fatal("restart should NOT be pending after a live-applied reload")
	}
}

func TestApplyConfigEdit_Deferred_SetsRestartPending(t *testing.T) {
	s := NewServer(WithConfigReloader(fakeBoundedReloader{outcome: config.ReloadDeferred}))

	res := s.applyConfigEdit("project foo config")

	if res.Level != "warning" {
		t.Fatalf("Level = %q, want warning", res.Level)
	}
	if !strings.Contains(strings.ToLower(res.Message), "restart") {
		t.Fatalf("message %q should mention a restart", res.Message)
	}
	pending, reason, _ := s.RestartPending()
	if !pending {
		t.Fatal("restart SHOULD be pending after a deferred reload")
	}
	if !strings.Contains(reason, "foo") {
		t.Fatalf("pending reason %q should name the edit", reason)
	}
}

// statusReloader is a fake reloader that also reports reload status, so the
// self-clearing banner logic can be exercised.
type statusReloader struct {
	fakeBoundedReloader
	status config.ReloadStatus
}

func (r statusReloader) Status() config.ReloadStatus { return r.status }

// TestRestartBanner_SelfClearsAfterSuccessfulReload covers the companion-review
// fix: once a reload cycle succeeds after the pending mark, the on-disk edit is
// applied, so the banner must drop rather than falsely persist.
func TestRestartBanner_SelfClearsAfterSuccessfulReload(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		fakeBoundedReloader: fakeBoundedReloader{outcome: config.ReloadDeferred},
	}))

	// A deferred save flags a pending restart.
	s.applyConfigEdit("project foo config")
	if pending, _, _ := s.RestartPending(); !pending {
		t.Fatal("expected restart pending after deferred reload")
	}

	// A reload cycle later succeeds (LastReload after the pending mark).
	s.configReloader = statusReloader{status: config.ReloadStatus{LastReload: time.Now().Add(time.Hour)}}

	if v := s.restartBanner(); v.Pending {
		t.Fatal("banner should self-clear once a reload succeeded after the pending mark")
	}
	if pending, _, _ := s.RestartPending(); pending {
		t.Fatal("pending flag should be cleared")
	}
}

// TestRestartBanner_PersistsWhileReloadWedged covers the wedge case: no fresh
// successful reload, so the banner must stay up until restart.
func TestRestartBanner_PersistsWhileReloadWedged(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		fakeBoundedReloader: fakeBoundedReloader{outcome: config.ReloadDeferred},
		// Wedged: last reload is old and activation is stuck pending.
		status: config.ReloadStatus{PendingActivation: true},
	}))

	s.applyConfigEdit("project foo config")

	if v := s.restartBanner(); !v.Pending {
		t.Fatal("banner should persist while the reloader is wedged")
	}
}

// TestRestartBanner_FixItHref_WhenReloadHasErrors covers the task 3.4
// entry point: the persistent banner deep-links the Fix-It Doctor's
// failed_reload panel ONLY when the wired reloader's Status() reports a
// genuine validation error (HasErrors), not merely "pending" for a
// blocked/deferred reason.
func TestRestartBanner_FixItHref_WhenReloadHasErrors(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		fakeBoundedReloader: fakeBoundedReloader{outcome: config.ReloadFailed, err: errFakeReload},
		status:              config.ReloadStatus{HasErrors: true, Errors: []string{"validate: bad llm.timeout"}},
	}))
	s.applyConfigEdit("project foo config")

	v := s.restartBanner()
	if !v.Pending {
		t.Fatal("expected the banner pending after a failed reload")
	}
	if v.FixItHref != "/ui/fixit/failed_reload/daemon" {
		t.Fatalf("FixItHref = %q, want the failed_reload deep link", v.FixItHref)
	}
}

// TestRestartBanner_NoFixItHref_WhenBlockedNotErrored covers the negative:
// a pending restart caused by in-flight-task blocking (no validation
// error) must NOT show the fix-it link — there's nothing for the doctor
// to ground on.
func TestRestartBanner_NoFixItHref_WhenBlockedNotErrored(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		fakeBoundedReloader: fakeBoundedReloader{outcome: config.ReloadBlocked, err: errFakeReload},
		status:              config.ReloadStatus{HasErrors: false, Blocked: true},
	}))
	s.applyConfigEdit("project foo config")

	v := s.restartBanner()
	if !v.Pending {
		t.Fatal("expected the banner pending after a blocked reload")
	}
	if v.FixItHref != "" {
		t.Fatalf("FixItHref = %q, want empty (no validation error on record)", v.FixItHref)
	}
}

// TestRestartBanner_NoFixItHref_WhenNotPending covers the trivial no-op
// path: nothing pending means no banner and no fix-it link, regardless
// of reloader state.
func TestRestartBanner_NoFixItHref_WhenNotPending(t *testing.T) {
	s := NewServer(WithConfigReloader(statusReloader{
		status: config.ReloadStatus{HasErrors: true, Errors: []string{"stale error from a prior cycle"}},
	}))

	v := s.restartBanner()
	if v.Pending {
		t.Fatal("expected no pending restart")
	}
	if v.FixItHref != "" {
		t.Fatalf("FixItHref = %q, want empty when nothing is pending", v.FixItHref)
	}
}

// TestRestartBanner_NoFixItHref_WithoutStatusReader covers a reloader
// that doesn't implement reloadStatusReader (legacy fakes, the
// no-reloader smoke path) — the banner must still degrade gracefully,
// never panic, and never fabricate a fix-it link it can't back up.
func TestRestartBanner_NoFixItHref_WithoutStatusReader(t *testing.T) {
	s := NewServer(WithConfigReloader(fakeBoundedReloader{outcome: config.ReloadFailed, err: errFakeReload}))
	s.applyConfigEdit("project foo config")

	v := s.restartBanner()
	if !v.Pending {
		t.Fatal("expected the banner pending after a failed reload")
	}
	if v.FixItHref != "" {
		t.Fatalf("FixItHref = %q, want empty without a Status()-capable reloader", v.FixItHref)
	}
}

func TestApplyConfigEdit_Failed_SetsRestartPendingAndError(t *testing.T) {
	s := NewServer(WithConfigReloader(fakeBoundedReloader{outcome: config.ReloadFailed, err: errFakeReload}))

	res := s.applyConfigEdit("project foo config")

	if res.Level != "error" {
		t.Fatalf("Level = %q, want error", res.Level)
	}
	if pending, _, _ := s.RestartPending(); !pending {
		t.Fatal("restart SHOULD be pending after a failed reload (edit is on disk)")
	}
}
