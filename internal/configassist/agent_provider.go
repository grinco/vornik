package configassist

import (
	"context"
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// The configuration assistant's AGENT ENTRYPOINT — design §6.3.2 and §6.3.2a,
// plan §9 (WP8).
//
// NOT A SERVER. §6.3.2a settled the selfops registration question the plan had
// held this behind, and settled it by removing most of it: the daemon already
// serves agents built-in tool providers through one composed executor
// (`internal/api/mcp_composed.go`) — document tools, A2A consult, tool grants.
// This is a fourth. There is nothing to register, no address, no discovery
// protocol and no startup ordering to get wrong; the tool appears in
// Tools(projectID) when the provider is wired and the project has it open.
//
// THE MOST DANGEROUS OF THE FOUR ENTRYPOINTS, and the design says so plainly:
// an agent's prompt contains third-party text — a fetched page, an issue body,
// a PR comment — so this is a path from untrusted text to a proposed change to
// the deployment. The mitigations CONFINE that; they do not detect it:
//
//   - classes A and B2 only, enforced by the engine's GateEntrypointCeiling;
//   - never auto-applies, whatever the project's opt-in says (MayAutoApply);
//   - every call and every refusal audited here, because the composed executor
//     does NOT write a tool-audit row (§6.3.2a — verified, and a builder who
//     assumes otherwise leaves the surface with no forensic record);
//   - a trace, so the forensics are possible after an injection rather than
//     the injection being detected before one.
const agentToolName = "mcp__vornik__propose_config"

// AgentAudit records one call at the entrypoint. Narrow on purpose: this is a
// tool-call audit, not the proposal ledger, and the ledger already carries what
// was proposed.
type AgentAudit interface {
	Log(ctx context.Context, entry *persistence.ToolAuditEntry) error
}

// AgentProvider serves the agent entrypoint as a built-in MCP tool.
type AgentProvider struct {
	engine *Engine
	audit  AgentAudit
}

// NewAgentProvider wraps an engine. A nil engine yields a nil provider so the
// container can wire unconditionally and ComposedMCPExecutor's nil checks do
// the rest — the same shape Consult and Grants already use.
func NewAgentProvider(e *Engine, audit AgentAudit) *AgentProvider {
	if e == nil {
		return nil
	}
	return &AgentProvider{engine: e, audit: audit}
}

var agentToolParams = json.RawMessage(`{"type":"object","properties":{` +
	`"intent":{"type":"string","description":"The configuration change to propose, in plain language. One change."},` +
	`"context_sources":{"type":"array","items":{"type":"string"},"description":"Where this came from — URLs, issue ids, file paths you read. Recorded in the proposal's trace for a human reviewing it."}` +
	`},"required":["intent"]}`)

// Tools advertises the entrypoint. One tool, and it proposes: apply and
// rollback are NOT here and never become agent-reachable (§6.3.2a). An agent
// proposes; a human applies.
func (p *AgentProvider) Tools(_ string) []chat.Tool {
	if p == nil {
		return nil
	}
	return []chat.Tool{{
		Type: "function",
		Function: chat.ToolFunction{
			Name: agentToolName,
			Description: "Propose a configuration change to this deployment. It is FILED FOR HUMAN " +
				"REVIEW and never applied automatically. Only low-risk classes are available here; " +
				"anything touching permissions, topology, models or steering prose is refused and must " +
				"be done by an operator. Say where the request came from in context_sources.",
			Parameters: agentToolParams,
		},
	}}
}

// Owns reports whether this provider serves the named tool.
func (p *AgentProvider) Owns(qualifiedName string) bool {
	return p != nil && qualifiedName == agentToolName
}

// Execute runs one agent-originated proposal.
func (p *AgentProvider) Execute(ctx context.Context, projectID, qualifiedName, argsJSON string) (string, error) {
	if p == nil || p.engine == nil {
		return "The configuration assistant is not available on this deployment.", nil
	}
	if !p.Owns(qualifiedName) {
		return "", errNotAgentTool(qualifiedName)
	}
	var args struct {
		Intent         string   `json:"intent"`
		ContextSources []string `json:"context_sources"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	intent := strings.TrimSpace(args.Intent)
	if intent == "" {
		return "The 'intent' argument is required: say what configuration change you want proposed.", nil
	}

	// The trace. Task and execution are DAEMON-VERIFIED — the handler above
	// checked each against the task-scoped key before putting them in the
	// context — which is exactly why no step is carried: see §6.3.2a.
	taskID, _ := ctx.Value(mcp.TaskIDHeaderKey{}).(string)
	executionID, _ := ctx.Value(mcp.ExecutionIDHeaderKey{}).(string)
	trace := &Trace{TaskID: taskID, ExecutionID: executionID, ContextSources: args.ContextSources}

	res, err := p.engine.Propose(ctx, Request{
		ProjectID:  projectID,
		Intent:     intent,
		Entrypoint: EntrypointAgent,
		RequestID:  persistence.GenerateID("careq"),
		Trace:      trace,
		Actor: Actor{
			Kind: "agent",
			// The TASK is the actor, not a person. A proposal whose actor read
			// as human would be the injection's best possible disguise.
			Principal:    "task:" + taskID,
			CredentialID: "task:" + taskID,
			SourceID:     "task:" + taskID,
		},
	})
	p.record(ctx, projectID, taskID, executionID, intent, res, err)
	if err != nil {
		return "The assistant failed before it could decide anything: " + err.Error() +
			"\n\nNothing was applied.", nil
	}
	return renderAgentResult(res), nil
}

// record writes the tool-audit row for this call, including refusals.
//
// The composed executor does NOT audit (§6.3.2a, verified against the tree):
// ComposedMCPExecutor.Execute dispatches to each provider and runs only the
// ingress result guard, and the MCP handler above it validates the origin
// headers and calls the executor without logging. So a provider that assumed
// "tool calls are audited" would leave the one reach-raising entrypoint with no
// record of what it refused — on a surface whose entire safety claim is
// "confined, not detected".
func (p *AgentProvider) record(ctx context.Context, projectID, taskID, executionID, intent string, res *Result, runErr error) {
	if p.audit == nil {
		return
	}
	// Outcome uses the column's OWN vocabulary — "ok" or "error" — and nothing
	// invented. models.go is emphatic that consumers must match a class
	// positively and never infer one from absence, and a row carrying
	// "refused:ENTRYPOINT_CLASS_CEILING" in a field every reader treats as a
	// two-value enum would be a private dialect in a shared column.
	//
	// A REFUSAL IS "ok": the call completed and the entrypoint answered. What
	// it answered goes in ToolOutput, which is where a reader looks for it.
	outcome := "ok"
	if runErr != nil {
		outcome = "error"
	}
	entry := &persistence.ToolAuditEntry{
		ToolName:    agentToolName,
		ProjectID:   projectID,
		TaskID:      taskID,
		ExecutionID: executionID,
		Outcome:     outcome,
		// The INTENT is agent-supplied text, and recording it is the point:
		// after an injection it is the clearest evidence of what the agent was
		// steered to ask for.
		ToolInput:  intent,
		ToolOutput: auditOutcome(res, runErr),
	}
	if err := p.audit.Log(ctx, entry); err != nil {
		p.engine.Logger.Warn().Err(err).Msg("configassist: agent entrypoint call could not be audited")
	}
}

// auditOutcome describes what the entrypoint did, for the audit row's output
// field. Distinct from the agent-facing reply: this one names the refusal CODE,
// which is what an operator reconstructing an injection greps for.
func auditOutcome(res *Result, runErr error) string {
	switch {
	case runErr != nil:
		return "failed: " + runErr.Error()
	case res == nil:
		return "no result"
	case res.Refusal != nil:
		return "refused: " + res.Refusal.Code
	case res.Proposal != nil:
		return "filed: " + res.Proposal.ID + " class=" + res.Class
	}
	return "no proposal and no refusal"
}

func renderAgentResult(res *Result) string {
	switch {
	case res == nil:
		return "The assistant returned nothing at all. Nothing was applied."
	case res.Refusal != nil:
		return res.Refusal.Message
	case res.Proposal == nil:
		return "The assistant finished without filing anything and without refusing. Nothing was applied."
	}
	var b strings.Builder
	b.WriteString("Filed proposal " + res.Proposal.ID)
	if res.Class != "" {
		b.WriteString(" (class " + res.Class + ")")
	}
	b.WriteString(" for human review.")
	// Said explicitly because this reply goes to a MODEL, which will otherwise
	// summarise "filed" to a human as "done". Nothing an agent proposes is ever
	// applied without a person (§6.3.2).
	b.WriteString("\n\nIt has NOT been applied and will not be applied automatically. " +
		"A person reviews it in the control plane. Do not report this as a completed change.")
	return b.String()
}

// errNotAgentTool is the misroute error, kept as a function so the message
// names what was asked for.
func errNotAgentTool(name string) error {
	return &notAgentToolError{name: name}
}

type notAgentToolError struct{ name string }

func (e *notAgentToolError) Error() string {
	return "configassist: not the agent entrypoint's tool: " + e.name
}
