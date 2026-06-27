package service

// A2A wiring helpers — adapter shims between the existing
// service-container dependencies and the narrow interfaces
// internal/conversation/a2a expects. Lives in its own file so the
// boot path stays focused on lifecycle, not the protocol-specific
// translation glue.

import (
	"context"

	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskcreate"
)

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
