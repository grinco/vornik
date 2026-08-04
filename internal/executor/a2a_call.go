package executor

// Outbound A2A client step. The `a2a_call` workflow step type lets a
// vornik workflow delegate to a third-party A2A-compliant agent (vendor
// scraper, partner specialist, another vornik daemon's published
// workflow) without a custom dispatcher shim.
//
// The protocol mechanics — submit, SSE stream, terminal resolution —
// live in the shared internal/a2a/client package so the agent-initiated
// consult tools reuse the exact same client (a2a-expert-federation-
// design §5). This step is a thin adapter: it maps the workflow step's
// frontmatter onto a client.CallRequest and the client's result back
// onto a2aCallResult.
//
// What this step DOES NOT do (deferred):
//   - Schema validation against step.Expect.
//   - Live-pubsub bridge of the partner's progress into the operator UI.
//   - input-required → checkpoint (Phase C); for now it surfaces as an
//     error so on_fail fires.
//   - Outbound auth via the secrets store; for v1 the step reads an env
//     var named by step.APIKeyEnv, keeping secrets out of the YAML.
//     (The consult tools resolve keys from the secrets store instead.)

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/registry"
)

// a2aClient is the package-shared outbound A2A client, so chained
// a2a_call steps keep the connection pool warm.
var a2aClient = client.New()

// a2aCallResult is the structured output the step writes to
// state.LastResult on success. JSON-encoded so downstream steps + gates
// can read fields directly.
type a2aCallResult struct {
	TaskID       string `json:"taskId"`
	State        string `json:"state"`
	Text         string `json:"text,omitempty"`
	PartnerAgent string `json:"partner_agent"`
}

// handleA2ACallStep runs one a2a_call step end-to-end. Returns the final
// result + nil on success, or the result + an error to fire step.OnFail.
func (e *Executor) handleA2ACallStep(ctx context.Context, stepID string, step *registry.WorkflowStep) (*a2aCallResult, error) {
	if step == nil {
		return nil, fmt.Errorf("a2a_call %s: step is nil", stepID)
	}
	if strings.TrimSpace(step.AgentURL) == "" {
		return nil, fmt.Errorf("a2a_call %s: agent_url is required", stepID)
	}
	if strings.TrimSpace(step.Prompt) == "" {
		return nil, fmt.Errorf("a2a_call %s: prompt is required", stepID)
	}

	timeout := time.Duration(0)
	if step.Timeout != "" {
		if d, err := time.ParseDuration(step.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}
	apiKey := ""
	if step.APIKeyEnv != "" {
		apiKey = strings.TrimSpace(os.Getenv(step.APIKeyEnv))
	}

	res, err := a2aClient.Call(ctx, client.CallRequest{
		AgentURL: step.AgentURL,
		APIKey:   apiKey,
		Text:     step.Prompt,
		Timeout:  timeout,
	})
	out := &a2aCallResult{
		TaskID:       res.TaskID,
		State:        res.State,
		Text:         res.Answer,
		PartnerAgent: res.PartnerAgent,
	}
	if err != nil {
		return out, fmt.Errorf("a2a_call %s: %w", stepID, err)
	}
	return out, nil
}
