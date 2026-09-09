package executor

import "vornik.io/vornik/internal/stepoutcome"

// claimRefusal is the typed error a claim verifier returns when it refuses a
// step: the agent asserted something in result.json that the daemon could
// check and found untrue.
//
// It exists so the step-outcome classifier can read the CLASS from the error's
// type rather than from its sentence. Before it, the workflow-step pipeline
// path handed these errors to the refiner as opaque text, the refiner had no
// arm for either phrase, and every one landed in `unclassified` — 65 rows
// all-time, 7 of 7 over the trailing 30 days on 2026-09-02, while the
// plan-step path finalised the same refusal as verify_claims_failed. Same
// discipline as MODEL_UNHEALTHY (2026-09-02 design D1): a condition the daemon
// generates is not recovered by parsing the prose the daemon wrote about it.
//
// The message contract is untouched. classifyShapeFailure routes the
// corrective retry on "agent fabrication detected", and migration 170 keyed
// its history rewrite on "but verification failed"; the type is added
// alongside the text, never in place of it.
type claimRefusal struct {
	// class is the stepoutcome class the refusal records —
	// ClassVerifyFailed for a role claim (a commit cited that does not
	// exist, a pass with nothing in the audit), ClassMissingOutput for a
	// declared file that is not on disk.
	class string
	msg   string
}

func (r *claimRefusal) Error() string { return r.msg }

// outcome is the outcome both refusals record: the output parsed and then
// failed the "claims match reality" invariant, which is what the plan-step
// path already writes for the same refusal (plan_step.go, verifyRoleClaims).
// The two paths must agree, or the same lie counts differently depending on
// which workflow shape told it.
func (r *claimRefusal) outcome() stepoutcome.Outcome { return stepoutcome.SchemaViolation }
