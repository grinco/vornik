// Package consult exposes A2A domain experts to running agents as synthetic
// MCP tools (`mcp__consult__<peer>`). It is the agent-initiated half of the A2A
// expert federation: the dispatcher/chat agent and container workflow agents
// call a consult tool synchronously, mid-task, and get the expert's grounded
// answer back in their context.
//
// It mirrors internal/api's DocumentToolProvider (a daemon-side synthetic MCP
// provider) so it plugs into the same ComposedMCPExecutor seam that already
// serves container agents — no agent-image rebuild. See
// https://docs.vornik.io
package consult

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
)

const (
	serverName = "consult"
	toolPrefix = "mcp__" + serverName + "__"

	// Bounded per-task counter map (spam guard, not an authoritative ledger).
	maxTrackedTasks = 1000
	evictBatch      = 100
)

// TaskHopLookup returns the inbound consult-hop count for a task — the value the
// task carried when it was itself created by an A2A consult (0 for any normally
// created task). Nil-safe at the provider; used only to propagate the loop guard
// for the symmetric-peer future.
type TaskHopLookup interface {
	InboundConsultHops(ctx context.Context, taskID string) int
}

// Provider is a synthetic MCP tool provider: one `mcp__consult__<peer>` tool per
// configured a2a.peers entry, executed via the shared outbound A2A client.
type Provider struct {
	peers   map[string]config.A2APeer
	consult config.A2AConsultConfig
	client  *client.Client
	hops    TaskHopLookup // optional

	mu    sync.Mutex
	calls map[string]int // taskID -> consult count this task
	order []string       // insertion order, for bounded eviction
}

// New builds a Provider. hops may be nil (hop propagation degrades to 1).
func New(peers map[string]config.A2APeer, consult config.A2AConsultConfig, c *client.Client, hops TaskHopLookup) *Provider {
	return &Provider{peers: peers, consult: consult, client: c, hops: hops, calls: map[string]int{}}
}

var consultParams = json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"The question to ask the domain expert. One clear question."}},"required":["question"]}`)

// Tools materialises one consult tool per configured peer (deterministic order).
// The description comes from the peer's config (agent-card enrichment deferred).
func (p *Provider) Tools(_ string) []chat.Tool {
	if p == nil || len(p.peers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.peers))
	for k := range p.peers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]chat.Tool, 0, len(keys))
	for _, k := range keys {
		desc := strings.TrimSpace(p.peers[k].Description)
		if desc == "" {
			desc = fmt.Sprintf("Consult the '%s' domain expert over A2A: ask one question, get a grounded, cited answer fresh from that domain's source of truth.", k)
		}
		out = append(out, chat.Tool{
			Type: "function",
			Function: chat.ToolFunction{
				Name:        toolPrefix + k,
				Description: desc,
				Parameters:  consultParams,
			},
		})
	}
	return out
}

// Owns reports whether qualifiedName is a consult tool (for ComposedMCPExecutor
// routing).
func (p *Provider) Owns(qualifiedName string) bool {
	return p != nil && strings.HasPrefix(qualifiedName, toolPrefix)
}

// Execute runs one consult. Operational failures (empty question, budget,
// unreachable expert) return a clean STRING result the agent can read and react
// to; only structural bugs return a Go error.
func (p *Provider) Execute(ctx context.Context, _ /*projectID*/, qualifiedName, argsJSON string) (string, error) {
	if p == nil {
		return "", errors.New("consult provider is nil")
	}
	if !p.Owns(qualifiedName) {
		return "", fmt.Errorf("consult: not a consult tool: %s", qualifiedName)
	}
	key := strings.TrimPrefix(qualifiedName, toolPrefix)
	peer, ok := p.peers[key]
	if !ok {
		return fmt.Sprintf("No consult peer configured named %q.", key), nil
	}

	var args struct {
		Question string `json:"question"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)
	q := strings.TrimSpace(args.Question)
	if q == "" {
		return "The 'question' argument is required.", nil
	}

	taskID, _ := ctx.Value(mcp.TaskIDHeaderKey{}).(string)
	if taskID != "" && !p.allowCall(taskID) {
		return fmt.Sprintf("Consult budget exhausted for this task (max %d). Proceed with what you have.", p.consult.EffectiveMaxCallsPerTask()), nil
	}

	inbound := 0
	if p.hops != nil && taskID != "" {
		inbound = p.hops.InboundConsultHops(ctx, taskID)
	}

	res, err := p.client.Call(ctx, client.CallRequest{
		AgentURL: peer.URL,
		APIKey:   peer.APIKey,
		Text:     q,
		Metadata: map[string]any{config.ConsultHopHeader: inbound + 1},
		Timeout:  p.consult.EffectiveTimeout(),
	})
	if err != nil {
		// Do NOT let the caller quietly substitute local memory — that is how a
		// stale RAG answer got served when the consult timed out (task …470f).
		return fmt.Sprintf("Consult of %s did not return an answer (%s). Do NOT answer from your own memory as a substitute — it may be stale. Tell the user the %s expert could not be reached in time and offer to retry.", key, err.Error(), key), nil
	}
	answer := strings.TrimSpace(res.Answer)
	if answer == "" {
		return fmt.Sprintf("Consulted %s (A2A) but it returned no answer.", key), nil
	}
	// Provenance prefix (best-effort staleness hygiene; see design §7).
	return fmt.Sprintf("Consulted %s (A2A): %s", key, answer), nil
}

// allowCall increments the per-task consult counter and reports whether the call
// is within EffectiveMaxCallsPerTask. Bounded map: evict the oldest keys when
// full — a spam guard, not an exact ledger.
func (p *Provider) allowCall(taskID string) bool {
	limit := p.consult.EffectiveMaxCallsPerTask()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, seen := p.calls[taskID]; !seen {
		if len(p.order) >= maxTrackedTasks {
			for _, old := range p.order[:evictBatch] {
				delete(p.calls, old)
			}
			p.order = append(p.order[:0], p.order[evictBatch:]...)
		}
		p.order = append(p.order, taskID)
	}
	if p.calls[taskID] >= limit {
		return false
	}
	p.calls[taskID]++
	return true
}
