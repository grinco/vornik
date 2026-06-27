package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcher_Scan(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "test.yaml")

	err := os.WriteFile(configFile, []byte("key: value\n"), 0644)
	require.NoError(t, err)

	w := NewWatcher([]string{tmpDir}, WithWatchLogger(zerolog.Nop()))

	changed := w.scan()
	assert.Empty(t, changed, "first scan should not report changes")

	time.Sleep(20 * time.Millisecond)
	err = os.WriteFile(configFile, []byte("key: newvalue\n"), 0644)
	require.NoError(t, err)

	changed = w.scan()
	assert.Contains(t, changed, configFile)
}

func TestWatcher_OnChange(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	err := os.WriteFile(configFile, []byte("key: value\n"), 0644)
	require.NoError(t, err)

	w := NewWatcher([]string{tmpDir},
		WithWatchInterval(50*time.Millisecond),
		WithWatchLogger(zerolog.Nop()),
	)

	var mu sync.Mutex
	var changedFiles []string
	w.OnChange(func(changed []string) {
		mu.Lock()
		defer mu.Unlock()
		changedFiles = append(changedFiles, changed...)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = w.Start(ctx)
	require.NoError(t, err)
	defer w.Stop()

	// Wait for initial scan
	time.Sleep(100 * time.Millisecond)

	// Modify file
	err = os.WriteFile(configFile, []byte("key: newvalue\n"), 0644)
	require.NoError(t, err)

	// Wait for detection
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	contains := assert.Contains(t, changedFiles, configFile)
	mu.Unlock()
	assert.True(t, contains)
}

func TestWatcher_ScanDetectsDeletedFile(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")

	err := os.WriteFile(configFile, []byte("key: value\n"), 0o644)
	require.NoError(t, err)

	w := NewWatcher([]string{tmpDir}, WithWatchLogger(zerolog.Nop()))
	assert.Empty(t, w.scan())

	require.NoError(t, os.Remove(configFile))

	changed := w.scan()
	assert.Contains(t, changed, configFile)
}

func TestWatcher_AddRemovePath(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))

	w.AddPath("/etc/vornik")
	assert.Len(t, w.paths, 1)

	w.RemovePath("/etc/vornik")
	assert.Len(t, w.paths, 0)
}

func TestWatcher_RestartAfterStop(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "config.yaml")
	require.NoError(t, os.WriteFile(configFile, []byte("key: value\n"), 0o644))

	w := NewWatcher([]string{tmpDir},
		WithWatchInterval(20*time.Millisecond),
		WithWatchLogger(zerolog.Nop()),
	)

	var mu sync.Mutex
	var changedFiles []string
	w.OnChange(func(changed []string) {
		mu.Lock()
		defer mu.Unlock()
		changedFiles = append(changedFiles, changed...)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, w.Start(ctx))
	time.Sleep(50 * time.Millisecond)
	w.Stop()

	require.NoError(t, w.Start(ctx))
	defer w.Stop()
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, os.WriteFile(configFile, []byte("key: restarted\n"), 0o644))
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, changedFiles, configFile)
}

func TestConfigReloader_Reload(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	var loadCalled, validateCalled, activateCalled bool

	r.SetLoader(func() error {
		loadCalled = true
		return nil
	})
	r.SetValidator(func() error {
		validateCalled = true
		return nil
	})
	r.SetActivator(func() error {
		activateCalled = true
		return nil
	})

	err := r.Reload()
	require.NoError(t, err)

	assert.True(t, loadCalled)
	assert.True(t, validateCalled)
	assert.True(t, activateCalled)
	assert.False(t, r.Status().HasErrors)
}

// TestConfigReloader_Warnings_SurfaceInStatus is the regression for
// the 2026-05-27 companion-onboarding silent-strip bug. Pre-fix,
// non-fatal strip warnings from StripInvalidFromStaged only landed
// in journald — the HTTP response said "success" and the
// reload-status endpoint exposed no warning channel. Now the
// validator wiring can call RecordReloadWarning, and Status()
// surfaces it.
func TestConfigReloader_Warnings_SurfaceInStatus(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error {
		// Mimic the real validator wiring: emit warnings, return nil
		// (the warnings are non-fatal, the reload itself succeeds).
		r.RecordReloadWarning("project 'alpha' references non-existent workflow 'gone'")
		r.RecordReloadWarning("project 'beta' references non-existent swarm 'orphan'")
		return nil
	})
	r.SetActivator(func() error { return nil })

	err := r.Reload()
	require.NoError(t, err, "reload with warnings is still a success")

	status := r.Status()
	assert.False(t, status.HasErrors)
	assert.True(t, status.HasWarnings, "warnings must surface in Status()")
	assert.Len(t, status.Warnings, 2)
	assert.Contains(t, status.Warnings[0], "non-existent workflow")
	assert.Contains(t, status.Warnings[1], "non-existent swarm")
}

