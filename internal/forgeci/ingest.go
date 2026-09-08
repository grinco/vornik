// Package forgeci records completed CI runs and decides what they trigger.
//
// Design: https://docs.vornik.io §4, §5.
//
// SHARED BY BOTH INGRESSES, for the same reason forgereview.Coordinator is: the
// GitHub App channel and the generic relay webhook are two doors into the same
// behaviour, and this repository has already shipped a forge feature that
// reached one and missed the other. One implementation means a rule change
// cannot land on one path alone.
package forgeci

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
)

// Run is what an ingress hands in: the reference a webhook carried.
type Run struct {
	ProjectID    string
	Repo         string
	RunID        int64
	HeadSHA      string
	Number       int
	Conclusion   string
	WorkflowName string
	WorkflowPath string
}

// Ingest records completed runs and applies the trigger rules.
//
// Nil-safe throughout: a project without CI ingestion has none, and must behave
// exactly as it did before this feature existed.
type Ingest struct {
	outcomes persistence.ForgeCIOutcomeRepository
	reader   forge.CIReader
	cfg      Config
	Logger   zerolog.Logger
}

// New builds an Ingest. A nil outcome store yields a nil Ingest: the caller's
// nil check is then the single place "CI ingestion is off" is expressed.
func New(outcomes persistence.ForgeCIOutcomeRepository, reader forge.CIReader, cfg Config, logger zerolog.Logger) *Ingest {
	if outcomes == nil {
		return nil
	}
	return &Ingest{outcomes: outcomes, reader: reader, cfg: cfg, Logger: logger}
}

// Cfg exposes the resolved configuration to a caller applying Decide.
func (g *Ingest) Cfg() Config {
	if g == nil {
		return Config{}
	}
	return g.cfg
}

// Config is the resolved per-project CI configuration (design §8).
// Config is the resolved per-project CI configuration (design §8).
type Config struct {
	ArtifactName      string
	MaxArtifactBytes  int64
	MaxExcerptBytes   int
	ReviewOnFailure   bool
	SuccessWorkflowID string
}

// Record fetches the run's full detail and stores it.
//
// Best-effort by design: a recording failure must not stop the trigger
// decision. The event already carries the conclusion, which is what the
// decision reads — losing the stored row costs a later review its context, not
// this delivery its behaviour.
func (g *Ingest) Record(ctx context.Context, ev Run) *persistence.ForgeCIOutcome {
	if g == nil || g.outcomes == nil {
		return nil
	}

	now := time.Now().UTC()
	out := &persistence.ForgeCIOutcome{
		ProjectID:    ev.ProjectID,
		Repo:         ev.Repo,
		RunID:        ev.RunID,
		HeadSHA:      ev.HeadSHA,
		Number:       ev.Number,
		WorkflowName: ev.WorkflowName,
		WorkflowPath: ev.WorkflowPath,
		RunAttempt:   1,
		Conclusion:   ev.Conclusion,
		CompletedAt:  now,
		RecordedAt:   now,
	}

	// The provider fills in what the webhook does not carry: per-job
	// conclusions, the run's real timing, and the artifact.
	if g.reader != nil {
		if run, err := g.reader.FetchCIRun(ctx, ev.Repo, ev.RunID); err == nil {
			applyRun(out, run)
		} else {
			g.Logger.Warn().Err(err).
				Str("repo", ev.Repo).Int64("run_id", ev.RunID).
				Msg("github task creator: CI run detail unavailable; recording the conclusion alone")
		}
		g.attachArtifact(ctx, out, ev)
	}

	if err := g.outcomes.Upsert(ctx, out); err != nil {
		g.Logger.Warn().Err(err).
			Str("repo", ev.Repo).Int64("run_id", ev.RunID).
			Msg("github task creator: CI outcome upsert failed")
	}
	return out
}

// applyCIRun copies the provider's detail onto the row, keeping the webhook's
// values wherever the API returned nothing.
func applyRun(out *persistence.ForgeCIOutcome, run forge.CIRun) {
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
func (g *Ingest) attachArtifact(ctx context.Context, out *persistence.ForgeCIOutcome, ev Run) {
	name := g.cfg.ArtifactName
	if name == "" {
		return // content is opt-in; conclusions only
	}
	arts, err := g.reader.ListCIArtifacts(ctx, ev.Repo, ev.RunID)
	if err != nil {
		g.Logger.Warn().Err(err).Str("repo", ev.Repo).
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
		g.Logger.Debug().Str("artifact", name).Str("repo", ev.Repo).
			Msg("github task creator: run uploaded no such artifact; recording conclusions only")
		return
	}

	data, err := g.reader.FetchCIArtifact(ctx, ev.Repo, match.ID, g.cfg.MaxArtifactBytes)
	if err != nil {
		// A too-large artifact is reported at INFO rather than swallowed: the
		// operator's response is to raise the cap or upload less, and they can
		// only do that if they know it happened.
		if errors.Is(err, forge.ErrCIArtifactTooLarge) {
			g.Logger.Info().Err(err).Str("artifact", name).
				Msg("github task creator: artifact exceeds the configured ceiling; not stored")
			return
		}
		g.Logger.Warn().Err(err).Str("artifact", name).
			Msg("github task creator: artifact download failed")
		return
	}

	excerpt, truncated := capExcerpt(string(data), g.cfg.MaxExcerptBytes)
	out.ArtifactExcerpt = excerpt
	out.ArtifactBytes = len(excerpt)
	out.ArtifactTruncated = truncated
}

// capExcerpt trims to the byte cap, reporting whether it had to.
//
// Truncation is RETURNED, never inferred by a caller comparing lengths: a
// truncated plan that reads as a complete one is a wrong answer presented as a
// right one, and the flag is what lets the renderer say so.
// capExcerpt trims to the byte cap.
func capExcerpt(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

// Decision is what a completed run does beyond being recorded.
type Decision struct {
	// Enqueue is false for the record-only cases.
	Enqueue bool
	// WorkflowID names the workflow to run; empty means the caller's default.
	WorkflowID string
	// Reason is a short token for the log.
	Reason string
}

// Decide applies the design's §5 table.
func Decide(out *persistence.ForgeCIOutcome, cfg Config) Decision {
	switch {
	case out == nil:
		return Decision{Reason: "no outcome recorded"}

	case out.Failed():
		// A failure is where a human wants Forge to speak. It needs a pull
		// request to speak ON, though: a failed default-branch build — or a
		// fork PR, whose event carries no pull request — is recorded and says
		// nothing, because there is no thread to post a review to.
		if !cfg.ReviewOnFailure {
			return Decision{Reason: "review_on_failure disabled"}
		}
		if !out.HasPullRequest() {
			return Decision{Reason: "failed run has no pull request to review"}
		}
		return Decision{Enqueue: true, Reason: "ci failed"}

	case out.Conclusion == "success":
		// Green enriches silently unless the operator asked for something. Off
		// by default: an empty workflow id.
		if cfg.SuccessWorkflowID == "" {
			return Decision{Reason: "green, no success workflow configured"}
		}
		return Decision{
			Enqueue: true, WorkflowID: cfg.SuccessWorkflowID, Reason: "ci succeeded",
		}

	default:
		// skipped / neutral / action_required: nothing was attempted, so
		// there is nothing to say.
		return Decision{Reason: "conclusion " + out.Conclusion + " is not actionable"}
	}
}

// Context is the reference a triggered task carries.
func Context(out *persistence.ForgeCIOutcome) map[string]string {
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
