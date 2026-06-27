package config

import (
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// TestReload_SerializesConcurrentCycles is the regression for the
// 2026-06-04 bug-sweep finding: Reload() has many concurrent triggers
// (SIGHUP, POST /config/reload, the file watcher, retryPendingLoop,
// the LISTEN/NOTIFY peer broadcast, the workflow applier, the project
// wizard) but held no lock across its loader → validator → activator
// phases. Two interleaved cycles shared the Registry's single staged
// slot: reload B's Stage() could overwrite the set reload A had just
// validated, and A's ActivateStaged() then promoted B's NOT-yet-
// validated config. The reloadMu fix serializes whole cycles.
//
// The instrumented phases track how many cycles are inside the
// loader→activator window simultaneously; pre-fix two goroutines
// overlap (max=2), post-fix cycles are strictly sequential (max=1).
func TestReload_SerializesConcurrentCycles(t *testing.T) {
	var (
		mu            sync.Mutex
		inCycle       int
		maxConcurrent int
	)
	enter := func() {
		mu.Lock()
		inCycle++
		if inCycle > maxConcurrent {
			maxConcurrent = inCycle
		}
		mu.Unlock()
	}
	exit := func() {
		mu.Lock()
		inCycle--
		mu.Unlock()
	}

	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { // Stage()
		enter()
		time.Sleep(15 * time.Millisecond)
		return nil
	})
	r.SetValidator(func() error { // DiffStaged / StripInvalidFromStaged
		time.Sleep(15 * time.Millisecond)
		return nil
	})
	r.SetActivator(func() error { // ActivateStaged()
		time.Sleep(15 * time.Millisecond)
		exit()
		return nil
	})

	const callers = 4
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Reload()
		}()
	}
	wg.Wait()

	if maxConcurrent != 1 {
		t.Fatalf("max concurrent reload cycles = %d, want 1 — interleaved cycles can activate a staged config a different cycle validated", maxConcurrent)
	}
}
