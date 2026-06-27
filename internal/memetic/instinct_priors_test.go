package memetic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/instinctmodel"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// fakeInstincts is a table-driven fake for both InstinctSource (List)
// and InstinctSink (Upsert / AddEvidence). It records every call so a
// test can assert the gate-off no-op and the rejection write-back
// shape without a live database.
type fakeInstincts struct {
	listRows    []*persistence.Instinct
	listErr     error
	listFilter  persistence.InstinctFilter
	listFilters []persistence.InstinctFilter
	listCalls   int

	upserted   []*persistence.Instinct
	upsertID   string
	upsertErr  error
	evidence   []*persistence.InstinctEvidence
	evidenceOK bool
	evidErr    error

	// apps captures every RecordApplication row (the architect-evidence
	// application-logging surface, review item W2). recordAppErr, when
	// set, makes RecordApplication fail so a test can assert the write
	// error is swallowed and never fails the propose turn.
	apps         []*persistence.InstinctApplication
	recordAppErr error
}

func (f *fakeInstincts) RecordApplication(_ context.Context, app *persistence.InstinctApplication) error {
	f.apps = append(f.apps, app)
	return f.recordAppErr
}

func (f *fakeInstincts) List(_ context.Context, filter persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	f.listCalls++
	f.listFilter = filter
	f.listFilters = append(f.listFilters, filter)
	return f.listRows, f.listErr
}

func (f *fakeInstincts) Upsert(_ context.Context, in *persistence.Instinct) (string, error) {
	f.upserted = append(f.upserted, in)
	if f.upsertErr != nil {
		return "", f.upsertErr
	}
	if f.upsertID == "" {
		return "inst_1", nil
	}
	return f.upsertID, nil
}

func (f *fakeInstincts) AddEvidence(_ context.Context, ev *persistence.InstinctEvidence) (bool, error) {
	f.evidence = append(f.evidence, ev)
	if f.evidErr != nil {
		return false, f.evidErr
	}
	return f.evidenceOK, nil
}

func (f *fakeInstincts) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	return nil
}

func (f *fakeInstincts) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	return nil, nil
}

// workflowPrior builds an *persistence.Instinct keyed to workflowID via
// the same canonical trigger the architect queries on, so loadPriors
// matches it.
func workflowPrior(t *testing.T, workflowID, action, source string, confidence float64, support, contradict int) *persistence.Instinct {
	t.Helper()
	trig := workflowInstinctTrigger(workflowID)
	tj, err := instinctmodel.MarshalTrigger(trig)
	if err != nil {
		t.Fatalf("marshal trigger: %v", err)
	}
	return &persistence.Instinct{
		ID:              "inst_" + action,
		Scope:           persistence.InstinctScopeProject,
		Domain:          persistence.InstinctDomainWorkflow,
		TriggerKey:      instinctmodel.TriggerKey(persistence.InstinctDomainWorkflow, trig),
		Trigger:         tj,
		Action:          action,
		Confidence:      confidence,
		SupportCount:    support,
		ContradictCount: contradict,
		Source:          source,
		Status:          persistence.InstinctStatusActive,
	}
}

func newArchitectWithInstincts(content string, evidence []string, fi *fakeInstincts) (*Architect, *stubProposalSink, *stubProvider) {
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	provider := &stubProvider{content: content}
	var opts []ArchitectOption
	if fi != nil {
		opts = append(opts, WithInstincts(fi))
	}
	a := New(
		provider,
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		sink,
		DefaultConfig(),
		opts...,
	)
	return a, sink, provider
}

// recoveryPriorInstinct builds a recovery-domain instinct keyed on
// {role, error_class} — the shape the observer's extraction worker
// mines from failure→ok transitions.
func recoveryPriorInstinct(t *testing.T, role, errorClass, action string, confidence float64, support, contradict int, status string) *persistence.Instinct {
	t.Helper()
	trig := instinctmodel.Trigger{Role: role, ErrorClass: errorClass}
	tj, err := instinctmodel.MarshalTrigger(trig)
	if err != nil {
		t.Fatalf("marshal trigger: %v", err)
	}
	return &persistence.Instinct{
		ID:              "inst_rec_" + action,
		Scope:           persistence.InstinctScopeProject,
		Domain:          persistence.InstinctDomainRecovery,
		TriggerKey:      instinctmodel.TriggerKey(persistence.InstinctDomainRecovery, trig),
		Trigger:         tj,
		Action:          action,
		Confidence:      confidence,
		SupportCount:    support,
		ContradictCount: contradict,
		Source:          persistence.InstinctSourceObserver,
		Status:          status,
	}
}

