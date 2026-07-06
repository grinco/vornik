package projectwizard

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/registry"
)

type mcpServerApplier struct{ known map[string]bool }

type mcpServerArgs struct {
	Name         string   `json:"name"`
	AllowedTools []string `json:"allowed_tools"`
}

func (a mcpServerApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in mcpServerArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "mcp_server", Field: "args", Message: err.Error()}
	}
	if in.Name == "" {
		return &ComposeError{AddonType: "mcp_server", Field: "name", Message: "required"}
	}
	if !a.known[in.Name] {
		return &ComposeError{AddonType: "mcp_server", Field: "name",
			Message: fmt.Sprintf("no MCP server %q is configured on the daemon", in.Name)}
	}
	for _, s := range cp.Project.MCP.Servers {
		if s.Name == in.Name {
			return &ComposeError{AddonType: "mcp_server", Field: "name",
				Message: fmt.Sprintf("server %q already attached", in.Name)}
		}
	}
	cp.Project.MCP.Servers = append(cp.Project.MCP.Servers, registry.MCPServerConfig{
		Name:         in.Name,
		AllowedTools: in.AllowedTools,
	})
	return nil
}

type scheduleApplier struct{}

type scheduleArgs struct {
	Interval string `json:"interval"`
	Goal     string `json:"goal"`
	TaskType string `json:"task_type"`
}

func (a scheduleApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in scheduleArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "schedule", Field: "args", Message: err.Error()}
	}
	dur, err := time.ParseDuration(in.Interval)
	if err != nil || dur <= 0 {
		return &ComposeError{AddonType: "schedule", Field: "interval",
			Message: fmt.Sprintf("%q is not a positive Go duration", in.Interval)}
	}
	if in.Goal == "" {
		return &ComposeError{AddonType: "schedule", Field: "goal",
			Message: "required (cron mode fires the goal verbatim each tick)"}
	}
	taskType := in.TaskType
	if taskType == "" {
		taskType = "task"
	}
	if cp.Project.Autonomy.Enabled {
		return &ComposeError{AddonType: "schedule", Field: "schedule",
			Message: "project already has an autonomy schedule; only one schedule/rag_source addon (or a base template's own schedule) is allowed"}
	}
	cp.Project.Autonomy.Enabled = true
	cp.Project.Autonomy.Mode = registry.AutonomyModeCron
	cp.Project.Autonomy.Goal = in.Goal
	cp.Project.Autonomy.PollInterval = in.Interval
	cp.Project.Autonomy.CronTaskType = taskType
	if len(cp.Project.Autonomy.AllowedTaskTypes) == 0 {
		cp.Project.Autonomy.AllowedTaskTypes = []string{taskType}
	}
	// Identical-prompt cron ticks must not be deduped by the completion
	// window; 0 disables it (matches the report-pipeline template).
	cp.Project.Autonomy.DuplicateWindow = "0"
	return nil
}

type ragSourceApplier struct{}

type ragSourceArgs struct {
	Source  string `json:"source"`
	Cadence string `json:"cadence"`
}

