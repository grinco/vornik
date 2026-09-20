package projectdeps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaterialiseFetchesOnceAndIsIdempotent(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "deps"), PostureConnected)
	calls := 0
	fetch := func(_ context.Context, dir string) error {
		calls++
		writeTree(t, dir, map[string]string{"lib/numpy/__init__.py": "x"})
		return nil
	}

	first, err := s.Materialise(context.Background(), "k1", fetch)
	if err != nil {
		t.Fatalf("Materialise() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(first, "lib", "numpy", "__init__.py")); err != nil {
		t.Fatalf("materialised tree missing its content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(first, CompletionMarker)); err != nil {
		t.Fatalf("completion marker missing: %v", err)
	}

	second, err := s.Materialise(context.Background(), "k1", fetch)
	if err != nil {
		t.Fatalf("second Materialise() = %v", err)
	}
	if second != first {
		t.Fatalf("second call returned %q, want %q", second, first)
	}
	if calls != 1 {
		t.Fatalf("fetch ran %d times, want 1 — a present key must not refetch", calls)
	}
}

func TestMaterialiseLeavesNothingBehindWhenTheFetchFails(t *testing.T) {
	root := filepath.Join(t.TempDir(), "deps")
	s := NewStore(root, PostureConnected)
	boom := errors.New("index unreachable")

	_, err := s.Materialise(context.Background(), "k1", func(_ context.Context, dir string) error {
		writeTree(t, dir, map[string]string{"lib/half/__init__.py": "x"})
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Materialise() = %v, want the fetch error", err)
	}
	if _, statErr := os.Stat(s.Path("k1")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a failed fetch must not publish a partial tree, stat = %v", statErr)
	}
	// The staging directory must be gone too, or a retried job accretes
	// half-fetched trees under the root until the disk fills.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("deps root still holds %d entries after a failed fetch: %v", len(entries), entries)
	}
	if done, err := s.IsMaterialised("k1"); err != nil || done {
		t.Fatalf("IsMaterialised() = %v, %v; want false, nil", done, err)
	}
}

func TestIsMaterialisedRequiresTheMarkerNotJustTheDirectory(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root, PostureConnected)
	if err := os.MkdirAll(filepath.Join(s.Path("k1"), "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Mounting a partial tree presents the missing half as an import
	// error inside an agent, which is the failure this design removes.
	done, err := s.IsMaterialised("k1")
	if err != nil {
		t.Fatalf("IsMaterialised() = %v", err)
	}
	if done {
		t.Fatal("a directory without the completion marker must not read as materialised")
	}
}

func TestMaterialiseRefusesAnUnmarkedDirectoryRatherThanDeletingIt(t *testing.T) {
	s := NewStore(t.TempDir(), PostureConnected)
	if err := os.MkdirAll(s.Path("k1"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTree(t, s.Path("k1"), map[string]string{"operator-notes.txt": "hand placed"})

	_, err := s.Materialise(context.Background(), "k1", func(context.Context, string) error {
		t.Fatal("fetch must not run over an unrecognised directory")
		return nil
	})
	if !errors.Is(err, ErrCorruptMaterialisation) {
		t.Fatalf("Materialise() = %v, want ErrCorruptMaterialisation", err)
	}
	if !strings.Contains(err.Error(), s.Path("k1")) {
		t.Fatalf("the refusal must name the path, got %q", err)
	}
	if _, statErr := os.Stat(filepath.Join(s.Path("k1"), "operator-notes.txt")); statErr != nil {
		t.Fatalf("the refusal must not delete what it found: %v", statErr)
	}
}

func TestMaterialiseRefusesToFetchWhenAirGapped(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "deps"), PostureAirGapped)

	_, err := s.Materialise(context.Background(), "k1", func(context.Context, string) error {
		t.Fatal("an air-gapped deployment must not reach an index")
		return nil
	})
	if !errors.Is(err, ErrAirGapped) {
		t.Fatalf("Materialise() = %v, want ErrAirGapped", err)
	}
	// The refusal is loud and specific (design §3 property 3): the
	// alternative is an agent that silently reviews code it could not run.
	if !strings.Contains(err.Error(), "vornikctl deps import") {
		t.Fatalf("the refusal must name the operator action, got %q", err)
	}
	if !strings.Contains(err.Error(), "k1") {
		t.Fatalf("the refusal must name the key the operator has to supply, got %q", err)
	}
}

func TestMaterialiseServesAnImportedBundleWhenAirGapped(t *testing.T) {
	// `vornikctl deps import` is slice 2, but the read side must already
	// be posture-independent: an air-gapped deployment whose key is
	// present is a cache HIT, not a refusal.
	s := NewStore(t.TempDir(), PostureAirGapped)
	writeTree(t, s.Path("k1"), map[string]string{
		"lib/numpy/__init__.py": "x",
		CompletionMarker:        "key: k1\n",
	})

	got, err := s.Materialise(context.Background(), "k1", func(context.Context, string) error {
		t.Fatal("a present key must not fetch")
		return nil
	})
	if err != nil {
		t.Fatalf("Materialise() = %v, want the imported bundle", err)
	}
	if got != s.Path("k1") {
		t.Fatalf("Materialise() = %q, want %q", got, s.Path("k1"))
	}
}

func TestCompletionMarkerRecordsItsKey(t *testing.T) {
	s := NewStore(t.TempDir(), PostureConnected)
	dir, err := s.Materialise(context.Background(), "pip-linux-amd64-abc", func(_ context.Context, d string) error {
		writeTree(t, d, map[string]string{"lib/x.py": "1"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, CompletionMarker))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "pip-linux-amd64-abc") {
		t.Fatalf("marker = %q, want it to name its key", body)
	}
	if !strings.Contains(string(body), "completed:") {
		t.Fatalf("marker = %q, want a completion timestamp", body)
	}
}

func TestStoreRootAndPath(t *testing.T) {
	root := "/var/lib/vornik/deps"
	s := NewStore(root, PostureConnected)
	if s.Root() != root {
		t.Fatalf("Root() = %q, want %q", s.Root(), root)
	}
	if got, want := s.Path("k1"), filepath.Join(root, "k1"); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestIsMaterialisedSurfacesAStatErrorRatherThanReportingAbsent(t *testing.T) {
	// A key path that is a FILE makes the marker stat fail with ENOTDIR,
	// which is neither "present" nor "absent". Reporting absent would
	// send the daemon to refetch into a path it can never publish.
	root := t.TempDir()
	s := NewStore(root, PostureConnected)
	if err := os.WriteFile(s.Path("k1"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.IsMaterialised("k1"); err == nil {
		t.Fatal("IsMaterialised() = nil error, want the stat failure surfaced")
	}
	if _, err := s.Materialise(context.Background(), "k1", func(context.Context, string) error {
		t.Fatal("fetch must not run when the key's state is unreadable")
		return nil
	}); err == nil {
		t.Fatal("Materialise() = nil error, want the stat failure surfaced")
	}
}

func TestMaterialiseSurfacesAnUnwritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	s := NewStore(filepath.Join(parent, "deps"), PostureConnected)
	_, err := s.Materialise(context.Background(), "k1", func(context.Context, string) error {
		t.Fatal("fetch must not run when the root cannot be created")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "create deps root") {
		t.Fatalf("Materialise() = %v, want a create-deps-root failure", err)
	}
}

func TestMaterialiseTreatsALostPublishRaceAsSuccess(t *testing.T) {
	// Two materialisers of the same key can run on two nodes, or on one
	// node either side of a leader change. The loser's rename fails —
	// and its result is byte-identical by construction, because the key
	// IS the content. A complete target is therefore a success, not a
	// conflict (design §5.3a).
	s := NewStore(filepath.Join(t.TempDir(), "deps"), PostureConnected)

	got, err := s.Materialise(context.Background(), "k1", func(_ context.Context, dir string) error {
		writeTree(t, dir, map[string]string{"lib/numpy/__init__.py": "mine"})
		// The winner publishes while this fetch is still running.
		writeTree(t, s.Path("k1"), map[string]string{
			"lib/numpy/__init__.py": "theirs",
			CompletionMarker:        "key: k1\n",
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Materialise() = %v, want the winner's tree", err)
	}
	if got != s.Path("k1") {
		t.Fatalf("Materialise() = %q, want %q", got, s.Path("k1"))
	}
	body, err := os.ReadFile(filepath.Join(got, "lib", "numpy", "__init__.py"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "theirs" {
		t.Fatalf("content = %q, want the published tree left intact", body)
	}
}

func TestMaterialiseSurfacesAPublishFailureThatIsNotARace(t *testing.T) {
	// The target exists but is INCOMPLETE, so the rename failure is not
	// a lost race and must not be reported as success.
	s := NewStore(filepath.Join(t.TempDir(), "deps"), PostureConnected)

	_, err := s.Materialise(context.Background(), "k1", func(_ context.Context, dir string) error {
		writeTree(t, dir, map[string]string{"lib/x.py": "1"})
		writeTree(t, s.Path("k1"), map[string]string{"lib/half.py": "1"})
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "publish k1") {
		t.Fatalf("Materialise() = %v, want a publish failure", err)
	}
}