// newArchitectWithInstinctsAndRollup mirrors newArchitectWithInstincts
// but lets the test pin the telemetry rollup (the recovery-prior match
// is keyed on the rollup's failing {role, error_class} pairs).
func newArchitectWithInstinctsAndRollup(content string, evidence []string, fi *fakeInstincts, rollup *workflowtelemetry.Rollup) (*Architect, *stubProposalSink, *stubProvider) {
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	provider := &stubProvider{content: content}
	var opts []ArchitectOption
	if fi != nil {
		opts = append(opts, WithInstincts(fi))
	}
	a := New(
		provider,
		&stubTelemetry{rollup: rollup},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		sink,
		DefaultConfig(),
		opts...,
	)
	return a, sink, provider
}

// failingRollup is a rollup whose build step keeps failing with
// container_non_zero_exit — the shape that should pull matching
// recovery-domain instincts into the prompt.
func failingRollup() *workflowtelemetry.Rollup {
	return &workflowtelemetry.Rollup{
		WorkflowID:   "simple-workflow",
		RunCount:     9,
		FailureCount: 4,
		Steps: []workflowtelemetry.StepRollup{
			{StepID: "build", Role: "builder", TopErrorClass: "container_non_zero_exit"},
		},
		TopFailureClasses: []workflowtelemetry.FailureClassCount{
			{ErrorClass: "container_non_zero_exit", Count: 4},
		},
	}
}

// TestArchitect_RecoveryPriors_SurfacedForFailingClasses — recovery-
// domain instincts whose trigger matches a failure class the rollup
// actually observed are surfaced in the prompt; non-matching classes,
// retired rows, and contradict-majority rows are not. Regression for
// the 2026-06-06 report: the architect proposed structural changes
// blind to recovery actions that already resolved the failures.
func TestArchitect_RecoveryPriors_SurfacedForFailingClasses(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"the build step keeps failing", evidence, 0.65)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			// Matching {role, error_class} → surfaced.
			recoveryPriorInstinct(t, "builder", "container_non_zero_exit",
				"retrying the step resolved the container_non_zero_exit failure", 0.57, 5, 0, persistence.InstinctStatusCandidate),
			// Class-only trigger (no role) → surfaced.
			recoveryPriorInstinct(t, "", "container_non_zero_exit",
				"switching off model X resolved the container_non_zero_exit failure", 0.41, 3, 0, persistence.InstinctStatusCandidate),
			// Failure class the rollup never saw → NOT surfaced.
			recoveryPriorInstinct(t, "builder", "context_timeout",
				"retrying the step resolved the context_timeout failure", 0.62, 4, 0, persistence.InstinctStatusCandidate),
			// Retired → NOT surfaced.
			recoveryPriorInstinct(t, "builder", "container_non_zero_exit",
				"a retired recovery for container_non_zero_exit", 0.12, 4, 2, persistence.InstinctStatusRetired),
			// Contradict-majority (signal decayed) → NOT surfaced.
			recoveryPriorInstinct(t, "builder", "container_non_zero_exit",
				"a contradicted recovery for container_non_zero_exit", 0.2, 1, 3, persistence.InstinctStatusCandidate),
		},
	}
	a, _, provider := newArchitectWithInstinctsAndRollup(out, evidence, fi, failingRollup())

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	// Two List calls: workflow-domain priors + recovery-domain priors.
	if fi.listCalls != 2 {
		t.Fatalf("expected 2 List calls (workflow + recovery), got %d", fi.listCalls)
	}
	var sawRecoveryFilter bool
	for _, f := range fi.listFilters {
		if f.Domain != nil && *f.Domain == persistence.InstinctDomainRecovery {
			sawRecoveryFilter = true
		}
	}
	if !sawRecoveryFilter {
		t.Errorf("no List call filtered by the recovery domain: %+v", fi.listFilters)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if !strings.Contains(userMsg, "Observed recovery patterns") {
		t.Fatalf("prompt missing recovery-priors section:\n%s", userMsg)
	}
	if !strings.Contains(userMsg, "retrying the step resolved the container_non_zero_exit failure") {
		t.Errorf("prompt missing matching {role,class} recovery action")
	}
	if !strings.Contains(userMsg, "switching off model X resolved the container_non_zero_exit failure") {
		t.Errorf("prompt missing matching class-only recovery action")
	}
	if strings.Contains(userMsg, "context_timeout failure") {
		t.Errorf("prompt leaked a recovery action for a class the rollup never observed")
	}
	if strings.Contains(userMsg, "a retired recovery") {
		t.Errorf("prompt leaked a retired recovery instinct")
	}
	if strings.Contains(userMsg, "a contradicted recovery") {
		t.Errorf("prompt leaked a contradict-majority recovery instinct")
	}
}

