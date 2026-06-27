package architecture

// Public artifact-purity gates for the Vornik Community Edition repo.
//
// Injected at export time (it replaces the upstream import-law test, which
// pins a different internal build boundary). These gates assert the published
// artifact is pure: the Enterprise IP set is absent from the tree and
// unreachable from the Community main.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// TestNoEnterpriseInTree is the structural anti-re-addition gate: the public
// Community repo must NEVER contain internal/enterprise (the Enterprise IP
// set). This guards against accidental re-addition after publication — beyond
// the import-law below, the directory itself must not exist.
func TestNoEnterpriseInTree(t *testing.T) {
	p := filepath.Join(repoRoot(t), "internal", "enterprise")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("internal/enterprise must not exist in the Community repo (stat err = %v)", err)
	}
}

// TestCommunityMainIsEEFree asserts the Community daemon (cmd/vornik)
// transitively imports no Enterprise package and no Enterprise overlay module
// path. Run from anywhere in the module via the fully-qualified package path.
func TestCommunityMainIsEEFree(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "vornik.io/vornik/cmd/vornik").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "/internal/enterprise") || strings.Contains(dep, "vornik.io/vornik/enterprise") {
			t.Errorf("cmd/vornik must not import the Enterprise package %q", dep)
		}
	}
}
