package service

// A2A wiring helpers — adapter shims between the existing
// service-container dependencies and the narrow interfaces
// internal/conversation/a2a expects. Lives in its own file so the
// boot path stays focused on lifecycle, not the protocol-specific
// translation glue.

import (
	"context"

	a2aclient "vornik.io/vornik/internal/a2a/client"
	"vornik.io/vornik/internal/a2a/consult"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

// buildConsultProvider materialises the A2A consult-tool provider
// (mcp__consult__<peer>) from a2a.peers config, or nil when no peers are
// configured. Called at both wiring sites — the container-agent
// ComposedMCPExecutor and the dispatcher's mcpManager — so agents on either
// surface can consult domain experts. Each site gets its own instance; a task
// never spans both paths, so the per-task consult counter needn't be shared.
func (c *Container) buildConsultProvider() *consult.Provider {
	if len(c.Config.A2A.Peers) == 0 {
		return nil
	}
	// Hop-lookup is nil for v1: no v1 task is itself created via an A2A consult,
	// so inbound hops are always 0 (→ outbound 1). Wire a task-reading adapter
	// when experts-consult-experts (deeper chains) lands.
	return consult.New(c.Config.A2A.Peers, c.Config.A2A.Consult, a2aclient.New(), nil)
}

// a2aTaskCreatorAdapter wraps *taskcreate.Creator so it satisfies
// the a2a.TaskCreator interface without leaking the wider Params
// surface (idempotency, raw context, etc.) into the protocol
// package. Translation is a thin field copy.
type a2aTaskCreatorAdapter struct {
	inner *taskcreate.Creator
}

// Create translates the A2A submit payload into a taskcreate.Params
// and delegates. The CreationSource flows through verbatim so the
// audit trail records "A2A" rather than "USER" for tasks the
// protocol surface spawned.
func (a a2aTaskCreatorAdapter) Create(ctx context.Context, p a2a.TaskCreateParams) (*persistence.Task, error) {
	return a.inner.Create(ctx, taskcreate.Params{
		ProjectID:      p.ProjectID,
		WorkflowID:     p.WorkflowID,
		TaskType:       p.TaskType,
		Prompt:         p.Prompt,
		Priority:       p.Priority,
		CreationSource: p.CreationSource,
		ExtraContext:   p.ExtraContext,
	})
}
