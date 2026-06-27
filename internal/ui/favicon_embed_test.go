// Package ui: verifies that the iOS-icon PNG assets are embedded in staticFS.
//
// The //go:embed static/* directive in server.go covers all files under
// static/. This test opens each asset by name to confirm the embed actually
// included the files — a missing file would compile silently (Go doesn't error
// on missing embed patterns when the glob matches at least one file) but would
// cause http.FileServer to 404 on /ui/static/apple-touch-icon.png etc.
package ui

import (
	"testing"
)

// TestStaticFS_IOSIconAssetsEmbedded opens the three new icon assets from
// staticFS. If any file is missing the embed directive was not applied
// (e.g. the file wasn't git-added before build) and the test fails.
func TestStaticFS_IOSIconAssetsEmbedded(t *testing.T) {
	assets := []string{
		"static/apple-touch-icon.png",
		"static/favicon-32.png",
		"static/favicon.ico",
	}
	for _, name := range assets {
		f, err := staticFS.Open(name)
		if err != nil {
			t.Errorf("staticFS.Open(%q) failed: %v — asset missing from embed", name, err)
			continue
		}
		if cerr := f.Close(); cerr != nil {
			t.Errorf("staticFS: closing %q: %v", name, cerr)
		}
	}
}
