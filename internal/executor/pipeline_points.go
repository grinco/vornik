package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pipeline"
	"vornik.io/vornik/internal/registry"
)

// The executor's step-outcome point (2026-09-04-pipeline-points-design.md
// §3.4): the eight checks that decide whether an agent step's result is
// accepted, registered in the order they always ran. Each participant is the
// block that used to sit inline in executeAgentStep, moved verbatim; the
// caller keeps what was never a participant — the container-log tail on the
// first three refusals, and the container removal.
//
// Inter-participant edges, stated so a reorder is visibly wrong:
//
//	output_file_contract (1) writes GlobVerified          → plausibility (3) reads it
//	hallucination_detector (6) writes HallucinationSignals, HallucinationDetail → the caller, onto the outcome row
//	trading_floor (7) rewrites ResultBytes                → outcome_verifiers (8) and the caller
//
// No other field is written by a participant.

// StepOutcome is the mutable record the participants share.
type StepOutcome struct {
	Task       *persistence.Task
	Execution  *persistence.Execution
	Step       *registry.WorkflowStep
	StepID     string
	RoleConfig *registry.SwarmRole
	// ResultBytes is the redacted result; RawResultBytes the unredacted one
	// the structural checks evaluate (see the scan-site comment in
	// executeAgentStep). trading_floor rewrites ResultBytes when it passes.
	ResultBytes, RawResultBytes []byte
	WorkspaceDir, ProjectDir    string
	StepStart                   time.Time
	PreStepHEAD, PostStepHEAD   string

	// Written by participants.
	GlobVerified         bool
	HallucinationSignals []byte
	HallucinationDetail  string
	// Err is the refusing participant's own error value, returned to the
	// caller as-is so typed verifier errors keep their type.
	Err error
}

// stepOutcomeExitTier names the participants whose refusal is reported as a
// containerExitError with the container-log tail appended — the three that
// used to write agentError.
var stepOutcomeExitTier = map[string]bool{
	"output_file_contract": true,
	"tool_contract":        true,
	"plausibility":         true,
}

// stepOutcomeRemoveMsg keeps each refusal's container-removal log line.
var stepOutcomeRemoveMsg = map[string]string{
	"claimed_files":          "failed to remove container after verify failure",
	"role_claims":            "failed to remove container after role-claim verify failure",
	"hallucination_detector": "failed to remove container after hallucination failure",
	"trading_floor":          "failed to remove container after trading-floor hard-fail",
	"outcome_verifiers":      "failed to remove container after verifier failure",
}

type executorPipelineLogger struct{ l zerolog.Logger }

func (z executorPipelineLogger) Warn(msg string, args ...any) {
	ev := z.l.Warn()
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			ev = ev.Interface(k, args[i+1])
		}
	}
	ev.Msg(msg)
}

// stepOutcomePoint builds the chain on first use.
func (e *Executor) stepOutcomePoint() *pipeline.Decide[*StepOutcome] {
	e.stepOutcomeOnce.Do(func() {
		c := pipeline.NewDecide[*StepOutcome](pipeline.ExecutorStepOutcome, executorPipelineLogger{e.logger})
		c.Register("output_file_contract", e.outputFileContractParticipant)
		c.Register("tool_contract", e.toolContractParticipant)
		c.Register("plausibility", e.plausibilityParticipant)
		c.Register("claimed_files", e.claimedFilesParticipant)
		c.Register("role_claims", e.roleClaimsParticipant)
		c.Register("hallucination_detector", e.hallucinationDetectorParticipant)
		c.Register("trading_floor", e.tradingFloorParticipant)
		c.Register("outcome_verifiers", e.outcomeVerifiersParticipant)
		e.stepOutcomeChain = c
	})
	return e.stepOutcomeChain
}

func refuse(err error) pipeline.Verdict { return pipeline.Verdict{Refused: true, Reason: err.Error()} }

// Output-file contract: a step declaring require_output_glob must have
// written at least one matching file DURING this step, or it fails loud with
// the "schema violation:" prefix (one corrective shape retry, then on_fail).
// Incident task_20260712143854_429a3500d692d23c: 7 of 8 deep-research
// subtasks completed without writing their promised findings file and the
// parent chain "succeeded" into an empty publish. GlobVerified records that
// the filesystem — not the model — confirmed this step's declared output; the
// plausibility participant reads it so a self-reported file list cannot fail
// a step whose output was already verified for real.
func (e *Executor) outputFileContractParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	if in.Step.RequireOutputGlob == "" {
		return pipeline.Verdict{}
	}
	if outputGlobSatisfied(in.WorkspaceDir, in.ProjectDir, in.Step.RequireOutputGlob, in.StepStart) {
		in.GlobVerified = true
		return pipeline.Verdict{}
	}
	return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf(
		"schema violation: output contract for step %q not met — no file matching %q was written during this step. You MUST write the declared output file before finishing.",
		in.StepID, in.Step.RequireOutputGlob)}
}