// TestConfigReloader_Warnings_ResetEachCycle — warnings from a
// previous reload must not leak into the next one. The current
// status always reflects the latest cycle only.
func TestConfigReloader_Warnings_ResetEachCycle(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	r.SetLoader(func() error { return nil })
	r.SetActivator(func() error { return nil })

	// First reload: 1 warning.
	r.SetValidator(func() error {
		r.RecordReloadWarning("first cycle")
		return nil
	})
	require.NoError(t, r.Reload())
	require.Len(t, r.Status().Warnings, 1)

	// Second reload: 0 warnings — the first cycle's warning must
	// not leak through.
	r.SetValidator(func() error { return nil })
	require.NoError(t, r.Reload())
	assert.Empty(t, r.Status().Warnings,
		"warnings must reset at the start of each Reload cycle")
	assert.False(t, r.Status().HasWarnings)
}

// TestConfigReloader_RecordReloadWarning_EmptyIgnored — defensive
// against accidental empty pushes (e.g. a future caller does
// `RecordReloadWarning(err.Error())` on a nil-shaped error).
func TestConfigReloader_RecordReloadWarning_EmptyIgnored(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())
	r.RecordReloadWarning("")
	assert.Empty(t, r.Status().Warnings)
}

func TestConfigReloader_ValidationError(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error {
		return fmt.Errorf("invalid config")
	})
	r.SetActivator(func() error { return nil })

	err := r.Reload()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validation failed")
	assert.True(t, r.Status().HasErrors)
	assert.False(t, r.Status().PendingActivation)
}

func TestConfigReloader_BlockedStatus(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error {
		return &ActivationBlockedError{Reason: "waiting for in-flight work"}
	})

	err := r.Reload()
	require.Error(t, err)

	status := r.Status()
	assert.True(t, status.HasErrors)
	assert.True(t, status.PendingActivation)
	assert.True(t, status.Blocked)
	assert.Equal(t, "waiting for in-flight work", status.BlockedReason)
}

func TestConfigReloader_ActivationErrorClearsPendingActivation(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())

	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return fmt.Errorf("boom") })

	err := r.Reload()
	require.Error(t, err)

	status := r.Status()
	assert.True(t, status.HasErrors)
	assert.False(t, status.PendingActivation)
	assert.False(t, status.Blocked)
}

// Slice 3a — postReloadHook fires after a successful Reload().
// Used by multi-instance deployments to NOTIFY peer replicas.

func TestConfigReloader_PostReloadHook_FiresOnSuccess(t *testing.T) {
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })

	hookCalls := 0
	r.SetPostReloadHook(func() { hookCalls++ })

	require.NoError(t, r.Reload())
	assert.Equal(t, 1, hookCalls, "hook should fire once on successful reload")

	require.NoError(t, r.Reload())
	assert.Equal(t, 2, hookCalls, "hook should fire on every successful reload")
}

func TestConfigReloader_PostReloadHook_SuppressedOnError(t *testing.T) {
	// Validation failure must NOT fire the hook — otherwise
	// peers would reload from a half-baked / failing state.
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return fmt.Errorf("bad config") })
	r.SetActivator(func() error { return nil })

	hookCalls := 0
	r.SetPostReloadHook(func() { hookCalls++ })

	require.Error(t, r.Reload())
	assert.Equal(t, 0, hookCalls, "hook must not fire on validation failure")
}

func TestConfigReloader_PostReloadHook_PanicIsIsolated(t *testing.T) {
	// A panicking hook (e.g. nil-DB NOTIFY exec) must not
	// poison the Reload return path. Operator's POST /reload
	// should still respond success.
	w := NewWatcher([]string{}, WithWatchLogger(zerolog.Nop()))
	r := NewConfigReloader(w, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })
	r.SetPostReloadHook(func() { panic("nope") })

	assert.NotPanics(t, func() {
		_ = r.Reload()
	}, "panic in postReloadHook must be recovered by Reload")
}
