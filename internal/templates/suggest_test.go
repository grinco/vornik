package templates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuggestFreeProjectID(t *testing.T) {
	root := t.TempDir()
	projDir := filepath.Join(root, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := SuggestFreeProjectID(root, "fresh"); got != "fresh" {
		t.Fatalf("free id must return itself, got %q", got)
	}
	for _, name := range []string{"taken.yaml", "taken-2.yaml"} {
		if err := os.WriteFile(filepath.Join(projDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := SuggestFreeProjectID(root, "taken"); got != "taken-3" {
		t.Fatalf("want taken-3, got %q", got)
	}
}

// TestSuggestFreeProjectID_RejectsTraversalID — regression for CodeQL
// go/path-injection (2026-07-18). A projectID that is not a single safe path
// component must never be joined into the config tree; the suggester returns ""
// (no suggestion) rather than stat-ing an out-of-tree path.
func TestSuggestFreeProjectID_RejectsTraversalID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..", "../escape", "with/slash", `back\slash`, "", "."} {
		if got := SuggestFreeProjectID(root, id); got != "" {
			t.Errorf("id=%q: want \"\" (rejected), got %q", id, got)
		}
	}
}
