// Package editions declares the customer-facing CE/EE capability matrix that
// is rendered into docs/public/editions.md, and cross-checks it against the
// featuredoctor registry so the published matrix cannot drift from the code.
//
// The matrix rows are CURATED (their capability labels and granularity are a
// product/marketing decision, coarser than the operational featuredoctor
// registry). featuredoctor remains the FAILSAFE authority for editions: a row
// that links to a featuredoctor feature (via FeatureID) inherits that feature's
// edition truth, and CrossCheck refuses any matrix that under-promises an
// Enterprise feature, downgrades one to Community, or surfaces a feature hidden
// from the public docs (trading). New Enterprise features therefore cannot ship
// without a matching Enterprise row.
package editions

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/featuredoctor"
)

// Row is one customer-facing capability line in the editions matrix.
type Row struct {
	Capability string // customer-facing label
	Community  bool   // available in the Community edition
	Enterprise bool   // available in the Enterprise edition
	// FeatureID optionally links the row to a featuredoctor.Feature.ID. When
	// set, CrossCheck derives the row's edition truth from that feature so the
	// docs cannot contradict the code. "" => not a doctor-managed feature
	// (always-on core, or a capability not modelled as an opt-in feature).
	FeatureID string
}

// hiddenFeatureIDs are featuredoctor features deliberately kept OUT of the
// public docs (the trading domain — see cmd/docs-gen publicDocExcludedSections
// and the mkdocs exclude). A matrix row must never link to one.
var hiddenFeatureIDs = map[string]bool{
	"trading-series": true,
}

// Matrix is the curated CE/EE capability matrix, in display order. Trading is
// intentionally absent (hidden from public docs).
func Matrix() []Row {
	return []Row{
		{Capability: "Task orchestration (tasks, leases, durable execution)", Community: true, Enterprise: true},
		{Capability: "Workflows", Community: true, Enterprise: true},
		{Capability: "Tool access over MCP", Community: true, Enterprise: true},
		{Capability: "Control CLI (`vornikctl`) + HTTP API", Community: true, Enterprise: true},
		{Capability: "Counterfactual replay / “Black Box”", Enterprise: true},
		{Capability: "Learning / “Instinct” layer (learned budgets)", Enterprise: true, FeatureID: "instinct"},
		{Capability: "Clustering / horizontal scale", Enterprise: true, FeatureID: "cluster"},
		{Capability: "Admin suite", Enterprise: true},
		{Capability: "Memory firewall", Community: true, Enterprise: true /* FeatureID OMITTED — CrossCheck-exempt; gated at runtime, not via featuredoctor (see TestMemoryFirewallIsCommunity) */},
		{Capability: "OIDC / SSO", Enterprise: true},
		{Capability: "Log shipping (Logship)", Enterprise: true},
	}
}

func cell(on bool) string {
	if on {
		return "✅"
	}
	return "—"
}

// RenderMatrix emits the matrix as a GitHub-flavoured Markdown table. This is
// the generated block written into docs/public/editions.md.
func RenderMatrix() string {
	var b strings.Builder
	b.WriteString("| Capability | Community | Enterprise |\n|---|:---:|:---:|\n")
	for _, r := range Matrix() {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Capability, cell(r.Community), cell(r.Enterprise))
	}
	return b.String()
}

// CrossCheck validates the curated matrix against the live feature registry,
// returning a sorted list of human-readable mismatches (empty => consistent).
func CrossCheck(features []featuredoctor.Feature) []string {
	return crossCheckRows(Matrix(), features)
}

// crossCheckRows is the pure cross-check over explicit rows + features.
func crossCheckRows(rows []Row, features []featuredoctor.Feature) []string {
	byID := make(map[string]featuredoctor.Feature, len(features))
	for _, f := range features {
		byID[f.ID] = f
	}

	var mismatches []string
	linked := map[string]int{} // FeatureID -> count of rows linking it

	for _, r := range rows {
		if r.FeatureID == "" {
			continue
		}
		linked[r.FeatureID]++
		f, ok := byID[r.FeatureID]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf(
				"matrix row %q links unknown feature %q", r.Capability, r.FeatureID))
			continue
		}
		if hiddenFeatureIDs[r.FeatureID] {
			mismatches = append(mismatches, fmt.Sprintf(
				"matrix row %q links hidden feature %q (must not appear in public docs)", r.Capability, r.FeatureID))
			continue
		}
		if f.IsEnterprise() {
			if !r.Enterprise || r.Community {
				mismatches = append(mismatches, fmt.Sprintf(
					"matrix row %q links Enterprise feature %q but is not Enterprise-only (Community=%t, Enterprise=%t)",
					r.Capability, r.FeatureID, r.Community, r.Enterprise))
			}
		} else if !r.Community {
			mismatches = append(mismatches, fmt.Sprintf(
				"matrix row %q links Community feature %q but is not marked Community", r.Capability, r.FeatureID))
		}
	}

	for _, r := range rows {
		if r.FeatureID != "" && linked[r.FeatureID] > 1 {
			mismatches = append(mismatches, fmt.Sprintf(
				"feature %q is linked by more than one matrix row", r.FeatureID))
			linked[r.FeatureID] = 0 // report once
		}
	}

	// Every visible Enterprise feature MUST have a linking row, so a new EE
	// feature cannot ship without being declared in the customer matrix.
	for _, f := range features {
		if f.IsEnterprise() && !hiddenFeatureIDs[f.ID] && linked[f.ID] == 0 {
			mismatches = append(mismatches, fmt.Sprintf(
				"Enterprise feature %q has no Enterprise row in the editions matrix", f.ID))
		}
	}

	sort.Strings(mismatches)
	return mismatches
}
