package api

import (
	"context"
	"net/http"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forgeci"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// CI-outcome ingestion on the GENERIC webhook ingress
// (2026-09-08-forge-ci-outcomes-design.md §3.2, §5).
//
// The App channel reaches the same logic through the task creator. Both call
// the same forgeci.Ingest, because this repository has already shipped a forge
// feature to one ingress and missed the one actually in use.

// ForgeCIIngest is the CI recorder+decider the webhook path uses.
//
// An interface so the api package does not depend on the concrete type, exactly
// as ForgeReviewCoordinator is. Nil is supported and degrades to "do not record,
// route as before".
type ForgeCIIngest interface {
	Record(ctx context.Context, run forgeci.Run) *persistence.ForgeCIOutcome
	Cfg() forgeci.Config
}

// WithForgeCIIngest wires CI ingestion. Job-tier only, for the same reason as
// the classifier and the review coordinator.
func WithForgeCIIngest(i func(projectID string) ForgeCIIngest) ServerOption {
	return func(s *Server) { s.forgeCI = i }
}

// applyForgeCIRules records the run and applies the trigger table.
//
// Returns true when the delivery is fully handled — which is the COMMON case:
// every green run without a success workflow, and every failure with no pull
// request, is recorded and enqueues nothing.
func (s *Server) applyForgeCIRules(ctx context.Context, w http.ResponseWriter,
	project *registry.Project, deliveryID string, job forge.ForgeJob) bool {
	ingest := s.forgeCI(project.ID)
	if ingest == nil {
		// Ingestion is off for this project. Let the delivery continue down
		// the normal path rather than dropping it: the source's own filter
		// decides whether a non-forge event creates a task.
		return false
	}

	out := ingest.Record(ctx, forgeci.Run{
		ProjectID:    project.ID,
		Repo:         job.Repo,
		RunID:        job.CI.RunID,
		HeadSHA:      job.CI.HeadSHA,
		Number:       job.Number,
		Conclusion:   job.CI.Conclusion,
		WorkflowName: job.CI.WorkflowName,
		WorkflowPath: job.CI.WorkflowPath,
	})

	d := forgeci.Decide(out, ingest.Cfg())
	if d.Enqueue {
		return false // fall through and create the task
	}

	s.logger.Info().
		Str("project_id", project.ID).Str("repo", job.Repo).
		Int64("run_id", job.CI.RunID).Str("conclusion", job.CI.Conclusion).
		Str("delivery", deliveryID).Str("reason", d.Reason).
		Msg("webhook: CI outcome recorded, no work enqueued")
	respondJSON(w, http.StatusOK, map[string]string{"status": "ci_recorded"})
	return true
}
