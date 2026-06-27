// Package a2a implements the Google Agent-to-Agent (A2A) protocol
// surface for vornik. The package contains:
//
//   - AgentCard: the on-the-wire schema at /.well-known/agent.json
//     and per-workflow card endpoints.
//   - A pure builder that derives a card from the registry state of
//     one (project, workflow) pair. No DB persistence — cards are
//     deterministic from the in-memory registry and re-render on
//     config reload.
//   - HTTP handlers (handlers.go) for the well-known card index,
//     per-agent card, task submission, and SSE result streaming.
//
// Design contract:
// https://docs.vornik.io
//
// What this package does NOT do:
//
//   - It does not replace internal/taskcreate.createDelegatedTasks.
//     A2A is a boundary protocol; internal delegation stays
//     queue-backed and untouched.
//   - It does not implement outbound A2A delegation (a2a_call
//     workflow step). That's Phase B and lands in
//     internal/executor.
//   - It does not persist agent cards. The card is a deterministic
//     view of registry state; reload regenerates it.
package a2a

import (
	"fmt"
	"strings"

	"vornik.io/vornik/internal/registry"
)

// AgentCardSpecVersion is the A2A spec revision the cards we emit
// claim to follow. Bumped when we adopt a new spec revision after
// running the contract tests against it.
const AgentCardSpecVersion = "1.0"

// AgentCard is the JSON shape served at /.well-known/agent.json
// and per-workflow card URLs. Field naming follows the A2A spec
// verbatim — `protocolVersion`, `securitySchemes`, etc. — so a
// generic A2A client can decode without a vornik-specific shim.
type AgentCard struct {
	// ProtocolVersion is the A2A spec version this card targets.
	ProtocolVersion string `json:"protocolVersion"`
	// Name is the operator-facing agent name, derived from the
	// workflow's displayName (with workflowId as fallback).
	Name string `json:"name"`
	// Description summarises the agent in one or two sentences.
	// Sourced from Workflow.Description.
	Description string `json:"description,omitempty"`
	// URL is the agent's primary endpoint — the base under which
	// /tasks live. Constructed from the daemon's public base URL
	// + the per-agent path.
	URL string `json:"url"`
	// Version is the workflow's semver — operators bump it when
	// the workflow's shape changes meaningfully.
	Version string `json:"version,omitempty"`
	// Capabilities advertises which optional protocol features
	// this agent supports. vornik's inbound surface supports
	// streaming but not push notifications in v1.
	Capabilities AgentCardCapabilities `json:"capabilities"`
	// Skills lists the discrete jobs this agent can do. Today we
	// emit one Skill per published workflow — the workflow IS the
	// skill.
	Skills []AgentCardSkill `json:"skills"`
	// SecuritySchemes describes how a caller authenticates. vornik
	// surfaces only the API-key scheme today; OAuth client-creds is
	// reserved for Phase D.
	SecuritySchemes map[string]AgentCardSecurityScheme `json:"securitySchemes,omitempty"`
	// Metadata carries vornik-specific bits that off-protocol
	// clients can ignore. Pinned under a `vornik.*` namespace so
	// the spec field stays clean.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AgentCardCapabilities is the protocol-feature advertisement
// block. All three flags follow the spec's defaults — false
// unless we explicitly support it.
type AgentCardCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// AgentCardSkill is one published capability. vornik emits one
// skill per published workflow; the skill ID is the workflow ID
// so the per-agent card URL and the skill ID stay aligned.
type AgentCardSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// AgentCardSecurityScheme describes one authentication mechanism
// a caller can use. Today vornik advertises X-API-Key only; the
// shape matches OpenAPI 3.0 security schemes which the A2A spec
// borrows verbatim.
type AgentCardSecurityScheme struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	In   string `json:"in,omitempty"`
}

// PublishedAgent pairs a project + workflow with its agent card.
// Used by the handlers to render the card index at /.well-known/
// agent.json and to route per-agent requests to the right
// workflow.
type PublishedAgent struct {
	ProjectID  string
	WorkflowID string
	Card       AgentCard
}

