package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// blockingShutdowner models *http.Server: its Shutdown blocks until the
// context is done (a stuck in-flight /chat/completions proxy request held
// open for the agent's whole tool-loop), and it records whether Close()
// was force-called.
type blockingShutdowner struct {
	mu          sync.Mutex
	closeCalled bool
	block       bool // true: Shutdown blocks until ctx done; false: returns nil now
}

func (b *blockingShutdowner) Shutdown(ctx context.Context) error {
	if b.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (b *blockingShutdowner) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeCalled = true
	return nil
}

func (b *blockingShutdowner) wasClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeCalled
}

// Regression: the 2026-06-20 incident where http.Server.Shutdown blocked
// on an in-flight chat-proxy request past systemd's TimeoutStopSec=30s and
// the daemon was SIGABRT-killed mid-drain. The helper must bound the drain
// and force-close.
func TestShutdownHTTPWithDeadline_ForceClosesOnTimeout(t *testing.T) {
	srv := &blockingShutdowner{block: true}
	start := time.Now()
	shutdownHTTPWithDeadline(context.Background(), srv, 50*time.Millisecond, zerolog.Nop())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown must return ~budget, took %v", d)
	}
	if !srv.wasClosed() {
		t.Fatal("Close() must be force-called when graceful Shutdown exceeds the budget")
	}
}

func TestShutdownHTTPWithDeadline_CleanDrainNoForceClose(t *testing.T) {
	srv := &blockingShutdowner{block: false}
	shutdownHTTPWithDeadline(context.Background(), srv, 5*time.Second, zerolog.Nop())
	if srv.wasClosed() {
		t.Fatal("Close() must NOT be called when graceful Shutdown completes within budget")
	}
}

func TestShutdownHTTPWithDeadline_NilSafe(_ *testing.T) {
	shutdownHTTPWithDeadline(context.Background(), nil, time.Second, zerolog.Nop()) // must not panic
}
