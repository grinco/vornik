package service

// Coverage-uplift sweep (2026-06-18). Pins the one pure helper in
// subsystem_memory_ingest.go that isn't composition-root wiring:
// collectorsCtxFrom. Build/Start/Stop construct real subsystems
// (DB connections, goroutines) and are intentionally out of scope —
// they're exercised by the e2e lane.
//
// collectorsCtxFrom selects the container's long-running collectors
// ctx when present, else falls back to the supplied ctx. The fallback
// arm was already reachable; this adds the c.collectorsCtx != nil arm
// and the c == nil guard.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCollectorsCtxFrom(t *testing.T) {
	type ctxKey string
	base := context.WithValue(context.Background(), ctxKey("which"), "base")
	collectors := context.WithValue(context.Background(), ctxKey("which"), "collectors")

	t.Run("nil container falls back to supplied ctx", func(t *testing.T) {
		got := collectorsCtxFrom(base, nil)
		assert.Equal(t, "base", got.Value(ctxKey("which")))
	})

	t.Run("container without collectorsCtx falls back", func(t *testing.T) {
		got := collectorsCtxFrom(base, &Container{})
		assert.Equal(t, "base", got.Value(ctxKey("which")))
	})

	t.Run("container with collectorsCtx prefers it", func(t *testing.T) {
		got := collectorsCtxFrom(base, &Container{collectorsCtx: collectors})
		assert.Equal(t, "collectors", got.Value(ctxKey("which")))
	})
}
