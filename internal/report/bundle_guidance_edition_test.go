package report

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/version"
)

// TestBundleGuidance_CommunityNamesTheLocalPath is the SECOND half of the
// 2026-08-05 CE dead end, and it inverts the first.
//
// `vornikctl report` closed by telling every reporter to run `vornikctl
// support-report`, which answered a CE caller with 501. The first fix was to
// stop naming a command that could not work. The real fix, landed 2026-09-04,
// was to make it work: `--local` builds the same bundle in-process from the
// database and config on the host, authorised by the shell access the operator
// already has. So the CE text names the command again — with --local, and
// saying which sections the local path cannot produce.
func TestBundleGuidance_CommunityNamesTheLocalPath(t *testing.T) {
	g := BundleGuidance("--task task_123", version.EditionCommunity)

	assert.Contains(t, g, "vornikctl support-report --local",
		"CE can collect a bundle now; the guidance must name the path that works")
	assert.NotContains(t, g, "vornikctl support-report --task task_123",
		"the plain daemon invocation still 501s on CE — only the --local form may be advertised")
	// The honest limits, so an absent section is not read as a broken one.
	assert.Contains(t, strings.ToLower(g), "health and metrics",
		"say which sections the local path cannot collect")
	assert.Contains(t, strings.ToLower(g), "black box",
		"the EE-only trace must still be named as absent by construction")
	assert.Contains(t, strings.ToLower(g), "doctor",
		"point CE reporters at the diagnostics the report body already carries")
}

// TestBundleGuidance_EnterpriseStillAdvertisesTheBundle — the EE text is the
// load-bearing one (operator instruction 2026-08-03: a reporter who does not
// know the archive's name, that they must inspect it, or that GitHub takes it
// by drag-and-drop, does not attach it). It must survive the edition split
// intact.
func TestBundleGuidance_EnterpriseStillAdvertisesTheBundle(t *testing.T) {
	g := BundleGuidance("--task task_123", version.EditionEnterprise)

	assert.Contains(t, g, "vornikctl support-report --task task_123")
	assert.Contains(t, g, "tar -tzf", "the reporter must be told how to inspect it")
	assert.Contains(t, g, "MANIFEST.json")
	assert.Contains(t, strings.ToLower(g), "drag",
		"the drag-and-drop attach step is why reports arrive with logs")
}

// TestBundleGuidance_UnstampedEditionTreatedAsCommunity — an empty or garbage
// edition string must not leak the Enterprise instructions into a CE build.
// Mirrors version.NormalizeEdition's fail-safe: an untrusted edition collapses
// to the less-privileged one.
//
// What "leaked" means changed on 2026-09-04: both editions now name the
// command, so the tell is the INVOCATION — the daemon form still 501s on
// Community, and the local form is the one that works there.
func TestBundleGuidance_UnstampedEditionTreatedAsCommunity(t *testing.T) {
	for _, edition := range []string{"", "  ", "ENTERPRISE", "enterprise-ish", "nonsense"} {
		g := BundleGuidance("--task task_123", edition)
		assert.NotContains(t, g, "vornikctl support-report --task task_123",
			"edition %q must fail safe to Community", edition)
		assert.Contains(t, g, "vornikctl support-report --local",
			"edition %q must still get the Community path that works", edition)
	}
}