// TestArchitect_RecoveryPriors_NoFailuresNoQuery — a rollup with no
// failing classes must not even query the recovery domain (and the
// prompt carries no recovery section). Keeps the gate-off/healthy-
// workflow propose turn byte-for-byte unchanged.
func TestArchitect_RecoveryPriors_NoFailuresNoQuery(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"telemetry is healthy", evidence, 0.65)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			recoveryPriorInstinct(t, "builder", "container_non_zero_exit",
				"retrying the step resolved the container_non_zero_exit failure", 0.57, 5, 0, persistence.InstinctStatusCandidate),
		},
	}
	healthy := &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}
	a, _, provider := newArchitectWithInstinctsAndRollup(out, evidence, fi, healthy)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if fi.listCalls != 1 {
		t.Fatalf("expected 1 List call (workflow priors only), got %d", fi.listCalls)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "Observed recovery patterns") {
		t.Errorf("recovery section rendered for a rollup with no failures")
	}
}

// TestArchitect_Priors_CitedWhenWired — a positive workflow-domain
// instinct is surfaced in the prompt AND folded into the proposal's
// motivation; a stronger prior raises the proposal confidence.
func TestArchitect_Priors_CitedWhenWired(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"telemetry shows the implement loop dominates failures",
		evidence, 0.65)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			workflowPrior(t, "simple-workflow",
				"adding a verify step before review correlated with success", persistence.InstinctSourceObserver, 0.82, 7, 1),
		},
	}
	a, sink, provider := newArchitectWithInstincts(out, evidence, fi)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if fi.listCalls != 1 {
		t.Fatalf("expected exactly 1 List call, got %d", fi.listCalls)
	}
	if fi.listFilter.Domain == nil || *fi.listFilter.Domain != persistence.InstinctDomainWorkflow {
		t.Errorf("List should filter by workflow domain, got %+v", fi.listFilter)
	}
	// Prompt should carry the priors section + the action text.
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if !strings.Contains(userMsg, "Learned priors for this workflow") {
		t.Errorf("prompt missing priors section:\n%s", userMsg)
	}
	if !strings.Contains(userMsg, "adding a verify step before review") {
		t.Errorf("prompt missing prior action text")
	}
	// Proposal motivation should cite the prior.
	if !strings.Contains(sink.inserted.Motivation, "Learned priors (instinct layer)") {
		t.Errorf("motivation not augmented: %q", sink.inserted.Motivation)
	}
	if !strings.Contains(sink.inserted.Motivation, "adding a verify step before review") {
		t.Errorf("motivation missing prior action: %q", sink.inserted.Motivation)
	}
	// Confidence raised from 0.65 toward the 0.82 prior.
	if got.Confidence < 0.81 || got.Confidence > 0.83 {
		t.Errorf("confidence should rise to ~0.82, got %v", got.Confidence)
	}
}

