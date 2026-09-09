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
	// Comment posts the factual CI-status comment for a run that cannot
	// produce a review (design §13).
	Comment(ctx context.Context, out *persistence.ForgeCIOutcome) (bool, error)
	// AlreadyReviewed reports whether this run's head has already been
	// reviewed and posted about, which is what turns a failure into a comment
	// rather than a review.
	AlreadyReviewed(ctx context.Context, projectID string, out *persistence.ForgeCIOutcome) bool
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

	d := forgeci.Decide(out, ingest.Cfg(), ingest.AlreadyReviewed(ctx, project.ID, out))
	if d.Enqueue {
		return false // fall through and create the task
	}
	if d.Comment {
		// A failure on a head whose review is already posted: say so factually
		// rather than running a reviewer that would be refused (design §13).
		posted, err := ingest.Comment(ctx, out)
		if err != nil {
			s.logger.Warn().Err(err).
				Str("project_id", project.ID).Str("repo", job.Repo).
				Int64("run_id", job.CI.RunID).
				Msg("webhook: CI status comment failed; it will retry on a redelivery")
		}
		s.logger.Info().
			Str("project_id", project.ID).Str("repo", job.Repo).
			Int64("run_id", job.CI.RunID).Bool("commented", posted).
			Str("delivery", deliveryID).Str("reason", d.Reason).
			Msg("webhook: CI outcome recorded, commented instead of reviewing")
		// Report what HAPPENED, not what was attempted. A redelivery whose
		// claim was refused posted nothing, and answering "ci_commented" to it
		// is a control that cannot distinguish "commented now" from "commented
		// already" — the operator reading the response would conclude this
		// delivery published something it did not.
		status := "ci_recorded"
		if posted {
			status = "ci_commented"
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": status})
		return true
	}

	s.logger.Info().
		Str("project_id", project.ID).Str("repo", job.Repo).
		Int64("run_id", job.CI.RunID).Str("conclusion", job.CI.Conclusion).
		Str("delivery", deliveryID).Str("reason", d.Reason).
		Msg("webhook: CI outcome recorded, no work enqueued")
	respondJSON(w, http.StatusOK, map[string]string{"status": "ci_recorded"})
	return true
}
