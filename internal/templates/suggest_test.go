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
