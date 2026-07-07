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
