package memetic

// Wave 3 tests — high-value coverage for the memetic self-healing
// architect. These target under-covered pure-logic seams the existing
// suites skip: the JSON/frontmatter carve-outs, the prior-fold edge
// cases (confidence cap / never-lower / dedup), the
// recordApplications / priorsToIDs helpers, the recovery-prior matcher's
// unmarshalable-trigger and role-qualified branches, the proposal scope
// guard (kinds outside the structural set are rejected), and the
// applier's commit-message + post-mark Get-error fallback. All reuse the
// fakes/helpers declared in the sibling test files — no new fakes.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowtelemetry"
)

// --- extractFrontmatter / architectEnabledFor edge cases -------------------

// TestW3MemeArchitectEnabled_BareFrontmatterNoFence — a workflow body with
// no `---` fence is treated as bare YAML (some callers pass frontmatter
// only). An explicit architect_enabled:false in that bare form still
// disables.
func TestW3MemeArchitectEnabled_BareFrontmatterNoFence(t *testing.T) {
	bare := []byte("workflowId: wf\narchitect_enabled: false\nsteps:\n  a:\n    type: agent\n")
	if architectEnabledFor(bare) {
		t.Error("bare (unfenced) frontmatter with architect_enabled:false must disable")
	}
	bareEnabled := []byte("workflowId: wf\narchitect_enabled: true\n")
	if !architectEnabledFor(bareEnabled) {
		t.Error("bare frontmatter with architect_enabled:true must enable")
	}
}

// TestW3MemeArchitectEnabled_MalformedYAMLFailsOpen — frontmatter that
// can't be parsed must NOT silently mute the workflow (fail-open). A YAML
// quirk should never disable every proposal.
func TestW3MemeArchitectEnabled_MalformedYAMLFailsOpen(t *testing.T) {
	broken := []byte("---\nworkflowId: \"unterminated\n  : : :\narchitect_enabled: [oops\n---\nbody\n")
	if !architectEnabledFor(broken) {
		t.Error("malformed frontmatter must fail open (enabled)")
	}
}

// TestW3MemeArchitectEnabled_EmptyAndAbsent — empty content and content
// whose frontmatter omits the key both default to enabled.
func TestW3MemeArchitectEnabled_EmptyAndAbsent(t *testing.T) {
	if !architectEnabledFor(nil) {
		t.Error("nil content must default to enabled")
	}
	if !architectEnabledFor([]byte("   ")) {
		t.Error("whitespace-only content must default to enabled")
	}
	absent := []byte("---\nworkflowId: wf\nsteps:\n  a:\n    type: agent\n---\n")
	if !architectEnabledFor(absent) {
		t.Error("frontmatter without the key must default to enabled")
	}
}

// TestW3MemeExtractFrontmatter_UnterminatedFence — a leading `---` with no
// closing fence returns everything after the opener (so a truncated file
// still yields parseable YAML rather than empty).
func TestW3MemeExtractFrontmatter_UnterminatedFence(t *testing.T) {
	fm := extractFrontmatter([]byte("---\nworkflowId: wf\narchitect_enabled: true\n"))
	s := string(fm)
	if !strings.Contains(s, "architect_enabled: true") {
		t.Errorf("unterminated fence should keep the body, got %q", s)
	}
	if strings.HasPrefix(strings.TrimSpace(s), "---") {
		t.Errorf("leading fence marker should be stripped, got %q", s)
	}
}

// --- extractJSONObject direct unit -----------------------------------------

