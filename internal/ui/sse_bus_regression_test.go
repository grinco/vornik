package ui

import (
	"sync"
	"testing"
)

// TestSSEBus_PublishRacesUnsubscribe_NoPanic is the regression for the
// 2026-06-04 bug sweep: Publish snapshotted the subscriber set, then
// released the read lock before sending. An unsubscribe could close a
// subscriber's channel in that window, and `select { case s.ch <-
// event: default: }` panics on a send to a closed channel (the send
// case becomes runnable rather than falling through to default),
// crashing the publishing goroutine.
//
// This stresses the exact interleave: a tight publisher fan-out racing
// continuous subscribe/unsubscribe churn. Pre-fix it panics with "send
// on closed channel"; post-fix the fan-out runs under the read lock so
// close (write lock) cannot interleave.
func TestSSEBus_PublishRacesUnsubscribe_NoPanic(t *testing.T) {
	bus := NewSSEBus()
	const taskID = "task-1"

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// A couple of tight publishers.
	for p := 0; p < 2; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					bus.Publish(taskID, SSEEvent{Kind: "status", Data: "<div>x</div>"})
				}
			}
		}()
	}

	// Continuous subscribe/unsubscribe churn on the same task. Each
	// subscriber is drained so the buffer has room (forcing the send
	// case, not default) and then unsubscribed (closing its channel).
	for i := 0; i < 3000; i++ {
		sub, cancel := bus.Subscribe(taskID)
		drained := make(chan struct{})
		go func() {
			for range sub.Events() {
			}
			close(drained)
		}()
		cancel()
		<-drained
	}

	close(stop)
	wg.Wait()
}
