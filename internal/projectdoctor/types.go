// Package projectdoctor diagnoses a single project's readiness — the
// post-creation counterpart to internal/featuredoctor (which is
// daemon/feature-scoped). Each check reads the committed
// registry.Project and live daemon subsystems through the narrow
// interfaces in Deps, returning a CheckResult with a traffic-light
// Status plus operator-facing remediation. See
// https://docs.vornik.io Phase 2.
package projectdoctor

// Status is a check's traffic-light outcome.
type Status string

// Status constants define the traffic-light outcomes.
const (
	StatusGreen   Status = "green"   // check passed
	StatusYellow  Status = "yellow"  // soft/transient failure — heads-up, does not block completeness
	StatusRed     Status = "red"     // hard failure — must fix
	StatusNeutral Status = "neutral" // not applicable (e.g. no secrets declared, autonomy off)
	StatusUnknown Status = "unknown" // could not evaluate (e.g. project did not resolve)
)

// CheckItem is one sub-entity row within a check (a single declared
// secret, a single MCP server).
type CheckItem struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// CheckResult is the outcome of one doctor check.
type CheckResult struct {
	Key         string            `json:"key"`   // stable id: config_valid|secrets|mcp|model|schedule|smoke
	Title       string            `json:"title"` // human label
	Status      Status            `json:"status"`
	Detail      string            `json:"detail,omitempty"`      // one-line summary of what was found
	Remediation string            `json:"remediation,omitempty"` // how to fix; shown when not green
	FixHref     string            `json:"fixHref,omitempty"`     // link to the surface that fixes it
	Required    bool              `json:"required"`              // green/yellow required for "complete"
	Items       []CheckItem       `json:"items,omitempty"`       // per-sub-entity rows
	Meta        map[string]string `json:"meta,omitempty"`        // check-specific extras (smoke taskID, cost, next-fire)
}

// Report is the full readiness diagnosis for one project.
type Report struct {
	ProjectID string        `json:"projectId"`
	Checks    []CheckResult `json:"checks"`
	Complete  bool          `json:"complete"`
}

// ComputeComplete reports whether setup is complete: no REQUIRED
// check is red or unknown. Yellow (soft/transient) and neutral (n/a)
// are acceptable; non-required checks never block.
func ComputeComplete(checks []CheckResult) bool {
	for _, c := range checks {
		if !c.Required {
			continue
		}
		if c.Status == StatusRed || c.Status == StatusUnknown {
			return false
		}
	}
	return true
}

// WorstOf returns the most severe status by precedence
// red > unknown > yellow > green > neutral. Used to roll per-item
// statuses up into a check's overall status. Empty input => neutral.
func WorstOf(statuses ...Status) Status {
	rank := map[Status]int{
		StatusNeutral: 0,
		StatusGreen:   1,
		StatusYellow:  2,
		StatusUnknown: 3,
		StatusRed:     4,
	}
	worst := StatusNeutral
	for _, s := range statuses {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	return worst
}
