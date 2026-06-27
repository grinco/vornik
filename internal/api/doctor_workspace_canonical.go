package api

// Doctor check for legacy autonomy/ workspaces. Pairs with the
// canonical-context pre-load (LLD §3) — once every workspace is
// on .autonomy/, the "mixed" / "plain_autonomy" telemetry stops
// firing and the dual-convention support in the resolver can
// sunset.
//
// Triggers a WARNING when any project still has the legacy
// autonomy/ layout, surfaces the operator-actionable fix:
// `vornikctl workspace canonicalise`. Mixed workspaces (both
// directories) escalate to ERROR — they need manual resolution,
// the CLI deliberately doesn't merge.

import (
	"fmt"
	"sort"

	"vornik.io/vornik/internal/workspacecanonicalise"
)

// checkWorkspaceCanonical reports whether any project workspace
// still uses the legacy autonomy/ layout instead of .autonomy/.
func (h *DoctorHandlers) checkWorkspaceCanonical() DoctorCheck {
	name := "workspace_canonical_layout"
	if h.workspacesRoot == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no workspaces root configured, skipping"}
	}
	results, err := workspacecanonicalise.Scan(h.workspacesRoot)
	if err != nil {
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("failed to scan workspaces root: %v", err),
		}
	}
	legacy := workspacecanonicalise.LegacyProjects(results)
	mixed := workspacecanonicalise.MixedProjects(results)

	switch {
	case len(legacy) == 0 && len(mixed) == 0:
		return DoctorCheck{
			Name:    name,
			Status:  "OK",
			Message: fmt.Sprintf("all %d workspace(s) use the .autonomy/ canonical layout", len(results)),
		}
	case len(mixed) > 0:
		// Mixed wins: both directories present is a real bug
		// (canonical-context pre-load flags "mixed" source; the
		// resolver picks the first-hit dir, which can silently
		// disagree across machines).
		items := make([]string, 0, len(mixed)+len(legacy))
		for _, p := range mixed {
			items = append(items, "MIXED: "+p+" (both autonomy/ and .autonomy/ present)")
		}
		for _, p := range legacy {
			items = append(items, "LEGACY: "+p+" (autonomy/ only — run canonicalise)")
		}
		sort.Strings(items)
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: fmt.Sprintf("%d workspace(s) need manual resolution + %d legacy. Inspect each MIXED project + pick the surviving directory; then run `vornikctl workspace canonicalise` for the rest.", len(mixed), len(legacy)),
			Items:   items,
		}
	default:
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("%d workspace(s) still use the legacy autonomy/ layout — run `vornikctl workspace canonicalise` to migrate.", len(legacy)),
			Items:   legacy,
		}
	}
}
