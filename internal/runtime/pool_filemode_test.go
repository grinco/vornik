package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInjectTask_FileMode0600 asserts the warm-pool task input
// files are written 0o600 — owner-read only.
//
// task.json carries the full step payload (prompt + inline
// secrets pulled from project config); .ready/.shutdown are
// timing oracles. Both must be 0o600 so that any local user
// on the host can't read the payload before the container
// consumes it.
//
// We exercise WarmPool.InjectTask directly with a fake PoolEntry
// pointing at a temp dir — no need for a live container.
func TestInjectTask_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes are advisory on Windows")
	}
	dir := t.TempDir()
	inputDir := filepath.Join(dir, "in")
	outputDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(inputDir, 0o700); err != nil {
		t.Fatalf("mkdir input: %v", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	entry := &PoolEntry{
		InputDir:  inputDir,
		OutputDir: outputDir,
	}
	pool := &WarmPool{}
	if err := pool.InjectTask(entry, []byte(`{"task":"hello"}`)); err != nil {
		t.Fatalf("InjectTask: %v", err)
	}
	for _, name := range []string{"task.json", ".ready"} {
		info, err := os.Stat(filepath.Join(inputDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0o600", name, perm)
		}
	}
}
