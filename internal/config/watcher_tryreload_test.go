package config

import (
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestTryReload_CycleExceedsBound_ReturnsDeferred covers the watchdog path:
// the lock is free, but the cycle runs longer than the bound, so TryReload
// returns ReloadDeferred early rather than waiting for the cycle to finish.
func TestTryReload_CycleExceedsBound_ReturnsDeferred(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { time.Sleep(500 * time.Millisecond); return nil })

	start := time.Now()
	outcome, err := r.TryReload(50 * time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != ReloadDeferred {
		t.Fatalf("outcome = %v, want ReloadDeferred (cycle exceeded bound)", outcome)
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("TryReload waited %v — should have returned near the 50ms bound", elapsed)
	}
}

// TestTryReload_ActivationBlocked_ReturnsBlocked maps the in-flight-task
// activation gate to ReloadBlocked.
func TestTryReload_ActivationBlocked_ReturnsBlocked(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return &ActivationBlockedError{Reason: "in-flight tasks"} })

	outcome, err := r.TryReload(time.Second)
	if outcome != ReloadBlocked {
		t.Fatalf("outcome = %v, want ReloadBlocked", outcome)
	}
	if err == nil {
		t.Fatal("expected the activation-blocked error to be returned")
	}
}

// TestTryReload_HardError_ReturnsFailed maps a validation/activation error to
// ReloadFailed.
func TestTryReload_HardError_ReturnsFailed(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return errors.New("bad config") })
	r.SetActivator(func() error { return nil })

	outcome, err := r.TryReload(time.Second)
	if outcome != ReloadFailed {
		t.Fatalf("outcome = %v, want ReloadFailed", outcome)
	}
	if err == nil {
		t.Fatal("expected the validation error to be returned")
	}
}

// TestTryReload_LockHeld_ReturnsDeferredWithoutBlocking is the regression for
// the 2026-07-06 incident: a reload cycle wedged inside the activator held
// reloadMu forever, so every UI config save — which called Reload()
// synchronously — blocked on reloadMu.Lock() and the HTTP handler never
// returned (surfacing as a Cloudflare 524 / browser "connection timeout").
// TryReload must observe the held lock and return ReloadDeferred promptly.
func TestTryReload_LockHeld_ReturnsDeferredWithoutBlocking(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())

	entered := make(chan struct{})
	release := make(chan struct{})
	r.SetLoader(func() error {
		close(entered)
		<-release // wedge inside the cycle, holding reloadMu
		return nil
	})
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })

	go func() { _ = r.Reload() }() // acquires reloadMu, then wedges
	<-entered
	defer close(release)

	type res struct {
		outcome ReloadOutcome
		err     error
	}
	done := make(chan res, 1)
	go func() {
		o, err := r.TryReload(time.Second)
		done <- res{o, err}
	}()

	select {
	case got := <-done:
		if got.outcome != ReloadDeferred {
			t.Fatalf("outcome = %v, want ReloadDeferred (lock held)", got.outcome)
		}
		if got.err != nil {
			t.Fatalf("unexpected error: %v", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryReload blocked on the held reloadMu instead of returning ReloadDeferred")
	}
}

// TryReload is the bounded, non-blocking-on-contention entry point used by
// the UI config-save handlers. See
// https://docs.vornik.io

func TestTryReload_FastCycle_ReturnsApplied(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })

	outcome, err := r.TryReload(time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != ReloadApplied {
		t.Fatalf("outcome = %v, want ReloadApplied", outcome)
	}
}
