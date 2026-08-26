package imagemanifest

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot is defined in manifest_test.go.

// TestEveryManifestPathExists — the check that was missing.
//
// The manifest is the authoritative list of buildable images (contract C4), and
// nothing asserted that the paths in it are real. They were, in the Enterprise
// superset. They were not in the Community export, which does
// `rm -rf services images/vornik-broker images/vornik-scraper-login` — so CE
// shipped a manifest naming six Containerfiles absent from its own tree, and
// `cmd/vornik-images` / the image_freshness doctor check would have referenced
// images CE cannot build.
//
// It surfaced on 2026-08-26 as the export's EE-feature marker sweep going red,
// which is a leak gate, not a correctness gate: it happened to name one of the
// six. This test covers all of them, in whichever tree it runs — so the EE
// checkout asserts the EE paths and the exported CE checkout asserts the CE
// ones.
func TestEveryManifestPathExists(t *testing.T) {
	root := repoRoot(t)
	for _, img := range manifest {
		t.Run(img.Tag, func(t *testing.T) {
			if img.Containerfile == "" {
				if img.Condition != ConditionExcluded {
					t.Errorf("%s has no Containerfile but is not ConditionExcluded", img.Tag)
				}
				return
			}
			if _, err := os.Stat(filepath.Join(root, img.Containerfile)); err != nil {
				t.Errorf("manifest entry %s names Containerfile %q, which does not exist in this tree.\n"+
					"If the image is Enterprise-only it belongs in manifest_enterprise.go, which the\n"+
					"CE export prunes — see https://docs.vornik.io",
					img.Tag, img.Containerfile)
			}
			if img.Context != "" {
				if _, err := os.Stat(filepath.Join(root, img.Context)); err != nil {
					t.Errorf("manifest entry %s names build context %q, which does not exist", img.Tag, img.Context)
				}
			}
		})
	}
}
