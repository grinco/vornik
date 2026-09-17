package api

import (
	"fmt"

	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/pricing/upstream"
)

// checkPricingDrift reports prices that disagree with the PINNED upstream
// snapshot. It is the sibling `pricing_coverage` needs: coverage answers "is
// there an entry", and a present-but-stale rate is invisible to every other
// check while still driving cost metrics, budget enforcement and spend
// attribution.
//
// It decides nothing (C6). Upstream is not automatically right — the largest
// disagreement in this deployment is `deepseek-v4-flash`, where OUR number is
// the operator's vendor-supplied rate and upstream is wrong by 4.6x — so the
// finding carries both numbers and no code path writes pricing.yaml.
//
// Design https://docs.vornik.io,
// amendment 2026-09-17.
func (h *DoctorHandlers) checkPricingDrift() DoctorCheck {
	snap, err := upstream.Load()
	if err != nil {
		return DoctorCheck{
			Name:    "pricing_drift",
			Status:  "WARNING",
			Message: fmt.Sprintf("pinned upstream price snapshot unreadable: %v", err),
		}
	}
	return h.checkPricingDriftAgainst(snap)
}

// checkPricingDriftAgainst is the check against an explicit snapshot, so the
// comparison is testable without rebuilding the embedded artifact.
func (h *DoctorHandlers) checkPricingDriftAgainst(snap upstream.Snapshot) DoctorCheck {
	name := "pricing_drift"
	if h.pricingPath == "" {
		// Examined nothing. Reporting OK here is the exact failure the parent
		// design exists to remove.
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "pricing.yaml path not configured; no prices were compared"}
	}
	table, err := pricing.Load(h.pricingPath)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR",
			Message: fmt.Sprintf("load pricing table: %v", err)}
	}

	ours := upstream.RatesFrom(table)
	rep := snap.Compare(ours, upstream.DriftThreshold)
	total := len(ours)

	// C1: the denominator is what was COMPARED. "3 prices drifted" would read
	// as 3 of the whole table; it means 3 of the ids that upstream prices at
	// all. The rest are a naming-convention gap whose resolution needs to know
	// which provider serves the id — a string match would book another
	// provider's price.
	scope := fmt.Sprintf("compared %d of %d priced id(s) against %s@%s; %d could not be verified (no upstream entry)",
		rep.Compared, total, snap.Provenance.Repo, shortCommit(snap.Provenance.Commit), rep.Unmatched)

	var items []string
	for _, d := range rep.Drifted {
		items = append(items, fmt.Sprintf(
			"%s — ours in/out %.4f/%.4f, upstream %.4f/%.4f (%.0f%% apart). "+
				"Decide which is right; nothing is applied automatically",
			d.ID, d.OurInput, d.OurOutput, d.UpstreamInput, d.UpstreamOutput, d.Ratio*100))
	}
	// D8: an available field is a different action from a disagreement, so it
	// is listed separately and never counted as drift.
	for _, e := range rep.CacheReadAvailable {
		items = append(items, fmt.Sprintf(
			"%s — upstream has a cache_read rate of %.4f (context %d) that this table does not carry. "+
				"Omitting the tier overstated a prompt-heavy agent workload by 2.6x on 2026-09-17",
			e.ID, e.CacheRead, e.MaxInputTokens))
	}

	if len(rep.Drifted) == 0 {
		msg := "no price disagrees with the pinned snapshot by more than 10% — " + scope
		if len(items) == 0 {
			return DoctorCheck{Name: name, Status: "OK", Message: msg}
		}
		// Enrichment alone is not a wrong price, so the check still reads OK.
		return DoctorCheck{Name: name, Status: "OK", Message: msg, Items: items}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d price(s) disagree with the pinned upstream snapshot — %s", len(rep.Drifted), scope),
		Items:   items,
	}
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
