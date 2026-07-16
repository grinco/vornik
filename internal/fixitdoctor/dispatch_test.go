package fixitdoctor

// Task 3.3 dispatcher tests (fix-it-doctor-design.md §8). Covers the
// safety invariant end to end: unknown/param-invalid kinds refused;
// each kind routes to its OWN pipeline only (spy fakes assert no
// cross-talk); nothing executes without an explicit Dispatch call;
// CE has no config_apply path; set_secret's value never comes from the
// model; scope/IDOR refusals; audit + applied_actions recording.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/version"
)

// --- pipeline fakes ------------------------------------------------------

type fakeGatePipeline struct {
	planCalls  []string
	applyCalls []string
	applyErr   error
	detail     string
	diff       string
}

func (f *fakeGatePipeline) Plan(_ context.Context, key string) (string, error) {
	f.planCalls = append(f.planCalls, key)
	return f.diff, nil
}

func (f *fakeGatePipeline) Apply(_ context.Context, key string) (string, error) {
	f.applyCalls = append(f.applyCalls, key)
	if f.applyErr != nil {
		return "", f.applyErr
	}
	return f.detail, nil
}

type fakeConfigProposalPipeline struct {
	fileCalls     []string // "projectID|key|value"
	applyCalls    []string // "proposalID|actor"
	rollbackCalls []string
	proposalID    string
	diff          string
	fileErr       error
	applyErr      error
	rollbackErr   error
}

func (f *fakeConfigProposalPipeline) File(_ context.Context, projectID, key, value string) (string, string, error) {
	f.fileCalls = append(f.fileCalls, projectID+"|"+key+"|"+value)
	if f.fileErr != nil {
		return "", "", f.fileErr
	}
	id := f.proposalID
	if id == "" {
		id = "prop_test1"
	}
	return id, f.diff, nil
}

func (f *fakeConfigProposalPipeline) Apply(_ context.Context, proposalID, actor string) error {
	f.applyCalls = append(f.applyCalls, proposalID+"|"+actor)
	return f.applyErr
}

func (f *fakeConfigProposalPipeline) Rollback(_ context.Context, proposalID string) error {
	f.rollbackCalls = append(f.rollbackCalls, proposalID)
	return f.rollbackErr
}

type fakeTaskRetrier struct {
	calls  []string // "projectID|taskID"
	detail string
	err    error
}

func (f *fakeTaskRetrier) Retry(_ context.Context, projectID, taskID string) (string, error) {
	f.calls = append(f.calls, projectID+"|"+taskID)
	if f.err != nil {
		return "", f.err
	}
	return f.detail, nil
}

type fakeIntegrationReprober struct {
	calls   []string // "projectID|integrationID"
	summary string
	healthy bool
	err     error
}

func (f *fakeIntegrationReprober) Reprobe(_ context.Context, projectID, integrationID string) (string, bool, error) {
	f.calls = append(f.calls, projectID+"|"+integrationID)
	if f.err != nil {
		return "", false, f.err
	}
	return f.summary, f.healthy, nil
}

type fakeSecretSetter struct {
	calls []string // "projectID|field|value"
	err   error
	// declared, when non-nil, models the REAL fixitSecretSetter adapter's
	// declared-names gate (internal/service/fixit_dispatch_adapter.go
	// wraps projectdoctor.Doctor.SetSecret, translating its "not declared
	// by project" refusal into ErrActionConflict) — Set rejects any field
	// absent from this set instead of always returning the canned err,
	// so a test can prove the gate discriminates BY FIELD rather than
	// merely exercising Dispatch's generic ErrActionConflict mapping.
	declared map[string]bool
}

func (f *fakeSecretSetter) Set(_ context.Context, projectID, field, value string) error {
	if f.declared != nil && !f.declared[field] {
		return fmt.Errorf("%w: %q is not declared by project %q", ErrActionConflict, field, projectID)
	}
	f.calls = append(f.calls, projectID+"|"+field+"|"+value)
	return f.err
}

type fakeAuditRecorder struct {
	entries []*persistence.AdminAuditEntry
	err     error
}

func (f *fakeAuditRecorder) Insert(_ context.Context, entry *persistence.AdminAuditEntry) error {
	f.entries = append(f.entries, entry)
	return f.err
}

// --- test harness --------------------------------------------------------

