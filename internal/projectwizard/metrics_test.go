package projectwizard

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"vornik.io/vornik/internal/persistence"
)

// turnsMetricValueAnyTier sums the counter across every tier label for
// the given outcome — most tests here don't care which tier fired
// (the tier label is asserted separately by TestMetrics_Turns_Tier*),
// they just want "did this outcome fire once".
func turnsMetricValueAnyTier(t *testing.T, m *Metrics, outcome string) float64 {
	t.Helper()
	if m == nil || m.TurnsTotal == nil {
		return 0
	}
	total := 0.0
	for _, tier := range []string{"1", "2", "3", tierLabelUnknown} {
		c, err := m.TurnsTotal.GetMetricWithLabelValues(tier, outcome)
		if err != nil {
			t.Fatalf("GetMetricWithLabelValues: %v", err)
		}
		var dst dto.Metric
		if err := c.Write(&dst); err != nil {
			t.Fatalf("metric write: %v", err)
		}
		if dst.Counter != nil {
			total += dst.Counter.GetValue()
		}
	}
	return total
}

func turnsMetricValue(t *testing.T, m *Metrics, tier, outcome string) float64 {
	t.Helper()
	if m == nil || m.TurnsTotal == nil {
		return 0
	}
	c, err := m.TurnsTotal.GetMetricWithLabelValues(tier, outcome)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	var dst dto.Metric
	if err := c.Write(&dst); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	if dst.Counter == nil {
		return 0
	}
	return dst.Counter.GetValue()
}

func commitsMetricValue(t *testing.T, m *Metrics, outcome string) float64 {
	t.Helper()
	c, err := m.CommitsTotal.GetMetricWithLabelValues(outcome)
	if err != nil {
		t.Fatalf("commits metric: %v", err)
	}
	var dst dto.Metric
	_ = c.Write(&dst)
	if dst.Counter == nil {
		return 0
	}
	return dst.Counter.GetValue()
}

// bundleValidatedMetricValue reads
// vornik_composer_bundles_validated_total{result} — shared by the I2
// whole-branch-review regression tests in composer_engine_test.go that
// pin the double-count fix (a rejectBundle call must never itself
// record this counter; only applyBundle's two staged-validation
// outcomes do).
func bundleValidatedMetricValue(t *testing.T, m *Metrics, result string) float64 {
	t.Helper()
	c, err := m.BundlesValidatedTotal.GetMetricWithLabelValues(result)
	if err != nil {
		t.Fatalf("bundles validated metric: %v", err)
	}
	var dst dto.Metric
	_ = c.Write(&dst)
	if dst.Counter == nil {
		return 0
	}
	return dst.Counter.GetValue()
}

// composerCommitsMetricValue reads vornik_composer_commits_total{tier,result} —
// the journaled bundle-commit path's own counter (task 1.2b), distinct
// from the pre-existing project_wizard commitsMetricValue above.
//
// recordComposerCommit's identical nolint) but the helper mirrors the
// counter's real {tier,result} label shape rather than hard-coding it.
//
//nolint:unparam // every call site passes composerCommitTier3 today (see
func composerCommitsMetricValue(t *testing.T, m *Metrics, tier, result string) float64 {
	t.Helper()
	c, err := m.ComposerCommitsTotal.GetMetricWithLabelValues(tier, result)
	if err != nil {
		t.Fatalf("composer commits metric: %v", err)
	}
	var dst dto.Metric
	_ = c.Write(&dst)
	if dst.Counter == nil {
		return 0
	}
	return dst.Counter.GetValue()
}

func TestMetrics_Turns_AssistantReplyOnHappyPath(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	_, err := w.Converse(context.Background(), "", "op_1", "hi")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if turnsMetricValue(t, metrics, "1", turnOutcomeAssistantReply) != 1 {
		t.Errorf("expected 1 assistant_reply, got %.2f", turnsMetricValue(t, metrics, "1", turnOutcomeAssistantReply))
	}
}

