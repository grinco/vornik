package authsession_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/authsession"
)

// TestNoLiteralSessionCookieNames is the ratchet behind the centralisation.
//
// There were SEVEN spellings of these names — one constant in the EE login
// flow and six string literals in internal/api. A mismatch between the name a
// login SETS and the name the middleware READS is a login that silently does
// not authenticate: both halves look right in isolation, and nothing fails
// until someone tries to log in.
//
// So the literals are forbidden outside this package. Anyone adding a seventh
// use gets a test failure naming the constant to use instead.
func TestNoLiteralSessionCookieNames(t *testing.T) {
	root := repoRoot(t)
	forbidden := map[string]string{
		`"vornik_session"`:      "authsession.SessionCookieName",
		`"vornik_session_ui"`:   "authsession.UIMarkerCookieName",
		`"vornik_session_caps"`: "authsession.CapsCookieName",
		`"vornik_csrf"`:         "authsession.CSRFCookieName",
	}

	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			// This package DECLARES them, and tests may assert the
			// literal value on purpose.
			if strings.Contains(path, filepath.Join("internal", "authsession")) || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for literal, constant := range forbidden {
				if strings.Contains(string(body), literal) {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, rel+": "+literal+" → use "+constant)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%d file(s) spell a session cookie name as a literal:\n  %s\n\n"+
			"A name that is SET in one place and READ in another must come from one constant; "+
			"a mismatch is a login that silently does not authenticate, and both halves look "+
			"right in isolation.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// The constants themselves are pinned, because changing one is a breaking
// change for every browser holding a cookie — an accidental rename would log
// every operator out and read as an outage.
func TestCookieNamesArePinned(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{authsession.SessionCookieName, "vornik_session"},
		{authsession.UIMarkerCookieName, "vornik_session_ui"},
		{authsession.CapsCookieName, "vornik_session_caps"},
		{authsession.CSRFCookieName, "vornik_csrf"},
	} {
		if tc.got != tc.want {
			t.Errorf("cookie name = %q, want %q — renaming one logs every browser out", tc.got, tc.want)
		}
	}
}

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
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