// newDispatchTestService builds a Service with a fake session store
// pre-seeded with one open session bound to ref, whose LastEnvelope
// carries the given actions. Returns the service + the store (to
// inspect persisted state) + the session id.
func newDispatchTestService(t *testing.T, ref FailureRef, actions []ProposedAction) (*Service, *fakeFixItSessionStore, string) {
	t.Helper()
	store := newFakeFixItStore()
	env := FixItEnvelope{Message: "proposed fix", Actions: actions}
	envJSON, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	session := &persistence.FixItSession{
		ID:           persistence.GenerateID("fix"),
		OperatorID:   "op-1",
		FailureKind:  string(ref.Kind),
		FailureRefID: ref.ID,
		ProjectID:    ref.ProjectID,
		LastEnvelope: envJSON,
	}
	if err := store.Insert(context.Background(), session); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	svc := &Service{
		Sessions: store,
		Metrics:  NewMetrics(prometheus.NewRegistry()),
		Edition:  version.EditionEnterprise,
	}
	return svc, store, session.ID
}

var testRef = FailureRef{Kind: FailureKindDegradedFeature, ID: "instinct", ProjectID: "proj-1"}

// --- guardrail re-assertion ------------------------------------------------

func TestDispatch_UnknownActionKind_Refused(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKind("shell_exec"), Label: "run a shell command", Params: map[string]string{"cmd": "rm -rf /"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected", res.Result)
	}
	if got := testutilCounterTotal(t, svc.Metrics.GuardrailHitsTotal, GuardrailReasonUnknownKind); got != 1 {
		t.Errorf("guardrail unknown_kind count = %v, want 1", got)
	}
	// Nothing should have mutated the session's applied-action count into a
	// success — verify no AdminAuditEntry-worthy state (no pipeline exists
	// to call in this test, so nothing CAN have executed).
	row, _ := store.Get(context.Background(), sid)
	var records []appliedActionRecord
	_ = json.Unmarshal(row.AppliedActions, &records)
	if len(records) != 1 || records[0].Result != ActionResultRejected {
		t.Fatalf("applied_actions = %+v, want one rejected record", records)
	}
}

func TestDispatch_ParamInvalid_Refused(t *testing.T) {
	// config_apply_gate with no "key" param.
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "enable it", Params: map[string]string{}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	gate := &fakeGatePipeline{}
	svc.GatePipeline = gate

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected", res.Result)
	}
	if len(gate.applyCalls) != 0 {
		t.Fatalf("gate.Apply called %d times, want 0 (param-invalid must never route)", len(gate.applyCalls))
	}
	if got := testutilCounterTotal(t, svc.Metrics.GuardrailHitsTotal, GuardrailReasonParamsInvalid); got != 1 {
		t.Errorf("guardrail params_invalid count = %v, want 1", got)
	}
}

func TestDispatch_InjectionShapedKey_GateRejectsNonRegisteredGate(t *testing.T) {
	// An injection-shaped key that isn't a real gate: the pipeline itself
	// (the real featuredoctor-backed adapter) is the source of truth on
	// "is this a registered gate" — here the fake simulates that refusal
	// via ErrActionConflict, and Dispatch must classify it as REJECTED,
	// never silently succeed.
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "enable it",
		Params: map[string]string{"key": "os.exec.allow_shell"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	gate := &fakeGatePipeline{applyErr: ErrActionConflict}
	svc.GatePipeline = gate

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected for a non-registered gate key", res.Result)
	}
	if len(gate.applyCalls) != 1 || gate.applyCalls[0] != "os.exec.allow_shell" {
		t.Fatalf("gate.Apply calls = %v", gate.applyCalls)
	}
}

// --- config_apply_gate (CE) ------------------------------------------------

