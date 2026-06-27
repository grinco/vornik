package executor

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// TestMarkRetryable_NilInput — nil error short-circuits to nil
// so callers can wrap without a pre-check. Production code uses
// the form `return markRetryable(possiblyNilErr)`.
func TestMarkRetryable_NilInput(t *testing.T) {
	assert.Nil(t, markRetryable(nil))
}

// TestSerializeCheckpointMetadata_NilCheckpoint — nil input
// surfaces an error rather than a bogus null-payload JSON
// document. Otherwise the persisted task_message metadata
// would be `null`, breaking UI rendering downstream.
func TestSerializeCheckpointMetadata_NilCheckpoint(t *testing.T) {
	b, err := SerializeCheckpointMetadata(nil)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "nil checkpoint")
}

// TestWithWorkflowResolver_NonNil — the option setter assigns
// the resolver onto the executor. Mirrors the nil-resolver test
// for the active branch.
func TestWithWorkflowResolver_NonNil(t *testing.T) {
	e := &Executor{}
	r := &MockWorkflowResolver{projects: map[string]*registry.Project{"p": {ID: "p"}}}
	WithWorkflowResolver(r)(e)
	assert.Same(t, r, e.workflows)
}

// TestSetWorkflowResolver_NonNil — runtime setter with non-nil
// resolver assigns it. The pre-existing test pinned nil-no-op;
// this completes the table.
func TestSetWorkflowResolver_NonNil(t *testing.T) {
	e := &Executor{}
	r := &MockWorkflowResolver{}
	e.SetWorkflowResolver(r)
	assert.Same(t, r, e.workflows)
}

// TestRecordModelFallback_NilSafe — every defensive early-return
// for the metrics method: nil receiver, empty role / primary /
// fallback labels. The counter must remain unincremented on each.
func TestRecordModelFallback_NilSafe(t *testing.T) {
	registry := prometheus.NewRegistry()
	m := NewMetrics(registry)

	// nil receiver
	var nilM *Metrics
	nilM.RecordModelFallback("lead", "a", "b") // must not panic

	// empty role
	m.RecordModelFallback("", "a", "b")
	// empty primary
	m.RecordModelFallback("lead", "", "b")
	// empty fallback
	m.RecordModelFallback("lead", "a", "")
	assert.Equal(t, 0.0, testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("lead", "a", "b")),
		"empty-label calls must not increment the counter")

	// non-empty all labels → counter increments
	m.RecordModelFallback("lead", "primary", "fallback")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.ModelFallbackTotal.WithLabelValues("lead", "primary", "fallback")))
}

// TestRetryableError_Error_Unwrap — pin the wrapping
// retryableError shape: Error() returns the inner error's
// Error(); Unwrap() returns the inner error so errors.Is can
// traverse.
func TestRetryableError_ErrorAndUnwrap(t *testing.T) {
	inner := assertErrf("transient io blip")
	re := markRetryable(inner)
	require.NotNil(t, re)
	assert.Equal(t, inner.Error(), re.Error())

	// Pull the inner error out via Unwrap (via errors.As semantics).
	var unwrapped interface{ Unwrap() error }
	assert.True(t, asInterface(re, &unwrapped))
	require.NotNil(t, unwrapped)
	assert.Equal(t, inner, unwrapped.Unwrap())
}

// asInterface is the test-side cast helper for the
// retryableError unwrap check. Returns true when the value
// implements the target interface.
func asInterface(val any, target any) bool {
	switch tgt := target.(type) {
	case *interface{ Unwrap() error }:
		if u, ok := val.(interface{ Unwrap() error }); ok {
			*tgt = u
			return true
		}
	}
	return false
}