func TestMetrics_Turns_ValidationError(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeMissingID})
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	_, err := w.Converse(context.Background(), "", "op_1", "hi")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if turnsMetricValue(t, metrics, "1", turnOutcomeValidationError) != 1 {
		t.Errorf("expected 1 validation_error, got %.2f", turnsMetricValue(t, metrics, "1", turnOutcomeValidationError))
	}
	if turnsMetricValueAnyTier(t, metrics, turnOutcomeAssistantReply) != 0 {
		t.Errorf("validation failure should not double-count as assistant_reply")
	}
}

func TestMetrics_Turns_LLMError(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{err: errors.New("upstream down")})
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	_, _ = w.Converse(context.Background(), "", "op_1", "hi")
	if turnsMetricValue(t, metrics, tierLabelUnknown, turnOutcomeLLMError) != 1 {
		t.Errorf("expected 1 llm_error, got %.2f", turnsMetricValue(t, metrics, tierLabelUnknown, turnOutcomeLLMError))
	}
}

// TestMetrics_Turns_Fallback_OnConsecutiveValidationFailures is the
// telemetry seam for task 1.3's circuit breaker (design §7 row 1 /
// §5.8 "soak needs to SEE how often fallback fires"): the 3rd
// consecutive tier-3 validation failure must record
// outcome="fallback" on the existing vornik_project_wizard_turns_total
// metric (folded in rather than a new counter — see metrics.go's
// TurnsTotal doc comment), and the two prior bounces must have
// recorded validation_error, not fallback.
func TestMetrics_Turns_Fallback_OnConsecutiveValidationFailures(t *testing.T) {
	badBundle := validComposedBundle()
	badBundle.Project["defaultWorkflowId"] = "does-not-exist"
	w, _, _ := newWizardForTest(
		tier3Reply(t, "fail 1", true, badBundle),
		tier3Reply(t, "fail 2", true, badBundle),
		tier3Reply(t, "fail 3", true, badBundle),
	)
	wireComposer(w)
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics

	sessionID := ""
	for i := 0; i < 3; i++ {
		res, err := w.Converse(context.Background(), sessionID, "op_1", unrelatedDescription)
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		sessionID = res.SessionID
	}
	if got := turnsMetricValue(t, metrics, "3", turnOutcomeValidationError); got != 2 {
		t.Errorf("expected 2 validation_error turns before the fallback fired, got %.2f", got)
	}
	if got := turnsMetricValueAnyTier(t, metrics, turnOutcomeFallback); got != 1 {
		t.Errorf("expected exactly 1 fallback turn recorded, got %.2f", got)
	}
}

func TestMetrics_Turns_Rejected_TurnCap(t *testing.T) {
	w, _, _ := newWizardForTest(
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeAskQuestion},
	)
	w.MaxTurns = 1
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	res, _ := w.Converse(context.Background(), "", "op_1", "first")
	_, err := w.Converse(context.Background(), res.SessionID, "op_1", "second")
	if !errors.Is(err, ErrTurnsExhausted) {
		t.Fatalf("expected turn-cap error, got %v", err)
	}
	if turnsMetricValue(t, metrics, tierLabelUnknown, turnOutcomeRejected) != 1 {
		t.Errorf("expected 1 rejected, got %.2f", turnsMetricValue(t, metrics, tierLabelUnknown, turnOutcomeRejected))
	}
}

func TestMetrics_Commits_OnSuccess(t *testing.T) {
	w, store, _ := newWizardForTest()
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	w.Writer = &capturingWriter{}
	w.Validator = RegistryValidator{}
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commitsMetricValue(t, metrics, commitOutcomeCreated) != 1 {
		t.Errorf("expected 1 created commit, got %.2f", commitsMetricValue(t, metrics, commitOutcomeCreated))
	}
}

