package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// systemStepCov_handler is a minimal SystemHandler used by the
// registry guard tests. Local to this file to avoid colliding with
// the recordingSystemHandler in system_step_test.go.
type systemStepCov_handler struct{ name string }

func (h *systemStepCov_handler) Name() string { return h.name }
func (h *systemStepCov_handler) Execute(_ context.Context, _ SystemStepInput) (SystemStepResult, error) {
	return SystemStepResult{}, nil
}

// TestSystemStepCov_RegisterNilGuards — Register is a no-op for a nil
// receiver and for a nil handler. The service container relies on the
// nil-handler no-op to pass conditionally constructed handlers without
// nil-guarding every call site.
func TestSystemStepCov_RegisterNilGuards(t *testing.T) {
	var nilReg *SystemHandlerRegistry
	nilReg.Register(&systemStepCov_handler{name: "x"}) // must not panic

	reg := NewSystemHandlerRegistry()
	reg.Register(nil) // nil handler no-op
	assert.Empty(t, reg.Names(), "registering nil must not add an entry")

	reg.Register(&systemStepCov_handler{name: "rag.extract"})
	assert.Equal(t, []string{"rag.extract"}, reg.Names())

	// Last-write-wins on duplicate names.
	replacement := &systemStepCov_handler{name: "rag.extract"}
	reg.Register(replacement)
	got, ok := reg.Get("rag.extract")
	assert.True(t, ok)
	assert.Same(t, replacement, got, "duplicate Register must overwrite (last-write-wins)")
}

// TestSystemStepCov_GetNilReceiver — Get on a nil registry returns
// (nil,false) rather than dereferencing the nil map.
func TestSystemStepCov_GetNilReceiver(t *testing.T) {
	var nilReg *SystemHandlerRegistry
	h, ok := nilReg.Get("anything")
	assert.Nil(t, h)
	assert.False(t, ok)
}

// TestSystemStepCov_NamesNilReceiver — Names on a nil registry returns
// nil (the doctor/validator treats nil as "no handlers wired").
func TestSystemStepCov_NamesNilReceiver(t *testing.T) {
	var nilReg *SystemHandlerRegistry
	assert.Nil(t, nilReg.Names())
}