// TestArchitect_Priors_NegativeFramedAsDeclined — an architect-reject
// instinct is framed as "do NOT re-propose" and is NOT folded into the
// motivation as support.
func TestArchitect_Priors_NegativeFramedAsDeclined(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"base motivation", evidence, 0.7)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			workflowPrior(t, "simple-workflow",
				"operator rejected a add_step proposal for workflow simple-workflow",
				persistence.InstinctSourceArchitectReject, 0.5, 0, 3),
		},
	}
	a, sink, provider := newArchitectWithInstincts(out, evidence, fi)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if !strings.Contains(userMsg, "previously DECLINED") {
		t.Errorf("prompt missing declined framing:\n%s", userMsg)
	}
	// Negative prior must not be cited as a positive in the motivation.
	if strings.Contains(sink.inserted.Motivation, "Learned priors (instinct layer)") {
		t.Errorf("negative prior should not be folded into motivation: %q", sink.inserted.Motivation)
	}
	if sink.inserted.Motivation != "base motivation" {
		t.Errorf("motivation should be unchanged with only a negative prior: %q", sink.inserted.Motivation)
	}
}

// TestArchitect_Priors_NoopWhenGateOff — no instinct source wired: the
// architect never queries instincts and the proposal is identical to
// the no-priors path (byte-for-byte motivation + confidence).
func TestArchitect_Priors_NoopWhenGateOff(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"telemetry shows the implement loop dominates failures",
		evidence, 0.65)
	a, sink, provider := newArchitectWithInstincts(out, evidence, nil)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Confidence != 0.65 {
		t.Errorf("confidence must be untouched when gate off, got %v", got.Confidence)
	}
	if sink.inserted.Motivation != "telemetry shows the implement loop dominates failures" {
		t.Errorf("motivation must be untouched when gate off: %q", sink.inserted.Motivation)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "Learned priors") {
		t.Errorf("prompt must not contain priors section when gate off")
	}
}

// TestArchitect_Priors_OtherWorkflowIgnored — a workflow-domain
// instinct for a DIFFERENT workflow must not leak into this proposal.
func TestArchitect_Priors_OtherWorkflowIgnored(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.7)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			workflowPrior(t, "other-workflow", "irrelevant action", persistence.InstinctSourceObserver, 0.9, 9, 0),
		},
	}
	a, sink, provider := newArchitectWithInstincts(out, evidence, fi)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Confidence != 0.7 {
		t.Errorf("other-workflow prior must not raise confidence, got %v", got.Confidence)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "irrelevant action") {
		t.Errorf("other-workflow prior leaked into prompt")
	}
	if strings.Contains(sink.inserted.Motivation, "irrelevant action") {
		t.Errorf("other-workflow prior leaked into motivation")
	}
}

// TestArchitect_Priors_RetiredAndNilSkipped — retired instincts and
// nil rows are filtered out of the priors set.
func TestArchitect_Priors_RetiredAndNilSkipped(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.7)
	retired := workflowPrior(t, "simple-workflow", "retired action", persistence.InstinctSourceObserver, 0.9, 9, 0)
	retired.Status = persistence.InstinctStatusRetired
	fi := &fakeInstincts{listRows: []*persistence.Instinct{nil, retired}}
	a, sink, provider := newArchitectWithInstincts(out, evidence, fi)

	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if got.Confidence != 0.7 {
		t.Errorf("retired prior must not raise confidence, got %v", got.Confidence)
	}
	if strings.Contains(sink.inserted.Motivation, "retired action") {
		t.Error("retired prior leaked into motivation")
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "retired action") {
		t.Error("retired prior leaked into prompt")
	}
}

// TestArchitect_Priors_ListErrorIsBestEffort — a List error must not
// fail the propose turn; the proposal lands without priors.
func TestArchitect_Priors_ListErrorIsBestEffort(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.7)
	fi := &fakeInstincts{listErr: context.DeadlineExceeded}
	a, _, _ := newArchitectWithInstincts(out, evidence, fi)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("List error must not fail propose, got %v", err)
	}
}

