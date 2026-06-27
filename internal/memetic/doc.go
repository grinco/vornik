// Package memetic implements the workflow-architect agent that
// reads per-workflow execution telemetry, proposes structural YAML
// edits, and persists those proposals for operator review. Slice 2
// of the self-evolving-workflows arc (see
// https://docs.vornik.io).
//
// "Memetic" — workflows propose their own evolution. Every other
// agentic framework treats workflow definitions as immutable
// operator artifacts; vornik's architect reasons ABOUT the
// workflow, not just INSIDE it.
//
// Slice 2 ships the propose path only — emit a pending row in
// workflow_proposals, gated by every safety rail in the design.
// Slice 4 ships the apply path; Slice 5 ships rollback. The
// architect never writes to configs/workflows/ directly; the
// operator approval flow does.
//
// Safety rails (encoded in this package):
//
//   - Evidence required: every proposal cites ≥ MinEvidenceRunIDs
//     run IDs (default 3) that reference executions of THIS
//     workflow. Validated before insert.
//   - Confidence floor: proposals below MinConfidence (default
//     0.6) are dropped so operators don't drown in low-signal
//     proposals.
//   - YAML well-formed: the proposed YAML must parse cleanly
//     through registry.ValidateWorkflowMarkdown — invalid
//     proposals never reach the operator.
//   - Rate limit: enforced at the persistence layer by a partial
//     unique index (workflow_id) WHERE status='pending'. Insert
//     surfaces ErrProposalRateLimited if a pending proposal
//     already exists; this layer returns it verbatim.
//
// What the architect proposes (closed set, v1):
//   - Add a new step
//   - Remove a step
//   - Add a transition (on_success / on_failure)
//   - Tune retry / timeout / max_iters on an existing step
//   - Reorder transitions
//
// What it cannot propose (Slice 2 boundary):
//   - Prompt edits (those drift fast; out-of-band for v1)
//   - New roles (would require a registry change)
//   - Anything not expressible as a YAML diff
//
// Narrow interfaces (TelemetrySource, WorkflowSource, ProposalSink)
// keep this package free of filesystem / database wiring; the
// admin endpoint (Slice 2c) wires the real adapters.
package memetic
