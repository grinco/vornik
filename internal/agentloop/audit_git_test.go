package agentloop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditGitRevisionOption(t *testing.T) {
	ws, out := t.TempDir(), t.TempDir()
	if data, err := exec.Command("git", "init", ws).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	target := filepath.Join(out, "created")
	raw, _ := json.Marshal(map[string]string{"path": ".", "revision": "--output=" + target})
	got := Dispatch(Env{Workspace: ws}, "git_diff", raw)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("git option created outside file: %v; response %q", err, got)
	}
}

func TestAuditGitToolsRejectOptions(t *testing.T) {
	for _, tool := range []string{"git_diff", "git_log", "git_show", "git_status"} {
		for _, rev := range []string{"-", "--help", "--ext-diff", "--output=elsewhere"} {
			raw, _ := json.Marshal(map[string]string{"revision": rev})
			got := Dispatch(Env{Workspace: t.TempDir()}, tool, raw)
			if !strings.Contains(got, "revision must not start") {
				t.Errorf("%s %s: %s", tool, rev, got)
			}
		}
	}
}
