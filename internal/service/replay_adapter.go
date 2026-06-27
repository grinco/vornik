package service

import (
	"context"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/replay"
)

// adminAuditOrNil returns the container's admin-audit repo when
// wired, nil otherwise. Used to plumb fork audit logging without
// crashing on bare-bones deployments where admin_audit isn't set
// up (e.g. unit tests + minimal local installs).
func adminAuditOrNil(c *Container) persistence.AdminAuditRepository {
	if c == nil || c.repos == nil {
		return nil
	}
	return c.repos.AdminAudit
}

// forkExecutorAdapter satisfies api.ForkExecutor by wrapping a
// *replay.Forker. The two packages don't share types (replay's
// own ForkRequest/ForkResult; the api surface has its own
// JSON-shaped types) so the adapter does the field-by-field
// translation in one place. Keeps the api package free of an
// import on replay and lets the JSON contract evolve
// independently of the internal type.
type forkExecutorAdapter struct {
	forker *replay.Forker
}

func newForkExecutorAdapter(f *replay.Forker) api.ForkExecutor {
	if f == nil {
		return nil
	}
	return &forkExecutorAdapter{forker: f}
}

func (a *forkExecutorAdapter) Fork(ctx context.Context, sourceExecutionID string, req api.ForkExecutorRequest) (*api.ForkExecutorResult, error) {
	res, err := a.forker.Fork(ctx, sourceExecutionID, replay.ForkRequest{
		StepID:         req.StepID,
		PromptOverride: req.PromptOverride,
	})
	if err != nil {
		return nil, err
	}
	return &api.ForkExecutorResult{
		TaskID: res.TaskID,
		URL:    res.URL,
	}, nil
}
