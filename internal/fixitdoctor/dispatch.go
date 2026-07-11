package fixitdoctor

// Task 3.3 — the deny-by-default action dispatcher
// (https://docs.vornik.io §5.3/§5.4/§5.6/§6/§7).
//
// THE SAFETY INVARIANT: the LLM proposes (3.2's envelope), the SERVER
// disposes. Dispatch executes ONLY the six ActionKinds the enum names,
// each through an existing rollback-capable pipeline, and ONLY on an
// explicit Apply call naming one already-proposed, already-persisted
// action from the session's last envelope — never a kind or param the
// caller invents fresh. There is no shell, no arbitrary file write, no
// arbitrary tool: every case in dispatchOne below routes to a narrow,
// named interface method; adding a seventh kind is a code change here,
// never something a model response can trigger.
//
// Re-assertion, not trust: ValidateActions (envelope_validate.go)
// already dropped unknown-kind/param-invalid actions before persisting
// the envelope, but Dispatch re-checks both here — an envelope
// persisted under one edition (e.g. a config later downgraded from
// Enterprise to Community) or a future bug in that first gate must not
// silently promote a stale persisted action into an executable one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/version"
)

// ActionResult* label the "result" dimension of
// vornik_fixit_actions_total{kind,result} (§5.6/§8).
const (
	ActionResultApplied  = "applied"
	ActionResultRejected = "rejected"
	ActionResultFailed   = "failed"
)

// ErrActionConflict is returned by a pipeline seam when the action is no
// longer applicable — the underlying object changed shape since it was
// proposed (a task that's no longer terminal, a gate no longer
// registered, a secret field no longer declared) — as opposed to an
// unexpected failure. Dispatch maps it to ActionResultRejected rather
// than ActionResultFailed: "this can't be applied anymore, re-ground and
// re-propose" is a different operator story than "something broke
// applying it".
var ErrActionConflict = errors.New("fixitdoctor: action no longer applicable")

// Dispatch-level errors — mapped to HTTP status by the caller (mirrors
// Converse's refusal taxonomy in service.go).
var (
	// ErrNoSuchAction — actionIndex doesn't resolve to a proposed action
	// in the session's last envelope (out of range, or no envelope yet).
	ErrNoSuchAction = errors.New("fixitdoctor: no such proposed action")
)

// GatePipeline is the config_apply_gate (CE) execution seam:
// featuredoctor.PlanEnable -> preview diff -> ApplyEnable (Backup/Write/
// Validate/Restore-on-fail already inside the pipeline).
type GatePipeline interface {
	// Plan resolves key to a registered feature-doctor gate and renders
	// a human-readable diff of what applying it would change. Returns
	// ErrActionConflict if key isn't a registered gate.
	Plan(ctx context.Context, key string) (diff string, err error)
	// Apply plans + applies the gate change. Returns ErrActionConflict
	// if key isn't a registered gate; any other error means the
	// underlying pipeline already rolled back (ApplyEnable's contract).
	Apply(ctx context.Context, key string) (detail string, err error)
}

// ConfigProposalPipeline is the config_apply (EE-only) execution seam:
// files a ControlPlaneProposal{Kind:config, ProposedBy:fix_it_doctor}
// then drives ApplyEngine.Apply with the human operator as actor
// (proposer != approver, clearing ErrProposalSelfApprove by design).
type ConfigProposalPipeline interface {
	// File records the proposal and returns its id + rendered diff.
	File(ctx context.Context, projectID, key, value string) (proposalID, diff string, err error)
	// Apply drives ApplyEngine.Apply(ctx, proposalID, actor, false).
	// actor is the human operator id (never "fix_it_doctor").
	Apply(ctx context.Context, proposalID, actor string) error
	// Rollback drives ApplyEngine.Rollback — the §5.4 "Rollback
	// affordance" a successfully applied config_apply exposes for the
	// session's lifetime.
	Rollback(ctx context.Context, proposalID string) error
}

// TaskRetrier is the retry_task execution seam (RetryTask/
// RequeueTerminalTask's first-wins/409 semantics live inside it).
type TaskRetrier interface {
	// Retry returns ErrActionConflict when the task is no longer in a
	// retryable terminal state (another retry already won the race).
	Retry(ctx context.Context, projectID, taskID string) (detail string, err error)
}