func TestMetrics_Commits_OnWriterFailure(t *testing.T) {
	w, store, _ := newWizardForTest()
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	w.Writer = &capturingWriter{err: errors.New("oom")}
	w.Validator = RegistryValidator{}
	sessionID := pinReadySession(t, store, "op_1", minimalValidProposal())
	_, err := w.Commit(context.Background(), sessionID, "op_1")
	if err == nil {
		t.Fatal("expected writer error to bubble")
	}
	if commitsMetricValue(t, metrics, commitOutcomeFailed) != 1 {
		t.Errorf("expected 1 failed commit, got %.2f", commitsMetricValue(t, metrics, commitOutcomeFailed))
	}
}

// TestMetrics_Commits_OnCompositionMissingProjectID guards Minor #2
// from the whole-branch review: commitComposition's missing-projectId
// early return must record a failed-commit metric, the same as the
// re-compose and WriteFiles error paths already do.
func TestMetrics_Commits_OnCompositionMissingProjectID(t *testing.T) {
	w, store, _ := newWizardForTest()
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	w.Writer = &multiFileCapturingWriter{}
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	comp := &Composition{Template: "custom-base", Params: map[string]ParamValue{}}
	sessionID := pinReadySessionWithComposition(t, store, comp)
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err == nil {
		t.Fatal("expected missing projectId error")
	}
	if commitsMetricValue(t, metrics, commitOutcomeFailed) != 1 {
		t.Errorf("expected 1 failed commit, got %.2f", commitsMetricValue(t, metrics, commitOutcomeFailed))
	}
}

// TestMetrics_Commits_OnCompositionUnsafeProjectID mirrors the above
// for the isSafeProjectID early return.
func TestMetrics_Commits_OnCompositionUnsafeProjectID(t *testing.T) {
	w, store, _ := newWizardForTest()
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	w.Writer = &multiFileCapturingWriter{}
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	comp := &Composition{Template: "custom-base", Params: map[string]ParamValue{"projectId": {"../escape"}}}
	sessionID := pinReadySessionWithComposition(t, store, comp)
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err == nil {
		t.Fatal("expected invalid projectId error")
	}
	if commitsMetricValue(t, metrics, commitOutcomeFailed) != 1 {
		t.Errorf("expected 1 failed commit, got %.2f", commitsMetricValue(t, metrics, commitOutcomeFailed))
	}
}

// TestMetrics_Commits_OnCompositionWriterNotMultiFile mirrors the
// above for the non-MultiFileProjectWriter early return.
func TestMetrics_Commits_OnCompositionWriterNotMultiFile(t *testing.T) {
	w, store, _ := newWizardForTest()
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	w.Writer = &capturingWriter{} // single-file only — no WriteFiles
	w.Templates = &composeFakeTemplateSource{known: map[string]bool{"custom-base": true}, files: baseFiles()}
	w.KnownMCP = func(context.Context) map[string]bool { return map[string]bool{} }

	comp := &Composition{Template: "custom-base", Params: map[string]ParamValue{"projectId": {"pricing-watch"}}}
	sessionID := pinReadySessionWithComposition(t, store, comp)
	if _, err := w.Commit(context.Background(), sessionID, "op_1"); err == nil {
		t.Fatal("expected writer-does-not-support-multi-file error")
	}
	if commitsMetricValue(t, metrics, commitOutcomeFailed) != 1 {
		t.Errorf("expected 1 failed commit, got %.2f", commitsMetricValue(t, metrics, commitOutcomeFailed))
	}
}

func TestConverse_ConcurrentSessionCap(t *testing.T) {
	w, store, _ := newWizardForTest(
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeAskQuestion},
		chatReply{content: envelopeAskQuestion},
	)
	w.MaxActiveSessions = 2
	// Seed two uncommitted sessions for the operator.
	for i := 0; i < 2; i++ {
		_ = store.Insert(context.Background(), &persistence.ProjectWizardSession{
			ID:         persistence.GenerateID("pw"),
			OperatorID: "op_1",
		})
	}
	// Third new session should be refused.
	_, err := w.Converse(context.Background(), "", "op_1", "I want a third")
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("expected ErrTooManySessions, got %v", err)
	}
}

