package report

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/version"
)

// TestBundleGuidance_CommunityDoesNotAdvertiseEnterpriseCommand pins the fix
// for the 2026-08-05 CE dead end.
//
// `vornikctl report` is a Community command, and it closed by telling every
// reporter to run `vornikctl support-report` for the fuller evidence. That
// command is gated behind the Enterprise admin surface and answers a CE caller
// with "Enterprise-only feature: ... not built into Community Edition". So the
// product's own bug-reporting flow instructed a CE user to run a command that
// can never work, and the reporter hit a wall at exactly the moment they were
// trying to help.
func TestBundleGuidance_CommunityDoesNotAdvertiseEnterpriseCommand(t *testing.T) {
	g := BundleGuidance("--task task_123", version.EditionCommunity)

	assert.NotContains(t, g, "vornikctl support-report",
		"CE has no support-report command — advertising it sends the reporter into a 501")
	assert.NotContains(t, g, "--max-size",
		"support-report's flags are meaningless without the command")
	// It must still be useful: say why, and what the reporter CAN do.
	assert.Contains(t, strings.ToLower(g), "enterprise",
		"explain that the bundle collector is an Enterprise feature rather than going silent")
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
func TestBundleGuidance_UnstampedEditionTreatedAsCommunity(t *testing.T) {
	for _, edition := range []string{"", "  ", "ENTERPRISE", "enterprise-ish", "nonsense"} {
		g := BundleGuidance("--task task_123", edition)
		assert.NotContains(t, g, "vornikctl support-report",
			"edition %q must fail safe to Community", edition)
	}
}