// IntegrationReprober is the reprobe_integration execution seam
// (integrations.Prober.Probe re-run; idempotent).
type IntegrationReprober interface {
	Reprobe(ctx context.Context, projectID, integrationID string) (summary string, healthy bool, err error)
}

// SecretSetter is the set_secret execution seam
// (projectdoctor.Doctor.SetSecret's declared-names gate lives inside
// it). value is NEVER read from a ProposedAction's Params by Dispatch —
// it always comes from the caller's masked, user-typed input.
type SecretSetter interface {
	// Set returns ErrActionConflict when field isn't declared for
	// projectID (the declared-names gate).
	Set(ctx context.Context, projectID, field, value string) error
}

// AuditRecorder is the narrow AdminAuditRepository seam Dispatch writes
// one row to per successfully APPLIED action.
type AuditRecorder interface {
	Insert(ctx context.Context, entry *persistence.AdminAuditEntry) error
}

// DispatchResult is what one Apply call produces — streamed into the
// transcript, recorded on the session, and reported to the caller.
type DispatchResult struct {
	Kind   ActionKind
	Result string // ActionResult* — applied | rejected | failed
	Detail string // human-readable, streamed into the transcript
	Diff   string // unified diff / rendered change — config actions only
	// RollbackID is the ControlPlaneProposal id, populated only for a
	// successfully applied config_apply (the Rollback affordance's key).
	RollbackID string
}

// appliedActionRecord is the JSON shape persisted into
// fixit_sessions.applied_actions — one entry per Dispatch call
// (including rejected/failed attempts, so an operator's session history
// shows what was tried, not only what stuck).
type appliedActionRecord struct {
	Kind       ActionKind `json:"kind"`
	Result     string     `json:"result"`
	Detail     string     `json:"detail"`
	RollbackID string     `json:"rollback_id,omitempty"`
	AppliedAt  time.Time  `json:"applied_at"`
}

// fixitActionAuditPrefix namespaces every audit Action under
// "fixit.action." (design §5.6 review finding 10) so it can't collide
// with a same-named action verb elsewhere in the admin_audit table
// (e.g. a plain "config_apply" action from a different subsystem).
const fixitActionAuditPrefix = "fixit.action."

// Dispatch applies ONE already-proposed action from sessionID's last
// envelope — the deny-by-default dispatcher (§5.3). actionIndex indexes
// into that envelope's Actions slice (0-based); secretValue is used ONLY
// for ActionKindSetSecret and is ignored for every other kind (in
// particular it is never substituted for a param the model supplied —
// see SecretSetter's doc comment).
//
// Refusals (mirroring Converse's IDOR/closed-session conventions):
//   - unknown/foreign sessionID -> persistence.ErrNotFound
//   - closed session -> ErrSessionClosed
//   - out-of-range actionIndex / no envelope yet -> ErrNoSuchAction
//
// Every other outcome (unknown kind, param-invalid, pipeline conflict,
// pipeline failure, pipeline not configured) is NOT an error return —
// it's a DispatchResult with Result != applied, exactly like a rejected
// HTTP request the caller can render inline in the transcript rather
// than a 5xx.
func (s *Service) Dispatch(ctx context.Context, sessionID, operatorID string, actionIndex int, secretValue string) (*DispatchResult, error) {
	if s == nil || s.Sessions == nil {
		return nil, errors.New("fixitdoctor: not fully wired")
	}
	session, ref, err := s.resumeSession(ctx, sessionID, operatorID)
	if err != nil {
		return nil, err
	}

	action, ok := lastProposedAction(session.LastEnvelope, actionIndex)
	if !ok {
		return nil, ErrNoSuchAction
	}

	result := s.dispatchOne(ctx, ref, operatorID, action, secretValue)

	s.Metrics.recordAction(string(action.Kind), result.Result)

	if result.Result == ActionResultApplied {
		s.auditApplied(ctx, ref, operatorID, action, result)
	}
	if err := s.recordAppliedAction(ctx, session, result); err != nil {
		return result, fmt.Errorf("fixitdoctor: record applied action: %w", err)
	}
	return result, nil
}