func TestDispatch_ConfigApplyGate_Applied(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "turn on instinct",
		Params: map[string]string{"key": "instinct.enabled"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)
	gate := &fakeGatePipeline{detail: "instinct.enabled -> true", diff: "- false\n+ true"}
	svc.GatePipeline = gate
	audit := &fakeAuditRecorder{}
	svc.Audit = audit

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultApplied {
		t.Fatalf("Result = %q, want applied; detail=%q", res.Result, res.Detail)
	}
	if len(gate.applyCalls) != 1 || gate.applyCalls[0] != "instinct.enabled" {
		t.Fatalf("gate.Apply calls = %v", gate.applyCalls)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(audit.entries))
	}
	entry := audit.entries[0]
	if entry.Action != "fixit.action.config_apply_gate" {
		t.Errorf("audit Action = %q, want fixit.action.config_apply_gate", entry.Action)
	}
	if entry.Target != testRef.ID {
		t.Errorf("audit Target = %q, want %q", entry.Target, testRef.ID)
	}
	if entry.Principal != "op-1" {
		t.Errorf("audit Principal = %q, want op-1", entry.Principal)
	}
	if v, err := svc.Metrics.ActionsTotal.GetMetricWithLabelValues("config_apply_gate", ActionResultApplied); err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	} else if got := testutil.ToFloat64(v); got != 1 {
		t.Errorf("actions_total{config_apply_gate,applied} = %v, want 1", got)
	}

	row, _ := store.Get(context.Background(), sid)
	var records []appliedActionRecord
	if err := json.Unmarshal(row.AppliedActions, &records); err != nil {
		t.Fatalf("decode applied_actions: %v", err)
	}
	if len(records) != 1 || records[0].Result != ActionResultApplied || records[0].Kind != ActionKindConfigApplyGate {
		t.Fatalf("applied_actions = %+v", records)
	}
	transcript, err := decodeTranscript(row.Transcript)
	if err != nil || len(transcript) != 1 || transcript[0].Content != "instinct.enabled -> true" {
		t.Fatalf("transcript = %+v, err=%v", transcript, err)
	}
}

func TestDispatch_ConfigApplyGate_FailureNotAudited(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "x", Params: map[string]string{"key": "instinct.enabled"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.GatePipeline = &fakeGatePipeline{applyErr: errors.New("validate failed; restored")}
	audit := &fakeAuditRecorder{}
	svc.Audit = audit

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed", res.Result)
	}
	if len(audit.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0 on a failed apply", len(audit.entries))
	}
}

func TestDispatch_ConfigApplyGate_NotConfigured(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApplyGate, Label: "x", Params: map[string]string{"key": "instinct.enabled"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	// svc.GatePipeline left nil.
	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed (not configured)", res.Result)
	}
}

// --- config_apply (EE only) ------------------------------------------------

func TestDispatch_ConfigApply_CommunityEdition_RejectedWithoutFilingProposal(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApply, Label: "x",
		Params: map[string]string{"key": "chat.timeout", "value": "30s"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.Edition = version.EditionCommunity
	proposals := &fakeConfigProposalPipeline{}
	svc.ConfigProposals = proposals // wired but must never be reached in CE

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected on a Community build", res.Result)
	}
	if len(proposals.fileCalls) != 0 || len(proposals.applyCalls) != 0 {
		t.Fatalf("proposal pipeline reached on CE build: file=%v apply=%v", proposals.fileCalls, proposals.applyCalls)
	}
}

func TestDispatch_ConfigApply_Enterprise_FilesAsFixItDoctor_AppliesAsHumanUser(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApply, Label: "x",
		Params: map[string]string{"key": "chat.timeout", "value": "30s"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions) // Edition already Enterprise
	proposals := &fakeConfigProposalPipeline{proposalID: "prop_77", diff: "- 10s\n+ 30s"}
	svc.ConfigProposals = proposals
	audit := &fakeAuditRecorder{}
	svc.Audit = audit

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultApplied {
		t.Fatalf("Result = %q, want applied; detail=%q", res.Result, res.Detail)
	}
	if res.RollbackID != "prop_77" {
		t.Errorf("RollbackID = %q, want prop_77", res.RollbackID)
	}
	if len(proposals.fileCalls) != 1 || proposals.fileCalls[0] != "proj-1|chat.timeout|30s" {
		t.Fatalf("File calls = %v", proposals.fileCalls)
	}
	// The dispatcher must apply with the HUMAN operator as actor — never
	// "fix_it_doctor" — so proposer (baked into File by the real adapter)
	// and approver (this Apply call) are provably different identities.
	if len(proposals.applyCalls) != 1 || proposals.applyCalls[0] != "prop_77|op-1" {
		t.Fatalf("Apply calls = %v, want actor=op-1 (the human), never fix_it_doctor", proposals.applyCalls)
	}
	if len(audit.entries) != 1 || audit.entries[0].Action != "fixit.action.config_apply" {
		t.Fatalf("audit entries = %+v", audit.entries)
	}
}

