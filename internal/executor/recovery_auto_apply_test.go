package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
)

// TestAutoApplyConfig_Eligible pins the v2 gate: enabled + confidence floor +
// optional error-class allowlist.
func TestAutoApplyConfig_Eligible(t *testing.T) {
	// minCleanSupport is 0 (off) throughout this test, so support/contradict
	// are ignored — the clean-support gate has its own test
	// (TestExecutorCov_AutoApplyCleanSupportGate).
	off := autoApplyConfig{}
	if off.eligible(0.99, 99, 0, "Timeout") {
		t.Error("disabled config must never be eligible")
	}

	noAllow := autoApplyConfig{enabled: true, minConfidence: 0.85}
	if !noAllow.eligible(0.85, 99, 0, "AnyClass") {
		t.Error("confidence == floor with empty allowlist should be eligible")
	}
	if noAllow.eligible(0.84, 99, 0, "AnyClass") {
		t.Error("below the floor must not be eligible")
	}

	allow := autoApplyConfig{
		enabled: true, minConfidence: 0.8,
		allowedClasses: map[string]struct{}{"Timeout": {}},
	}
	if !allow.eligible(0.9, 99, 0, "Timeout") {
		t.Error("allowlisted class above floor should be eligible")
	}
	if allow.eligible(0.9, 99, 0, "DataCorruption") {
		t.Error("class outside the allowlist must not be eligible")
	}
}

// TestWithInstinctAutoApply_DefaultsAndAllowlist — the option substitutes the
// 0.85 default for an unset confidence and drops empty allowlist entries.
func TestWithInstinctAutoApply_DefaultsAndAllowlist(t *testing.T) {
	e := &Executor{}
	WithInstinctAutoApply(true, 0, 0, []string{"Timeout", ""})(e)
	if e.instinctAutoApply.minConfidence != 0.85 {
		t.Errorf("min_confidence default = %v, want 0.85", e.instinctAutoApply.minConfidence)
	}
	if _, ok := e.instinctAutoApply.allowedClasses["Timeout"]; !ok {
		t.Error("Timeout should be in the allowlist")
	}
	if _, ok := e.instinctAutoApply.allowedClasses[""]; ok {
		t.Error("empty class must be dropped from the allowlist")
	}

	// Disabled leaves min_confidence untouched (gate is off anyway).
	d := &Executor{}
	WithInstinctAutoApply(false, 0, 0, nil)(d)
	if d.instinctAutoApply.enabled {
		t.Error("disabled should stay disabled")
	}
}

// TestAttachLearnedRemediations_AutoApplyRecordsAutoApplied — with the gate on,
// an eligible remediation is marked AutoApplied and its application row is
// recorded 'auto_applied'; an ineligible one (below floor) stays 'ignored'.
func TestAttachLearnedRemediations_AutoApplyRecordsAutoApplied(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "hi", "scout", "Timeout", "retrying resolved the Timeout failure", 0.95),
		activeRecoveryInstinct(t, "lo", "scout", "Timeout", "switching model resolved it", 0.60),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		instinctAutoApply: autoApplyConfig{enabled: true, minConfidence: 0.85},
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)

	if len(rc.LearnedRemediations) != 2 {
		t.Fatalf("expected 2 remediations, got %d", len(rc.LearnedRemediations))
	}
	gotResult := map[string]string{}
	for _, a := range repo.applications {
		gotResult[a.InstinctID] = a.Result
	}
	if gotResult["hi"] != persistence.InstinctResultAutoApplied {
		t.Errorf("high-confidence instinct result = %q, want auto_applied", gotResult["hi"])
	}
	if gotResult["lo"] != persistence.InstinctResultIgnored {
		t.Errorf("below-floor instinct result = %q, want ignored", gotResult["lo"])
	}

	// The prompt block renders the auto-applied one under the directive header.
	block := learnedRemediationsBlock(rc.LearnedRemediations)
	if !strings.Contains(block, "apply_these_proven_remediations") {
		t.Errorf("block missing directive header:\n%s", block)
	}
	if !strings.Contains(block, "similar_failures_previously_resolved_here") {
		t.Errorf("block missing advisory header for the below-floor remediation:\n%s", block)
	}
}

