package executor

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeSwarmResolver / fakeTaskWriter back the ApplyFallbackModelOverride
// tests without standing up a real registry or DB.
type fakeSwarmResolver struct {
	sw  *registry.Swarm
	err error
}

func (f fakeSwarmResolver) GetProjectWithSwarm(string) (*registry.Project, *registry.Swarm, error) {
	return nil, f.sw, f.err
}

type fakeTaskWriter struct {
	updated *persistence.Task
	err     error
}

func (f *fakeTaskWriter) Update(_ context.Context, task *persistence.Task) error {
	if f.err != nil {
		return f.err
	}
	f.updated = task
	return nil
}

// TestEffectiveRoleModelForTask_OperatorOverrideWins is the engine half
// of the 2026-06-11 "modelFallback steering has no effect" report. An
// operator action that forces a role onto its fallback model writes
// context.operator_model_override; that must win over the role's
// configured primary model.
func TestEffectiveRoleModelForTask_OperatorOverrideWins(t *testing.T) {
	payload, err := WithOperatorModelOverride(nil, map[string]string{"researcher": "minimax.minimax-m2.5"})
	require.NoError(t, err)

	task := &persistence.Task{Payload: payload}
	role := &registry.SwarmRole{Name: "researcher", Model: "gpt-5.4-mini"}

	e := &Executor{}
	got := e.effectiveRoleModelForTask(task, role)
	assert.Equal(t, "minimax.minimax-m2.5", got,
		"operator fallback override must beat the role's primary model")
}

// TestWithOperatorModelOverride_PreservesExistingPayload ensures merging
// the override doesn't drop the operator's original task context.
func TestWithOperatorModelOverride_PreservesExistingPayload(t *testing.T) {
	orig := json.RawMessage(`{"context":{"prompt":"do research"},"priority":7}`)
	merged, err := WithOperatorModelOverride(orig, map[string]string{"researcher": "fb-model"})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(merged, &m))
	assert.EqualValues(t, 7, m["priority"], "top-level fields must survive the merge")
	ctxMap := m["context"].(map[string]any)
	assert.Equal(t, "do research", ctxMap["prompt"], "existing context fields must survive")
	assert.Equal(t, "fb-model", operatorModelOverride(merged, "researcher"))
}

// TestWithOperatorModelOverride_NoopWhenEmpty — nothing to merge leaves
// the payload byte-identical (no spurious empty context block).
func TestWithOperatorModelOverride_NoopWhenEmpty(t *testing.T) {
	orig := json.RawMessage(`{"context":{"prompt":"x"}}`)
	got, err := WithOperatorModelOverride(orig, map[string]string{"researcher": ""})
	require.NoError(t, err)
	assert.Equal(t, string(orig), string(got))
}

func TestFallbackModelOverrides(t *testing.T) {
	sw := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "researcher", Model: "gpt-5.4-mini", ModelFallback: "minimax.minimax-m2.5"},
		{Name: "lead", Model: "zai.glm-5"}, // no fallback → omitted
		{Name: "writer", ModelFallback: "gpt-5.4"},
	}}
	got := FallbackModelOverrides(sw)
	assert.Equal(t, map[string]string{
		"researcher": "minimax.minimax-m2.5",
		"writer":     "gpt-5.4",
	}, got)
	assert.Nil(t, FallbackModelOverrides(nil))
}

func TestApplyFallbackModelOverride(t *testing.T) {
	sw := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "researcher", Model: "gpt-5.4-mini", ModelFallback: "minimax.minimax-m2.5"},
	}}
	t.Run("applies and persists", func(t *testing.T) {
		reg := fakeSwarmResolver{sw: sw}
		writer := &fakeTaskWriter{}
		task := &persistence.Task{ID: "t1", ProjectID: "p1", Payload: json.RawMessage(`{"context":{"prompt":"x"}}`)}
		applied, err := ApplyFallbackModelOverride(context.Background(), reg, writer, task)
		require.NoError(t, err)
		assert.True(t, applied)
		require.NotNil(t, writer.updated, "task must be persisted")
		assert.Equal(t, "minimax.minimax-m2.5", operatorModelOverride(writer.updated.Payload, "researcher"))
	})
	t.Run("no fallback configured is a no-op", func(t *testing.T) {
		reg := fakeSwarmResolver{sw: &registry.Swarm{Roles: []registry.SwarmRole{{Name: "lead", Model: "m"}}}}
		writer := &fakeTaskWriter{}
		applied, err := ApplyFallbackModelOverride(context.Background(), reg, writer, &persistence.Task{ID: "t2"})
		require.NoError(t, err)
		assert.False(t, applied)
		assert.Nil(t, writer.updated, "no DB write when nothing to override")
	})
	t.Run("resolver error propagates", func(t *testing.T) {
		reg := fakeSwarmResolver{err: errors.New("boom")}
		applied, err := ApplyFallbackModelOverride(context.Background(), reg, &fakeTaskWriter{}, &persistence.Task{ID: "t3"})
		require.Error(t, err)
		assert.False(t, applied)
	})
}

func TestParseFallbackModelDirective(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"model: fallback", true},
		{"Retry research. model:fallback please", true},
		{"use the @fallback model", true},
		{"MODEL: FALLBACK", true},
		// The predefined recovery action — was silently re-running on the SAME
		// model pre-2026-06-13 (task …29be) because none of the directive tokens
		// matched the camelCase identifier.
		{"Re-run the researcher on its modelFallback with the same artifact-path requirement in case the first model had an output/IO bias issue", true},
		{"please use the fallback model this time", true},
		{"the model fallback didn't work last time", false}, // prose mention, opposite order — no directive
		{"retry with tighter guardrails", false},
		{"", false},
	} {
		assert.Equalf(t, tc.want, ParseFallbackModelDirective(tc.text), "text=%q", tc.text)
	}
}
