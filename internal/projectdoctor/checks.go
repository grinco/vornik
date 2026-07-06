package projectdoctor

import (
	"context"
	"fmt"
	"time"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/registry"
)

// checkConfigValid reports whether the project's config resolves
// (project + swarm + workflows load and cross-reference cleanly).
// resolveErr is the error from ProjectResolver.ResolveProjectConfig,
// passed in so Run resolves once and shares the result.
func (d *Doctor) checkConfigValid(proj *registry.Project, resolveErr error) CheckResult {
	res := CheckResult{
		Key:      "config_valid",
		Title:    "Configuration valid",
		Required: true,
	}
	if resolveErr != nil {
		res.Status = StatusRed
		res.Detail = "Project config does not resolve: " + resolveErr.Error()
		res.Remediation = "Fix the reported error in the project config."
		res.FixHref = "/ui/projects/" + safeID(proj) + "/config"
		return res
	}
	res.Status = StatusGreen
	res.Detail = "Project, swarm, and workflows resolve cleanly."
	return res
}

// checkSchedule reports whether autonomy is armed. Neutral (and not
// required) when autonomy is off — a human-driven project needs no
// schedule. Red when enabled but the poll interval won't parse or a
// cron-mode project has no goal (cron fires the goal verbatim each
// tick, so an empty goal is a dead loop).
func (d *Doctor) checkSchedule(proj *registry.Project) CheckResult {
	res := CheckResult{Key: "schedule", Title: "Schedule armed"}
	a := proj.Autonomy
	if !a.Enabled {
		res.Status = StatusNeutral
		res.Required = false
		res.Detail = "Autonomy is off — this project runs on demand."
		return res
	}
	res.Required = true
	res.FixHref = "/ui/projects/" + proj.ID + "/config"
	interval := a.PollInterval
	if interval == "" {
		interval = "5m" // registry default
	}
	dur, err := time.ParseDuration(interval)
	if err != nil || dur <= 0 {
		res.Status = StatusRed
		res.Detail = fmt.Sprintf("Poll interval %q is not a valid duration.", a.PollInterval)
		res.Remediation = "Set autonomy.pollInterval to a Go duration like \"4h\"."
		return res
	}
	if a.Mode == registry.AutonomyModeCron && a.Goal == "" {
		res.Status = StatusRed
		res.Detail = "Cron mode fires the goal verbatim each tick, but the goal is empty."
		res.Remediation = "Set autonomy.goal, or switch mode to \"llm\"."
		return res
	}
	res.Status = StatusGreen
	res.Detail = fmt.Sprintf("Armed: fires every %s.", dur.String())
	res.Meta = map[string]string{"interval": dur.String()}
	return res
}

// safeID returns proj.ID or "" when proj is nil (config_valid may be
// called with a nil project on resolve failure).
func safeID(proj *registry.Project) string {
	if proj == nil {
		return ""
	}
	return proj.ID
}

// checkSecrets reports whether every secret the project declares by
// name (Permissions.Secrets) is present in the environment. Neutral
// (not required) when the project declares none. Unknown when no
// SecretReader is wired. Each declared secret becomes a CheckItem so
// the UI can render a per-secret masked-input fix.
func (d *Doctor) checkSecrets(proj *registry.Project) CheckResult {
	res := CheckResult{Key: "secrets", Title: "Secrets present"}
	names := proj.Permissions.Secrets
	if len(names) == 0 {
		res.Status = StatusNeutral
		res.Required = false
		res.Detail = "This project declares no secrets."
		return res
	}
	res.Required = true
	if d.deps.Secrets == nil {
		res.Status = StatusUnknown
		res.Detail = "Secret store not available."
		return res
	}
	statuses := make([]Status, 0, len(names))
	missing := 0
	for _, name := range names {
		item := CheckItem{Name: name}
		if d.deps.Secrets.Has(name) {
			item.Status = StatusGreen
			item.Detail = "present"
		} else {
			item.Status = StatusRed
			item.Detail = "missing"
			missing++
		}
		res.Items = append(res.Items, item)
		statuses = append(statuses, item.Status)
	}
	res.Status = WorstOf(statuses...)
	if missing > 0 {
		res.Detail = fmt.Sprintf("%d of %d declared secrets missing.", missing, len(names))
		res.Remediation = "Supply the missing secret values below."
	} else {
		res.Detail = fmt.Sprintf("All %d declared secrets present.", len(names))
	}
	return res
}