func TestDispatch_ConfigApply_FileError_Failed(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApply, Label: "x",
		Params: map[string]string{"key": "chat.timeout", "value": "30s"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.ConfigProposals = &fakeConfigProposalPipeline{fileErr: errors.New("field too large")}

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed", res.Result)
	}
}

func TestDispatch_ConfigApply_ApplyConflict_Rejected(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindConfigApply, Label: "x",
		Params: map[string]string{"key": "chat.timeout", "value": "30s"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.ConfigProposals = &fakeConfigProposalPipeline{applyErr: ErrActionConflict}

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected (drifted base hash)", res.Result)
	}
}

// --- RollbackConfigApply (§5.4 rollback affordance) ------------------------

// seedAppliedConfigApply records an applied config_apply for proposalID on the
// session, so a subsequent rollback of that proposal passes the ownership gate
// (review-20260716-d95b: rollback must target a proposal THIS session applied).
func seedAppliedConfigApply(t *testing.T, store *fakeFixItSessionStore, sid, proposalID string) {
	t.Helper()
	s, err := store.Get(context.Background(), sid)
	if err != nil {
		t.Fatalf("seedAppliedConfigApply get: %v", err)
	}
	recs, err := json.Marshal([]appliedActionRecord{{
		Kind: ActionKindConfigApply, Result: ActionResultApplied, RollbackID: proposalID, AppliedAt: time.Now().UTC(),
	}})
	if err != nil {
		t.Fatalf("seedAppliedConfigApply marshal: %v", err)
	}
	s.AppliedActions = recs
	if err := store.Update(context.Background(), s); err != nil {
		t.Fatalf("seedAppliedConfigApply update: %v", err)
	}
}

func TestRollbackConfigApply_Applied(t *testing.T) {
	svc, store, sid := newDispatchTestService(t, testRef, nil)
	proposals := &fakeConfigProposalPipeline{}
	svc.ConfigProposals = proposals
	audit := &fakeAuditRecorder{}
	svc.Audit = audit
	seedAppliedConfigApply(t, store, sid, "prop_1") // this session applied prop_1

	res, err := svc.RollbackConfigApply(context.Background(), sid, "op-1", "prop_1")
	if err != nil {
		t.Fatalf("RollbackConfigApply: %v", err)
	}
	if res.Result != ActionResultApplied || res.RollbackID != "prop_1" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(proposals.rollbackCalls) != 1 || proposals.rollbackCalls[0] != "prop_1" {
		t.Fatalf("Rollback calls = %v", proposals.rollbackCalls)
	}
	if len(audit.entries) != 1 || audit.entries[0].Action != "fixit.action.config_apply_rollback" {
		t.Fatalf("audit entries = %+v", audit.entries)
	}
	if audit.entries[0].Before != "prop_1" || audit.entries[0].After != "config change rolled back" {
		t.Fatalf("audit entry Before/After = %q/%q", audit.entries[0].Before, audit.entries[0].After)
	}
	row, _ := store.Get(context.Background(), sid)
	var records []appliedActionRecord
	// 2 records: the seeded config_apply + the rollback just recorded.
	if err := json.Unmarshal(row.AppliedActions, &records); err != nil || len(records) != 2 {
		t.Fatalf("applied_actions decode: err=%v records=%+v", err, records)
	}
}

func TestRollbackConfigApply_RejectsUnownedProposal(t *testing.T) {
	// review-20260716-d95b: RollbackConfigApply validated the operator (IDOR) but
	// not that the proposal belongs to this session — an operator could roll back
	// any proposal id. Rolling back a proposal this session never applied must be
	// REJECTED, and the underlying pipeline Rollback must NOT be called.
	svc, _, sid := newDispatchTestService(t, testRef, nil) // no applied actions
	proposals := &fakeConfigProposalPipeline{}
	svc.ConfigProposals = proposals
	svc.Audit = &fakeAuditRecorder{}

	res, err := svc.RollbackConfigApply(context.Background(), sid, "op-1", "prop_stranger")
	if err != nil {
		t.Fatalf("RollbackConfigApply: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected for an unowned proposal", res.Result)
	}
	if len(proposals.rollbackCalls) != 0 {
		t.Fatalf("pipeline Rollback must not be called for an unowned proposal; calls = %v", proposals.rollbackCalls)
	}
}

func TestRollbackConfigApply_NotConfigured(t *testing.T) {
	svc, _, sid := newDispatchTestService(t, testRef, nil)
	res, err := svc.RollbackConfigApply(context.Background(), sid, "op-1", "prop_1")
	if err != nil {
		t.Fatalf("RollbackConfigApply: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed (not configured)", res.Result)
	}
}