// TestAttachLearnedRemediations_AutoApplyOffStaysAdvisory — gate off: nothing
// is marked AutoApplied and every application is recorded 'ignored'.
func TestAttachLearnedRemediations_AutoApplyOffStaysAdvisory(t *testing.T) {
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{
		activeRecoveryInstinct(t, "hi", "scout", "Timeout", "retrying resolved the Timeout failure", 0.99),
	}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		instinctAutoApply: autoApplyConfig{}, // OFF
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)

	for _, r := range rc.LearnedRemediations {
		if r.AutoApplied {
			t.Errorf("auto-apply off: remediation %q must not be AutoApplied", r.Action)
		}
	}
	for _, a := range repo.applications {
		if a.Result != persistence.InstinctResultIgnored {
			t.Errorf("auto-apply off: application result = %q, want ignored", a.Result)
		}
	}
	// Directive header must be absent.
	if strings.Contains(learnedRemediationsBlock(rc.LearnedRemediations), "apply_these_proven_remediations") {
		t.Error("auto-apply off: directive header must not appear")
	}
}

// TestAttachLearnedRemediations_CleanSupportGate proves the clean-support gate
// is wired end-to-end: with minCleanSupport=10, a high-confidence instinct that
// has too few clean supports (4) is NOT auto-applied, while one with enough
// (12) is. This exercises the threading of the remediation row's
// support/contradict counts into eligible() at the call site.
func TestAttachLearnedRemediations_CleanSupportGate(t *testing.T) {
	hi := activeRecoveryInstinct(t, "hisup", "scout", "Timeout", "retrying resolved the Timeout failure", 0.95)
	hi.SupportCount = 12 // clears the clean-support bar
	lo := activeRecoveryInstinct(t, "lowsup", "scout", "Timeout", "switching model resolved it", 0.95)
	lo.SupportCount = 4 // below the bar, though confidence clears the floor
	repo := &stubInstinctRepo{rows: []*persistence.Instinct{hi, lo}}
	e := &Executor{
		instinctRepo:      repo,
		instinctPlaybooks: true,
		instinctAutoApply: autoApplyConfig{enabled: true, minConfidence: 0.85, minCleanSupport: 10},
		outcomeRepo:       &stubOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{{ErrorClass: "Timeout", Role: "scout"}}},
		logger:            zerolog.Nop(),
	}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error"}
	e.attachLearnedRemediations(context.Background(),
		&persistence.Task{ID: "t1", ProjectID: "proj"}, &persistence.Execution{ID: "exec1"}, rc)

	gotResult := map[string]string{}
	for _, a := range repo.applications {
		gotResult[a.InstinctID] = a.Result
	}
	if gotResult["hisup"] != persistence.InstinctResultAutoApplied {
		t.Errorf("12-clean-support instinct = %q, want auto_applied", gotResult["hisup"])
	}
	if gotResult["lowsup"] != persistence.InstinctResultIgnored {
		t.Errorf("4-support instinct = %q, want ignored (below clean-support bar)", gotResult["lowsup"])
	}
}

// TestLearnedRemediationsBlock_DirectiveOrdering — auto-applied remediations
// render before advisory ones.
func TestLearnedRemediationsBlock_DirectiveOrdering(t *testing.T) {
	block := learnedRemediationsBlock([]playbook.LearnedRemediation{
		{Action: "advisory-fix", Confidence: 0.7},
		{Action: "directive-fix", Confidence: 0.95, AutoApplied: true},
	})
	di := strings.Index(block, "directive-fix")
	ai := strings.Index(block, "advisory-fix")
	if di < 0 || ai < 0 || di > ai {
		t.Errorf("auto-applied remediation should render before advisory:\n%s", block)
	}
}