// Tool contract: an auth-class tool failure fails the step (no opt-in), and a
// step declaring require_tools must have completed each of them. Runs AFTER
// the output-file contract and BEFORE plausibility, because a connector
// rejection explains a missing file and a thin result far better than either
// explains itself. Incident 2026-08-25: a connector lost auth, the agent
// narrated the 401 correctly in its own payload, and the task completed —
// nothing between the tool call and the task's status could name what had
// happened. See tool_contract.go.
func (e *Executor) toolContractParticipant(ctx context.Context, in *StepOutcome) pipeline.Verdict {
	v := e.evaluateToolContract(ctx, in.Execution, in.Task, in.Step, in.StepID)
	if v == nil {
		return pipeline.Verdict{}
	}
	if v.AuthClass {
		e.logger.Error().
			Str("execution_id", in.Execution.ID).
			Str("task_id", in.Task.ID).
			Str("project_id", in.Task.ProjectID).
			Str("step", in.StepID).
			Str("outcome_class", "auth").
			Msg("step failed: a connector rejected the credential — " + v.Message)
	}
	return pipeline.Verdict{Refused: true, Reason: v.Message}
}

// Plausibility rules: layered on top of RequiredOutputKeys to catch the
// half-honest output ("approved":true with empty "feedback") that passes
// shape validation but isn't actually usable downstream. WarnOnly rules emit
// a log line and don't gate; gate-mode rules fail the step with
// INVALID_OUTPUT. Evaluated over RawResultBytes for the same reason as the
// required-keys check: a structural evaluation whose Detail is built from the
// field name plus the rule's own `When` config, never a payload value.
func (e *Executor) plausibilityParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	if len(in.RoleConfig.PlausibilityRules) == 0 || len(in.RawResultBytes) == 0 {
		return pipeline.Verdict{}
	}
	violations := EvaluatePlausibilityWithGroundTruth(in.RawResultBytes, in.RoleConfig.PlausibilityRules, in.GlobVerified)
	var blocking []string
	for _, v := range violations {
		if v.WarnOnly {
			e.logger.Warn().
				Str("execution_id", in.Execution.ID).
				Str("step", in.StepID).
				Str("role", in.Step.Role).
				Str("rule", v.RuleName).
				Str("detail", v.Detail).
				Msg("plausibility: warn-only rule fired — step still passes")
			continue
		}
		blocking = append(blocking, fmt.Sprintf("%s: %s", v.RuleName, v.Detail))
	}
	if len(blocking) == 0 {
		return pipeline.Verdict{}
	}
	return pipeline.Verdict{Refused: true, Reason: fmt.Sprintf("plausibility violation: role %q failed %d rule(s): %s",
		in.Step.Role, len(blocking), strings.Join(blocking, "; "))}
}

// Verify file claims (modified_files, outputArtifacts paths, produced_files)
// in result.json against the real filesystem — the same path the container
// saw mounted at /app/workspace/project. A claim that doesn't match reality
// fails the step so the next role doesn't silently run against a half-empty
// workspace.
func (e *Executor) claimedFilesParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	if err := e.verifyClaimedFiles(in.ResultBytes, in.WorkspaceDir, in.ProjectDir, in.StepStart); err != nil {
		in.Err = err
		return refuse(err)
	}
	return pipeline.Verdict{}
}

// Cross-cutting deception checks (testing.passed:true → toolAudit must show
// actual execution; review.checked_commit → object must exist;
// files_changed:N → real diff count must match).
func (e *Executor) roleClaimsParticipant(ctx context.Context, in *StepOutcome) pipeline.Verdict {
	if err := e.verifyRoleClaims(ctx, in.ResultBytes, in.PreStepHEAD, in.PostStepHEAD, in.ProjectDir); err != nil {
		in.Err = err
		return refuse(err)
	}
	return pipeline.Verdict{}
}

// Phase 1 hallucination detection: scan the agent's prose for claims and
// cross-reference each against this step's tool_audit + artifact list.
// Signals land on the outcome row regardless of severity; High-severity
// findings fail the step so the scheduler's retry path picks it up. The
// detector is best-effort: any error during build/scan degrades silently.
func (e *Executor) hallucinationDetectorParticipant(ctx context.Context, in *StepOutcome) pipeline.Verdict {
	if e.hallucinationDetector == nil {
		return pipeline.Verdict{}
	}
	signalBlob, detail, err := e.runHallucinationDetector(ctx, in.Task, in.Execution, in.StepID, in.ResultBytes)
	in.HallucinationSignals = signalBlob
	if err != nil {
		in.HallucinationDetail = detail
		in.Err = err
		return refuse(err)
	}
	return pipeline.Verdict{}
}

// Trading scorecard_floor SOFT-DROP (design 2026-07-25): before the
// verifiers run, drop floor-failing OPEN proposals from the strategist's
// result so a no-qualifying-candidate tick NO_ACTIONs instead of the whole
// step hard-failing. Integrity violations still HARD-fail the step here.
// No-op for non-trading projects / non-proposal steps (self-gated).
func (e *Executor) tradingFloorParticipant(_ context.Context, in *StepOutcome) pipeline.Verdict {
	filtered, err := e.filterTradingFloor(in.Task, in.ResultBytes)
	if err != nil {
		in.HallucinationDetail = err.Error()
		in.Err = err
		return refuse(err)
	}
	in.ResultBytes = filtered
	return pipeline.Verdict{}
}

// Phase 2 outcome verifiers: project-declared declarative invariants over
// (artifacts, audit, result.json). Distinct from Phase 1 which scans prose;
// Phase 2 scrutinises actual work. A verifier failure fails the step so the
// scheduler retries it.
func (e *Executor) outcomeVerifiersParticipant(ctx context.Context, in *StepOutcome) pipeline.Verdict {
	if err := e.runVerifiers(ctx, in.Task, in.Execution, in.StepID, in.ResultBytes, in.ProjectDir); err != nil {
		in.HallucinationDetail = err.Error()
		in.Err = err
		return refuse(err)
	}
	return pipeline.Verdict{}
}
