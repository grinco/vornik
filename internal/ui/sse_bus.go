package ui

import (
	"context"
	"sync"
	"time"
)

// Phase 80 — Server-Sent Events bus for live UI updates.
//
// Replaces the every-5s polling on task detail (and friends)
// with a push channel: the API + executor publish events on
// state changes, the UI subscribes via SSE, htmx-ext-sse swaps
// the affected DOM in place. Battery + bandwidth win on phone.
//
// Design notes:
//   - In-memory pub/sub, keyed by task_id. Per-process — fine
//     for the single-daemon deployment vornik targets. A multi-
//     instance vornik would need redis pubsub or NATS, but that's
//     not the current shape.
//   - Each subscriber gets its own bounded channel (16 events).
//     Slow subscribers drop the oldest events — we don't block
//     publishers. The UI re-fetches state on reconnect anyway.
//   - Bus is global; one instance per Server. Initialised lazily
//     on first Subscribe so test harnesses without SSE wiring
//     don't pay the cost.

// SSEEvent is one push payload. Kind is the event name htmx-ext-sse
// uses to route into a specific sse-swap target on the page; Data
// is rendered into the swap.
type SSEEvent struct {
	Kind string // "status" | "message" | "scratchpad" | "phase"
	Data string // pre-rendered HTML fragment (matches the hx-swap target's expected content)
}

// SSESubscriber is one connected client's channel.
type SSESubscriber struct {
	taskID string
	ch     chan SSEEvent
}

// SSEBus is the shared pub/sub.
type SSEBus struct {
	mu          sync.RWMutex
	subscribers map[string]map[*SSESubscriber]struct{} // taskID → set
}

// NewSSEBus constructs an empty bus.
func NewSSEBus() *SSEBus {
	return &SSEBus{
		subscribers: make(map[string]map[*SSESubscriber]struct{}),
	}
}

// Subscribe registers a new subscriber for taskID. Returns the
// subscriber + a function to call on disconnect (defer it).
func (b *SSEBus) Subscribe(taskID string) (*SSESubscriber, func()) {
	sub := &SSESubscriber{
		taskID: taskID,
		ch:     make(chan SSEEvent, 16),
	}
	b.mu.Lock()
	if b.subscribers[taskID] == nil {
		b.subscribers[taskID] = make(map[*SSESubscriber]struct{})
	}
	b.subscribers[taskID][sub] = struct{}{}
	b.mu.Unlock()

	return sub, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.subscribers[taskID]; ok {
			delete(set, sub)
			if len(set) == 0 {
				delete(b.subscribers, taskID)
			}
		}
		close(sub.ch)
	}
}

// Events returns the read channel for a subscriber. Read until
// the channel is closed (subscriber unregistered).
func (s *SSESubscriber) Events() <-chan SSEEvent { return s.ch }

// Publish fans out an event to every subscriber for taskID.
// Non-blocking: a subscriber whose buffer is full drops the
// event silently — the UI will catch up on its next reconnect.
//
// Safe to call from multiple goroutines (the executor's per-step
// finalize, the API's transition handlers, and the scheduled-tick
// monitor all write).
func (b *SSEBus) Publish(taskID string, event SSEEvent) {
	// Fan out while holding the read lock. The unsubscribe closure
	// closes sub.ch under the *write* lock, so keeping the read lock
	// for the duration of the sends makes the close mutually exclusive
	// with this loop. The previous version snapshotted the set then
	// released the lock before sending, so an unsubscribe could close a
	// channel between snapshot and send — and `select { case s.ch <-
	// event: default: }` panics on a send to a closed channel rather
	// than taking default, crashing the publisher's goroutine (bug
	// sweep 2026-06-04). Sends are non-blocking, so holding the read
	// lock here is cheap and never blocks on a slow subscriber.
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subscribers[taskID] {
		select {
		case s.ch <- event:
		default:
			// buffer full; drop. The connected client re-fetches
			// state on reconnect / next page load.
		}
	}
}

// PublishWithTimeout is the same as Publish but caps the time
// spent attempting non-blocking sends. Used in hot paths where
// a stuck subscriber slot shouldn't even trigger a goroutine
// scheduler hop.
func (b *SSEBus) PublishWithTimeout(ctx context.Context, taskID string, event SSEEvent, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		b.Publish(taskID, event)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	case <-time.After(timeout):
	}
}
