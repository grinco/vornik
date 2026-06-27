package artifacts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"testing"
)

func TestLocalBackend_PutGetRoundTrip(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	payload := []byte("hello world")
	n, err := b.Put(ctx, "p1/e1/out.txt", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put returned n=%d want %d", n, len(payload))
	}
	rc, err := b.Get(ctx, "p1/e1/out.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get: got %q want %q", got, payload)
	}
}

func TestLocalBackend_LeadingSlashKey(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	if _, err := b.Put(ctx, "/p1/x.txt", bytes.NewReader([]byte("v"))); err != nil {
		t.Fatalf("Put with leading slash: %v", err)
	}
	exists, err := b.Exists(ctx, "p1/x.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Fatal("expected key to exist without leading slash")
	}
}

func TestLocalBackend_RejectsTraversal(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	_, err = b.Put(ctx, "../escape.txt", bytes.NewReader([]byte("nope")))
	if err == nil {
		t.Fatal("expected Put to reject traversal key")
	}
}

func TestLocalBackend_EmptyKey(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if _, err := b.Put(context.Background(), "", bytes.NewReader(nil)); err == nil {
		t.Fatal("expected empty-key error")
	}
}

func TestLocalBackend_GetMissing(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	_, err = b.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLocalBackend_DeleteIdempotent(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	if _, err := b.Put(ctx, "del.txt", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := b.Delete(ctx, "del.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second call is idempotent.
	if err := b.Delete(ctx, "del.txt"); err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
	if err := b.Delete(ctx, "neverexisted"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestLocalBackend_StatExists(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	if _, err := b.Put(ctx, "f.txt", bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := b.Stat(ctx, "f.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 5 || info.Key != "f.txt" {
		t.Fatalf("Stat info = %+v", info)
	}
	ok, err := b.Exists(ctx, "f.txt")
	if err != nil || !ok {
		t.Fatalf("Exists: ok=%v err=%v", ok, err)
	}
	ok, err = b.Exists(ctx, "missing.txt")
	if err != nil || ok {
		t.Fatalf("Exists missing: ok=%v err=%v", ok, err)
	}
	if _, err := b.Stat(ctx, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stat missing: want ErrNotFound, got %v", err)
	}
}

func TestLocalBackend_List(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b, err := NewLocalBackend(root)
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	keys := []string{
		"p1/e1/a.txt",
		"p1/e1/b.txt",
		"p1/e2/c.txt",
		"p2/d.txt",
	}
	for _, k := range keys {
		if _, err := b.Put(ctx, k, bytes.NewReader([]byte(filepath.Base(k)))); err != nil {
			t.Fatalf("Put %q: %v", k, err)
		}
	}

	t.Run("all", func(t *testing.T) {
		var got []string
		if err := b.List(ctx, "", func(o ObjectInfo) error {
			got = append(got, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("List: %v", err)
		}
		sort.Strings(got)
		want := append([]string(nil), keys...)
		sort.Strings(want)
		if len(got) != len(want) {
			t.Fatalf("List len = %d want %d (got %v)", len(got), len(want), got)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("List[%d] = %q want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("prefix dir", func(t *testing.T) {
		var got []string
		if err := b.List(ctx, "p1", func(o ObjectInfo) error {
			got = append(got, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("List p1 got %v", got)
		}
	})

	t.Run("prefix missing", func(t *testing.T) {
		var got []string
		if err := b.List(ctx, "nope", func(o ObjectInfo) error {
			got = append(got, o.Key)
			return nil
		}); err != nil {
			t.Fatalf("List missing prefix: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("List missing got %v", got)
		}
	})

	t.Run("walker early-stop via io.EOF", func(t *testing.T) {
		var seen int
		err := b.List(ctx, "", func(o ObjectInfo) error {
			seen++
			if seen == 2 {
				return io.EOF
			}
			return nil
		})
		if err != nil {
			t.Fatalf("List early-stop: %v", err)
		}
		if seen < 1 {
			t.Fatalf("expected at least one walker call, got %d", seen)
		}
	})

	t.Run("walker error propagates", func(t *testing.T) {
		boom := errors.New("boom")
		err := b.List(ctx, "", func(o ObjectInfo) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("List walker err = %v want %v", err, boom)
		}
	})
}

func TestLocalBackend_DefaultBasePath(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := NewLocalBackend("")
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.BasePath() != "./artifacts" {
		t.Fatalf("BasePath = %q want ./artifacts", b.BasePath())
	}
}

func TestLocalBackend_ContextCancelled(t *testing.T) {
	t.Parallel()
	b, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalBackend: %v", err)
	}
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Put(ctx, "x", bytes.NewReader([]byte("y"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: err = %v", err)
	}
	if _, err := b.Get(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: err = %v", err)
	}
	if err := b.Delete(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete: err = %v", err)
	}
	if _, err := b.Exists(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Exists: err = %v", err)
	}
	if _, err := b.Stat(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stat: err = %v", err)
	}
	if err := b.List(ctx, "", func(ObjectInfo) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("List: err = %v", err)
	}
}