// TestArchitect_RecordRejection_WritesContradiction — the rejection
// write-back upserts a workflow-domain architect-reject instinct and
// attaches a contradict evidence row keyed on the proposal ID.
func TestArchitect_RecordRejection_WritesContradiction(t *testing.T) {
	fi := &fakeInstincts{upsertID: "inst_reject_1"}
	a, _, _ := newArchitectWithInstincts("", nil, fi)

	p := &persistence.WorkflowProposal{
		ID:         "wpr_xyz",
		WorkflowID: "simple-workflow",
		Kind:       persistence.WorkflowProposalKind("add_step"),
	}
	if err := a.RecordRejection(context.Background(), fi, p); err != nil {
		t.Fatalf("RecordRejection: %v", err)
	}
	if len(fi.upserted) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(fi.upserted))
	}
	inst := fi.upserted[0]
	if inst.Domain != persistence.InstinctDomainWorkflow {
		t.Errorf("domain: %q", inst.Domain)
	}
	if inst.Source != persistence.InstinctSourceArchitectReject {
		t.Errorf("source: %q", inst.Source)
	}
	if inst.Scope != persistence.InstinctScopeProject {
		t.Errorf("scope: %q", inst.Scope)
	}
	// Trigger key must match what loadPriors queries for, so the
	// contradiction lands on the same row a future prior would read.
	wantKey := instinctmodel.TriggerKey(persistence.InstinctDomainWorkflow, workflowInstinctTrigger("simple-workflow"))
	if inst.TriggerKey != wantKey {
		t.Errorf("trigger key mismatch: got %q want %q", inst.TriggerKey, wantKey)
	}
	if !strings.Contains(inst.Action, "add_step") || !strings.Contains(inst.Action, "simple-workflow") {
		t.Errorf("action text: %q", inst.Action)
	}
	if len(fi.evidence) != 1 {
		t.Fatalf("expected 1 evidence row, got %d", len(fi.evidence))
	}
	ev := fi.evidence[0]
	if ev.InstinctID != "inst_reject_1" {
		t.Errorf("evidence instinct id: %q", ev.InstinctID)
	}
	if ev.OutcomeID != "wpr_xyz" {
		t.Errorf("evidence outcome id (should be proposal id): %q", ev.OutcomeID)
	}
	if ev.Polarity != persistence.InstinctPolarityContradict {
		t.Errorf("evidence polarity: %q", ev.Polarity)
	}
}

// TestArchitect_RecordRejection_UnspecifiedKind — an untagged proposal
// produces the generic action text.
func TestArchitect_RecordRejection_UnspecifiedKind(t *testing.T) {
	fi := &fakeInstincts{}
	a, _, _ := newArchitectWithInstincts("", nil, fi)
	p := &persistence.WorkflowProposal{ID: "wpr_u", WorkflowID: "wf-u"}
	if err := a.RecordRejection(context.Background(), fi, p); err != nil {
		t.Fatalf("RecordRejection: %v", err)
	}
	if len(fi.upserted) != 1 {
		t.Fatalf("expected 1 upsert")
	}
	act := fi.upserted[0].Action
	if !strings.Contains(act, "structural proposal") || !strings.Contains(act, "wf-u") {
		t.Errorf("unexpected generic action: %q", act)
	}
}

// TestArchitect_RecordRejection_UpsertError — an upsert failure
// surfaces as an error and no evidence is written.
func TestArchitect_RecordRejection_UpsertError(t *testing.T) {
	fi := &fakeInstincts{upsertErr: context.DeadlineExceeded}
	a, _, _ := newArchitectWithInstincts("", nil, fi)
	p := &persistence.WorkflowProposal{ID: "wpr_e", WorkflowID: "wf-e"}
	if err := a.RecordRejection(context.Background(), fi, p); err == nil {
		t.Error("upsert error should surface")
	}
	if len(fi.evidence) != 0 {
		t.Error("no evidence should be written when upsert fails")
	}
}

// TestArchitect_RecordRejection_EvidenceError — an AddEvidence failure
// surfaces as an error.
func TestArchitect_RecordRejection_EvidenceError(t *testing.T) {
	fi := &fakeInstincts{evidErr: context.DeadlineExceeded}
	a, _, _ := newArchitectWithInstincts("", nil, fi)
	p := &persistence.WorkflowProposal{ID: "wpr_ee", WorkflowID: "wf-ee", Kind: persistence.WorkflowProposalKind("add_step")}
	if err := a.RecordRejection(context.Background(), fi, p); err == nil {
		t.Error("add-evidence error should surface")
	}
}

