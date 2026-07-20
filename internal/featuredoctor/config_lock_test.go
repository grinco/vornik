package featuredoctor

// TDD for the shared path-keyed config-file write lock
// (https://docs.vornik.io). Same file
// serializes; different files run concurrently; equivalent path strings share
// one lock.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

func TestLockConfigFile_SameFileSerializes(t *testing.T) {
	unlock := LockConfigFile("/tmp/vornik-a.yaml")
	acquired := make(chan struct{})
	go func() {
		u2 := LockConfigFile("/tmp/vornik-a.yaml")
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		t.Fatal("second lock on the same path acquired while the first was held")
	case <-time.After(60 * time.Millisecond):
		// good — blocked as expected
	}
	unlock()
	select {
	case <-acquired:
		// good — acquired once released
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired after the first released")
	}
}

func TestLockConfigFile_DifferentFilesConcurrent(t *testing.T) {
	u1 := LockConfigFile("/tmp/vornik-a.yaml")
	defer u1()
	done := make(chan struct{})
	go func() {
		u2 := LockConfigFile("/tmp/vornik-b.yaml") // different path — must not block
		u2()
		close(done)
	}()
	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("lock on a different path blocked behind an unrelated path")
	}
}

func TestLockConfigFile_CleanEquivalence(t *testing.T) {
	unlock := LockConfigFile("/x/c.yaml")
	acquired := make(chan struct{})
	go func() {
		u2 := LockConfigFile("/x/./c.yaml") // filepath.Clean-equivalent → same lock
		close(acquired)
		u2()
	}()
	select {
	case <-acquired:
		t.Fatal("equivalent path string did not share the lock")
	case <-time.After(60 * time.Millisecond):
	}
	unlock()
	<-acquired
}

// TestLockConfigFile_PreventsLostUpdate is the regression: two RMW transactions
// on the SAME file, each adding a distinct key under the lock, must both survive.
func TestLockConfigFile_PreventsLostUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("base: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rmw := func(key string) {
		unlock := LockConfigFile(path)
		defer unlock()
		content, err := os.ReadFile(path)
		if err != nil {
			t.Error(err)
			return
		}
		out, _, err := config.SetYAMLKey(content, key, "on")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			t.Error(err)
		}
	}
	var wg sync.WaitGroup
	for _, k := range []string{"integrations.slack", "featuredoctor.gate"} {
		wg.Add(1)
		go func(key string) { defer wg.Done(); rmw(key) }(k)
	}
	wg.Wait()
	final, _ := os.ReadFile(path)
	for _, want := range []string{"slack:", "gate:", "base:"} {
		if !strings.Contains(string(final), want) {
			t.Errorf("lost update: %q missing from final config:\n%s", want, final)
		}
	}
}
