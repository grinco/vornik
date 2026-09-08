package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"vornik.io/vornik/internal/forge"
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

// ciIngest is the CI half of the task creator's dependencies. Nil-safe
// throughout: a deployment without CI ingestion configured has neither, and
// must behave exactly as it did before this feature existed.
type ciIngest struct {
	outcomes persistence.ForgeCIOutcomeRepository
	reader   forge.CIReader
	cfg      ciIngestConfig
}

// ciIngestConfig is the resolved per-project CI configuration (design §8).
type ciIngestConfig struct {
	ArtifactName      string
	MaxArtifactBytes  int64
	MaxExcerptBytes   int
	ReviewOnFailure   bool
	SuccessWorkflowID string
}

// recordCIOutcome fetches the run's full detail and stores it.
//
// Best-effort by design: a recording failure must not stop the trigger
// decision. The event already carries the conclusion, which is what the
// decision reads — losing the stored row costs a later review its context, not
// this delivery its behaviour.
func (g *githubTaskCreator) recordCIOutcome(ctx context.Context, ev github.TaskCreationEvent) *persistence.ForgeCIOutcome {
	if g.ci == nil || g.ci.outcomes == nil || ev.CI == nil {
		return nil
	}

	now := time.Now().UTC()
	out := &persistence.ForgeCIOutcome{
		ProjectID:    g.project.ID,
		Repo:         ev.Repo,
		RunID:        ev.CI.RunID,
		HeadSHA:      ev.CI.HeadSHA,
		Number:       ev.Number,
		WorkflowName: ev.CI.WorkflowName,
		WorkflowPath: ev.CI.WorkflowPath,
		RunAttempt:   1,
		Conclusion:   ev.CI.Conclusion,
		CompletedAt:  now,
		RecordedAt:   now,
	}

	// The provider fills in what the webhook does not carry: per-job
	// conclusions, the run's real timing, and the artifact.
	if g.ci.reader != nil {
		if run, err := g.ci.reader.FetchCIRun(ctx, ev.Repo, ev.CI.RunID); err == nil {
			applyCIRun(out, run)
		} else {
			g.logger.Warn().Err(err).
				Str("repo", ev.Repo).Int64("run_id", ev.CI.RunID).
				Msg("github task creator: CI run detail unavailable; recording the conclusion alone")
		}
		g.attachCIArtifact(ctx, out, ev)
	}

	if err := g.ci.outcomes.Upsert(ctx, out); err != nil {
		g.logger.Warn().Err(err).
			Str("repo", ev.Repo).Int64("run_id", ev.CI.RunID).
			Msg("github task creator: CI outcome upsert failed")
	}
	return out
}

// applyCIRun copies the provider's detail onto the row, keeping the webhook's
// values wherever the API returned nothing.
func applyCIRun(out *persistence.ForgeCIOutcome, run forge.CIRun) {
	if run.Conclusion != "" {
		out.Conclusion = run.Conclusion
	}
	if run.HeadSHA != "" {
		out.HeadSHA = run.HeadSHA
	}
	if run.WorkflowPath != "" {
		out.WorkflowPath = run.WorkflowPath
	}
	if run.WorkflowName != "" {
		out.WorkflowName = run.WorkflowName
	}
	if run.RunAttempt > 0 {
		out.RunAttempt = run.RunAttempt
	}
	if !run.StartedAt.IsZero() {
		out.StartedAt = run.StartedAt
	}
	if !run.CompletedAt.IsZero() {
		out.CompletedAt = run.CompletedAt
	}
	// The webhook's pull request wins when the API has none, and vice versa:
	// either source may be the one that saw it.
	if out.Number == 0 && run.Number > 0 {
		out.Number = run.Number
	}
	for _, j := range run.Jobs {
		out.Jobs = append(out.Jobs, persistence.ForgeCIJob{
			Name: j.Name, Status: j.Status, Conclusion: j.Conclusion,
		})
	}
}

// attachCIArtifact downloads the configured artifact, if the run uploaded one.
//
// Every failure here is a WARNING and leaves the excerpt empty. A missing
// artifact is a fact about the pipeline, not a failure of the ingestion, and an
// operator reading a review needs the conclusions either way.
func (g *githubTaskCreator) attachCIArtifact(ctx context.Context, out *persistence.ForgeCIOutcome, ev github.TaskCreationEvent) {
	name := g.ci.cfg.ArtifactName
	if name == "" {
		return // content is opt-in; conclusions only
	}
	arts, err := g.ci.reader.ListCIArtifacts(ctx, ev.Repo, ev.CI.RunID)
	if err != nil {
		g.logger.Warn().Err(err).Str("repo", ev.Repo).
			Msg("github task creator: artifact listing failed")
		return
	}
	var match *forge.CIArtifact
	for i := range arts {
		if arts[i].Name == name && !arts[i].Expired {
			match = &arts[i]
			break
		}
	}
	if match == nil {
		g.logger.Debug().Str("artifact", name).Str("repo", ev.Repo).
			Msg("github task creator: run uploaded no such artifact; recording conclusions only")
		return
	}

	data, err := g.ci.reader.FetchCIArtifact(ctx, ev.Repo, match.ID, g.ci.cfg.MaxArtifactBytes)
	if err != nil {
		// A too-large artifact is reported at INFO rather than swallowed: the
		// operator's response is to raise the cap or upload less, and they can
		// only do that if they know it happened.
		if errors.Is(err, forge.ErrCIArtifactTooLarge) {
			g.logger.Info().Err(err).Str("artifact", name).
				Msg("github task creator: artifact exceeds the configured ceiling; not stored")
			return
		}
		g.logger.Warn().Err(err).Str("artifact", name).
			Msg("github task creator: artifact download failed")
		return
	}

	excerpt, truncated := capExcerpt(string(data), g.ci.cfg.MaxExcerptBytes)
	out.ArtifactExcerpt = excerpt
	out.ArtifactBytes = len(excerpt)
	out.ArtifactTruncated = truncated
}