// TestArchitect_RecordRejection_NilSinkNoop — a nil sink (gate off) is
// a silent no-op.
func TestArchitect_RecordRejection_NilSinkNoop(t *testing.T) {
	a, _, _ := newArchitectWithInstincts("", nil, nil)
	p := &persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "w"}
	if err := a.RecordRejection(context.Background(), nil, p); err != nil {
		t.Errorf("nil sink should be a no-op, got %v", err)
	}
}

// TestArchitect_RecordRejection_NoWorkflowID — a proposal with no
// workflow_id errors rather than writing a junk instinct.
func TestArchitect_RecordRejection_NoWorkflowID(t *testing.T) {
	fi := &fakeInstincts{}
	a, _, _ := newArchitectWithInstincts("", nil, fi)
	p := &persistence.WorkflowProposal{ID: "wpr_1", WorkflowID: "  "}
	if err := a.RecordRejection(context.Background(), fi, p); err == nil {
		t.Error("empty workflow_id should error")
	}
	if len(fi.upserted) != 0 {
		t.Error("no instinct should be written on a bad proposal")
	}
}

// TestArchitect_Priors_InstinctIDsRecorded — 2026-06-07 architecture
// review of continuous-learning-instinct-layer-design.md, finding 3
// (validated): applyPriors folded only the prior's ACTION TEXT into
// the motivation and discarded the instinct IDs, so a proposal could
// not be traced back to the instincts that shaped it. The IDs must
// land on the proposal row.
func TestArchitect_Priors_InstinctIDsRecorded(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML,
		"telemetry shows the implement loop dominates failures",
		evidence, 0.65)
	fi := &fakeInstincts{
		listRows: []*persistence.Instinct{
			workflowPrior(t, "simple-workflow",
				"adding a verify step before review correlated with success", persistence.InstinctSourceObserver, 0.82, 7, 1),
			// Negative prior — must NOT be recorded (it is not folded
			// into the proposal as support).
			workflowPrior(t, "simple-workflow",
				"operators declined widening the retry budget", persistence.InstinctSourceArchitectReject, 0.4, 1, 5),
		},
	}
	a, sink, _ := newArchitectWithInstincts(out, evidence, fi)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	want := "inst_adding a verify step before review correlated with success"
	if len(sink.inserted.InstinctIDs) != 1 || sink.inserted.InstinctIDs[0] != want {
		t.Errorf("InstinctIDs = %v, want exactly [%q] (positive prior only)", sink.inserted.InstinctIDs, want)
	}
}

// --- Architect-evidence application logging (review item W2) ---------------
//
// The architect records an instinct_applications row for every prior it
// consulted on a propose turn: accepted when the proposal lands, rejected
// when the turn fails after priors were loaded. The writer is wired via
// WithApplicationWriter; nil → no-op (gate off). Surface is always
// architect_evidence. These are TDD-red-first for the surfacing half of
// slice 7.

// newArchitectWithAppLogging mirrors newArchitectWithInstincts but also
// wires the ApplicationWriter (the fake) and optional metrics, and lets a
// test pre-invalidate the proposed YAML or force a low confidence by
// passing the raw LLM output.
func newArchitectWithAppLogging(out string, evidence []string, fi *fakeInstincts, m *observability.InstinctMetrics) (*Architect, *stubProposalSink) {
	sink := &stubProposalSink{}
	lookup := &stubExecLookup{validIDs: map[string]bool{}}
	for _, id := range evidence {
		lookup.validIDs[id] = true
	}
	provider := &stubProvider{content: out}
	var opts []ArchitectOption
	if fi != nil {
		opts = append(opts, WithInstincts(fi), WithApplicationWriter(fi))
	}
	if m != nil {
		opts = append(opts, WithInstinctMetrics(m))
	}
	a := New(
		provider,
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{WorkflowID: "simple-workflow", RunCount: 9}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		lookup,
		sink,
		DefaultConfig(),
		opts...,
	)
	return a, sink
}

// TestArchitect_AppLog_AcceptedOnPropose — a positive prior consulted on a
// successful propose turn is recorded as an accepted architect-evidence
// application.
func TestArchitect_AppLog_AcceptedOnPropose(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.65)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "adding a verify step", persistence.InstinctSourceObserver, 0.82, 7, 1),
	}}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(fi.apps) != 1 {
		t.Fatalf("expected 1 application row, got %d", len(fi.apps))
	}
	app := fi.apps[0]
	if app.Surface != persistence.InstinctSurfaceArchitectEvidence {
		t.Errorf("surface = %q", app.Surface)
	}
	if app.Result != persistence.InstinctResultAccepted {
		t.Errorf("result = %q, want accepted", app.Result)
	}
	if app.InstinctID != "inst_adding a verify step" {
		t.Errorf("instinct id = %q", app.InstinctID)
	}
}

