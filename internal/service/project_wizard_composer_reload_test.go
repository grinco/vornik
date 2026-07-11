package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/projectwizard"
	"vornik.io/vornik/internal/storage"
)

// TestConfigReloaderAdapter_Applied: an empty cycle (no loader/
// validator/activator set) completes instantly and cleanly, so the
// adapter reports ok with no error — the projectwizard.Reloader
// contract's happy path (design §5.6 step 5, task 1.2b slice ii).
func TestConfigReloaderAdapter_Applied(t *testing.T) {
	reloader := config.NewConfigReloader(config.NewWatcher(nil), zerolog.Nop())
	adapter := configReloaderAdapter{reloader: reloader}

	ok, err := adapter.TryReload(time.Second)
	if !ok || err != nil {
		t.Fatalf("expected ok=true err=nil, got ok=%v err=%v", ok, err)
	}
}

// TestConfigReloaderAdapter_Deferred: a cycle slower than the bound
// yields ReloadDeferred (TryReload never blocks past d); the adapter
// must map that to ok=false with a non-nil, descriptive error — never
// a blank reason, since commitBundleSession folds this straight into
// the operator-visible commit-failure message.
func TestConfigReloaderAdapter_Deferred(t *testing.T) {
	reloader := config.NewConfigReloader(config.NewWatcher(nil), zerolog.Nop())
	reloader.SetLoader(func() error {
		time.Sleep(50 * time.Millisecond)
		return nil
	})
	adapter := configReloaderAdapter{reloader: reloader}

	ok, err := adapter.TryReload(1 * time.Millisecond)
	if ok {
		t.Fatal("expected ok=false for a deferred (timed-out) reload")
	}
	if err == nil {
		t.Fatal("expected a non-nil error for a deferred reload")
	}
}

// TestConfigReloaderAdapter_Blocked: a validator returning an
// *config.ActivationBlockedError yields ReloadBlocked; the adapter
// must surface that exact error (the operator-facing "why").
func TestConfigReloaderAdapter_Blocked(t *testing.T) {
	reloader := config.NewConfigReloader(config.NewWatcher(nil), zerolog.Nop())
	reloader.SetValidator(func() error {
		return &config.ActivationBlockedError{Reason: "in-flight tasks pending"}
	})
	adapter := configReloaderAdapter{reloader: reloader}

	ok, err := adapter.TryReload(time.Second)
	if ok {
		t.Fatal("expected ok=false for a blocked reload")
	}
	if err == nil || !strings.Contains(err.Error(), "in-flight tasks pending") {
		t.Errorf("expected the blocked reason surfaced, got %v", err)
	}
}

// TestConfigReloaderAdapter_Failed: a validator returning a plain
// error yields ReloadFailed; the adapter must surface it too.
func TestConfigReloaderAdapter_Failed(t *testing.T) {
	reloader := config.NewConfigReloader(config.NewWatcher(nil), zerolog.Nop())
	reloader.SetValidator(func() error {
		return errors.New("schema is invalid")
	})
	adapter := configReloaderAdapter{reloader: reloader}

	ok, err := adapter.TryReload(time.Second)
	if ok {
		t.Fatal("expected ok=false for a failed reload")
	}
	if err == nil || !strings.Contains(err.Error(), "schema is invalid") {
		t.Errorf("expected the underlying validation error surfaced, got %v", err)
	}
}

// TestBuildProjectWizardOrNil_WiresReloaderWhenPresent confirms
// buildProjectWizardOrNil sets wiz.Reloader from c.ConfigReloader
// (adapted), the seam commitBundleSession polls after a bundle's
// project file lands (design §5.6 step 5).
func TestBuildProjectWizardOrNil_WiresReloaderWhenPresent(t *testing.T) {
	reloader := config.NewConfigReloader(config.NewWatcher(nil), zerolog.Nop())
	c := &Container{
		Logger:         zerolog.Nop(),
		Config:         &config.Config{},
		ChatClient:     &fakeModelListingProvider{models: []chat.ModelInfo{{ID: "m1"}}},
		mcpRegistry:    mcp.NewRegistry(nil, 0, zerolog.Nop()),
		repos:          &storage.Repositories{ProjectWizardSessions: newFakeWizardSessionStore()},
		ConfigReloader: reloader,
	}

	got := buildProjectWizardOrNil(c)
	adapter, ok := got.(*projectWizardAdapter)
	if !ok {
		t.Fatalf("expected *projectWizardAdapter, got %T", got)
	}
	if adapter.wizard.Reloader == nil {
		t.Fatal("expected wiz.Reloader wired when c.ConfigReloader is set")
	}
	// Sanity: the adapter actually delegates to the real reloader
	// rather than being some unrelated stand-in.
	if ok, err := adapter.wizard.Reloader.TryReload(time.Second); !ok || err != nil {
		t.Errorf("expected the wired reloader to complete an empty cycle cleanly, got ok=%v err=%v", ok, err)
	}
}

// TestBuildProjectWizardOrNil_NilConfigReloaderLeavesReloaderNil is the
// documented CE/minimal-wiring degradation: without a
// *config.ConfigReloader at all, wiz.Reloader stays nil rather than
// wrapping a nil pointer — commitBundleSession's nil-Reloader branch
// then never even calls into it.
func TestBuildProjectWizardOrNil_NilConfigReloaderLeavesReloaderNil(t *testing.T) {
	c := &Container{
		Logger:     zerolog.Nop(),
		Config:     &config.Config{},
		ChatClient: &fakeModelListingProvider{models: []chat.ModelInfo{{ID: "m1"}}},
		repos:      &storage.Repositories{ProjectWizardSessions: newFakeWizardSessionStore()},
	}

	got := buildProjectWizardOrNil(c)
	adapter, ok := got.(*projectWizardAdapter)
	if !ok {
		t.Fatalf("expected *projectWizardAdapter, got %T", got)
	}
	if adapter.wizard.Reloader != nil {
		t.Error("expected wiz.Reloader nil without a wired c.ConfigReloader")
	}
}

var _ projectwizard.Reloader = configReloaderAdapter{}
