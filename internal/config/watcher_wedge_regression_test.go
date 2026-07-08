package config

import (
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestHandleWatchedChange_HungReloadDoesNotWedgeLoop is the regression for the
// 2026-07-08 watcher wedge.
//
// Root cause: the watcher's OnChange callback called the UNBOUNDED Reload()
// synchronously ON the scan-loop goroutine. When a reload cycle wedged inside
// the activator (initMCP re-dialling an OFFLINE MCP server — pagedrop was
// down), Reload() never returned, so the scan loop stalled for ~35 min and NO
// live config change applied until a manual restart. handleWatchedChange now
// uses the bounded TryReload, so a wedged cycle can never block the loop: the
// callback returns near the bound (the cycle finishes in the background) and a
// subsequent change returns immediately.
func TestHandleWatchedChange_HungReloadDoesNotWedgeLoop(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.reloadBound = 50 * time.Millisecond

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	r.SetLoader(func() error {
		once.Do(func() { close(entered) })
		<-release // wedge inside the cycle, holding reloadMu (like a stalled initMCP)
		return nil
	})
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })
	defer close(release)

	// First change: the cycle wedges. The callback must still return near the
	// bound rather than blocking the (real) scan loop forever.
	first := make(chan struct{})
	go func() { r.handleWatchedChange([]string{"config.yaml"}); close(first) }()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWatchedChange blocked on a wedged reload — the scan loop would be stuck (2026-07-08 regression)")
	}
	<-entered // the background cycle actually started and holds reloadMu

	// Second change while the first cycle is still wedged: must return
	// immediately (TryLock fails → ReloadDeferred), proving the loop stays live.
	start := time.Now()
	second := make(chan struct{})
	go func() { r.handleWatchedChange([]string{"config.yaml"}); close(second) }()
	select {
	case <-second:
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("second change took %v — should return immediately while a reload is in flight", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second handleWatchedChange blocked — scan loop wedged")
	}
}

// TestHandleWatchedChange_FastReloadApplies confirms the happy path is
// unchanged: a quick reload still completes synchronously (ReloadApplied) via
// the callback, so normal edits apply live without deferral.
func TestHandleWatchedChange_FastReloadApplies(t *testing.T) {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.reloadBound = time.Second

	var loaded int
	r.SetLoader(func() error { loaded++; return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })

	done := make(chan struct{})
	go func() { r.handleWatchedChange([]string{"config.yaml"}); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fast reload callback did not return")
	}
	if loaded != 1 {
		t.Fatalf("loader ran %d times, want 1 (fast reload applied synchronously)", loaded)
	}
	if r.Status().LastReload.IsZero() {
		t.Fatal("a fast reload should have recorded a successful LastReload")
	}
}