// TestRollbackConfigApply_PipelineError_Failed proves a FAILED rollback
// (ApplyEngine.Rollback error — config corruption, missing snapshot,
// etc.) still leaves an audit trail: this was previously asymmetric
// with a successful rollback (see TestRollbackConfigApply_Applied),
// which audited but a failure did not (review-20260710-5a1b.md,
// Important finding).
func TestRollbackConfigApply_PipelineError_Failed(t *testing.T) {
	svc, store, sid := newDispatchTestService(t, testRef, nil)
	svc.ConfigProposals = &fakeConfigProposalPipeline{rollbackErr: errors.New("snapshot missing")}
	audit := &fakeAuditRecorder{}
	svc.Audit = audit
	seedAppliedConfigApply(t, store, sid, "prop_1") // owned, so we reach the pipeline

	res, err := svc.RollbackConfigApply(context.Background(), sid, "op-1", "prop_1")
	if err != nil {
		t.Fatalf("RollbackConfigApply: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed", res.Result)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 on a failed rollback", len(audit.entries))
	}
	entry := audit.entries[0]
	if entry.Action != "fixit.action.config_apply_rollback" {
		t.Fatalf("Action = %q, want fixit.action.config_apply_rollback", entry.Action)
	}
	if entry.Target != testRef.ID {
		t.Fatalf("Target = %q, want %q", entry.Target, testRef.ID)
	}
	if entry.Before != "prop_1" {
		t.Fatalf("Before (proposal id) = %q, want prop_1", entry.Before)
	}
	if entry.After != "snapshot missing" {
		t.Fatalf("After (error detail) = %q, want %q", entry.After, "snapshot missing")
	}
}

func TestRollbackConfigApply_ForeignSession_NotFound(t *testing.T) {
	svc, _, sid := newDispatchTestService(t, testRef, nil)
	_, err := svc.RollbackConfigApply(context.Background(), sid, "someone-else", "prop_1")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (IDOR)", err)
	}
}

// --- retry_task -------------------------------------------------------------

func TestDispatch_RetryTask_Applied(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "retry", Params: map[string]string{"task_id": "task-9"}}}
	ref := FailureRef{Kind: FailureKindFailedTask, ID: "task-9", ProjectID: "proj-1"}
	svc, _, sid := newDispatchTestService(t, ref, actions)
	retrier := &fakeTaskRetrier{detail: "task re-queued"}
	svc.ActionTaskRetrier = retrier

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultApplied {
		t.Fatalf("Result = %q, want applied", res.Result)
	}
	if len(retrier.calls) != 1 || retrier.calls[0] != "proj-1|task-9" {
		t.Fatalf("Retry calls = %v", retrier.calls)
	}
}

func TestDispatch_RetryTask_Conflict_Rejected(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "retry", Params: map[string]string{"task_id": "task-9"}}}
	ref := FailureRef{Kind: FailureKindFailedTask, ID: "task-9", ProjectID: "proj-1"}
	svc, _, sid := newDispatchTestService(t, ref, actions)
	svc.ActionTaskRetrier = &fakeTaskRetrier{err: ErrActionConflict}

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected (already retried by another tab)", res.Result)
	}
}

// --- reprobe_integration ------------------------------------------------

func TestDispatch_ReprobeIntegration_Applied(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindReprobeIntegration, Label: "recheck",
		Params: map[string]string{"integration_id": "telegram"}}}
	ref := FailureRef{Kind: FailureKindRedIntegration, ID: "telegram", ProjectID: ""}
	svc, _, sid := newDispatchTestService(t, ref, actions)
	prober := &fakeIntegrationReprober{summary: "telegram: OK", healthy: true}
	svc.IntegrationReprober = prober

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultApplied {
		t.Fatalf("Result = %q, want applied", res.Result)
	}
	if len(prober.calls) != 1 || prober.calls[0] != "|telegram" {
		t.Fatalf("Reprobe calls = %v", prober.calls)
	}
}

// --- set_secret --------------------------------------------------------