// BuildAgentCard renders a single AgentCard from registry state.
// The card is deterministic — same inputs always produce the
// same JSON shape, so a re-fetch after `vornikctl config reload`
// returns byte-identical content unless something actually
// changed.
//
// publicBaseURL is the operator-configured external URL of the
// daemon (e.g. https://daemon.example.com). The agent endpoint
// URL embeds this so external orchestrators can call back
// directly. An empty publicBaseURL falls back to a path-only URL
// the operator must reverse-proxy; we surface a metadata flag
// so the operator knows.
func BuildAgentCard(publicBaseURL, projectID string, wf *registry.Workflow, pushEnabled bool) (AgentCard, error) {
	if wf == nil {
		return AgentCard{}, fmt.Errorf("BuildAgentCard: workflow is nil")
	}
	if projectID == "" {
		return AgentCard{}, fmt.Errorf("BuildAgentCard: projectID is required")
	}
	name := wf.DisplayName
	if strings.TrimSpace(name) == "" {
		name = wf.ID
	}
	base := strings.TrimRight(publicBaseURL, "/")
	urlPath := agentEndpointPath(projectID, wf.ID)
	url := urlPath
	if base != "" {
		url = base + urlPath
	}
	card := AgentCard{
		ProtocolVersion: AgentCardSpecVersion,
		Name:            name,
		Description:     wf.Description,
		URL:             url,
		Version:         wf.Version,
		Capabilities: AgentCardCapabilities{
			Streaming:         true,
			PushNotifications: pushEnabled,
		},
		Skills: []AgentCardSkill{
			{
				ID:          wf.ID,
				Name:        name,
				Description: wf.Description,
				InputModes:  []string{"text"},
				OutputModes: []string{"text", "artifact"},
			},
		},
		SecuritySchemes: map[string]AgentCardSecurityScheme{
			"apiKey": {
				Type: "apiKey",
				Name: "X-API-Key",
				In:   "header",
			},
		},
		Metadata: map[string]any{
			"vornik.projectId":  projectID,
			"vornik.workflowId": wf.ID,
		},
	}
	if base == "" {
		card.Metadata["vornik.publicBaseUrlUnset"] = true
	}
	return card, nil
}

// AgentCardIndex is the body served at /.well-known/agent.json
// when the daemon publishes multiple agents. The spec allows a
// single agent at the well-known URL; for multi-agent daemons we
// extend with an `agents` array (per-spec extension under
// metadata is allowed). Clients that expect a single card see the
// first agent at the top level; spec-extended clients walk the
// `agents` array for the full list.
type AgentCardIndex struct {
	// The first published agent is duplicated at the top level so
	// single-agent clients work. When zero agents are published,
	// the embedded fields are zero-valued and `agents` is empty.
	AgentCard
	// Agents lists every published agent for clients that
	// understand the vornik extension. Always non-nil so JSON
	// emits `"agents": []` not `null`.
	Agents []AgentCard `json:"agents"`
}

// BuildAgentCardIndex assembles the top-level index from a list
// of PublishedAgents. The first agent (sort-stable by
// project/workflow IDs) is duplicated into the AgentCard
// embedded fields so single-card clients can read a useful
// answer; the full list lives under Agents.
func BuildAgentCardIndex(agents []PublishedAgent) AgentCardIndex {
	idx := AgentCardIndex{Agents: make([]AgentCard, 0, len(agents))}
	for _, a := range agents {
		idx.Agents = append(idx.Agents, a.Card)
	}
	if len(idx.Agents) > 0 {
		idx.AgentCard = idx.Agents[0]
	}
	return idx
}

// agentEndpointPath returns the URL path under which one agent's
// endpoints live: /a2a/v1/agents/<project>/<workflow>. Used by
// both the card builder (URL field) and the handlers (mux
// registration), so the literal isn't repeated.
func agentEndpointPath(projectID, workflowID string) string {
	return "/a2a/v1/agents/" + projectID + "/" + workflowID
}
