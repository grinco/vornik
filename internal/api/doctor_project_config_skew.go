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
func (h *DoctorHandlers) checkProjectConfigSkew(fix bool) DoctorCheck {
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

	examined, items, healed, skew := h.scanProjectFiles(projectsDir, entries, fix)
	sort.Strings(items)

	if len(items) == 0 {
		msg := fmt.Sprintf("%d project file(s) examined; all load under this binary's schema", examined)
		if len(healed) > 0 {
			// Say what was CHANGED, always. A repair an operator cannot see is
			// a repair they cannot review or undo.
			msg += fmt.Sprintf(" (%d misspelt key(s) repaired by --fix)", len(healed))
		}
		return DoctorCheck{Name: name, Status: "OK", Message: msg, Items: healed}
	}
	if len(healed) > 0 {
		items = append(items, healed...)
		sort.Strings(items)
	}

	// ERROR, not WARNING. Each of these is a project that does not exist right
	// now — this is an outage being reported, not a risk.
	msg := fmt.Sprintf("%d of %d project file(s) would NOT load; those projects do not exist in the running registry", len(items), examined)
	if skew > 0 {
		msg += fmt.Sprintf(" (%d look like config written for a NEWER release than this binary — deploy the binary first)", skew)
	}
	return DoctorCheck{Name: name, Status: "ERROR", Message: msg, Items: items}
}

// joinRepairs renders the per-key repairs for the operator-facing item.
func joinRepairs(changes []registry.KeyRepair) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, "; ")
}

// repairSpelling applies §13.4's narrow repair to one file and reports what
// happened: the operator-facing note, the diagnosis of the REPAIRED bytes when
// the file is still rejected, and whether it now loads.
//
// Split out because the branching belongs to the repair, not to the walk — and
// because a check that both edits files and classifies them wants the edit in
// one readable place.
func (h *DoctorHandlers) repairSpelling(path, fname string, data []byte, diag registry.SkewDiagnosis) (note string, redone registry.SkewDiagnosis, clean bool) {
	repaired, changes, healErr := registry.HealMisspelledKeys(data, diag)
	if healErr != nil || len(changes) == 0 {
		return "", registry.SkewDiagnosis{}, false
	}
	if writeErr := os.WriteFile(path, repaired, 0o600); writeErr != nil {
		return fmt.Sprintf("%s: could not write the repair (%v)", fname, writeErr), registry.SkewDiagnosis{}, false
	}
	note = fmt.Sprintf("%s: %s", fname, joinRepairs(changes))

	// Re-decode the REPAIRED bytes. A file carrying a convention error AND a
	// key unknown under every spelling is not fixed, and reporting it as fixed
	// would be the worse half of this feature.
	again := registry.DecodeProjectStrict(repaired)
	if again == nil {
		return note, registry.SkewDiagnosis{}, true
	}
	return note, registry.DiagnoseProjectDecodeError(fname, repaired, again), false
}

// scanProjectFiles decodes every project file with the DAEMON'S decoder and
// classifies what it finds — optionally repairing a convention error on the way
// (§13.4).
//
// Extracted from checkProjectConfigSkew because the walk and the verdict are
// two jobs: the verdict is three lines, and burying it under the walk is what
// pushed the function past the complexity bound. The decoder is still the
// daemon's own — a check that re-implements the rule it checks drifts from it,
// and then certifies a compatibility that does not hold.
func (h *DoctorHandlers) scanProjectFiles(projectsDir string, entries []os.DirEntry, fix bool) (examined int, items, healed []string, skew int) {
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
		// §13.4: --fix repairs a key whose ONLY defect is spelling, because
		// that is the one class with an unambiguous correct end state — the
		// operator's intent is legible in the key they typed. A key unknown
		// under every spelling is a deploy-ordering problem and is left alone;
		// "fixing" it would mean deleting it, which restores the project by
		// silently disabling what the operator was configuring.
		//
		// Opt-in, never at startup: a path that edits a customer's config
		// without being asked is very hard to reason about afterwards, and the
		// recovery it buys is minutes.
		if fix {
			note, redone, clean := h.repairSpelling(path, fname, data, diag)
			if note != "" {
				if clean || redone.File != "" {
					healed = append(healed, note)
				} else {
					items = append(items, note)
				}
			}
			if clean {
				continue
			}
			if redone.File != "" {
				diag = redone
			}
		}
		items = append(items, diag.Detail())
	}
	return examined, items, healed, skew
}