func TestDispatch_SetSecret_ValueNeverFromModel(t *testing.T) {
	// The model's proposed params carry only "key" per the envelope
	// contract; even if a compromised/hallucinating model smuggled a
	// "value" into Params, Dispatch must never read it — the ONLY source
	// of the secret value is the Dispatch call's own secretValue arg.
	actions := []ProposedAction{{Kind: ActionKindSetSecret, Label: "set token",
		Params: map[string]string{"key": "TELEGRAM_BOT_TOKEN", "value": "attacker-supplied-value"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	setter := &fakeSecretSetter{}
	svc.SecretSetter = setter

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "user-typed-secret")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultApplied {
		t.Fatalf("Result = %q, want applied", res.Result)
	}
	if len(setter.calls) != 1 || setter.calls[0] != "proj-1|TELEGRAM_BOT_TOKEN|user-typed-secret" {
		t.Fatalf("Set calls = %v, want the Dispatch-supplied value, never the model's", setter.calls)
	}
	// Never leak the value into the transcript/detail.
	if res.Detail == "user-typed-secret" || res.Detail == "attacker-supplied-value" {
		t.Fatalf("Detail leaked a secret value: %q", res.Detail)
	}
}

func TestDispatch_SetSecret_NonDeclaredField_Rejected(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindSetSecret, Label: "set token",
		Params: map[string]string{"key": "NOT_DECLARED"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.SecretSetter = &fakeSecretSetter{err: ErrActionConflict}

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "some-value")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected (field not declared)", res.Result)
	}
}

// TestDispatch_SetSecret_UndeclaredField_RejectedByDeclaredNamesGate is
// the 3.3 review nit (review-76e9): TestDispatch_SetSecret_
// NonDeclaredField_Rejected above only proves Dispatch maps a CANNED
// ErrActionConflict to Rejected — it doesn't prove the declared-names
// gate itself is what discriminates. Here the fake SecretSetter models
// the real gate (declared allowlist, mirroring fixitSecretSetter /
// projectdoctor.Doctor.SetSecret): the SAME envelope's first action (an
// undeclared field) is rejected and its second action (a declared
// field) is applied, proving the fix-it dispatcher genuinely re-checks
// the declared-names list per field rather than trusting the model's
// proposed key wholesale.
func TestDispatch_SetSecret_UndeclaredField_RejectedByDeclaredNamesGate(t *testing.T) {
	actions := []ProposedAction{
		{Kind: ActionKindSetSecret, Label: "set undeclared", Params: map[string]string{"key": "NOT_A_DECLARED_SECRET"}},
		{Kind: ActionKindSetSecret, Label: "set declared", Params: map[string]string{"key": "TELEGRAM_BOT_TOKEN"}},
	}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	setter := &fakeSecretSetter{declared: map[string]bool{"TELEGRAM_BOT_TOKEN": true}}
	svc.SecretSetter = setter

	rejected, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "some-value")
	if err != nil {
		t.Fatalf("Dispatch (undeclared field): %v", err)
	}
	if rejected.Result != ActionResultRejected {
		t.Fatalf("undeclared field: Result = %q, want rejected", rejected.Result)
	}
	if len(setter.calls) != 0 {
		t.Fatalf("undeclared field: Set should not have recorded a call, got %v", setter.calls)
	}

	applied, err := svc.Dispatch(context.Background(), sid, "op-1", 1, "user-typed-secret")
	if err != nil {
		t.Fatalf("Dispatch (declared field): %v", err)
	}
	if applied.Result != ActionResultApplied {
		t.Fatalf("declared field: Result = %q, want applied", applied.Result)
	}
	if len(setter.calls) != 1 || setter.calls[0] != "proj-1|TELEGRAM_BOT_TOKEN|user-typed-secret" {
		t.Fatalf("declared field: Set calls = %v", setter.calls)
	}
}

func TestDispatch_SetSecret_EmptyValue_RejectedWithoutCallingPipeline(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindSetSecret, Label: "set token", Params: map[string]string{"key": "X"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	setter := &fakeSecretSetter{}
	svc.SecretSetter = setter

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected (empty value)", res.Result)
	}
	if len(setter.calls) != 0 {
		t.Fatalf("Set called with empty value: %v", setter.calls)
	}
}

// --- link_out never dispatches a mutation --------------------------------

func TestDispatch_LinkOut_NeverMutates(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindLinkOut, Label: "open docs", Params: map[string]string{"url": "/ui/integrations/telegram"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)

	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultRejected {
		t.Fatalf("Result = %q, want rejected (nothing to apply)", res.Result)
	}
}