// RollbackConfigApply drives the §5.4 Rollback affordance for a
// previously-applied config_apply action: restores the
// ControlPlaneProposal's pre-apply snapshot via ApplyEngine.Rollback.
// IDOR/closed-session checks mirror Dispatch's (resumeSession); a
// missing ConfigProposals pipeline (nil, or a Community build that
// never wires one) fails closed rather than panicking.
func (s *Service) RollbackConfigApply(ctx context.Context, sessionID, operatorID, proposalID string) (*DispatchResult, error) {
	if s == nil || s.Sessions == nil {
		return nil, errors.New("fixitdoctor: not fully wired")
	}
	session, ref, err := s.resumeSession(ctx, sessionID, operatorID)
	if err != nil {
		return nil, err
	}
	result := &DispatchResult{Kind: ActionKindConfigApply, RollbackID: proposalID}
	if s.ConfigProposals == nil {
		result.Result = ActionResultFailed
		result.Detail = "config_apply rollback is not configured on this deployment"
	} else if err := s.ConfigProposals.Rollback(ctx, proposalID); err != nil {
		result.Result = ActionResultFailed
		result.Detail = err.Error()
	} else {
		result.Result = ActionResultApplied
		result.Detail = "config change rolled back"
	}

	s.Metrics.recordAction("config_apply_rollback", result.Result)
	s.auditRollback(ctx, ref, operatorID, proposalID, result)
	if err := s.recordAppliedAction(ctx, session, result); err != nil {
		return result, fmt.Errorf("fixitdoctor: record rollback: %w", err)
	}
	return result, nil
}

// dispatchOne re-asserts the guardrail (kind ∈ enum for this edition +
// param shape) and routes to exactly one pipeline. This is the whole of
// the "one dispatcher, a handler per kind, nothing else" contract —
// every branch returns before falling through, and the default case
// covers everything the switch doesn't explicitly name.
func (s *Service) dispatchOne(ctx context.Context, ref FailureRef, operatorID string, action ProposedAction, secretValue string) *DispatchResult {
	if !IsAllowedActionKind(action.Kind, s.Edition) {
		s.Metrics.recordGuardrailHit(GuardrailReasonUnknownKind)
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected,
			Detail: fmt.Sprintf("%q is not an allowed action on this deployment", action.Kind)}
	}
	if !validActionParams(action.Kind, action.Params) {
		s.Metrics.recordGuardrailHit(GuardrailReasonParamsInvalid)
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected,
			Detail: "action parameters failed validation"}
	}

	switch action.Kind {
	case ActionKindConfigApplyGate:
		return s.dispatchConfigApplyGate(ctx, action)
	case ActionKindConfigApply:
		return s.dispatchConfigApply(ctx, ref, operatorID, action)
	case ActionKindRetryTask:
		return s.dispatchRetryTask(ctx, ref, action)
	case ActionKindReprobeIntegration:
		return s.dispatchReprobeIntegration(ctx, ref, action)
	case ActionKindSetSecret:
		return s.dispatchSetSecret(ctx, ref, action, secretValue)
	case ActionKindLinkOut:
		// Pure client-side navigation — nothing to apply server-side.
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected,
			Detail: "link_out is navigation only; nothing to apply"}
	default:
		// Unreachable given IsAllowedActionKind above, but the deny-by-
		// default posture means an unrecognised kind is ALWAYS refused,
		// never routed — even if a future enum entry is added to
		// AllowedActionKinds without a corresponding case here.
		s.Metrics.recordGuardrailHit(GuardrailReasonUnknownKind)
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected,
			Detail: fmt.Sprintf("%q has no dispatcher handler", action.Kind)}
	}
}

func (s *Service) dispatchConfigApplyGate(ctx context.Context, action ProposedAction) *DispatchResult {
	if s.GatePipeline == nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed,
			Detail: "config_apply_gate is not configured on this deployment"}
	}
	// key is UNTRUSTED input — it rides in action.Params, which
	// originated as a model proposal (re-asserted, not re-derived: the
	// model cannot invent a key that resolves to anything dangerous).
	// The authoritative gate check is GatePipeline.Plan/Apply's
	// findGateFeature lookup (fixit_dispatch_adapter.go) — it iterates
	// featuredoctor.Registry()'s REGISTERED Gates and returns
	// ErrActionConflict for any key that isn't one, so only a key a
	// feature actually declares as a gate can ever reach ApplyEnable.
	key := action.Params["key"]
	diff, _ := s.GatePipeline.Plan(ctx, key) // best-effort preview; Apply re-validates
	detail, err := s.GatePipeline.Apply(ctx, key)
	if err != nil {
		if errors.Is(err, ErrActionConflict) {
			return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected, Detail: err.Error(), Diff: diff}
		}
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error(), Diff: diff}
	}
	return &DispatchResult{Kind: action.Kind, Result: ActionResultApplied, Detail: detail, Diff: diff}
}

