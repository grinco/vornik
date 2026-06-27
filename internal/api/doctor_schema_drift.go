package api

import (
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// checkSchemaGateDrift surfaces the same workflow-gate vs role-schema
// mismatches that registry's stripInvalidProjects refuses at config
// load (item 11 of https://docs.vornik.io), but
// expressed as a doctor WARNING so an operator running `vornikctl
// doctor` after editing a YAML — but BEFORE restarting the daemon —
// gets a heads-up before the hard refusal hits.
//
// Same logic as item 11 to keep the failure message consistent across
// the two surfaces; the difference is just that doctor doesn't strip
// or refuse, it just lists what the strip path would do.
//
// Severity stays WARNING because the same refusal will happen at load
// time anyway — the doctor's job is to surface it earlier, not to
// gate. An ERROR severity would be redundant with config_validation's
// own surface for the same class of bug.
func (h *DoctorHandlers) checkSchemaGateDrift() DoctorCheck {
	const name = "schema_gate_drift"
	if h.configDir == "" {
		return DoctorCheck{Name: name, Status: "OK", Message: "no config directory configured, skipping"}
	}

	// Stage without activating so we can capture stripInvalidProjects'
	// findings without mutating the live registry. Stage builds a
	// fresh ConfigSet from disk; StripInvalidFromStaged returns the
	// drift list as *ValidationError without affecting the active
	// snapshot.
	reg := registry.New()
	if err := reg.Stage(h.configDir); err != nil {
		// Stage parse failures are config_validation's job. We can't
		// do schema-gate analysis on broken YAML — skip rather than
		// double-report.
		return DoctorCheck{Name: name, Status: "OK", Message: "configs failed to stage; config_validation reports the underlying error"}
	}
	verr := reg.StripInvalidFromStaged()
	if verr == nil {
		return DoctorCheck{Name: name, Status: "OK", Message: "no schema/gate drift across loaded projects"}
	}

	// Filter for the schema-gate-specific findings; other strip
	// reasons (missing swarm, missing workflow, missing role) are
	// surfaced by config_validation already. The strip path's error
	// message anchors on "outputSchema" specifically, so a substring
	// match is the cheapest reliable filter here.
	var driftItems []string
	for _, e := range verr.Errors {
		msg := e.Error()
		if strings.Contains(msg, "outputSchema") {
			driftItems = append(driftItems, msg)
		}
	}
	if len(driftItems) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: "no schema/gate drift across loaded projects"}
	}
	sort.Strings(driftItems)
	return DoctorCheck{
		Name:    name,
		Status:  "WARNING",
		Message: fmt.Sprintf("%d project(s) have a workflow gate referencing a path the role's outputSchema doesn't declare", len(driftItems)),
		Items:   driftItems,
	}
}