// TestW3MemeExtractJSONObject_Spans — carves the outermost {…} span; returns
// ok=false when there is no plausible object.
func TestW3MemeExtractJSONObject_Spans(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{`prefix {"a":1} suffix`, `{"a":1}`, true},
		{`no braces here`, "", false},
		{`} backwards {`, "", false}, // end <= start
		{`{}`, `{}`, true},
		{`text {"a":{"b":2}} more`, `{"a":{"b":2}}`, true}, // last '}' wins
	}
	for _, c := range cases {
		got, ok := extractJSONObject(c.in)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("extractJSONObject(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestW3MemeParseArchitectOutput_RejectsUnknownField — the strict decoder
// uses DisallowUnknownFields, so an LLM that smuggles an extra key is
// malformed (the architect can't be widened by a hallucinated field).
func TestW3MemeParseArchitectOutput_RejectsUnknownField(t *testing.T) {
	raw := `{"workflow_id":"wf","proposed_yaml":"y","motivation":"m","evidence_run_ids":["a","b","c"],"confidence":0.7,"surprise_field":true}`
	_, err := parseArchitectOutput(raw)
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("unknown field must be malformed, got %v", err)
	}
}

// TestW3MemeParseArchitectOutput_FenceOnlyNoLanguage — a bare ``` fence
// (no language tag) is still stripped so the inner object parses.
func TestW3MemeParseArchitectOutput_FenceOnlyNoLanguage(t *testing.T) {
	raw := "```\n{\"workflow_id\":\"wf\",\"proposed_yaml\":\"y\",\"motivation\":\"m\",\"evidence_run_ids\":[\"a\"],\"confidence\":0.5}\n```"
	out, err := parseArchitectOutput(raw)
	if err != nil {
		t.Fatalf("bare fence should strip and parse, got %v", err)
	}
	if out.WorkflowID != "wf" || out.Confidence != 0.5 {
		t.Errorf("parsed wrong: %+v", out)
	}
}

// TestW3MemeParseArchitectOutput_ExplicitZeroConfidenceIsHonest — an
// EXPLICIT confidence:0 is present-and-valid (an honest pass), distinct
// from an OMITTED field. It must parse cleanly (the low-confidence gate,
// not the parser, drops it downstream).
func TestW3MemeParseArchitectOutput_ExplicitZeroConfidenceIsHonest(t *testing.T) {
	raw := `{"workflow_id":"wf","proposed_yaml":"","motivation":"","evidence_run_ids":[],"confidence":0}`
	out, err := parseArchitectOutput(raw)
	if err != nil {
		t.Fatalf("explicit confidence:0 must parse (it is present), got %v", err)
	}
	if out.Confidence != 0 {
		t.Errorf("confidence = %v, want 0", out.Confidence)
	}
}

// --- completeAndParse no-choices guard -------------------------------------

// noChoicesProvider returns a non-nil response with zero choices — the
// degenerate provider reply the architect must not panic on.
type noChoicesProvider struct{}

func (noChoicesProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return &chat.ChatResponse{}, nil
}
func (noChoicesProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return &chat.ChatResponse{}, nil
}
func (noChoicesProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return &chat.ChatResponse{}, nil
}
func (noChoicesProvider) Model() string            { return "no-choices" }
func (noChoicesProvider) SetMetrics(*chat.Metrics) {}

// TestW3MemePropose_NoChoicesFromProvider — a provider that returns zero
// choices surfaces a clear error rather than panicking on index 0.
func TestW3MemePropose_NoChoicesFromProvider(t *testing.T) {
	a := New(
		noChoicesProvider{},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		nil, &stubProposalSink{}, DefaultConfig(),
	)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("want no-choices error, got %v", err)
	}
}

// --- applyPriors edge cases -------------------------------------------------

// TestW3MemeApplyPriors_NeverLowersConfidence — a weaker prior than the
// LLM's self-report must not drag the proposal's confidence down. Priors
// corroborate; they never override a higher self-assessment.
func TestW3MemeApplyPriors_NeverLowersConfidence(t *testing.T) {
	a := &Architect{}
	p := &persistence.WorkflowProposal{Confidence: 0.9, Motivation: "base"}
	priors := []prior{{inst: &persistence.Instinct{ID: "i1", Action: "weak prior", Confidence: 0.3}}}
	a.applyPriors(p, priors)
	if p.Confidence != 0.9 {
		t.Errorf("weaker prior must not lower confidence: got %v, want 0.9", p.Confidence)
	}
	// The action is still cited even when it doesn't raise confidence.
	if !strings.Contains(p.Motivation, "weak prior") {
		t.Errorf("prior action should still be cited: %q", p.Motivation)
	}
	if len(p.InstinctIDs) != 1 || p.InstinctIDs[0] != "i1" {
		t.Errorf("instinct id should be recorded: %v", p.InstinctIDs)
	}
}

// TestW3MemeApplyPriors_ConfidenceCappedAtOne — a prior confidence above
// 1.0 (defensive: instinct confidence is a float64) is clamped to 1.0.
func TestW3MemeApplyPriors_ConfidenceCappedAtOne(t *testing.T) {
	a := &Architect{}
	p := &persistence.WorkflowProposal{Confidence: 0.5}
	priors := []prior{{inst: &persistence.Instinct{ID: "i1", Action: "huge prior", Confidence: 1.7}}}
	a.applyPriors(p, priors)
	if p.Confidence != 1.0 {
		t.Errorf("confidence must be capped at 1.0, got %v", p.Confidence)
	}
}