func (s *Service) dispatchConfigApply(ctx context.Context, ref FailureRef, operatorID string, action ProposedAction) *DispatchResult {
	// Defense-in-depth re-assertion of the CE/EE boundary — IsAllowedActionKind
	// above already excludes config_apply from a Community edition's
	// vocabulary, but a nil ConfigProposals pipeline (e.g. a CE build that
	// never wires one at all) fails closed the same way regardless.
	//
	// This is a DOUBLE gate, deliberately: (1) build-time — CE's
	// AllowedActionKinds enum (envelope.go IsAllowedActionKind) never
	// lists config_apply at all, so a CE binary's dispatchOne switch is
	// unreachable for this kind; (2) runtime — the edition check here
	// catches a persisted envelope surviving an EE->CE config downgrade
	// (see the file-level "re-assertion, not trust" doc comment above).
	// Neither check alone is trusted to be sufficient on its own.
	if version.NormalizeEdition(s.Edition) != version.EditionEnterprise || s.ConfigProposals == nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected,
			Detail: "config_apply requires the Enterprise edition"}
	}
	key := action.Params["key"]
	value := action.Params["value"]
	proposalID, diff, err := s.ConfigProposals.File(ctx, ref.ProjectID, key, value)
	if err != nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error()}
	}
	if err := s.ConfigProposals.Apply(ctx, proposalID, operatorID); err != nil {
		if errors.Is(err, ErrActionConflict) {
			return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected, Detail: err.Error(), Diff: diff}
		}
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error(), Diff: diff}
	}
	return &DispatchResult{Kind: action.Kind, Result: ActionResultApplied, Detail: "config change applied", Diff: diff, RollbackID: proposalID}
}

func (s *Service) dispatchRetryTask(ctx context.Context, ref FailureRef, action ProposedAction) *DispatchResult {
	if s.ActionTaskRetrier == nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed,
			Detail: "retry_task is not configured on this deployment"}
	}
	taskID := action.Params["task_id"]
	detail, err := s.ActionTaskRetrier.Retry(ctx, ref.ProjectID, taskID)
	if err != nil {
		if errors.Is(err, ErrActionConflict) {
			return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected, Detail: err.Error()}
		}
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error()}
	}
	return &DispatchResult{Kind: action.Kind, Result: ActionResultApplied, Detail: detail}
}

func (s *Service) dispatchReprobeIntegration(ctx context.Context, ref FailureRef, action ProposedAction) *DispatchResult {
	if s.IntegrationReprober == nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed,
			Detail: "reprobe_integration is not configured on this deployment"}
	}
	integrationID := action.Params["integration_id"]
	summary, _, err := s.IntegrationReprober.Reprobe(ctx, ref.ProjectID, integrationID)
	if err != nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error()}
	}
	// A re-probe always "applies" — it re-runs a live check, regardless of
	// whether the fresh result is healthy — the action executed cleanly;
	// the resulting color is reported in Detail, not the dispatch Result.
	return &DispatchResult{Kind: action.Kind, Result: ActionResultApplied, Detail: summary}
}

func (s *Service) dispatchSetSecret(ctx context.Context, ref FailureRef, action ProposedAction, secretValue string) *DispatchResult {
	if s.SecretSetter == nil {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed,
			Detail: "set_secret is not configured on this deployment"}
	}
	if secretValue == "" {
		return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected, Detail: "a secret value is required"}
	}
	field := action.Params["key"]
	// secretValue is the ONLY source of the value — action.Params is never
	// consulted for anything value-shaped (validActionParams for
	// ActionKindSetSecret doesn't even require a "value" param to exist).
	if err := s.SecretSetter.Set(ctx, ref.ProjectID, field, secretValue); err != nil {
		if errors.Is(err, ErrActionConflict) {
			return &DispatchResult{Kind: action.Kind, Result: ActionResultRejected, Detail: err.Error()}
		}
		return &DispatchResult{Kind: action.Kind, Result: ActionResultFailed, Detail: err.Error()}
	}
	// Detail deliberately omits the field name's value and the value
	// itself — never echo secret material back into the transcript/audit.
	return &DispatchResult{Kind: action.Kind, Result: ActionResultApplied, Detail: "secret updated"}
}

