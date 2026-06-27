// Package workspacelock provides per-project (per-key) workspace mutual
// exclusion shared across every subsystem that mutates a project's git
// workspace on a single node: the executor (around every repo-mutation), the
// UI artifact-delete path, and (from Task 2.4) the git-over-HTTPS push/read
// handler.
//
// The chosen safety model for git-over-HTTPS is "lock-on-mutation": EVERY
// workspace writer must take the SAME per-project lock instance, so concurrent
// writers serialise and readers (e.g. upload-pack / git fetch) share. The
// service container builds ONE *Locker and injects it into all three
// subsystems.
//
// Process-local (sync.RWMutex) is correct for v1 single-node. The multi-node
// cross-node gate (a Postgres advisory lock) wraps this primitive later — see
// https://docs.vornik.io §4.7.
//
// The lock is a LEAF and is NON-REENTRANT (it is backed by sync.RWMutex):
// never acquire it while holding another lock, and never nest a Lock on the
// same key from the same goroutine — that deadlocks.
package workspacelock

import "sync"

// Locker hands out per-key *sync.RWMutexes, created lazily on first use.
//
// Semantics (matching the executor's prior RWMutex-backed namedLock):
//   - exclusive Lock excludes a second Lock and any RLock for the same key;
//   - multiple RLock holders for the same key proceed concurrently;
//   - TryLock returns false while the key is held exclusively OR by any reader;
//   - distinct keys are fully independent;
//   - an empty key is a no-op (returns a no-op unlock, and TryLock returns
//     (no-op, true)) — preserves the pre-RWMutex behaviour where callers pass
//     an empty project ID for tasks with no project.
type Locker struct {
	// locks maps key -> *sync.RWMutex. sync.Map suits the
	// load-or-store-once, read-mostly access pattern (keys are project IDs,
	// a small bounded set created once and reused).
	locks sync.Map
}

// New returns a ready-to-use Locker.
func New() *Locker {
	return &Locker{}
}

// rwmutex returns the (lazily created) RWMutex for key.
func (l *Locker) rwmutex(key string) *sync.RWMutex {
	m, _ := l.locks.LoadOrStore(key, &sync.RWMutex{})
	return m.(*sync.RWMutex)
}

// Lock acquires the exclusive (write) lock for key and returns a closure that
// releases it. Blocks until the lock is available. For an empty key it is a
// no-op returning a no-op unlock.
func (l *Locker) Lock(key string) (unlock func()) {
	if key == "" {
		return func() {}
	}
	m := l.rwmutex(key)
	m.Lock()
	return m.Unlock
}

// RLock acquires a shared (read) lock for key and returns a closure that
// releases it. Multiple RLock holders proceed concurrently; an exclusive Lock
// waits for all of them to release. For an empty key it is a no-op returning a
// no-op unlock.
func (l *Locker) RLock(key string) (unlock func()) {
	if key == "" {
		return func() {}
	}
	m := l.rwmutex(key)
	m.RLock()
	return m.RUnlock
}

// TryLock attempts to acquire the exclusive (write) lock for key without
// blocking. It returns (unlock, true) when acquired and (nil, false) when the
// lock is currently held (exclusively or by any reader). For an empty key it
// is a no-op that always returns (no-op unlock, true).
func (l *Locker) TryLock(key string) (unlock func(), ok bool) {
	if key == "" {
		return func() {}, true
	}
	m := l.rwmutex(key)
	if m.TryLock() {
		return m.Unlock, true
	}
	return nil, false
}