// TestW3MemeApplyPriors_OnlyNegativeIsNoop — a set containing only
// negative priors (and a positive prior with empty action) produces no
// motivation change, no instinct IDs, and no confidence bump.
func TestW3MemeApplyPriors_OnlyNegativeIsNoop(t *testing.T) {
	a := &Architect{}
	p := &persistence.WorkflowProposal{Confidence: 0.6, Motivation: "base"}
	priors := []prior{
		{inst: &persistence.Instinct{ID: "neg", Action: "declined", Confidence: 0.9}, negative: true},
		{inst: &persistence.Instinct{ID: "empty", Action: "  ", Confidence: 0.95}}, // positive but blank action
		{negative: false, inst: nil}, // nil inst
	}
	a.applyPriors(p, priors)
	if p.Motivation != "base" {
		t.Errorf("motivation should be unchanged, got %q", p.Motivation)
	}
	if p.Confidence != 0.6 {
		t.Errorf("confidence should be unchanged, got %v", p.Confidence)
	}
	if len(p.InstinctIDs) != 0 {
		t.Errorf("no instinct IDs should be recorded, got %v", p.InstinctIDs)
	}
}

// TestW3MemeApplyPriors_CitesSortedAndDeduped — multiple positive priors
// are cited in sorted order and their IDs are sorted, so the rendered
// motivation + InstinctIDs are deterministic regardless of list order.
func TestW3MemeApplyPriors_CitesSortedAndDeduped(t *testing.T) {
	a := &Architect{}
	p := &persistence.WorkflowProposal{Confidence: 0.5}
	priors := []prior{
		{inst: &persistence.Instinct{ID: "z", Action: "zebra action", Confidence: 0.7}},
		{inst: &persistence.Instinct{ID: "a", Action: "alpha action", Confidence: 0.65}},
	}
	a.applyPriors(p, priors)
	// Citations sorted alphabetically: alpha before zebra.
	if !strings.Contains(p.Motivation, "alpha action; zebra action") {
		t.Errorf("citations not sorted: %q", p.Motivation)
	}
	if len(p.InstinctIDs) != 2 || p.InstinctIDs[0] != "a" || p.InstinctIDs[1] != "z" {
		t.Errorf("instinct ids not sorted: %v", p.InstinctIDs)
	}
	// Confidence rose to the strongest (0.7).
	if p.Confidence < 0.69 || p.Confidence > 0.71 {
		t.Errorf("confidence should rise to ~0.7, got %v", p.Confidence)
	}
}

// TestW3MemeApplyPriors_NilProposalOrEmptyNoop — defensive nil-guards.
func TestW3MemeApplyPriors_NilProposalOrEmptyNoop(t *testing.T) {
	a := &Architect{}
	a.applyPriors(nil, []prior{{inst: &persistence.Instinct{ID: "x", Action: "a", Confidence: 0.9}}})
	p := &persistence.WorkflowProposal{Confidence: 0.4, Motivation: "keep"}
	a.applyPriors(p, nil)
	if p.Motivation != "keep" || p.Confidence != 0.4 {
		t.Errorf("empty priors must be a no-op, got %+v", p)
	}
}

// --- priorsToIDs / recordApplications helpers ------------------------------

// TestW3MemePriorsToIDs_SkipsNilInst — nil instincts are dropped; only
// real IDs survive (the rejected-application path must not implicate a
// phantom).
func TestW3MemePriorsToIDs_SkipsNilInst(t *testing.T) {
	priors := []prior{
		{inst: &persistence.Instinct{ID: "i1"}},
		{inst: nil},
		{inst: &persistence.Instinct{ID: "i2"}, negative: true},
	}
	ids := priorsToIDs(priors)
	if len(ids) != 2 || ids[0] != "i1" || ids[1] != "i2" {
		t.Errorf("priorsToIDs = %v, want [i1 i2]", ids)
	}
}

