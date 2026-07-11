package integrations

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateProjectIDForPath(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{"empty", "", true},
		{"simple", "proj-a", false},
		{"underscore", "proj_a", false},
		{"alnum", "Proj123", false},
		{"traversal", "../../etc/x", true},
		{"embedded traversal", "proj-a/../../etc", true},
		{"contains slash", "proj/a", true},
		{"contains backslash", `proj\a`, true},
		{"dot", "proj.a", true},
		{"just dots", "..", true},
		{"space", "proj a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProjectIDForPath(tc.id)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateProjectIDForPath(%q) error = %v, wantErr %v", tc.id, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalidProjectID) {
				t.Errorf("validateProjectIDForPath(%q) error = %v, want errors.Is(err, ErrInvalidProjectID)", tc.id, err)
			}
		})
	}
}

func TestSafeProjectPath_RejectsTraversal(t *testing.T) {
	root := t.TempDir()
	_, err := safeProjectPath(root, "../../etc/x", "projects", "../../etc/x.yaml")
	if err == nil {
		t.Fatal("safeProjectPath() = nil error, want rejection for a traversal-shaped project ID")
	}
}

func TestSafeProjectPath_ValidStaysUnderRoot(t *testing.T) {
	root := t.TempDir()
	got, err := safeProjectPath(root, "proj-a", "projects", "proj-a.yaml")
	if err != nil {
		t.Fatalf("safeProjectPath: %v", err)
	}
	want := filepath.Join(root, "projects", "proj-a.yaml")
	// Compare via filepath.Clean since safepath may resolve symlinks in root
	// (e.g. on macOS /tmp -> /private/tmp); assert containment instead of
	// byte-equality for portability.
	rel, err := filepath.Rel(root, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("safeProjectPath() = %q, want it to stay under root %q (got rel=%q, err=%v)", got, root, rel, err)
	}
	if filepath.Base(got) != filepath.Base(want) {
		t.Errorf("safeProjectPath() = %q, want basename %q", got, filepath.Base(want))
	}
}
