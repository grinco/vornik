package autonomy

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeChatProvider is a minimal chat.Provider whose CompleteWithTools
// returns a scripted error, so the autonomy-eval error-classification
// path can be exercised without a real LLM backend.
type fakeChatProvider struct {
	err   error
	calls int
}

func (f *fakeChatProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, f.err
}
func (f *fakeChatProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	f.calls++
	return nil, f.err
}
func (f *fakeChatProvider) CompleteWithToolsStream(context.Context, []chat.Message, []chat.Tool, chat.StreamCallback) (*chat.ChatResponse, error) {
	return nil, f.err
}
func (f *fakeChatProvider) Model() string            { return "fake" }
func (f *fakeChatProvider) SetMetrics(*chat.Metrics) {}

// llmEvalProject returns a project whose autonomy config passes every
// pre-LLM gate (rate-limit, budget, preCheck, no active tasks) so
// evaluate() reaches the CompleteWithTools call.
func llmEvalProject() *registry.Project {
	return &registry.Project{
		ID: "janka",
		Autonomy: registry.ProjectAutonomy{
			Enabled:         true,
			MaxTasksPerHour: 10,
			Goal:            "test goal",
		},
	}
}

// TestEvaluate_CallerCancelIsBenignAborted is the Janka first-eval-
// after-reload fix: when the in-flight eval LLM call dies with
// context.Canceled (autonomy loop torn down mid-eval by reload /
// shutdown), the eval must record the benign ABORTED outcome, NOT
// increment ErrorsTotal, and return nil — the loop re-evaluates next
// tick. It is expected teardown, not an LLM failure.
func TestEvaluate_CallerCancelIsBenignAborted(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	metrics := NewMetrics(prometheus.NewRegistry())
	client := &fakeChatProvider{err: context.Canceled}
	m := New(client, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evalRepo),
		WithMetrics(metrics),
	)

	err := m.evaluate(context.Background(), llmEvalProject())
	require.NoError(t, err, "caller-cancel teardown must return nil (benign)")
	require.Equal(t, 1, client.calls, "the LLM call must actually have been reached")

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1, "exactly one audit row for the aborted eval")
	assert.Equal(t, persistence.AutonomyOutcomeAborted, entries[0].Outcome)
	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ErrorsTotal.WithLabelValues("janka")),
		"caller-cancel must NOT bump the error metric")
}

// TestEvaluate_GenericLLMErrorStaysError confirms an ordinary LLM
// error keeps the original behaviour: LLM_ERROR outcome, ErrorsTotal
// incremented, and a wrapped error returned.
func TestEvaluate_GenericLLMErrorStaysError(t *testing.T) {
	repo := &mockTaskRepo{}
	evalRepo := &captureEvalRepo{}
	metrics := NewMetrics(prometheus.NewRegistry())
	client := &fakeChatProvider{err: errors.New("upstream 502")}
	m := New(client, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evalRepo),
		WithMetrics(metrics),
	)

	err := m.evaluate(context.Background(), llmEvalProject())
	require.Error(t, err, "a real LLM error must surface")
	assert.Contains(t, err.Error(), "502")

	entries := evalRepo.snapshot()
	require.Len(t, entries, 1)
	assert.Equal(t, persistence.AutonomyOutcomeLLMError, entries[0].Outcome)
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ErrorsTotal.WithLabelValues("janka")),
		"a real LLM error must bump the error metric")
}
