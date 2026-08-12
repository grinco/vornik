package architecture

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Slice D of https://docs.vornik.io
//
// llmspend.Recorder makes "what happens to this component's spend?" a question the
// compiler asks at every construction site. The answer "nothing" is spelled
// llmspend.Disabled(), and that is the whole point of the design: a nil recorder is
// INVISIBLE, whereas a call to Disabled() is greppable, classifiable and reviewable.
//
// This law is what turns "greppable" into "gated". Every non-test production call
// site must be classified below with a reason. Tests may call it freely — a test
// that does not want a ledger is ordinary, and requiring an allowlist entry per test
// would train people to add entries without thinking, which is how a registry stops
// meaning anything.
//
// What this proves: no production component silently opts out of billing.
// What it does not prove: that an enabled recorder is actually CALLED, in the right
// order, with the right project. Those are per-component behavioural tests
// (the "an unusable response still bills" pattern) and remain necessary.

type disabledUse struct {
	// file is the non-test source file, relative to the module root.
	file string
	// reason must say why this component's spend is knowingly unattributed and
	// what it would take to bill it. Required — an unexplained opt-out is
	// indistinguishable from an accident, which is the state this design left.
	reason string
}

// llmspendDisabledAllowlist classifies every production call to llmspend.Disabled().
//
// Empty today: no component has migrated to the seam yet (slice C), so nothing has
// had the opportunity to opt out. The one known future entry is the memetic
// architect, which the chat call-site registry already classifies as deliberately
// unaccounted — it is constructed over a daemon-level provider with no project in
// scope, so there is nothing to attribute its spend to without inventing a project.
var llmspendDisabledAllowlist = map[string]disabledUse{}

// TestProductionDisabledUsesAreClassified fails when production code opts out of
// billing without a recorded reason.
func TestProductionDisabledUsesAreClassified(t *testing.T) {
	root := moduleRoot(t)
	found := map[string][]string{} // file -> collapsed lines

	for _, sub := range []string{"internal", "cmd"} {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if info.Name() == "testdata" || info.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			// Tests may disable billing freely; see the package comment.
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			// The seam's own definition of Disabled is not a use of it.
			if strings.HasPrefix(rel, "internal/llmspend/") {
				return nil
			}
			for _, line := range strings.Split(string(b), "\n") {
				trimmed := strings.TrimSpace(line)
				// A doc comment mentioning the function is not a call to it. Without
				// this, the law fires on its own explanatory prose — which it did on
				// first run, in llmspend_wiring.go and titler.go.
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
					continue
				}
				if strings.Contains(line, "llmspend.Disabled()") {
					found[rel] = append(found[rel], strings.Join(strings.Fields(line), " "))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}

	files := make([]string, 0, len(found))
	for f := range found {
		files = append(files, f)
	}
	sort.Strings(files)

	for _, f := range files {
		if _, ok := llmspendDisabledAllowlist[f]; !ok {
			t.Errorf("%s calls llmspend.Disabled() in production code without a classification.\n"+
				"  lines: %v\n"+
				"Add it to llmspendDisabledAllowlist with a reason saying why this component's "+
				"spend is knowingly unattributed and what it would take to bill it. Opting out of "+
				"the ledger is a decision the operator's billing requirement makes reviewable, not "+
				"a default.", f, found[f])
		}
	}
}

// TestDisabledAllowlistHasNoStaleEntries keeps the allowlist describing the code.
// A stale entry reads as evidence somebody checked a call site that no longer
// exists.
func TestDisabledAllowlistHasNoStaleEntries(t *testing.T) {
	root := moduleRoot(t)
	for f, use := range llmspendDisabledAllowlist {
		if strings.TrimSpace(use.reason) == "" {
			t.Errorf("%s: empty reason — an unexplained opt-out is indistinguishable from an accident", f)
		}
		b, err := os.ReadFile(filepath.Join(root, use.file))
		if err != nil {
			t.Errorf("%s: allowlisted file %s cannot be read: %v", f, use.file, err)
			continue
		}
		if !strings.Contains(string(b), "llmspend.Disabled()") {
			t.Errorf("%s no longer calls llmspend.Disabled() — remove the allowlist entry so it "+
				"keeps describing the code", use.file)
		}
	}
}
