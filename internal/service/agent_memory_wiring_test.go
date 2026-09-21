package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/runtime"
)

// TestApplyAgentMemoryLimit covers the startup wiring, which
// review-20260921-bfb8 identified as the single integration point for the
// whole change and the place the round-3 outage lived — with zero coverage.
//
// Three outcomes have to flow correctly: derive-and-wire, refuse-as-an-init
// error, and unbounded-with-a-warning. A unit test on resolveAgentMemoryLimit
// proves the arithmetic; only this proves the arithmetic is connected.
func TestApplyAgentMemoryLimit(t *testing.T) {
	const (
		mib = int64(1024 * 1024)
		gib = 1024 * mib
	)
	reader := func(v int64, err error) hostMemoryReader {
		return func() (int64, error) { return v, err }
	}

	newContainer := func(raw string, concurrency int) *Container {
		return &Container{
			Logger: zerolog.Nop(),
			Config: &config.Config{
				Runtime:   config.RuntimeConfig{AgentMemoryLimit: raw},
				Scheduler: config.SchedulerConfig{MaxConcurrentTasks: concurrency},
			},
		}
	}

	t.Run("absent derives and wires an option", func(t *testing.T) {
		c := newContainer("", 4)
		opts, err := c.applyAgentMemoryLimit(nil, reader(32*gib, nil), reader(18*gib, nil))
		if err != nil {
			t.Fatalf("derive path failed: %v", err)
		}
		if len(opts) != 1 {
			t.Fatalf("got %d options, want 1 — the derived limit did not reach the manager", len(opts))
		}
		// Prove it is the limit and not some other option.
		m := &runtime.Manager{}
		opts[0](m)
		if got := m.AgentMemoryLimitForTest(); got != int64(float64(32*gib)*0.5)/4 {
			t.Fatalf("manager limit = %d, want the derived value", got)
		}
	})

	// The outage path: a refusal must stop initialisation, not warn.
	t.Run("an impossible explicit config refuses initialisation", func(t *testing.T) {
		c := newContainer("64GiB", 8)
		_, err := c.applyAgentMemoryLimit(nil, reader(16*gib, nil), reader(8*gib, nil))
		if err == nil {
			t.Fatal("an over-total explicit limit did not fail scheduler init")
		}
		if !strings.Contains(err.Error(), "agent memory limit") {
			t.Fatalf("error %q does not name the subsystem", err)
		}
	})

	// An unreadable host must not refuse: the derivation has no basis, and the
	// honest fallback is the historical unbounded behaviour.
	t.Run("an unreadable host total leaves containers unbounded without refusing", func(t *testing.T) {
		c := newContainer("", 4)
		opts, err := c.applyAgentMemoryLimit(nil, reader(0, errors.New("no /proc")), reader(0, nil))
		if err != nil {
			t.Fatalf("unreadable host refused init: %v", err)
		}
		if len(opts) != 0 {
			t.Fatalf("got %d options, want 0 — unbounded must wire no limit", len(opts))
		}
	})

	t.Run("an explicit none wires nothing", func(t *testing.T) {
		c := newContainer("none", 4)
		opts, err := c.applyAgentMemoryLimit(nil, reader(32*gib, nil), reader(18*gib, nil))
		if err != nil {
			t.Fatalf("explicit none refused: %v", err)
		}
		if len(opts) != 0 {
			t.Fatalf("got %d options, want 0", len(opts))
		}
	})

	// Existing options must survive: the function appends, it does not own the
	// slice.
	t.Run("existing options are preserved", func(t *testing.T) {
		c := newContainer("", 4)
		existing := []runtime.ManagerOption{runtime.WithRunAsUser("1000:1000")}
		opts, err := c.applyAgentMemoryLimit(existing, reader(32*gib, nil), reader(18*gib, nil))
		if err != nil {
			t.Fatalf("append path failed: %v", err)
		}
		if len(opts) != 2 {
			t.Fatalf("got %d options, want 2 (the existing one plus the limit)", len(opts))
		}
	})
}