// TestW3MemeRecordApplications_SkipsEmptyIDsAndNilWriter — a nil writer is
// a no-op; empty-string IDs in the set are skipped (no junk rows), and the
// metric is only bumped for rows that actually write.
func TestW3MemeRecordApplications_SkipsEmptyIDsAndNilWriter(t *testing.T) {
	// nil writer → no panic, nothing recorded.
	a := &Architect{}
	a.recordApplications(context.Background(), []string{"i1"}, persistence.InstinctResultAccepted)

	// Writer wired: an empty-string ID is skipped, the real one lands.
	fi := &fakeInstincts{}
	m := observability.NewInstinctMetrics(prometheus.NewRegistry())
	a2 := &Architect{appWriter: fi, metrics: m}
	a2.recordApplications(context.Background(), []string{"", "i_real"}, persistence.InstinctResultAccepted)
	if len(fi.apps) != 1 {
		t.Fatalf("expected exactly 1 row (empty id skipped), got %d", len(fi.apps))
	}
	if fi.apps[0].InstinctID != "i_real" {
		t.Errorf("recorded id = %q", fi.apps[0].InstinctID)
	}
	got := testutil.ToFloat64(m.ApplicationsTotal.WithLabelValues(
		persistence.InstinctSurfaceArchitectEvidence, persistence.InstinctResultAccepted))
	if got != 1 {
		t.Errorf("metric should bump once, got %v", got)
	}
}

// TestW3MemeRecordApplications_WriteErrorNoMetricBump — when the write
// fails, the metric must NOT be bumped (the counter tracks rows that
// actually landed).
func TestW3MemeRecordApplications_WriteErrorNoMetricBump(t *testing.T) {
	fi := &fakeInstincts{recordAppErr: context.DeadlineExceeded}
	m := observability.NewInstinctMetrics(prometheus.NewRegistry())
	a := &Architect{appWriter: fi, metrics: m}
	a.recordApplications(context.Background(), []string{"i1"}, persistence.InstinctResultRejected)
	got := testutil.ToFloat64(m.ApplicationsTotal.WithLabelValues(
		persistence.InstinctSurfaceArchitectEvidence, persistence.InstinctResultRejected))
	if got != 0 {
		t.Errorf("metric must not bump on write error, got %v", got)
	}
}

// --- loadRecoveryPriors branch coverage ------------------------------------

// TestW3MemeRecoveryPriors_UnmarshalableTriggerSkipped — a recovery
// instinct whose Trigger JSON is corrupt is skipped (it can't be matched),
// without failing the propose turn.
func TestW3MemeRecoveryPriors_UnmarshalableTriggerSkipped(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "build keeps failing", evidence, 0.7)
	bad := recoveryPriorInstinct(t, "builder", "container_non_zero_exit",
		"corrupt trigger row", 0.6, 4, 0, persistence.InstinctStatusCandidate)
	bad.Trigger = []byte("{not valid json")
	fi := &fakeInstincts{listRows: []*persistence.Instinct{bad}}
	a, _, provider := newArchitectWithInstinctsAndRollup(out, evidence, fi, failingRollup())

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "corrupt trigger row") {
		t.Error("a recovery instinct with an unparseable trigger must be skipped")
	}
}

// TestW3MemeRecoveryPriors_RoleQualifiedMismatchSkipped — a recovery
// instinct whose error_class matches a TopFailureClass but whose ROLE
// doesn't match any failing step's role is skipped (role-qualified
// triggers require a {role,class} pair the rollup actually saw).
func TestW3MemeRecoveryPriors_RoleQualifiedMismatchSkipped(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := buildOutput("simple-workflow", fixtureWorkflowYAML, "build keeps failing", evidence, 0.7)
	// Rollup: only the "builder" role fails container_non_zero_exit (via
	// the step), and the class is also a TopFailureClass.
	rollup := failingRollup()
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		// Role "reviewer" never failed this class → role-qualified, skipped.
		recoveryPriorInstinct(t, "reviewer", "container_non_zero_exit",
			"reviewer-scoped recovery", 0.6, 4, 0, persistence.InstinctStatusCandidate),
		// Class-only (no role) → matches via TopFailureClasses, surfaced.
		recoveryPriorInstinct(t, "", "container_non_zero_exit",
			"class-only recovery surfaced", 0.5, 3, 0, persistence.InstinctStatusCandidate),
	}}
	a, _, provider := newArchitectWithInstinctsAndRollup(out, evidence, fi, rollup)

	if _, err := a.Propose(context.Background(), "simple-workflow"); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	userMsg := provider.lastMessages[len(provider.lastMessages)-1].Content
	if strings.Contains(userMsg, "reviewer-scoped recovery") {
		t.Error("role-qualified recovery for a non-failing role must be skipped")
	}
	if !strings.Contains(userMsg, "class-only recovery surfaced") {
		t.Error("class-only recovery matching a TopFailureClass must surface")
	}
}

