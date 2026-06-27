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

// TestOperatorOverridesCov_OperatorModelOverride_DefensiveBranches walks
// every early-return guard in operatorModelOverride.
func TestOperatorOverridesCov_OperatorModelOverride_DefensiveBranches(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		role    string
		want    string
	}{
		{name: "empty payload", payload: "", role: "researcher", want: ""},
		{name: "empty role", payload: `{"context":{}}`, role: "", want: ""},
		{name: "malformed json", payload: `{not json`, role: "researcher", want: ""},
		{name: "no context block", payload: `{"priority":1}`, role: "researcher", want: ""},
		{name: "no override block", payload: `{"context":{"prompt":"x"}}`, role: "researcher", want: ""},
		{name: "role not present", payload: `{"context":{"operator_model_override":{"writer":"m"}}}`, role: "researcher", want: ""},
		{name: "hit", payload: `{"context":{"operator_model_override":{"researcher":"fb"}}}`, role: "researcher", want: "fb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload json.RawMessage
			if tc.payload != "" {
				payload = json.RawMessage(tc.payload)
			}
			assert.Equal(t, tc.want, operatorModelOverride(payload, tc.role))
		})
	}
}

// TestOperatorOverridesCov_WithOperatorModelOverride_UnmarshalError — a
// non-empty but malformed payload surfaces the unmarshal error.
func TestOperatorOverridesCov_WithOperatorModelOverride_UnmarshalError(t *testing.T) {
	_, err := WithOperatorModelOverride(json.RawMessage(`{not json`), map[string]string{"r": "m"})
	require.Error(t, err)
}

// TestOperatorOverridesCov_WithOperatorModelOverride_CreatesContextBlock —
// a nil payload becomes a minimal object carrying the override.
func TestOperatorOverridesCov_WithOperatorModelOverride_CreatesContextBlock(t *testing.T) {
	merged, err := WithOperatorModelOverride(nil, map[string]string{"researcher": "fb"})
	require.NoError(t, err)
	assert.Equal(t, "fb", operatorModelOverride(merged, "researcher"))
}

// TestOperatorOverridesCov_ApplyFallback_NilArgs — the nil-guard returns
// (false, nil) for any nil dependency.
func TestOperatorOverridesCov_ApplyFallback_NilArgs(t *testing.T) {
	reg := fakeSwarmResolver{sw: &registry.Swarm{}}
	w := &fakeTaskWriter{}
	task := &persistence.Task{ID: "t"}

	applied, err := ApplyFallbackModelOverride(context.Background(), reg, w, nil)
	require.NoError(t, err)
	assert.False(t, applied)

	applied, err = ApplyFallbackModelOverride(context.Background(), nil, w, task)
	require.NoError(t, err)
	assert.False(t, applied)

	applied, err = ApplyFallbackModelOverride(context.Background(), reg, nil, task)
	require.NoError(t, err)
	assert.False(t, applied)
}

// TestOperatorOverridesCov_ApplyFallback_TaskUpdateError — the swarm has a
// fallback so the override is built, but persisting the task fails; the
// error propagates and applied is false.
func TestOperatorOverridesCov_ApplyFallback_TaskUpdateError(t *testing.T) {
	sw := &registry.Swarm{Roles: []registry.SwarmRole{
		{Name: "researcher", Model: "primary", ModelFallback: "fb"},
	}}
	reg := fakeSwarmResolver{sw: sw}
	w := &fakeTaskWriter{err: errors.New("update failed")}
	task := &persistence.Task{ID: "t", ProjectID: "p", Payload: json.RawMessage(`{"context":{"prompt":"x"}}`)}

	applied, err := ApplyFallbackModelOverride(context.Background(), reg, w, task)
	require.Error(t, err)
	assert.False(t, applied)
}