// auditApplied writes one AdminAuditEntry for a successfully applied
// action (§5.6). Best-effort + nil-safe (see Service.Audit's doc
// comment) — an audit-sink failure must not turn an already-applied fix
// into an error response.
func (s *Service) auditApplied(ctx context.Context, ref FailureRef, operatorID string, action ProposedAction, result *DispatchResult) {
	if s.Audit == nil {
		return
	}
	entry := &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Principal: operatorID,
		Source:    "ui",
		Action:    fixitActionAuditPrefix + string(action.Kind),
		Target:    ref.ID,
		After:     result.Diff,
	}
	_ = s.Audit.Insert(ctx, entry)
}

// auditRollback writes one AdminAuditEntry for a config_apply rollback
// attempt on BOTH outcomes — success and failure alike. This is
// deliberately asymmetric with auditApplied (applied-only): a FAILED
// rollback (config corruption, missing pre-apply snapshot,
// ApplyEngine.Rollback error) is exactly the kind of event an operator
// needs a trail for — silently dropping it here would be the one gap
// left after config_apply itself is fully audited. Mirrors
// auditApplied's shape (Principal/Source/Target); Before carries the
// proposal id under rollback and After carries the human-readable
// outcome (success detail or the pipeline error), so both outcomes are
// diff-able the same way a config_apply's Before/After pair is.
// Best-effort + nil-safe, same as auditApplied.
func (s *Service) auditRollback(ctx context.Context, ref FailureRef, operatorID, proposalID string, result *DispatchResult) {
	if s.Audit == nil {
		return
	}
	entry := &persistence.AdminAuditEntry{
		ID:        persistence.GenerateID("admaud"),
		Principal: operatorID,
		Source:    "ui",
		Action:    fixitActionAuditPrefix + "config_apply_rollback",
		Target:    ref.ID,
		Before:    proposalID,
		After:     result.Detail,
	}
	_ = s.Audit.Insert(ctx, entry)
}

// recordAppliedAction appends one appliedActionRecord to the session's
// applied_actions JSONB column and streams the result into the
// transcript as a system turn, then persists both in one Update call.
func (s *Service) recordAppliedAction(ctx context.Context, session *persistence.FixItSession, result *DispatchResult) error {
	var records []appliedActionRecord
	if len(session.AppliedActions) > 0 {
		_ = json.Unmarshal(session.AppliedActions, &records) // best-effort; corrupt blob starts a fresh list rather than failing the apply
	}
	records = append(records, appliedActionRecord{
		Kind:       result.Kind,
		Result:     result.Result,
		Detail:     result.Detail,
		RollbackID: result.RollbackID,
		AppliedAt:  time.Now().UTC(),
	})
	recordsJSON, err := json.Marshal(records)
	if err != nil {
		return err
	}
	session.AppliedActions = recordsJSON

	transcript, err := decodeTranscript(session.Transcript)
	if err != nil {
		transcript = nil // corrupt transcript degrades to "start fresh" rather than blocking the apply
	}
	transcript = append(transcript, Turn{Role: "system", Content: result.Detail, CreatedAt: time.Now().UTC()})
	transcriptJSON, err := json.Marshal(transcript)
	if err != nil {
		return err
	}
	session.Transcript = transcriptJSON

	return s.Sessions.Update(ctx, session)
}

// lastProposedAction returns the actionIndex'th action from envelopeJSON
// (a session's LastEnvelope column), or ok=false when there's no
// envelope, it doesn't decode, or the index is out of range.
func lastProposedAction(envelopeJSON []byte, actionIndex int) (ProposedAction, bool) {
	if len(envelopeJSON) == 0 || actionIndex < 0 {
		return ProposedAction{}, false
	}
	var env FixItEnvelope
	if err := json.Unmarshal(envelopeJSON, &env); err != nil {
		return ProposedAction{}, false
	}
	if actionIndex >= len(env.Actions) {
		return ProposedAction{}, false
	}
	return env.Actions[actionIndex], true
}
