package projectwizard

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"vornik.io/vornik/internal/persistence"
)

func turnsMetricValue(t *testing.T, m *Metrics, outcome string) float64 {
	t.Helper()
	if m == nil || m.TurnsTotal == nil {
		return 0
	}
	c, err := m.TurnsTotal.GetMetricWithLabelValues(outcome)
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

func TestMetrics_Turns_AssistantReplyOnHappyPath(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{content: envelopeAskQuestion})
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	_, err := w.Converse(context.Background(), "", "op_1", "hi")
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if turnsMetricValue(t, metrics, turnOutcomeAssistantReply) != 1 {
		t.Errorf("expected 1 assistant_reply, got %.2f", turnsMetricValue(t, metrics, turnOutcomeAssistantReply))
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
	if turnsMetricValue(t, metrics, turnOutcomeValidationError) != 1 {
		t.Errorf("expected 1 validation_error, got %.2f", turnsMetricValue(t, metrics, turnOutcomeValidationError))
	}
	if turnsMetricValue(t, metrics, turnOutcomeAssistantReply) != 0 {
		t.Errorf("validation failure should not double-count as assistant_reply")
	}
}

func TestMetrics_Turns_LLMError(t *testing.T) {
	w, _, _ := newWizardForTest(chatReply{err: errors.New("upstream down")})
	metrics := NewMetrics(prometheus.NewRegistry())
	w.Metrics = metrics
	_, _ = w.Converse(context.Background(), "", "op_1", "hi")
	if turnsMetricValue(t, metrics, turnOutcomeLLMError) != 1 {
		t.Errorf("expected 1 llm_error, got %.2f", turnsMetricValue(t, metrics, turnOutcomeLLMError))
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
	if turnsMetricValue(t, metrics, turnOutcomeRejected) != 1 {
		t.Errorf("expected 1 rejected, got %.2f", turnsMetricValue(t, metrics, turnOutcomeRejected))
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
