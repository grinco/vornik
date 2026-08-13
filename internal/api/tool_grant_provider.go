// Package api — grant_step_tools, exposed to worker agents via MCP.
//
// Registry design §10.1–§10.4. The project lead narrows which MCP tools a step is
// ADVERTISED, bounded by the operator-authored role ceiling.
//
// Registered under the built-in "vornik" MCP server like the document_* tools, so
// the agent's mcp-bridge picks it up from the daemon's /mcp/tools catalog with no
// entrypoint changes.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

const grantStepToolsTool = "grant_step_tools"

// ExecutionByTaskReader resolves a task's execution. Satisfied by
// persistence.ExecutionRepository.
type ExecutionByTaskReader interface {
	GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error)
}

// ToolGrantProvider serves grant_step_tools. Nil-safe throughout: an unwired
// provider advertises nothing, which leaves the ceiling as the only narrowing.
type ToolGrantProvider struct {
	Grants persistence.ExecutionToolGrantRepository
	// Executions is the NARROW read this provider needs — resolving which execution
	// and step the calling task is on. Narrow rather than the full repository so the
	// provider is testable without standing one up, and so the dependency states what
	// it actually uses.
	Executions ExecutionByTaskReader
	// Ceiling resolves the role ceiling for a task. Injected rather than reaching
	// for the registry directly so this file stays testable without one.
	Ceiling func(ctx context.Context, taskID string) []string
}

// Tools advertises grant_step_tools when the provider is wired.
//
// Deliberately ONE tool with a small schema: the point of the feature is to shrink
// the advertised surface, so a large addition to it would be self-defeating.
func (p *ToolGrantProvider) Tools(_ string) []chat.Tool {
	if p == nil || p.Grants == nil {
		return nil
	}
	return []chat.Tool{{
		Type: "function",
		Function: chat.ToolFunction{
			Name: "mcp__" + builtinMCPServer + "__" + grantStepToolsTool,
			Description: "Narrow which MCP tools a step of THIS execution is offered, to keep its " +
				"prompt small. Pass the tools that step actually needs. You can only ever narrow " +
				"within what the role is already permitted — you cannot grant a tool the role " +
				"lacks, and the request is refused if it names one. Call again with a superset to " +
				"escalate if a step turns out to need more (limited attempts per step).",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"step_id": {"type": "string", "description": "Step to scope. Defaults to the current step."},
					"tools":   {"type": "array", "items": {"type": "string"},
					            "description": "Fully-qualified tool names (mcp__server__tool) this step needs."},
					"escalation": {"type": "boolean", "description": "True when widening an earlier grant for the same step."}
				},
				"required": ["tools"]
			}`),
		},
	}}
}

// Owns reports whether a qualified name targets this provider.
func (p *ToolGrantProvider) Owns(qualifiedName string) bool {
	if p == nil || p.Grants == nil {
		return false
	}
	return qualifiedName == "mcp__"+builtinMCPServer+"__"+grantStepToolsTool
}

type grantStepToolsArgs struct {
	StepID     string   `json:"step_id"`
	Tools      []string `json:"tools"`
	Escalation bool     `json:"escalation"`
}

// Execute records a grant after checking it against the live ceiling.
//
// The agent-visible result NEVER names a refused tool: the lead's context can carry
// attacker-influenced bytes, and echoing refusals lets that text enumerate the
// ceiling one probe at a time (§10.3(4)). Refused names go to the audit row.
func (p *ToolGrantProvider) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if !p.Owns(qualifiedName) {
		return "", fmt.Errorf("not a tool-grant tool: %s", qualifiedName)
	}
	var args grantStepToolsArgs
	if strings.TrimSpace(argsJSON) != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if len(args.Tools) == 0 {
		return "", errors.New("tools is required: pass the tools this step needs (an empty " +
			"grant would leave the step with none)")
	}

	taskID, _ := ctx.Value(mcp.TaskIDHeaderKey{}).(string)
	if taskID == "" {
		return "", errors.New("grant_step_tools needs a task context; it scopes a step of the " +
			"calling execution")
	}
	if p.Executions == nil {
		return "", errors.New("execution store not wired")
	}
	exec, err := p.Executions.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil {
		return "", errors.New("no execution for this task")
	}
	stepID := strings.TrimSpace(args.StepID)
	if stepID == "" && exec.CurrentStepID != nil {
		stepID = *exec.CurrentStepID
	}
	if stepID == "" {
		return "", errors.New("cannot resolve which step to scope")
	}

	var ceiling []string
	if p.Ceiling != nil {
		ceiling = p.Ceiling(ctx, taskID)
	}

	// Escalation budget is checked BEFORE evaluating, so a refused escalation still
	// costs a slot — the limit bounds audited cycles, and a refused cycle costs the
	// same write as an accepted one.
	if args.Escalation {
		n, cerr := p.Grants.EscalationCount(ctx, exec.ID, stepID)
		if cerr == nil && n >= maxEscalationsPerStep {
			return "", fmt.Errorf("escalation limit reached for this step (%d); proceed with the "+
				"tools already granted", maxEscalationsPerStep)
		}
	}

	outcome := EvaluateToolGrant(args.Tools, ceiling)
	row := &persistence.ExecutionToolGrant{
		ExecutionID: exec.ID, ProjectID: projectID, StepID: stepID,
		RequestedTools: args.Tools,
		Accepted:       len(outcome.RefusedNames) == 0,
		RefusedTools:   outcome.RefusedNames,
		IsEscalation:   args.Escalation,
		CeilingHash:    outcome.CeilingHash,
		Actor:          "lead:" + taskID,
	}
	if rerr := p.Grants.Record(ctx, row); rerr != nil {
		// A grant that cannot be recorded must not take effect: the advertise path
		// reads the store, so an unrecorded grant would silently do nothing while
		// the lead believed it had scoped the step.
		return "", fmt.Errorf("could not record the grant: %w", rerr)
	}
	if !row.Accepted {
		return "", errors.New(outcome.Message)
	}
	return fmt.Sprintf("Scoped step %s to %d tool(s). Later steps are unaffected; call again "+
		"with escalation=true if this step turns out to need more.", stepID, len(outcome.Accepted)), nil
}