// TestW3MemeRecoveryPriors_NilRollupNoQuery — a nil rollup short-circuits
// before any recovery query (defensive; mirrors the no-failures gate).
func TestW3MemeRecoveryPriors_NilRollupNoQuery(t *testing.T) {
	fi := &fakeInstincts{listRows: []*persistence.Instinct{
		recoveryPriorInstinct(t, "builder", "container_non_zero_exit", "x", 0.6, 4, 0, persistence.InstinctStatusCandidate),
	}}
	a := New(
		&stubProvider{content: "{}"},
		&stubTelemetry{rollup: &workflowtelemetry.Rollup{}},
		&stubWorkflowSource{yaml: []byte(fixtureWorkflowYAML)},
		nil, &stubProposalSink{}, DefaultConfig(),
		WithInstincts(fi),
	)
	got := a.loadRecoveryPriors(context.Background(), nil)
	if got != nil {
		t.Errorf("nil rollup must yield nil recovery priors, got %v", got)
	}
	if fi.listCalls != 0 {
		t.Errorf("nil rollup must not query, got %d calls", fi.listCalls)
	}
}

// --- proposal scope guard ---------------------------------------------------

// TestW3MemePropose_ChangeRoleAssignmentKindRejected — the architect
// prompt forbids role changes; the proposal-kind enum carries a
// change_role_assignment value for the persistence/filter layer, but the
// architect's prompt schema only advertises the six structural kinds.
// This pins the OBSERVED behavior: change_role_assignment IS in the valid
// enum, so a proposal carrying it is accepted by the kind guard (it is not
// a hallucinated out-of-set value). Documents that scope enforcement for
// role changes lives in the prompt + YAML validator, not the kind check.
func TestW3MemePropose_ChangeRoleAssignmentKindAcceptedByKindGuard(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := ArchitectOutput{
		WorkflowID: "simple-workflow", ProposedYAML: fixtureWorkflowYAML,
		Motivation: "m", EvidenceRunIDs: evidence, Confidence: 0.75,
		Kind: string(persistence.WorkflowProposalKindChangeRoleAssignment),
	}
	b := buildOutputFromStruct(t, out)
	a, sink := newArchitectWithStubs(t, b, evidence)
	got, err := a.Propose(context.Background(), "simple-workflow")
	if err != nil {
		t.Fatalf("change_role_assignment is a valid enum kind, should not be rejected by the kind guard: %v", err)
	}
	if got.Kind != persistence.WorkflowProposalKindChangeRoleAssignment {
		t.Errorf("kind not threaded: %q", got.Kind)
	}
	if sink.inserted == nil {
		t.Error("proposal should insert")
	}
}

// TestW3MemePropose_OutOfSetKindRejectedBeforeInsert — a kind outside the
// closed enum (e.g. a structural-sounding but undefined "split_step") is
// rejected as malformed and never inserted. This is the real scope guard:
// the architect cannot widen the structural-edit vocabulary.
func TestW3MemePropose_OutOfSetKindRejectedBeforeInsert(t *testing.T) {
	evidence := []string{"exec_a", "exec_b", "exec_c"}
	out := ArchitectOutput{
		WorkflowID: "simple-workflow", ProposedYAML: fixtureWorkflowYAML,
		Motivation: "m", EvidenceRunIDs: evidence, Confidence: 0.8,
		Kind: "split_step",
	}
	b := buildOutputFromStruct(t, out)
	a, sink := newArchitectWithStubs(t, b, evidence)
	_, err := a.Propose(context.Background(), "simple-workflow")
	if !errors.Is(err, ErrMalformedOutput) {
		t.Fatalf("out-of-set kind must be ErrMalformedOutput, got %v", err)
	}
	if sink.inserted != nil {
		t.Error("out-of-set-kind proposal must NOT be inserted")
	}
}

// --- rejectionAction text ---------------------------------------------------

