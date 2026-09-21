package api

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// checkConfigClassCompat reports deployed workflows naming a step error class
// the installed binary rejects — a tree that will take the whole daemon down
// on its next restart.
//
// The walk itself lives in registry.ScanDeployedTreeForDeadClasses, because the
// OFFLINE doctor (internal/cli) needs the same scan for the pre-cutover
// preflight and neither doctor package may import the other. One
// implementation, so the two paths cannot drift into diagnosing different
// things.
//
// WHAT THIS DOES NOT COVER, stated in the message rather than only in the
// design: it answers "would the binary running NOW refuse this tree". It cannot
// answer "will the next binary refuse it", because the next binary is not
// running when that answer is needed. Upgrade coverage is step 4b of
// deployments/podman/UPDATING.md, which runs the NEW binary's offline doctor
// against the deployed tree before the swap.
func (h *DoctorHandlers) checkConfigClassCompat() DoctorCheck {
	const name = "config_class_compat"
	if h.configDir == "" {
		// SKIPPED, never OK: an unevaluated check reporting OK is the
		// silent-control failure the doctor-skipped-vs-ok design exists for.
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "no config directory configured, skipping"}
	}

	scan := registry.ScanDeployedTreeForDeadClasses(h.configDir)
	if len(scan.Findings) > 0 {
		// ERROR, not WARNING, and the asymmetry with workflow_swarm_compat is
		// deliberate: that check reports a latent per-project mismatch where
		// partial service survives; this one predicts a daemon that will not
		// start at all. Per the severity gradation in
		// 2026-08-27-fail-open-census-and-coverage-denominators-design.md,
		// "this deployment is broken" is the ERROR row.
		return DoctorCheck{
			Name:   name,
			Status: "ERROR",
			Message: fmt.Sprintf("%d deployed workflow step(s) name a step error class this "+
				"binary rejects; the daemon will FAIL TO START on its next restart and flap "+
				"on Restart=on-failure. Valid classes: %s. This answers only whether the "+
				"binary running now refuses this tree — upgrade coverage is UPDATING.md "+
				"step 4b, which runs the new binary's doctor before cutover",
				len(scan.Findings), strings.Join(stepoutcome.ErrorClasses(), ", ")),
			Items: append(scan.Findings, scan.Skipped...),
		}
	}

	msg := "every deployed workflow's retry classes are accepted by this binary"
	if len(scan.Skipped) > 0 {
		msg += fmt.Sprintf(" (%d file(s) unparseable, skipped individually)", len(scan.Skipped))
	}
	// doctor-vacuous: the tree WAS examined and the empty set is a real
	// finding. The unparseable files are named individually in Items rather
	// than collapsing the whole check to SKIPPED — a per-file skip must not
	// hide a dead class in the files that DID parse, which is the failure the
	// per-file scan exists to prevent. SKIPPED is reserved above for the case
	// where nothing was evaluated at all: no config directory.
	return DoctorCheck{Name: name, Status: "OK", Message: msg, Items: scan.Skipped}
}

// stepOutcomeClassesForTest exposes the binary's declared class set to this
// package's tests, so the "valid set comes from the loader's source" assertion
// iterates it rather than snapshotting it. A snapshot would fail on every
// legitimate class addition and train someone to delete the test, restoring
// the local-copy drift the assertion prevents.
func stepOutcomeClassesForTest() []string { return stepoutcome.ErrorClasses() }
