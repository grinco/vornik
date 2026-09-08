package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Recording a completed CI run, and deciding what it triggers
// (2026-09-08-forge-ci-outcomes-design.md §4, §5).
//
// The RUN IS ALWAYS RECORDED. Whether it also starts work is a separate
// decision, and the two are kept apart on purpose: a green run that triggers
// nothing must still be readable by the next review, and an outcome nobody
// acted on is exactly the evidence an operator wants when asking why a review
// said nothing about a failing pipeline.

// createFromCIOutcome is the CI branch of Create.
//
// Records the run, then applies §5's table. "Record and enqueue nothing" is the
// common case — every green run without a success workflow, every non-PR
// failure — and it returns nil, because nothing went wrong.
func (g *githubTaskCreator) createFromCIOutcome(ctx context.Context, ev github.TaskCreationEvent) error {
	if g.project == nil {
		return errors.New("github task creator: project is not configured")
	}
	if g.ci == nil {
		// The kind reached us with ingestion unconfigured. Ack rather than
		// error: the channel gates on the same flag, so this is a wiring
		// mismatch, not a delivery problem, and failing the webhook would make
		// GitHub retry something that will never succeed.
		g.logger.Debug().Str("repo", ev.Repo).
			Msg("github task creator: CI ingestion not configured; acking the run")
		return nil
	}

	out := g.ci.Record(ctx, forgeci.Run{
		ProjectID:    g.project.ID,
		Repo:         ev.Repo,
		RunID:        ev.CI.RunID,
		HeadSHA:      ev.CI.HeadSHA,
		Number:       ev.Number,
		Conclusion:   ev.CI.Conclusion,
		WorkflowName: ev.CI.WorkflowName,
		WorkflowPath: ev.CI.WorkflowPath,
	})
	d := forgeci.Decide(out, g.ci.Cfg())
	if !d.Enqueue {
		g.logger.Info().
			Str("repo", ev.Repo).Int64("run_id", ev.CI.RunID).
			Str("conclusion", ev.CI.Conclusion).Str("reason", d.Reason).
			Msg("github task creator: CI outcome recorded, no work enqueued")
		return nil
	}

	// A FAILURE enqueues a review, and goes through the shared coordinator so a
	// CI storm coalesces exactly as a push burst does. A GREEN run fires its
	// configured workflow per run and is deliberately NOT coalesced: a deposit
	// is about that run's output, and collapsing several would lose the thing
	// being deposited (design §5).
	if d.WorkflowID == "" && g.review != nil {
		if dec := g.review.Decide(ctx, g.project.ID, *forgeJobFromEvent(ev), false); dec.Skip {
			g.logger.Info().
				Str("repo", ev.Repo).Int("number", ev.Number).Str("reason", dec.Reason).
				Msg("github task creator: CI-triggered review coalesced onto an in-flight one")
			return nil
		}
	}

	return g.createCITask(ctx, ev, out, d)
}

