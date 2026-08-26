package podman

import (
	"os"
	"strings"
	"testing"
)

// These tests guard vornik-update.sh against the two defects that let a CE
// customer run the documented update path for six weeks without ever
// receiving an agent image (2026-08-25).
//
// The agent image bakes cmd/mcp-bridge, cmd/agent-helper and
// images/vornik-agent/entrypoint.sh, so an update that skips it delivers only
// half of any release that changed both sides. Commit 356e74cd — "fix(security):
// four agent tools bypassed the per-role allowlist" — was exactly such a
// release: it changed internal/agenttools AND the agent entrypoint, and every
// affected install got the daemon half only.

func readUpdater(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("vornik-update.sh")
	if err != nil {
		t.Fatalf("read vornik-update.sh: %v", err)
	}
	return string(data)
}

// TestUpdaterRebuildsImagesByDefault is the direct regression test for the
// reported bug. The script shipped with REBUILD_AGENT=0 and rebuilt only
// behind --rebuild-agent, while UPDATING.md told operators not to re-run the
// quickstart — the one path that rebuilt unconditionally. The recommended
// path was the one that skipped the image.
func TestUpdaterRebuildsImagesByDefault(t *testing.T) {
	body := readUpdater(t)

	if !strings.Contains(body, "REBUILD_IMAGES=1") {
		t.Error("image rebuild must DEFAULT to on: the opt-in default is what " +
			"froze a customer's agent image at install date for six weeks")
	}
	// Match an actual shell assignment at the start of a line, not any
	// mention: the script documents the old default in a comment, and that
	// history is worth keeping.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "REBUILD_AGENT=0") {
			t.Errorf("the opt-in default must be gone, found: %s", line)
		}
	}
	if !strings.Contains(body, "--no-rebuild-images") {
		t.Error("operators need an explicit opt-out; without one, inverting the " +
			"default removes a choice rather than fixing a bug")
	}
}

// TestUpdaterKeepsRebuildAgentAsCompatibleAlias: installed customers have
// --rebuild-agent in cron wrappers and timers. Breaking those to fix a default
// trades one silent failure for another.
func TestUpdaterKeepsRebuildAgentAsCompatibleAlias(t *testing.T) {
	body := readUpdater(t)
	if !strings.Contains(body, "--rebuild-agent") {
		t.Error("--rebuild-agent must still be ACCEPTED (deprecated, warning) so " +
			"existing wrapper scripts do not start failing on an unknown argument")
	}
}

// TestUpdaterBuildsImagesBeforeCutover pins contract C3.
//
// The original script rebuilt the agent AFTER installing the new binaries and
// only warn()ed on failure, so a failed rebuild left a NEW daemon running an
// OLD image — manufacturing precisely the half-applied state the rebuild
// exists to prevent. Every other step in the script follows a strict "fatal,
// nothing swapped" discipline; the image rebuild was the one that did not.
func TestUpdaterBuildsImagesBeforeCutover(t *testing.T) {
	body := readUpdater(t)

	rebuild := strings.Index(body, "rebuild_images")
	if rebuild < 0 {
		t.Fatal("no rebuild_images step found in vornik-update.sh")
	}
	// The cutover is the moment the new binaries are installed over the old.
	cutover := strings.Index(body, `install -m 0755 "$REPO_DIR/.bin/vornik"`)
	if cutover < 0 {
		t.Fatal("could not locate the binary-install cutover in vornik-update.sh")
	}

	if rebuild > cutover {
		t.Error("images must be rebuilt BEFORE the binaries are swapped in " +
			"(contract C3): a rebuild that fails after the cutover leaves a new " +
			"daemon with an old image, which is the half-applied state this change fixes")
	}
}

// TestUpdaterTreatsImageBuildFailureAsFatal is the other half of C3. A warn
// lets the update continue into exactly the state it was meant to prevent.
func TestUpdaterTreatsImageBuildFailureAsFatal(t *testing.T) {
	body := readUpdater(t)

	start := strings.Index(body, "rebuild_images()")
	if start < 0 {
		t.Fatal("rebuild_images() not found")
	}
	// Bound the search to the function body so an unrelated `warn` elsewhere
	// in the script cannot mask a real regression here.
	rest := body[start:]
	if end := strings.Index(rest, "\n}\n"); end > 0 {
		rest = rest[:end]
	}

	if !strings.Contains(rest, "die ") {
		t.Error("a failed image build must be fatal (die), not a warning: the " +
			"original script warned and carried on, leaving a new daemon on an old image")
	}
}

// TestUpdaterStampsProvenanceOnRebuild guards contract C2 at the one build
// site that runs on customer machines. An image rebuilt without these args
// carries no revision, so doctor's image_freshness check cannot tell whether
// it is current and the drift becomes undetectable again.
func TestUpdaterStampsProvenanceOnRebuild(t *testing.T) {
	body := readUpdater(t)
	for _, arg := range []string{"VORNIK_REVISION", "VORNIK_VERSION"} {
		if !strings.Contains(body, arg) {
			t.Errorf("rebuilds must stamp %s, or the resulting image is "+
				"unverifiable (contract C2)", arg)
		}
	}
}

// TestUpdaterSkipsImagesAlreadyAtTargetRevision: inverting the default is only
// safe if the common no-op update stays cheap. An update across a release that
// touched no image inputs should cost a label read per image, not a rebuild.
func TestUpdaterSkipsImagesAlreadyAtTargetRevision(t *testing.T) {
	body := readUpdater(t)
	if !strings.Contains(body, "org.opencontainers.image.revision") {
		t.Error("the updater must READ the revision label to decide whether a " +
			"rebuild is needed; without it every update rebuilds everything")
	}
}

// TestUpdaterUsesTheManifest is contract C4 at the update path: a new image
// must be covered without editing this script.
func TestUpdaterUsesTheManifest(t *testing.T) {
	body := readUpdater(t)
	if !strings.Contains(body, "vornik-images") {
		t.Error("the updater must read the image manifest rather than carrying " +
			"its own hardcoded list — a hardcoded list is how the cluster tags " +
			"ended up with no builder and how scraper/broker images were never " +
			"covered by any update path (contract C4)")
	}
}
