package api

// role_library doctor check — the mandatory Phase-1 guardrail for the
// NL automation composer's curated role library
// (https://docs.vornik.io §5.3, §11 Q1).
//
// The library (configs/role-library/*.md) is the outer boundary of
// every composed automation's capabilities: each archetype's `tools`
// list is the MAXIMUM a composed role may carry. A broken archetype
// (unknown tool, undeclared prompt splice point, bad runtime, empty
// requiredOutputKeys) must fail LOUDLY rather than silently degrade
// every future composition, and any broad/expanded allowlist is
// surfaced as a security callout. This check makes that contract
// enforceable from `POST /api/v1/doctor` (and thus `make lint` /
// vornikctl doctor), mirroring the role_prompt_sanity precedent.

import (
	"fmt"
	"sort"

	"vornik.io/vornik/internal/rolelibrary"
)

// SetSystemHandlerNames records the system-step handler names the
// role-library doctor check accepts as valid tool entries (in addition
// to built-in agent tools and mcp__ references). The daemon wires this
// from executor.SystemHandlerRegistry.Names() at boot; nil/unset means
// only built-in + mcp names validate, which is correct for the seeded
// library (it references no system handlers).
func (h *DoctorHandlers) SetSystemHandlerNames(names []string) {
	h.systemHandlerNames = append([]string(nil), names...)
}

// checkRoleLibrary loads configs/role-library and runs the mandatory
// role-library doctor check. Status semantics:
//   - ERROR when any archetype has a validation error (broken parts
//     bin — a composition grounded on it would misbehave).
//   - WARNING when the library is clean but ≥1 archetype trips the
//     broad-allowlist security flag (loud, review-worthy, not fatal).
//   - OK otherwise (including an absent library — presence is the
//     feature-doctor prereq's concern, not this shape check's).
func (h *DoctorHandlers) checkRoleLibrary() DoctorCheck {
	const name = "role_library"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config dir; skipping"}
	}
	// LoadWithFindings enumerates every malformed file as a finding instead of
	// aborting on the first, so the report shows the WHOLE library in one pass
	// (review-20260716-7e65 #1). err is only a directory-level read failure.
	archetypes, parseFindings, err := rolelibrary.LoadWithFindings(h.configDir)
	if err != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf("load role library: %v", err)}
	}
	if len(archetypes) == 0 && len(parseFindings) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no role-library entries configured"}
	}

	// Parse-failure findings are surfaced alongside the structural ones.
	findings := parseFindings
	findings = append(findings, rolelibrary.CheckLibrary(archetypes, h.systemHandlerNames)...)

	var errors, flags []string
	for _, f := range findings {
		switch f.Severity {
		case rolelibrary.SeverityError:
			errors = append(errors, f.String())
		case rolelibrary.SeverityFlag:
			flags = append(flags, f.String())
		}
	}

	if len(errors) > 0 {
		items := append([]string{}, errors...)
		items = append(items, flags...)
		sort.Strings(items)
		return DoctorCheck{
			Name:    name,
			Status:  "ERROR",
			Message: fmt.Sprintf("%d role-library archetype error(s) across %d entrie(s)", len(errors), len(archetypes)),
			Items:   items,
		}
	}
	if len(flags) > 0 {
		sort.Strings(flags)
		return DoctorCheck{
			Name:    name,
			Status:  "WARNING",
			Message: fmt.Sprintf("%d broad-allowlist flag(s) — review-worthy (§5.3)", len(flags)),
			Items:   flags,
		}
	}
	return DoctorCheck{
		Name:    name,
		Status:  "OK",
		Message: fmt.Sprintf("all %d role-library archetype(s) pass the check", len(archetypes)),
	}
}