// checkMCP cross-references the project's declared MCP servers against
// the daemon's cached reachability snapshot (non-blocking, so this
// never blocks page render). Per the Phase 2 design: a server the
// daemon doesn't know about is a hard misconfiguration (red); a
// configured server whose probe failed is transient (yellow) because
// daemon-scoped MCP processes may still be starting. Neutral (not
// required) when the project subscribes to no servers.
func (d *Doctor) checkMCP(ctx context.Context, proj *registry.Project) CheckResult {
	res := CheckResult{
		Key:     "mcp",
		Title:   "MCP servers",
		FixHref: "/ui/mcp",
	}
	servers := proj.MCP.Servers
	if len(servers) == 0 {
		res.Status = StatusNeutral
		res.Required = false
		res.Detail = "This project subscribes to no MCP servers."
		return res
	}
	res.Required = true
	if d.deps.MCP == nil {
		res.Status = StatusUnknown
		res.Detail = "MCP registry not available."
		return res
	}
	byName := make(map[string]mcp.ServerSnapshot)
	for _, s := range d.deps.MCP.Snapshot(ctx) {
		byName[s.Name] = s
	}
	statuses := make([]Status, 0, len(servers))
	for _, s := range servers {
		item := CheckItem{Name: s.Name}
		snap, configured := byName[s.Name]
		switch {
		case !configured:
			item.Status = StatusRed
			item.Detail = "not configured on the daemon"
		case snap.Reachable:
			item.Status = StatusGreen
			item.Detail = "reachable"
		default:
			item.Status = StatusYellow
			item.Detail = "configured but unreachable"
			if snap.Error != "" {
				item.Detail += ": " + snap.Error
			}
		}
		res.Items = append(res.Items, item)
		statuses = append(statuses, item.Status)
	}
	res.Status = WorstOf(statuses...)
	switch res.Status {
	case StatusRed:
		res.Detail = "One or more servers are not configured on the daemon."
		res.Remediation = "Add the server to the daemon MCP config, or remove it from the project."
	case StatusYellow:
		res.Detail = "All servers configured; one or more not reachable yet (may still be starting)."
		res.Remediation = "Re-run this check once the server is up; scheduled work waits for it."
	default:
		res.Detail = "All subscribed MCP servers are reachable."
	}
	return res
}

// checkModel probes the daemon's configured chat model for
// reachability (ModelPinger wraps chat.Pinger.Ping — lists models for
// HTTP, fans out to the fallback sub-provider for router, execs the
// binary for CLI). There are no per-project chat credentials, so this
// reflects daemon chat health. Red on a ping failure — a swarm cannot
// run without a reachable model backend.
func (d *Doctor) checkModel(ctx context.Context) CheckResult {
	res := CheckResult{
		Key:      "model",
		Title:    "Chat model responds",
		Required: true,
		FixHref:  "/ui/setup",
	}
	if d.deps.Model == nil {
		// No probe is available (chat disabled at the daemon level, or
		// no reachability-pingable provider). Degrade to a NON-BLOCKING
		// neutral rather than a required-unknown: an unprobeable backend
		// must not false-block a project's completeness. Ping error
		// below stays a required red — that's an actual reachability
		// failure worth blocking on.
		res.Status = StatusNeutral
		res.Required = false
		res.Detail = "No chat model backend is configured at the daemon level."
		res.Remediation = "Configure a chat model in daemon setup."
		return res
	}
	if err := d.deps.Model.Ping(ctx); err != nil {
		res.Status = StatusRed
		res.Detail = "Chat model did not respond: " + err.Error()
		res.Remediation = "Check the daemon chat endpoint / API key in setup."
		return res
	}
	res.Status = StatusGreen
	res.Detail = "Chat model backend is reachable."
	return res
}

// checkSmoke reports the last (or in-flight) smoke run. It NEVER
// triggers a run (that's TriggerSmoke, wired to an explicit button)
// and is never Required — it's a "prove it end-to-end" nicety that
// spends tokens. Neutral when no smoke has ever run for this project.
func (d *Doctor) checkSmoke(proj *registry.Project) CheckResult {
	res := CheckResult{Key: "smoke", Title: "Smoke test", Required: false}
	if d.deps.Smoke == nil {
		res.Status = StatusNeutral
		res.Detail = "Smoke runs are unavailable."
		return res
	}
	last, ok := d.deps.Smoke.Latest(proj.ID)
	if !ok {
		res.Status = StatusNeutral
		res.Detail = "Not run yet. Runs a real task (spends tokens)."
		return res
	}
	res.Status = last.Status
	res.Detail = last.Detail
	res.Meta = map[string]string{"taskId": last.TaskID}
	if last.Running {
		res.Meta["running"] = "true"
	}
	if last.USD > 0 {
		res.Meta["usd"] = fmt.Sprintf("%.4f", last.USD)
	}
	return res
}