// --- scope / IDOR --------------------------------------------------------

func TestDispatch_ForeignSession_NotFound(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "x", Params: map[string]string{"task_id": "t1"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)

	_, err := svc.Dispatch(context.Background(), sid, "someone-else", 0, "")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound (IDOR)", err)
	}
}

func TestDispatch_ClosedSession_Refused(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "x", Params: map[string]string{"task_id": "t1"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)
	if err := store.Close(context.Background(), sid, "op-1"); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err = %v, want ErrSessionClosed", err)
	}
}

func TestDispatch_ActionIndexOutOfRange(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "x", Params: map[string]string{"task_id": "t1"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)

	_, err := svc.Dispatch(context.Background(), sid, "op-1", 5, "")
	if !errors.Is(err, ErrNoSuchAction) {
		t.Fatalf("err = %v, want ErrNoSuchAction", err)
	}
}

func TestDispatch_NoEnvelopeYet(t *testing.T) {
	svc, _, sid := newDispatchTestService(t, testRef, nil)
	_, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if !errors.Is(err, ErrNoSuchAction) {
		t.Fatalf("err = %v, want ErrNoSuchAction", err)
	}
}

// --- not-configured pipelines fail closed, never panic --------------------

func TestDispatch_RetryTask_NotConfigured(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "x", Params: map[string]string{"task_id": "t1"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed (not configured)", res.Result)
	}
}

func TestDispatch_ReprobeIntegration_NotConfigured(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindReprobeIntegration, Label: "x", Params: map[string]string{"integration_id": "telegram"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed (not configured)", res.Result)
	}
}

func TestDispatch_ReprobeIntegration_PipelineError(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindReprobeIntegration, Label: "x", Params: map[string]string{"integration_id": "telegram"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	svc.IntegrationReprober = &fakeIntegrationReprober{err: errors.New("dial timeout")}
	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed", res.Result)
	}
}

func TestDispatch_SetSecret_NotConfigured(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindSetSecret, Label: "x", Params: map[string]string{"key": "X"}}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	res, err := svc.Dispatch(context.Background(), sid, "op-1", 0, "some-value")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Result != ActionResultFailed {
		t.Fatalf("Result = %q, want failed (not configured)", res.Result)
	}
}

func TestDispatch_SecondApply_AppendsToExistingAppliedActions(t *testing.T) {
	actions := []ProposedAction{{Kind: ActionKindRetryTask, Label: "x", Params: map[string]string{"task_id": "t1"}}}
	svc, store, sid := newDispatchTestService(t, testRef, actions)
	svc.ActionTaskRetrier = &fakeTaskRetrier{detail: "requeued"}

	if _, err := svc.Dispatch(context.Background(), sid, "op-1", 0, ""); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	if _, err := svc.Dispatch(context.Background(), sid, "op-1", 0, ""); err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	row, _ := store.Get(context.Background(), sid)
	var records []appliedActionRecord
	if err := json.Unmarshal(row.AppliedActions, &records); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("applied_actions len = %d, want 2 (appended, not overwritten)", len(records))
	}
	transcript, err := decodeTranscript(row.Transcript)
	if err != nil || len(transcript) != 2 {
		t.Fatalf("transcript = %+v, err=%v, want 2 system turns", transcript, err)
	}
}

func TestLastProposedAction_EdgeCases(t *testing.T) {
	if _, ok := lastProposedAction(nil, 0); ok {
		t.Error("nil envelope should not resolve")
	}
	if _, ok := lastProposedAction([]byte("not json"), 0); ok {
		t.Error("corrupt envelope should not resolve")
	}
	if _, ok := lastProposedAction([]byte(`{"message":"m","actions":[]}`), -1); ok {
		t.Error("negative index should not resolve")
	}
}

// --- audit not written on rejection/failure -------------------------------

func TestDispatch_AuditOnlyOnApplied(t *testing.T) {
	// unknown kind -> rejected, no pipeline, no audit.
	actions := []ProposedAction{{Kind: ActionKind("bogus"), Label: "x"}}
	svc, _, sid := newDispatchTestService(t, testRef, actions)
	audit := &fakeAuditRecorder{}
	svc.Audit = audit

	if _, err := svc.Dispatch(context.Background(), sid, "op-1", 0, ""); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(audit.entries) != 0 {
		t.Fatalf("audit entries = %d, want 0 for a rejected action", len(audit.entries))
	}
}
