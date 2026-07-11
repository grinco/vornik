package fixitdoctor

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
)

// StatusPollResult is the objective, server-checked state shown to the
// operator on a Resolved:true turn INSTEAD of auto-closing the session
// (fix-it-doctor-design.md §5.2: "the server does not auto-close; it
// runs the per-kind status poll ... and shows the objective state; the
// USER closes"). This is a read-only status CHECK, never a re-run of
// the failing work itself — retrying the task, re-applying config,
// etc. is task 3.3's action dispatcher, gated separately and only ever
// operator-initiated.
type StatusPollResult struct {
	// Summary is a short, human-readable description of the
	// currently-observed state (e.g. "task status: COMPLETED").
	Summary string
	// Healthy is the poll's best-effort verdict on whether the
	// failure actually looks resolved. Advisory only — the operator
	// decides whether to close the session, the server never does.
	Healthy bool
}

// PollResolvedStatus checks the underlying failing object's CURRENT
// status without re-executing anything, per FailureKind:
//   - failed_task: reads Task.Status (a.Tasks.Get).
//   - degraded_feature: re-runs featuredoctor.Diagnose (a read-only
//     check, same one the grounding bundle itself uses).
//   - red_integration: reads the latest CACHED probe result
//     (a.IntegrationProbes.LatestProbe — task 3.4 wired this against
//     ui.Server's real probe cache; see assembler.go's doc comments).
//     Actively forcing a fresh network probe on every Resolved:true
//     turn is a DELIBERATELY separate decision from that wiring —
//     reprobe_integration (dispatch.go, also wired for real in task
//     3.4) is the operator-initiated live re-probe; this read-only
//     poll intentionally stays cache-only so a Resolved:true turn
//     never triggers a network call as a side effect. Reports the
//     same "latest known" result the bundle would.
//   - failed_reload: reads whether a reload validation error is still
//     on record (a.ReloadStatus.LatestReloadError).
func (a *Assembler) PollResolvedStatus(ctx context.Context, ref FailureRef) (StatusPollResult, error) {
	switch ref.Kind {
	case FailureKindFailedTask:
		return a.pollFailedTask(ctx, ref)
	case FailureKindDegradedFeature:
		return a.pollDegradedFeature(ctx, ref)
	case FailureKindRedIntegration:
		return a.pollRedIntegration(ctx, ref)
	case FailureKindFailedReload:
		return a.pollFailedReload(ctx, ref)
	default:
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: unknown failure kind %q", ref.Kind)
	}
}

func (a *Assembler) pollFailedTask(ctx context.Context, ref FailureRef) (StatusPollResult, error) {
	if a.Tasks == nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: failed_task status poll requires a TaskRepository")
	}
	task, err := a.Tasks.Get(ctx, ref.ID)
	if err != nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: poll task %s: %w", ref.ID, err)
	}
	healthy := task.Status == persistence.TaskStatusCompleted
	return StatusPollResult{
		Summary: fmt.Sprintf("task status: %s", task.Status),
		Healthy: healthy,
	}, nil
}

func (a *Assembler) pollDegradedFeature(ctx context.Context, ref FailureRef) (StatusPollResult, error) {
	var feature *featuredoctor.Feature
	for _, f := range a.features() {
		if f.ID == ref.ID {
			ff := f
			feature = &ff
			break
		}
	}
	if feature == nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: unknown feature %q", ref.ID)
	}
	diag := featuredoctor.Diagnose(ctx, *feature, a.FeatureDeps)
	return StatusPollResult{
		Summary: fmt.Sprintf("feature status: %s", diag.Status),
		Healthy: diag.Status == featuredoctor.StatusOK,
	}, nil
}

func (a *Assembler) pollRedIntegration(ctx context.Context, ref FailureRef) (StatusPollResult, error) {
	if a.IntegrationProbes == nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: red_integration status poll requires an IntegrationProbeProvider")
	}
	result, _, ok, err := a.IntegrationProbes.LatestProbe(ctx, ref)
	if err != nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: poll probe for %q: %w", ref.ID, err)
	}
	if !ok {
		return StatusPollResult{Summary: "no probe result known", Healthy: false}, nil
	}
	return StatusPollResult{
		Summary: fmt.Sprintf("integration probe outcome: %s", result.Outcome),
		Healthy: result.Outcome == integrations.OutcomeOK,
	}, nil
}

func (a *Assembler) pollFailedReload(ctx context.Context, ref FailureRef) (StatusPollResult, error) {
	if a.ReloadStatus == nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: failed_reload status poll requires a ReloadStatusProvider")
	}
	_, ok, err := a.ReloadStatus.LatestReloadError(ctx, ref)
	if err != nil {
		return StatusPollResult{}, fmt.Errorf("fixitdoctor: poll reload status: %w", err)
	}
	if ok {
		return StatusPollResult{Summary: "reload validation: error still present", Healthy: false}, nil
	}
	return StatusPollResult{Summary: "reload validation: clean (no error on record)", Healthy: true}, nil
}

// statusSignal derives the short, opaque "current objective status"
// string used for FixItSession.StatusSignal — the re-ground-every-turn
// state-change comparison (§5.2: "if the underlying failure status
// changed since open, inject a notice"). Best-effort: any poll error
// yields an empty signal (comparison becomes a no-op that turn) rather
// than failing the whole converse call — the poll itself is a nicety on
// top of the bundle, not required for the conversation to proceed.
func (a *Assembler) statusSignal(ctx context.Context, ref FailureRef) string {
	res, err := a.PollResolvedStatus(ctx, ref)
	if err != nil {
		return ""
	}
	return res.Summary
}
