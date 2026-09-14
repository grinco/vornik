package api

// Doctor check: project_config_skew.
//
// UPGRADE SAFETY, the detection half (BACKLOG P1 2026-09-12, slice 1).
//
// The project loader is strict and fail-CLOSED: a key it does not model
// rejects the WHOLE FILE, and a rejected file is skipped rather than failing
// the registry. So a project written for a newer release than the running
// binary does not partially work — it does not exist, and neither does its
// autonomy, chat, email or webhook surface. On 2026-09-10 that took a project
// dark for ~30 minutes on a dev box with an operator watching. The same
// sequence on a customer install is a silent outage of a whole project with
// nobody to correlate it to the upgrade, because it surfaces as "my project is
// gone", not as "the upgrade rejected a key".
//
// This check reads the DEPLOYED project tree with the daemon's own decoder and
// reports every file that would be refused, with the diagnosis: which project
// consequently does not exist, whether the cause is a misspelt key or one this
// binary has never heard of, and the remedy for each.
//
// WHY NO --fix. Every other repairable check in this file repairs something
// whose correct end state is unambiguous. This one's is not: deleting the
// offending key restores the project by DISABLING whatever the operator was
// configuring, and doing that silently to a customer's config is a worse
// failure than the one it fixes. The remedy is a deploy (or a one-line edit
// the operator makes knowingly), so the check names it and stops. An auto-heal
// that rewrites a customer config is its own hazard.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkProjectConfigSkew reports deployed project files the running binary
// would refuse to load.
func (h *DoctorHandlers) checkProjectConfigSkew() DoctorCheck {
	name := "project_config_skew"

	if h.configDir == "" {
		// NOT CHECKED, and it says so. A check that cannot distinguish
		// "examined and clean" from "never examined" reports the first and
		// means the second — the class this deployment has spent months
		// retiring, and one an upgrade-safety check must not join.
		return DoctorCheck{
			Name:    name,
			Status:  "SKIPPED",
			Message: "config dir not wired; no project tree was examined (this is NOT a statement that the deployed projects load)",
		}
	}

	projectsDir := filepath.Join(h.configDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{Name: name, Status: "OK", Message: "no projects directory; nothing to skew"}
		}
		return DoctorCheck{
			Name:    name,
			Status:  "SKIPPED",
			Message: fmt.Sprintf("could not read %s (%v); no project file was examined", projectsDir, err),
		}
	}

	var (
		examined int
		items    []string
		skew     int
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fname := e.Name()
		if !strings.HasSuffix(fname, ".yaml") && !strings.HasSuffix(fname, ".yml") {
			continue
		}
		path := filepath.Join(projectsDir, fname)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			items = append(items, fmt.Sprintf("%s: unreadable (%v) — NOT examined", fname, readErr))
			continue
		}
		examined++
		// The DAEMON'S decoder, not a second one. A check that re-implements
		// the rule it is checking drifts from it, and then certifies a
		// compatibility that does not hold.
		decErr := registry.DecodeProjectStrict(data)
		if decErr == nil {
			continue
		}
		diag := registry.DiagnoseProjectDecodeError(fname, data, decErr)
		if diag.LikelyVersionSkew {
			skew++
		}
		items = append(items, diag.Detail())
	}
	sort.Strings(items)

	if len(items) == 0 {
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("%d project file(s) examined; all load under this binary's schema", examined),
		}
	}

	// ERROR, not WARNING. Each of these is a project that does not exist right
	// now — this is an outage being reported, not a risk.
	msg := fmt.Sprintf("%d of %d project file(s) would NOT load; those projects do not exist in the running registry", len(items), examined)
	if skew > 0 {
		msg += fmt.Sprintf(" (%d look like config written for a NEWER release than this binary — deploy the binary first)", skew)
	}
	return DoctorCheck{Name: name, Status: "ERROR", Message: msg, Items: items}
}