func (a ragSourceApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in ragSourceArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "rag_source", Field: "args", Message: err.Error()}
	}
	if in.Source == "" {
		return &ComposeError{AddonType: "rag_source", Field: "source", Message: "required"}
	}
	// The source is concatenated verbatim into the autonomy goal text as a
	// bullet line; a newline/control char would let it break out of that
	// line and corrupt the goal.
	if strings.ContainsAny(in.Source, "\n\r\t") {
		return &ComposeError{AddonType: "rag_source", Field: "source",
			Message: "must not contain newlines or control characters"}
	}
	dur, err := time.ParseDuration(in.Cadence)
	if err != nil || dur <= 0 {
		return &ComposeError{AddonType: "rag_source", Field: "cadence",
			Message: fmt.Sprintf("%q is not a positive Go duration", in.Cadence)}
	}
	// rag_source owns the llm autonomy mode and is additive: multiple
	// rag_source addons accumulate their sources into one llm-mode goal.
	// The only conflict is a DIFFERENT mode already owning the block (e.g.
	// a schedule addon's cron mode, or a base template's own cron
	// schedule) — last-wins there would leave an incoherent block.
	if cp.Project.Autonomy.Enabled && cp.Project.Autonomy.Mode != registry.AutonomyModeLLM {
		return &ComposeError{AddonType: "rag_source", Field: "source",
			Message: "conflicts with the project's existing cron/non-llm schedule; use one autonomy style"}
	}
	// All rag_source addons on one project share a single PollInterval, so
	// a second addon with a DIFFERING cadence would silently clobber the
	// first's cadence (last-wins) while still claiming to track both
	// sources — reject instead of picking one arbitrarily.
	if cp.Project.Autonomy.Enabled && cp.Project.Autonomy.Mode == registry.AutonomyModeLLM &&
		cp.Project.Autonomy.PollInterval != "" && in.Cadence != cp.Project.Autonomy.PollInterval {
		return &ComposeError{AddonType: "rag_source", Field: "cadence",
			Message: fmt.Sprintf("all rag_source addons on one project must share a cadence; got %q after %q",
				in.Cadence, cp.Project.Autonomy.PollInterval)}
	}
	// rag_source enables llm-mode freshness-tracking autonomy and records
	// the source in the goal. Full fetch→distill workflow grafting is out
	// of scope for the addon (a base template with a research/ingest role
	// carries that); the addon sets the intent + cadence so the doctor's
	// schedule check arms and the autonomy loop tracks the source.
	cp.Project.Autonomy.Enabled = true
	cp.Project.Autonomy.Mode = registry.AutonomyModeLLM
	cp.Project.Autonomy.PollInterval = in.Cadence
	line := "- Keep project memory current on: " + in.Source
	if cp.Project.Autonomy.Goal == "" {
		cp.Project.Autonomy.Goal = "Track and ingest documentation sources:\n" + line
	} else {
		cp.Project.Autonomy.Goal += "\n" + line
	}
	return nil
}

type chatToolsApplier struct{}

type chatToolsArgs struct {
	AllowedTools []string `json:"allowed_tools"`
}

func (a chatToolsApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in chatToolsArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "chat_tools", Field: "args", Message: err.Error()}
	}
	if len(in.AllowedTools) == 0 {
		return &ComposeError{AddonType: "chat_tools", Field: "allowed_tools", Message: "at least one tool required"}
	}
	have := map[string]bool{}
	for _, t := range cp.Project.Permissions.AllowedTools {
		have[t] = true
	}
	for _, t := range in.AllowedTools {
		if t == "" || have[t] {
			continue
		}
		have[t] = true
		cp.Project.Permissions.AllowedTools = append(cp.Project.Permissions.AllowedTools, t)
	}
	return nil
}

type rolePromptAppendApplier struct{}

type rolePromptAppendArgs struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

func (a rolePromptAppendApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in rolePromptAppendArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "role_prompt_append", Field: "args", Message: err.Error()}
	}
	if in.Text == "" {
		return &ComposeError{AddonType: "role_prompt_append", Field: "text", Message: "required"}
	}
	if cp.Swarm == nil {
		return &ComposeError{AddonType: "role_prompt_append", Field: "role",
			Message: "no swarm to edit"}
	}
	for i := range cp.Swarm.Roles {
		if cp.Swarm.Roles[i].Name == in.Role {
			if cp.Swarm.Roles[i].SystemPrompt == "" {
				cp.Swarm.Roles[i].SystemPrompt = in.Text
			} else {
				cp.Swarm.Roles[i].SystemPrompt += "\n\n" + in.Text
			}
			return nil
		}
	}
	return &ComposeError{AddonType: "role_prompt_append", Field: "role",
		Message: fmt.Sprintf("role %q does not exist in the swarm", in.Role)}
}

type secretRequirementApplier struct{}

type secretRequirementArgs struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

func (a secretRequirementApplier) Apply(cp *composedProject, args json.RawMessage) error {
	var in secretRequirementArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return &ComposeError{AddonType: "secret_requirement", Field: "args", Message: err.Error()}
	}
	if in.Name == "" {
		return &ComposeError{AddonType: "secret_requirement", Field: "name", Message: "required"}
	}
	for _, s := range cp.Project.Permissions.Secrets {
		if s == in.Name {
			return nil // idempotent
		}
	}
	cp.Project.Permissions.Secrets = append(cp.Project.Permissions.Secrets, in.Name)
	cp.Secrets = append(cp.Secrets, DeclaredSecret(in))
	return nil
}