// capExcerpt trims to the byte cap, reporting whether it had to.
//
// Truncation is RETURNED, never inferred by a caller comparing lengths: a
// truncated plan that reads as a complete one is a wrong answer presented as a
// right one, and the flag is what lets the renderer say so.
func capExcerpt(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

// ciTriggerDecision is what a completed run does beyond being recorded.
type ciTriggerDecision struct {
	// Enqueue is false for the record-only cases.
	Enqueue bool
	// WorkflowID names the workflow to run; empty means the caller's default.
	WorkflowID string
	// Reason is a short token for the log.
	Reason string
}

// decideCITrigger applies the design's §5 table.
//
// It reads the OUTCOME rather than the event so the decision matches what was
// stored — the provider may have corrected a conclusion the webhook carried.
func decideCITrigger(out *persistence.ForgeCIOutcome, cfg ciIngestConfig) ciTriggerDecision {
	switch {
	case out == nil:
		return ciTriggerDecision{Reason: "no outcome recorded"}

	case out.Failed():
		// A failure is where a human wants Forge to speak. It needs a pull
		// request to speak ON, though: a failed default-branch build — or a
		// fork PR, whose event carries no pull request — is recorded and says
		// nothing, because there is no thread to post a review to.
		if !cfg.ReviewOnFailure {
			return ciTriggerDecision{Reason: "review_on_failure disabled"}
		}
		if !out.HasPullRequest() {
			return ciTriggerDecision{Reason: "failed run has no pull request to review"}
		}
		return ciTriggerDecision{Enqueue: true, Reason: "ci failed"}

	case out.Conclusion == "success":
		// Green enriches silently unless the operator asked for something. Off
		// by default: an empty workflow id.
		if cfg.SuccessWorkflowID == "" {
			return ciTriggerDecision{Reason: "green, no success workflow configured"}
		}
		return ciTriggerDecision{
			Enqueue: true, WorkflowID: cfg.SuccessWorkflowID, Reason: "ci succeeded",
		}

	default:
		// skipped / neutral / action_required: nothing was attempted, so
		// there is nothing to say.
		return ciTriggerDecision{Reason: "conclusion " + out.Conclusion + " is not actionable"}
	}
}

// ciOutcomeContext is what the created task carries about the run.
//
// A REFERENCE, never content (design §5.1): the artifact excerpt is absent, and
// a consumer reads it through forge.fetch_ci, which is the one place that
// untrusted-wraps it. A task payload is persisted and rendered into prompts by
// machinery this function does not own.
func ciOutcomeContext(out *persistence.ForgeCIOutcome) map[string]string {
	if out == nil {
		return nil
	}
	return map[string]string{
		"ci_run_id":        strconv.FormatInt(out.RunID, 10),
		"ci_head_sha":      out.HeadSHA,
		"ci_conclusion":    out.Conclusion,
		"ci_workflow_name": out.WorkflowName,
		"ci_workflow_path": out.WorkflowPath,
	}
}

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

	out := g.recordCIOutcome(ctx, ev)
	d := decideCITrigger(out, g.ci.cfg)
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
	out *persistence.ForgeCIOutcome, d ciTriggerDecision) error {
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
	payload, err = mergeCIContext(payload, ciOutcomeContext(out))
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
func (c *Container) ciIngestForProject(p *registry.Project) *ciIngest {
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
	ing := &ciIngest{
		outcomes: c.repos.ForgeCIOutcomes,
		cfg: ciIngestConfig{
			ArtifactName:      ci.ArtifactName,
			MaxArtifactBytes:  ci.MaxArtifactBytes,
			MaxExcerptBytes:   ci.MaxExcerptBytes,
			ReviewOnFailure:   ci.ReviewOnFailure,
			SuccessWorkflowID: ci.SuccessWorkflowID,
		},
	}
	// The provider is OPTIONAL: without a CIReader the conclusion from the
	// webhook is still recorded, which is the whole trigger decision. Only the
	// per-job detail and the artifact are lost.
	cfg, ok := p.ResolveForge()
	if !ok {
		return ing
	}
	prov, err := forge.New(cfg)
	if err != nil {
		c.Logger.Warn().Err(err).Str("project_id", p.ID).
			Msg("forge provider unavailable; recording webhook conclusions only")
		return ing
	}
	r, ok := prov.(forge.CIReader)
	if !ok {
		c.Logger.Info().Str("project_id", p.ID).Str("provider", prov.Name()).
			Msg("forge provider cannot read CI outcomes; recording webhook conclusions only")
		return ing
	}
	ing.reader = r
	return ing
}
