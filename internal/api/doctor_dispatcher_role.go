package api

import (
	"errors"
	"fmt"
	"path/filepath"

	"vornik.io/vornik/internal/registry"
)

// checkDispatcherRole validates the prerequisite for pinning
// dispatcher cost to a configured project: the chosen project's
// swarm should have a "dispatcher" role declared. The bot's chat
// client doesn't run as a containerised agent — the role is
// metadata only — but having it present means the dashboard's
// role+model aggregation rows match the swarm catalogue, instead
// of surfacing "dispatcher" as a phantom that doesn't exist
// anywhere in config.
//
// Without --fix the check prints the gap and a remediation hint.
// With --fix the doctor patches the swarm YAML to add a minimal
// stub role: name=dispatcher + model=daemon's chat model + a
// noop runtime.image so role-list parsers don't choke on a
// missing required field.
//
// Skipped silently when no telegram.dispatcher_project_id is
// configured (legacy behaviour: chat cost lands on whichever
// project is the active chat).
func (h *DoctorHandlers) checkDispatcherRole(fix bool) DoctorCheck {
	c := DoctorCheck{Name: "dispatcher_role", Status: "OK"}

	if h.dispatcherProjectID == "" {
		c.Message = "telegram.dispatcher_project_id is not set; dispatcher cost lands on the chat's active project (legacy behaviour)"
		return c
	}
	if h.configDir == "" {
		c.Status = "WARNING"
		c.Message = "config dir unknown; cannot validate swarm role"
		return c
	}

	reg := registry.New()
	// Tolerate validation errors — we want to inspect what the
	// operator's YAML says about the dispatcher project, not
	// refuse to check it because some unrelated project is
	// misconfigured.
	if err := reg.Load(h.configDir); err != nil {
		var valErr *registry.ValidationError
		if !errors.As(err, &valErr) {
			c.Status = "ERROR"
			c.Message = fmt.Sprintf("registry load failed: %v", err)
			return c
		}
	}

	project := reg.GetProject(h.dispatcherProjectID)
	if project == nil {
		c.Status = "ERROR"
		c.Message = fmt.Sprintf("telegram.dispatcher_project_id=%q is not a known project; create the project YAML or fix the typo", h.dispatcherProjectID)
		return c
	}
	swarm := reg.GetSwarm(project.SwarmID)
	if swarm == nil {
		c.Status = "ERROR"
		c.Message = fmt.Sprintf("project %q has swarmId=%q which is not loaded", project.ID, project.SwarmID)
		return c
	}

	for _, role := range swarm.Roles {
		if role.Name == "dispatcher" {
			model := role.Model
			if model == "" {
				model = "(no model override)"
			}
			c.Message = fmt.Sprintf("swarm %q has dispatcher role configured (model=%s)", swarm.ID, model)
			return c
		}
	}

	c.Status = "WARNING"
	c.Message = fmt.Sprintf("project %q (swarm=%q) is the dispatcher billing target but the swarm has no \"dispatcher\" role; dashboard role+model rows will surface a phantom dispatcher entry", project.ID, swarm.ID)
	c.Items = []string{
		fmt.Sprintf("add a dispatcher role to swarms/%s.md manually,", swarm.ID),
		"or run with --fix to patch the SWARM.md automatically",
	}

	if fix {
		swarmPath := resolveDoctorSwarmFile(h.configDir, swarm.ID)
		model := h.dispatcherChatModel
		if model == "" {
			model = "set-via-chat.model"
		}
		if err := registry.PatchSwarmAddDispatcherRole(swarmPath, model); err != nil {
			c.Items = append(c.Items, "fix failed: "+err.Error())
		} else {
			c.Status = "OK"
			c.Message = fmt.Sprintf("added dispatcher role to swarm %q (model=%s); reload the daemon to activate", swarm.ID, model)
			c.Fixed = 1
		}
	}
	return c
}

// resolveDoctorSwarmFile returns the canonical on-disk path the
// doctor --fix flow should patch for a given swarm id. As of the
// 2026-05-17 YAML removal there's only one candidate:
// `<configDir>/swarms/<id>.md`. The legacy `.yaml` / `.yml`
// fallback ladder is gone — stale YAML files are not loaded by
// the registry anyway, and patching them would have no effect.
func resolveDoctorSwarmFile(configDir, swarmID string) string {
	return filepath.Join(configDir, "swarms", swarmID+".md")
}
