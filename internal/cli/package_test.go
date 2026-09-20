package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentpackage"
	"vornik.io/vornik/internal/persistence"
)

func TestEditedMarkerDistinguishesTheThreeStates(t *testing.T) {
	// An operator learns a file was edited from the LISTING, rather than
	// from an uninstall that refuses.
	dir := t.TempDir()
	body := []byte("# triage\n")
	if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workflows", "a.md"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	unchanged := persistence.PackageContribution{Path: "workflows/a.md", ContentHashAtInstall: agentpackage.ContentHash(body)}
	if got := editedMarker(dir, unchanged); got != "" {
		t.Fatalf("editedMarker(unchanged) = %q, want no marker", got)
	}

	edited := persistence.PackageContribution{Path: "workflows/a.md", ContentHashAtInstall: agentpackage.ContentHash([]byte("something else"))}
	if got := editedMarker(dir, edited); !strings.Contains(got, "edited") {
		t.Fatalf("editedMarker(edited) = %q, want an edited marker", got)
	}

	gone := persistence.PackageContribution{Path: "workflows/missing.md", ContentHashAtInstall: "whatever"}
	if got := editedMarker(dir, gone); !strings.Contains(got, "deleted") {
		t.Fatalf("editedMarker(deleted) = %q, want a deleted marker", got)
	}
}

func TestRollbackInstallRemovesWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	var written []string
	for _, name := range []string{"a.md", "b.md"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		written = append(written, p)
	}

	cause := errors.New("record provenance: database is gone")
	err := rollbackInstall(written, cause)
	if !errors.Is(err, cause) {
		t.Fatalf("rollbackInstall() = %v, want the original cause preserved", err)
	}
	for _, p := range written {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Fatalf("%s survived the rollback", p)
		}
	}
}

func TestRollbackInstallNamesWhatItCouldNotRemove(t *testing.T) {
	// A half-written install with no provenance is the state hardest to
	// recover from, because `uninstall` cannot see rows never recorded.
	// The operator gets the list either way.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(locked, "a.md")
	if err := os.WriteFile(stuck, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	err := rollbackInstall([]string{stuck}, errors.New("boom"))
	if err == nil || !strings.Contains(err.Error(), stuck) {
		t.Fatalf("rollbackInstall() = %v, want it to name the file it left behind", err)
	}
	if !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("err = %q, want it to tell the operator what to do", err)
	}
}

func TestRollbackInstallToleratesAFileAlreadyGone(t *testing.T) {
	cause := errors.New("boom")
	if err := rollbackInstall([]string{filepath.Join(t.TempDir(), "never-written.md")}, cause); !errors.Is(err, cause) {
		t.Fatalf("rollbackInstall() = %v, want the cause unchanged", err)
	}
}