// createCITask persists the task a triggering CI outcome produces.
//
// Mirrors Create's persistence half rather than reusing it, because that path
// resolves a workflow from the KIND and this one has already resolved it from
// the DECISION — a green run's success workflow is not the CI workflow.
func (g *githubTaskCreator) createCITask(ctx context.Context, ev github.TaskCreationEvent,
	out *persistence.ForgeCIOutcome, d forgeci.Decision) error {
	workflowID := d.WorkflowID
	if workflowID == "" {
		workflowID = g.project.GitHubApp.EffectiveCIWorkflowID(
			g.project.GitHubApp.EffectivePRReviewWorkflowID(g.project.DefaultWorkflowID))
	}

	payload, err := marshalGitHubTaskPayload(ev, ciOutcomeTaskType, g.project.DefaultPriority, workflowID)
	if err != nil {
		return fmt.Errorf("marshal github ci task payload: %w", err)
	}
	// The run's identifiers ride in the payload context so a workflow step can
	// find the outcome. A REFERENCE only — never the artifact excerpt, which is
	// read through forge.fetch_ci so there is one wrapping site (design §5.1).
	payload, err = mergeCIContext(payload, forgeci.Context(out))
	if err != nil {
		return fmt.Errorf("merge ci context: %w", err)
	}

	now := time.Now()
	task := &persistence.Task{
		ID:             persistence.GenerateID("task"),
		ProjectID:      g.project.ID,
		CreationSource: persistence.TaskCreationSourceUser,
		Status:         persistence.TaskStatusQueued,
		Priority:       g.project.DefaultPriority,
		Payload:        payload,
		Attempt:        1,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if workflowID != "" {
		wf := workflowID
		task.WorkflowID = &wf
	}
	if ev.IdempotencyKey != "" {
		k := ev.IdempotencyKey
		task.IdempotencyKey = &k
	}

	if err := g.taskRepo.Create(ctx, task); err != nil {
		if ev.IdempotencyKey != "" {
			if existing, getErr := g.taskRepo.GetByIdempotencyKey(ctx, g.project.ID, ev.IdempotencyKey); getErr == nil && existing != nil {
				return nil
			}
		}
		return fmt.Errorf("create github ci task: %w", err)
	}

	// Only a review claims the PR. A green run's deposit workflow is not a
	// review and must not take the claim, or the next push would coalesce onto
	// a task that never reviews anything.
	if d.WorkflowID == "" && g.review != nil && ev.Number > 0 {
		g.review.Claim(ctx, g.project.ID, *forgeJobFromEvent(ev), task.ID)
	}

	g.logger.Info().
		Str("project_id", g.project.ID).Str("task_id", task.ID).
		Str("repo", ev.Repo).Int("number", ev.Number).
		Int64("run_id", ev.CI.RunID).Str("conclusion", ev.CI.Conclusion).
		Str("workflow_id", workflowID).Str("reason", d.Reason).
		Msg("github task creator: CI outcome enqueued work")
	return nil
}

// mergeCIContext folds the run's identifiers into the payload's context map.
func mergeCIContext(payload []byte, extra map[string]string) ([]byte, error) {
	if len(extra) == 0 {
		return payload, nil
	}
	var p githubTaskPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	if p.Context == nil {
		p.Context = map[string]string{}
	}
	for k, v := range extra {
		p.Context[k] = v
	}
	return json.Marshal(p)
}

// ciIngestForProject builds the CI ingest for one project, or nil when the
// project has not enabled it.
//
// Nil is the DEFAULT and is load-bearing: CI ingestion needs a GitHub App
// permission the operator must grant and each repository owner must re-accept,
// so a daemon that upgrades must behave exactly as before until someone asks
// for the feature.
func (c *Container) ciIngestForProject(p *registry.Project) *forgeci.Ingest {
	if p == nil || !p.Forge.CI.Enabled {
		return nil
	}
	if c.repos == nil || c.repos.ForgeCIOutcomes == nil {
		c.Logger.Warn().Str("project_id", p.ID).
			Msg("forge CI ingestion enabled but no outcome store is wired; runs will not be recorded")
		return nil
	}
	ci := p.Forge.CI
	ci.CIDefaults()
	cfg := forgeci.Config{
		ArtifactName:      ci.ArtifactName,
		MaxArtifactBytes:  ci.MaxArtifactBytes,
		MaxExcerptBytes:   ci.MaxExcerptBytes,
		ReviewOnFailure:   ci.ReviewOnFailure,
		SuccessWorkflowID: ci.SuccessWorkflowID,
	}
	logger := c.Logger.With().Str("component", "forge_ci").Str("project_id", p.ID).Logger()

	// The provider is OPTIONAL: without a CIReader the webhook's conclusion is
	// still recorded, which is the whole trigger decision. Only the per-job
	// detail and the artifact are lost.
	fcfg, ok := p.ResolveForge()
	if !ok {
		return forgeci.New(c.repos.ForgeCIOutcomes, nil, cfg, logger)
	}
	prov, err := forge.New(fcfg)
	if err != nil {
		c.Logger.Warn().Err(err).Str("project_id", p.ID).
			Msg("forge provider unavailable; recording webhook conclusions only")
		return forgeci.New(c.repos.ForgeCIOutcomes, nil, cfg, logger)
	}
	reader, _ := prov.(forge.CIReader)
	if reader == nil {
		c.Logger.Info().Str("project_id", p.ID).Str("provider", prov.Name()).
			Msg("forge provider cannot read CI outcomes; recording webhook conclusions only")
	}
	return forgeci.New(c.repos.ForgeCIOutcomes, reader, cfg, logger)
}