// TestArchitect_AppLog_RejectedOnLowConfidence — a propose turn that fails
// the confidence floor AFTER priors were loaded records the priors as
// rejected.
func TestArchitect_AppLog_RejectedOnLowConfidence(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.10)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "adding a verify step", persistence.InstinctSourceObserver, 0.82, 7, 1),
	}}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err == nil {
		t.Fatal("expected ErrLowConfidence")
	}
	if len(fi.apps) != 1 {
		t.Fatalf("expected 1 application row, got %d", len(fi.apps))
	}
	if fi.apps[0].Result != persistence.InstinctResultRejected {
		t.Errorf("result = %q, want rejected", fi.apps[0].Result)
	}
}

// TestArchitect_AppLog_RejectedOnInvalidYAML — a propose turn that fails
// the YAML-validity gate after priors were loaded records rejected.
func TestArchitect_AppLog_RejectedOnInvalidYAML(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", "", "base", evidence, 0.9) // empty proposed YAML
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "adding a verify step", persistence.InstinctSourceObserver, 0.82, 7, 1),
	}}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err == nil {
		t.Fatal("expected ErrProposalYAMLInvalid")
	}
	if len(fi.apps) != 1 || fi.apps[0].Result != persistence.InstinctResultRejected {
		t.Fatalf("expected 1 rejected application, got %+v", fi.apps)
	}
}

// TestArchitect_AppLog_RateLimitedNotRejected — a propose turn that produces
// a VALID proposal but is throttled at insert (ErrProposalRateLimited) must
// NOT record the priors as rejected. The priors were not declined on quality
// grounds; the turn was throttled because a proposal is already pending.
// Recording rejected here would feed spurious negative lift and erode good
// workflow instincts on every throttled tick.
func TestArchitect_AppLog_RateLimitedNotRejected(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.9)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "adding a verify step", persistence.InstinctSourceObserver, 0.82, 7, 1),
	}}
	a, sink := newArchitectWithAppLogging(out, evidence, fi, nil)
	sink.err = persistence.ErrProposalRateLimited

	if _, err := a.Propose(context.Background(), "simple-workflow"); !errors.Is(err, persistence.ErrProposalRateLimited) {
		t.Fatalf("Propose err = %v, want ErrProposalRateLimited", err)
	}
	if len(fi.apps) != 0 {
		t.Fatalf("throttled turn must record NO application rows, got %d (%+v)", len(fi.apps), fi.apps)
	}
}

// TestArchitect_AppLog_NoWriteWhenNoPriors — no priors consulted → no
// application rows on either the accept or the reject path.
func TestArchitect_AppLog_NoWriteWhenNoPriors(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.9)
	fi := &fakeInstincts{} // no rows → no priors
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(fi.apps) != 0 {
		t.Errorf("expected no application rows with no priors, got %d", len(fi.apps))
	}
}

// TestArchitect_AppLog_MultipleInstinctIDs — every positive prior gets its
// own application row on accept.
func TestArchitect_AppLog_MultipleInstinctIDs(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.65)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "action one", persistence.InstinctSourceObserver, 0.7, 5, 0),
		workflowPrior(t, "simple-workflow", "action two", persistence.InstinctSourceObserver, 0.8, 6, 0),
	}}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(fi.apps) != 2 {
		t.Fatalf("expected 2 application rows, got %d", len(fi.apps))
	}
	ids := map[string]bool{}
	for _, app := range fi.apps {
		if app.Result != persistence.InstinctResultAccepted {
			t.Errorf("result = %q, want accepted", app.Result)
		}
		ids[app.InstinctID] = true
	}
	if !ids["inst_action one"] || !ids["inst_action two"] {
		t.Errorf("missing instinct ids: %v", ids)
	}
}

