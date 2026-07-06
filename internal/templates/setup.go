package templates

import (
	"fmt"
	"strings"
)

// SetupSpec is the template-declared prerequisite contract
// (project-creation-e2e-design §1b). Phase 1 only parses and
// validates it; the Phase 2 project doctor consumes it, and the
// gallery shows Summary() as "Needs:" chips so users see
// prerequisites before committing. Secrets are NEVER rendered into
// files — they are referenced by name and verified post-creation.
type SetupSpec struct {
	Secrets    []SetupSecret    `yaml:"secrets,omitempty"`
	MCPServers []SetupMCPServer `yaml:"mcpServers,omitempty"`
	// Model is "" (no requirement) or "required" (a working chat
	// model must respond before the project is considered ready).
	Model     string         `yaml:"model,omitempty"`
	SmokeTask *SmokeTaskSpec `yaml:"smokeTask,omitempty"`
	// Checks names the doctor checks this template opts into.
	// Closed set: secrets | mcp_reachable | model_ping |
	// schedule_armed | smoke.
	Checks []string `yaml:"checks,omitempty"`
}

// SetupSecret declares one secret the materialised project expects
// in the secrets store.
type SetupSecret struct {
	Name     string `yaml:"name"`
	Label    string `yaml:"label,omitempty"`
	Required bool   `yaml:"required,omitempty"`
}

// SetupMCPServer declares an MCP server the project expects the
// daemon to have configured. Name may reference template params
// (e.g. "{{.mcpServer}}") — resolution happens at doctor time.
type SetupMCPServer struct {
	Name     string `yaml:"name"`
	Hint     string `yaml:"hint,omitempty"`
	Required bool   `yaml:"required,omitempty"`
}

// SmokeTaskSpec is the goal the doctor's explicit smoke button
// fires as a real task.
type SmokeTaskSpec struct {
	Goal string `yaml:"goal"`
}

// knownSetupChecks is the closed check-name set from the spec's
// doctor table.
var knownSetupChecks = map[string]struct{}{
	"secrets": {}, "mcp_reachable": {}, "model_ping": {},
	"schedule_armed": {}, "smoke": {},
}

// validateSetup enforces the setup: block's invariants at Load
// time so a bad manifest fails daemon startup, not doctor render.
func validateSetup(s *SetupSpec) error {
	if s == nil {
		return nil
	}
	for i, sec := range s.Secrets {
		if strings.TrimSpace(sec.Name) == "" {
			return fmt.Errorf("setup.secrets[%d]: name is required", i)
		}
	}
	for i, srv := range s.MCPServers {
		if strings.TrimSpace(srv.Name) == "" {
			return fmt.Errorf("setup.mcpServers[%d]: name is required", i)
		}
	}
	if s.Model != "" && s.Model != "required" {
		return fmt.Errorf("setup.model must be empty or %q, got %q", "required", s.Model)
	}
	if s.SmokeTask != nil && strings.TrimSpace(s.SmokeTask.Goal) == "" {
		return fmt.Errorf("setup.smokeTask.goal must be non-empty when smokeTask is declared")
	}
	for _, c := range s.Checks {
		if _, ok := knownSetupChecks[c]; !ok {
			return fmt.Errorf("setup.checks: unknown check %q (expected secrets|mcp_reachable|model_ping|schedule_armed|smoke)", c)
		}
	}
	return nil
}

// Summary renders short operator-facing prerequisite chips for the
// gallery card ("Needs: GitHub token", "Needs: MCP server slack",
// "Needs: working chat model").
func (s *SetupSpec) Summary() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Secrets)+len(s.MCPServers)+1)
	for _, sec := range s.Secrets {
		label := sec.Label
		if label == "" {
			label = sec.Name
		}
		out = append(out, "Needs: "+label)
	}
	for _, srv := range s.MCPServers {
		out = append(out, "Needs: MCP server "+srv.Name)
	}
	if s.Model == "required" {
		out = append(out, "Needs: working chat model")
	}
	return out
}
