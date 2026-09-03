package imagemanifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is defined in manifest_test.go.

// fakeAgentExempt is the CLOSED allow-list of paths that may still say
// "fake-agent". Three historical documents plus the design that records the
// deletion — nothing else, ever.
//
// These are historical records, not live documentation: they describe what was
// true when they were written, and editing them to hide a deleted fixture would
// falsify the record. Everything ELSE is in scope, including the four live
// documents cleaned in the same change (https://docs.vornik.io, README.md,
// https://docs.vornik.io, https://docs.vornik.io) — those were cleaned,
// not exempted, so a reference reappearing in one of them must fail here.
//
// Whole-file rather than line-anchored, deliberately. An anchored exemption
// ("…-design.md:713") is tighter but goes stale the moment an unrelated edit
// shifts the line, and a stale ratchet gets disabled rather than fixed. The cost
// is that a new reference could slip into one of these three unnoticed — which
// is acceptable, because they are frozen records nobody should be editing, and
// an edit to one is itself the anomaly.
//
// See https://docs.vornik.io §11.5.
// A SLICE, NOT A MAP LITERAL, and that is load-bearing for the CE export.
// export-public-ce.sh rewrites internal doc pointers -- https://docs.vornik.io*,
// https://docs.vornik.io*, docs/<CAPS>.md -- to "https://docs.vornik.io", because CE ships
// none of those files and a dangling path sends a reader hunting. Five of the
// keys below are exactly those shapes, so in the EXPORTED tree they all collapse
// to one string: as a map literal that is `duplicate key ... in map literal`, a
// COMPILE error, and the CE export fails at its own imagemanifest gate. A slice
// tolerates the collapse, and the collapsed entry is inert in CE because it
// matches no relative path the walk produces -- which is right, since those
// documents are not in CE to be walked.
//
// Introduced by 53f8e533 and caught at the 2026.9.1 release by the verify-only
// export, before the public push. Do not "tidy" this back into a map literal.
var fakeAgentExemptPaths = []string{
	"https://docs.vornik.io",
	"https://docs.vornik.io",
	"https://docs.vornik.io",
	"https://docs.vornik.io",
	// DEVIATION from §11.4, which listed this as a live document to clean.
	// Reading it settled otherwise: the reference is inside
	// "### 2026.4.0 — Vertical Slice 1 / Status: released on 2026-04-11 /
	// Release highlights", a record of what that release actually shipped —
	// and it did ship the fake-agent flow. Editing it would falsify the
	// release record, which is precisely what §11.4 exempts the other three
	// historical documents to avoid. Same class, missed by a grep that had
	// not read the surrounding heading.
	"https://docs.vornik.io",
	// Same class as https://docs.vornik.io, and the first instance found by this
	// ratchet rather than by a grep: a release-notes file is a frozen record
	// of what a release shipped, and 2026.9.1 is the release that DELETED the
	// fixture. Its notes cannot describe that deletion without naming it, and
	// editing them later to hide the name would falsify the record the notes
	// exist to be. Exempted per-file rather than by a docs/release-notes/
	// prefix: a prefix would silently cover a FUTURE release that reintroduced
	// the fixture, which is exactly what this test is for.
	"docs/release-notes/2026.9.1.md",
	// The manifest's own `excluded` comment records why the map is empty; that
	// explanation is the point and cannot avoid naming what was removed.
	"internal/imagemanifest/manifest.go",
	// This file.
	"internal/imagemanifest/fake_agent_ratchet_test.go",
}

// fakeAgentExempt is the lookup form of fakeAgentExemptPaths.
var fakeAgentExempt = func() map[string]bool {
	m := make(map[string]bool, len(fakeAgentExemptPaths))
	for _, p := range fakeAgentExemptPaths {
		m[p] = true
	}
	return m
}()

// fakeAgentRatchetSkipDirs are trees this walk does not own. Every DOT-directory
// is skipped too (see the walk): .git, build scratch like .e2ecov, and
// .vornik-public-clone — the CE export's working clone, which is a checkout of
// an older tree and would otherwise report this repo's own history as offences.
var fakeAgentRatchetSkipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"bin":          true,
}

// TestFakeAgentStaysDeleted is a ratchet: the `fake-agent` fixture was deleted
// 2026-09-02 and must not come back by copy-paste.
//
// The image was a test fixture that had leaked into the release record as though
// the product shipped it, kept alive by a manual e2e walkthrough that had been
// unrunnable since 39f85103. Every remaining reference in the suite was an inert
// config STRING — no test built, pulled or ran it — which is exactly why they
// could sit there unnoticed, and exactly why a new one would too.
//
// Without this guard the strings drift back in from an older test, and the next
// reader concludes a deleted image is still required. That confusion is what the
// deletion exists to remove, so the guard is part of the deletion.
//
// Deliberately a source-tree walk rather than a `git grep`: it must hold in the
// exported CE tree, which is not a git checkout at export time.
func TestFakeAgentStaysDeleted(t *testing.T) {
	root := repoRoot(t)
	// Split so this literal does not match itself when the walk reads this file
	// via the exemption's absence in some future refactor.
	needle := "fake" + "-agent"

	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if fakeAgentRatchetSkipDirs[d.Name()] || (d.Name() != "." && strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if fakeAgentExempt[filepath.ToSlash(rel)] {
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yaml", ".yml", ".json", ".sh", ".tmpl", "":
		default:
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			// An unreadable file is not evidence of absence, so say so rather
			// than passing silently.
			offenders = append(offenders, filepath.ToSlash(rel)+" (unreadable: "+readErr.Error()+")")
			return nil
		}
		// A compiled binary can carry the string from a source file that no
		// longer exists, which is not a source-tree offence. The extensionless
		// case exists for shell scripts with no suffix, and `vornik` /
		// `vornik-images` sit in the repo root looking exactly like one.
		if bytes.IndexByte(body, 0) >= 0 {
			return nil
		}
		if strings.Contains(string(body), needle) {
			offenders = append(offenders, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(offenders) > 0 {
		t.Errorf("%q reappeared in %d file(s):\n  %s\n\n"+
			"The fixture was DELETED 2026-09-02 (images/fake-agent/, the make target, the\n"+
			"manifest exclusion, and the manual e2e walkthrough that justified it). Test\n"+
			"config strings use \"test-image:latest\" now.\n"+
			"If a reference is genuinely historical, add its path to fakeAgentExempt with a\n"+
			"reason — do NOT widen the walk or delete this test.\n"+
			"See https://docs.vornik.io §11.5.",
			needle, len(offenders), strings.Join(offenders, "\n  "))
	}
}
