// Package backendtest provides a contract-test suite that any
// artifacts.FileBackend implementation can run against. It is the
// FileBackend analogue of internal/persistence/repotest, modelled
// after the same dependency-inverted shape: callers pass a factory
// that returns a fresh backend on each invocation, the suite drives
// the FileBackend methods through their contract, and any divergence
// shows up as a uniform test failure regardless of the backend.
//
// Usage from a backend-specific test file:
//
//	func TestLocalBackend_Contract(t *testing.T) {
//	    backendtest.Run(t, func(t *testing.T) (artifacts.FileBackend, func()) {
//	        b, err := artifacts.NewLocalBackend(t.TempDir())
//	        require.NoError(t, err)
//	        return b, func() { _ = b.Close() }
//	    })
//	}
//
// Run owns ALL backend-portable behaviour assertions: Put/Get round
// trip, Delete idempotency, ErrNotFound surfacing, prefix-aware List,
// context cancellation. Backend-specific tests (filesystem traversal
// rejection, S3 prefix composition) live in the respective package
// alongside the implementation.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"testing"

	"vornik.io/vornik/internal/artifacts"
)

// Factory returns a fresh FileBackend plus a cleanup func the suite
// will call when the subtest finishes. Each subtest invokes Factory
// independently so per-test state doesn't leak between checks.
type Factory func(t *testing.T) (artifacts.FileBackend, func())

// Run drives every contract scenario against the backend produced
// by f. The suite uses t.Run for each scenario so a failure in one
// path doesn't mask the others.
func Run(t *testing.T, f Factory) {
	t.Helper()
	t.Run("PutGetRoundTrip", func(t *testing.T) { putGetRoundTrip(t, f) })
	t.Run("PutOverwrites", func(t *testing.T) { putOverwrites(t, f) })
	t.Run("GetMissingReturnsErrNotFound", func(t *testing.T) { getMissingReturnsErrNotFound(t, f) })
	t.Run("DeleteIdempotent", func(t *testing.T) { deleteIdempotent(t, f) })
	t.Run("ExistsAndStat", func(t *testing.T) { existsAndStat(t, f) })
	t.Run("StatMissingReturnsErrNotFound", func(t *testing.T) { statMissingReturnsErrNotFound(t, f) })
	t.Run("EmptyKeyRejected", func(t *testing.T) { emptyKeyRejected(t, f) })
	t.Run("List", func(t *testing.T) { listScenario(t, f) })
	t.Run("ContextCanceled", func(t *testing.T) { contextCanceled(t, f) })
}

func putGetRoundTrip(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	payload := []byte("hello backend")
	n, err := b.Put(ctx, "k/round-trip.txt", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put n=%d want %d", n, len(payload))
	}
	rc, err := b.Get(ctx, "k/round-trip.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q want %q", got, payload)
	}
}

func putOverwrites(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := b.Put(ctx, "k.txt", bytes.NewReader([]byte("v1"))); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if _, err := b.Put(ctx, "k.txt", bytes.NewReader([]byte("v2-longer"))); err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	rc, err := b.Get(ctx, "k.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "v2-longer" {
		t.Fatalf("got %q", got)
	}
}

func getMissingReturnsErrNotFound(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	_, err := b.Get(context.Background(), "missing-key")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("got %v want ErrNotFound", err)
	}
}

func deleteIdempotent(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := b.Put(ctx, "del.txt", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "del.txt"); err != nil {
		t.Fatalf("Delete first: %v", err)
	}
	if err := b.Delete(ctx, "del.txt"); err != nil {
		t.Fatalf("Delete second: %v", err)
	}
	if err := b.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func existsAndStat(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := b.Put(ctx, "stat.txt", bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err := b.Exists(ctx, "stat.txt")
	if err != nil || !ok {
		t.Fatalf("Exists: ok=%v err=%v", ok, err)
	}
	info, err := b.Stat(ctx, "stat.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 5 {
		t.Fatalf("info.Size = %d", info.Size)
	}
	ok, err = b.Exists(ctx, "nope")
	if err != nil {
		t.Fatalf("Exists missing: %v", err)
	}
	if ok {
		t.Fatal("Exists missing returned true")
	}
}

func statMissingReturnsErrNotFound(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	_, err := b.Stat(context.Background(), "missing")
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Fatalf("got %v want ErrNotFound", err)
	}
}

func emptyKeyRejected(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	if _, err := b.Put(context.Background(), "", bytes.NewReader(nil)); err == nil {
		t.Fatal("expected empty-key error")
	}
}

func listScenario(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx := context.Background()
	keys := []string{
		"p1/e1/a.txt",
		"p1/e1/b.txt",
		"p1/e2/c.txt",
		"p2/d.txt",
	}
	for _, k := range keys {
		if _, err := b.Put(ctx, k, bytes.NewReader([]byte(k))); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}
	t.Run("all", func(t *testing.T) {
		var got []string
		if err := b.List(ctx, "", func(o artifacts.ObjectInfo) error {
			got = append(got, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("List: %v", err)
		}
		sort.Strings(got)
		want := append([]string(nil), keys...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("List: got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("List[%d] = %q want %q", i, got[i], want[i])
			}
		}
	})
	t.Run("prefix-filtered", func(t *testing.T) {
		var got []string
		if err := b.List(ctx, "p1", func(o artifacts.ObjectInfo) error {
			got = append(got, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("List p1: got %v", got)
		}
	})
	t.Run("walker-eof-stops", func(t *testing.T) {
		count := 0
		err := b.List(ctx, "", func(o artifacts.ObjectInfo) error {
			count++
			return io.EOF
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if count < 1 {
			t.Fatalf("expected at least one walker call, got %d", count)
		}
	})
}

func contextCanceled(t *testing.T, f Factory) {
	b, cleanup := f(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Put(ctx, "k", bytes.NewReader([]byte("x"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put err = %v", err)
	}
	if _, err := b.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v", err)
	}
	if err := b.Delete(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete err = %v", err)
	}
}
