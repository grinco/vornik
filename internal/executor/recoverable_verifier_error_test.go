package executor

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/verifier"
)

// TestRecoverableVerifierError_Error covers the four branches of the
// Error() method: nil receiver, nil Err, populated Err, and nil-Err
// with the explicit fallback message.
func TestRecoverableVerifierError_Error(t *testing.T) {
	t.Run("nil receiver returns fallback", func(t *testing.T) {
		var rve *RecoverableVerifierError
		assert.Equal(t, "recoverable verifier failure", rve.Error())
	})
	t.Run("nil Err returns fallback", func(t *testing.T) {
		rve := &RecoverableVerifierError{}
		assert.Equal(t, "recoverable verifier failure", rve.Error())
	})
	t.Run("populated Err returns inner message", func(t *testing.T) {
		rve := &RecoverableVerifierError{Err: errors.New("inner boom")}
		assert.Equal(t, "inner boom", rve.Error())
	})
	t.Run("blocked URLs attached", func(t *testing.T) {
		rve := &RecoverableVerifierError{
			Err: errors.New("blocked"),
			BlockedURLs: []verifier.BlockedURL{
				{URL: "https://x.example", Reason: "phishing"},
			},
		}
		assert.Equal(t, "blocked", rve.Error())
		assert.Len(t, rve.BlockedURLs, 1)
	})
}

// TestRecoverableVerifierError_Unwrap covers the three branches.
func TestRecoverableVerifierError_Unwrap(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var rve *RecoverableVerifierError
		assert.Nil(t, rve.Unwrap())
	})
	t.Run("nil Err returns nil", func(t *testing.T) {
		rve := &RecoverableVerifierError{}
		assert.Nil(t, rve.Unwrap())
	})
	t.Run("populated Err is returned verbatim", func(t *testing.T) {
		inner := errors.New("inner")
		rve := &RecoverableVerifierError{Err: inner}
		assert.Same(t, inner, rve.Unwrap())
	})
}

// TestRecoverableVerifierError_AsViaErrorsAs — the errors.As contract
// the workflow router relies on must keep working.
func TestRecoverableVerifierError_AsViaErrorsAs(t *testing.T) {
	wrapped := &RecoverableVerifierError{Err: errors.New("inner")}
	var got *RecoverableVerifierError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As should match RecoverableVerifierError")
	}
	assert.Same(t, wrapped, got)
}