func TestConverse_ConcurrentCapIgnoresCommitted(t *testing.T) {
	w, store, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	w.MaxActiveSessions = 2
	// Two committed sessions don't count against the cap.
	committed := "test"
	for i := 0; i < 2; i++ {
		_ = store.Insert(context.Background(), &persistence.ProjectWizardSession{
			ID:                 persistence.GenerateID("pw"),
			OperatorID:         "op_1",
			CommittedProjectID: &committed,
		})
	}
	if _, err := w.Converse(context.Background(), "", "op_1", "fresh"); err != nil {
		t.Errorf("expected committed sessions to be ignored, got %v", err)
	}
}

func TestNewMetrics_RegistersComposerCounters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	if m.TurnsTotal == nil || m.CommitsTotal == nil || m.AbandonedTotal == nil {
		t.Fatal("expected the pre-existing wizard counters constructed")
	}
	if m.BundlesValidatedTotal == nil || m.GuardrailHitsTotal == nil {
		t.Fatal("expected both new composer counters constructed")
	}
	if m.ComposerCommitsTotal == nil {
		t.Fatal("expected the composer commits counter constructed")
	}
	// A CounterVec only appears in Gather() once it has a child;
	// touch each vec so the metric family names below are verifiable
	// against the exact names the lint-lld-contracts allowlist names.
	m.BundlesValidatedTotal.WithLabelValues("valid").Add(0)
	m.GuardrailHitsTotal.WithLabelValues("rule").Add(0)
	m.TurnsTotal.WithLabelValues("1", "assistant_reply").Add(0)
	m.ComposerCommitsTotal.WithLabelValues(composerCommitTier3, composerCommitResultCreated).Add(0)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"vornik_project_wizard_turns_total",
		"vornik_project_wizard_abandoned_total",
		"vornik_composer_bundles_validated_total",
		"vornik_composer_guardrail_hits_total",
		"vornik_composer_commits_total",
	} {
		if !names[want] {
			t.Errorf("expected metric %q to be registered, got %v", want, names)
		}
	}
}

func TestRecordComposerCommit(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	m.recordComposerCommit(composerCommitTier3, composerCommitResultCreated)
	m.recordComposerCommit(composerCommitTier3, composerCommitResultFailed)
	m.recordComposerCommit(composerCommitTier3, composerCommitResultFailed)

	if got := composerCommitsMetricValue(t, m, composerCommitTier3, composerCommitResultCreated); got != 1 {
		t.Errorf("created = %.0f, want 1", got)
	}
	if got := composerCommitsMetricValue(t, m, composerCommitTier3, composerCommitResultFailed); got != 2 {
		t.Errorf("failed = %.0f, want 2", got)
	}

	// Nil-safe.
	var nilMetrics *Metrics
	nilMetrics.recordComposerCommit(composerCommitTier3, composerCommitResultCreated)
}

func TestRecordBundleValidatedAndGuardrailHit(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	m.recordBundleValidated(bundleValidationResultValid)
	m.recordBundleValidated(bundleValidationResultInvalid)
	m.recordGuardrailHit(guardrailRuleToolOverreach)

	c, _ := m.BundlesValidatedTotal.GetMetricWithLabelValues(bundleValidationResultValid)
	var dst dto.Metric
	_ = c.Write(&dst)
	if dst.Counter.GetValue() != 1 {
		t.Errorf("expected 1 valid bundle recorded, got %v", dst.Counter.GetValue())
	}

	g, _ := m.GuardrailHitsTotal.GetMetricWithLabelValues(guardrailRuleToolOverreach)
	var gdst dto.Metric
	_ = g.Write(&gdst)
	if gdst.Counter.GetValue() != 1 {
		t.Errorf("expected 1 guardrail hit recorded, got %v", gdst.Counter.GetValue())
	}

	// Nil-safe.
	var nilMetrics *Metrics
	nilMetrics.recordBundleValidated("valid")
	nilMetrics.recordGuardrailHit("rule")
}
