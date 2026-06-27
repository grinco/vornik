package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanPathComponent(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "trims whitespace", in: " report.txt ", want: "report.txt"},
		{name: "empty", in: "  ", wantErr: "empty"},
		{name: "dot", in: ".", wantErr: "not allowed"},
		{name: "dotdot", in: "..", wantErr: "not allowed"},
		{name: "slash", in: "a/b", wantErr: "separators"},
		{name: "backslash", in: `a\b`, wantErr: "separators"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanPathComponent(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CleanPathComponent(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanPathComponent(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("CleanPathComponent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCleanFileNameRejectsPathComponents(t *testing.T) {
	if got, err := CleanFileName(" artifact.json "); err != nil || got != "artifact.json" {
		t.Fatalf("CleanFileName valid = %q, %v", got, err)
	}
	if _, err := CleanFileName("../secret.txt"); err == nil || !strings.Contains(err.Error(), "path components") {
		t.Fatalf("CleanFileName path traversal error = %v, want path-components refusal", err)
	}
}

func TestJoinUnderAllowsNestedPath(t *testing.T) {
	root := t.TempDir()

	got, err := JoinUnder(root, "artifacts", "out.txt")
	if err != nil {
		t.Fatalf("JoinUnder nested: %v", err)
	}
	want := filepath.Join(root, "artifacts", "out.txt")
	if got != want {
		t.Fatalf("JoinUnder nested = %q, want %q", got, want)
	}
}

func TestJoinUnderRejectsSyntacticEscape(t *testing.T) {
	root := t.TempDir()

	_, err := JoinUnder(root, "..", "outside.txt")
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("JoinUnder escape error = %v, want escapes-root refusal", err)
	}
}

func TestJoinUnderRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := JoinUnder(root, "link", "secret.txt")
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("JoinUnder symlink escape error = %v, want escapes-root refusal", err)
	}
}

func TestJoinUnderRejectsSymlinkParentForNewLeaf(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := JoinUnder(root, "link", "new-file.txt")
	if err == nil || !strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("JoinUnder symlink parent for new leaf error = %v, want escapes-root refusal", err)
	}
}

func TestJoinUnderAllowsSymlinkParentResolvingInsideRoot(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "real")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.Symlink(targetDir, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := JoinUnder(root, "link", "new-file.txt")
	if err != nil {
		t.Fatalf("JoinUnder symlink parent inside root: %v", err)
	}
	want := filepath.Join(targetDir, "new-file.txt")
	if got != want {
		t.Fatalf("JoinUnder symlink parent inside root = %q, want %q", got, want)
	}
}