// TestArchitect_AppLog_NilWriterIsNoop — no ApplicationWriter wired (gate
// off): the propose turn lands and nothing panics.
func TestArchitect_AppLog_NilWriterIsNoop(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.9)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "action", persistence.InstinctSourceObserver, 0.7, 5, 0),
	}}
	// Wire instincts only (priors consulted) but NOT the writer.
	a, _, _ := newArchitectWithInstincts(out, evidence, fi)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(fi.apps) != 0 {
		t.Errorf("nil writer should record nothing, got %d", len(fi.apps))
	}
}

// TestArchitect_AppLog_WriteErrorSwallowed — a RecordApplication error must
// not fail the propose turn.
func TestArchitect_AppLog_WriteErrorSwallowed(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.9)
	fi := &fakeInstincts{
		recordAppErr: context.DeadlineExceeded,
		listRows: []*persistence.Instinct{
			workflowPrior(t, "simple-workflow", "action", persistence.InstinctSourceObserver, 0.7, 5, 0),
		},
	}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)
	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("write error must be swallowed, got %v", err)
	}
}

// TestArchitect_AppLog_RejectedIncludesNegativePriors — on the reject path,
// both positive AND negative priors that were loaded are recorded rejected
// (the whole consulted set is implicated when the turn fails).
func TestArchitect_AppLog_RejectedIncludesNegativePriors(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.10)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "positive action", persistence.InstinctSourceObserver, 0.8, 7, 0),
		workflowPrior(t, "simple-workflow", "operator declined this", persistence.InstinctSourceArchitectReject, 0.4, 0, 3),
	}}
	a, _ := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err == nil {
		t.Fatal("expected low-confidence rejection")
	}
	if len(fi.apps) != 2 {
		t.Fatalf("expected both priors recorded on reject, got %d", len(fi.apps))
	}
	ids := map[string]bool{}
	for _, app := range fi.apps {
		if app.Result != persistence.InstinctResultRejected {
			t.Errorf("result = %q, want rejected", app.Result)
		}
		ids[app.InstinctID] = true
	}
	if !ids["inst_positive action"] || !ids["inst_operator declined this"] {
		t.Errorf("reject set should include negative prior: %v", ids)
	}
}

// TestArchitect_AppLog_NegativePriorsNotRecordedAccepted — on the accept
// path only POSITIVE priors (those folded into the proposal) are recorded;
// a negative prior is not recorded as accepted.
func TestArchitect_AppLog_NegativePriorsNotRecordedAccepted(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.65)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "positive action", persistence.InstinctSourceObserver, 0.8, 7, 0),
		workflowPrior(t, "simple-workflow", "operator declined this", persistence.InstinctSourceArchitectReject, 0.4, 0, 3),
	}}
	a, sink := newArchitectWithAppLogging(out, evidence, fi, nil)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(fi.apps) != 1 {
		t.Fatalf("expected only the positive prior recorded accepted, got %d", len(fi.apps))
	}
	if fi.apps[0].InstinctID != "inst_positive action" {
		t.Errorf("accepted application should be the positive prior, got %q", fi.apps[0].InstinctID)
	}
	// The proposal itself only folds the positive prior in.
	if len(sink.inserted.InstinctIDs) != 1 || sink.inserted.InstinctIDs[0] != "inst_positive action" {
		t.Errorf("InstinctIDs = %v", sink.inserted.InstinctIDs)
	}
}

// TestArchitect_AppLog_MetricEmitted — the architect bumps
// ApplicationsTotal{architect_evidence,accepted} when wired with metrics.
func TestArchitect_AppLog_MetricEmitted(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "base", evidence, 0.9)
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		workflowPrior(t, "simple-workflow", "action", persistence.InstinctSourceObserver, 0.7, 5, 0),
	}}
	m := observability.NewInstinctMetrics(prometheus.NewRegistry())
	a, _ := newArchitectWithAppLogging(out, evidence, fi, m)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	got := testutil.ToFloat64(m.ApplicationsTotal.WithLabelValues(
		persistence.InstinctSurfaceArchitectEvidence, persistence.InstinctResultAccepted))
	if got != 1 {
		t.Errorf("ApplicationsTotal{architect_evidence,accepted} = %v, want 1", got)
	}
}