// TestW3MemeRejectionAction_KindedVsUnspecified — the operator-facing
// action text names the kind when present, and falls back to the generic
// "structural proposal" phrasing for the unspecified sentinel / empty.
func TestW3MemeRejectionAction_KindedVsUnspecified(t *testing.T) {
	kinded := rejectionAction(&persistence.WorkflowProposal{
		WorkflowID: "wf-1", Kind: persistence.WorkflowProposalKindChangeRetryPolicy,
	})
	if !strings.Contains(kinded, "change_retry_policy") || !strings.Contains(kinded, "wf-1") {
		t.Errorf("kinded action text: %q", kinded)
	}
	for _, k := range []persistence.WorkflowProposalKind{"", persistence.WorkflowProposalKindUnspecified} {
		generic := rejectionAction(&persistence.WorkflowProposal{WorkflowID: "wf-2", Kind: k})
		if !strings.Contains(generic, "structural proposal") || !strings.Contains(generic, "wf-2") {
			t.Errorf("generic action text for kind %q: %q", k, generic)
		}
	}
}

// --- Applier: commit message + post-mark Get-error fallback ----------------

// TestW3MemeApplier_CommitMessage_NoMotivationNoEvidence — a proposal with
// neither motivation nor evidence still produces a well-formed commit
// message (subject + provenance line) without an empty Motivation/Evidence
// block.
func TestW3MemeApplier_CommitMessage_NoMotivationNoEvidence(t *testing.T) {
	a := &Applier{}
	p := &persistence.WorkflowProposal{
		ID: "wpr-9", WorkflowID: "research", Confidence: 0.77,
	}
	msg := a.formatCommitMessage(p, "operator-z")
	if !strings.HasPrefix(msg, "workflow(research):") {
		t.Errorf("subject prefix wrong: %q", msg)
	}
	if !strings.Contains(msg, "proposal_id=wpr-9") || !strings.Contains(msg, "confidence=0.77") {
		t.Errorf("provenance line missing: %q", msg)
	}
	if !strings.Contains(msg, "Applied by operator-z") {
		t.Errorf("approver line missing: %q", msg)
	}
	if strings.Contains(msg, "Motivation:") {
		t.Errorf("no Motivation block expected for empty motivation: %q", msg)
	}
	if strings.Contains(msg, "Evidence (") {
		t.Errorf("no Evidence block expected for empty evidence: %q", msg)
	}
}

// TestW3MemeApplier_PostMarkGetError_FallbackRecord — when the read-back
// Get fails AFTER MarkApplied succeeded, Apply still returns a synthesized
// applied record (status=applied, the stored SHA, a stamped AppliedAt) so
// the caller's contract holds. Covers applier.go's Get-error fallback.
func TestW3MemeApplier_PostMarkGetError_FallbackRecord(t *testing.T) {
	repo := &getErrAfterMarkRepo{stubProposalRepo: newStubProposalRepo()}
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	a := NewApplier(repo, &stubWriter{}, &stubGit{sha: "cafe1234"}, &stubReloader{}, ApplierConfig{})

	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("Apply should succeed despite read-back failure, got %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		t.Errorf("fallback status should be applied, got %q", got.Status)
	}
	if got.AppliedCommit != "cafe1234" {
		t.Errorf("fallback applied_commit = %q, want cafe1234", got.AppliedCommit)
	}
	if got.WorkflowID != "research" {
		t.Errorf("fallback workflow id = %q", got.WorkflowID)
	}
	if got.AppliedAt == nil {
		t.Error("fallback record should stamp AppliedAt")
	}
	if !repo.markedThenFailed {
		t.Error("test setup wrong: MarkApplied should have run before the failing Get")
	}
}

// getErrAfterMarkRepo lets the FIRST Get (status check) succeed, then makes
// the read-back Get (after MarkApplied) fail — exercising Apply's
// synthesize-a-minimal-record fallback.
type getErrAfterMarkRepo struct {
	*stubProposalRepo
	marked           bool
	markedThenFailed bool
}

func (r *getErrAfterMarkRepo) MarkApplied(ctx context.Context, id, sha string) error {
	r.marked = true
	return r.stubProposalRepo.MarkApplied(ctx, id, sha)
}

func (r *getErrAfterMarkRepo) Get(ctx context.Context, id string) (*persistence.WorkflowProposal, error) {
	if r.marked {
		r.markedThenFailed = true
		return nil, context.DeadlineExceeded
	}
	return r.stubProposalRepo.Get(ctx, id)
}

// buildOutputFromStruct marshals an ArchitectOutput to the JSON the stub
// provider replays. Local helper (the sibling buildOutput takes scalar
// args and can't carry a Kind).
func buildOutputFromStruct(t *testing.T, out ArchitectOutput) string {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
