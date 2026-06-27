package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubSchemaValidator captures every Validate call so tests
// can assert the resolve hook invoked schema validation on
// the expected envelope.
type stubSchemaValidator struct {
	has    map[string]bool
	failOn map[string]error // schema id → error to return; nil = pass
	calls  int
}

func (s *stubSchemaValidator) HasSchema(id string) bool {
	if s == nil {
		return false
	}
	return s.has[id]
}

func (s *stubSchemaValidator) Validate(id string, _ any) error {
	if s == nil {
		return nil
	}
	s.calls++
	if err, ok := s.failOn[id]; ok {
		return err
	}
	return nil
}

// TestResolveCPC_SchemaValidationPasses asserts that a
// well-formed envelope clears both tiers (shape + schema)
// and the CPC resolves as completed.
func TestResolveCPC_SchemaValidationPasses(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	validator := &stubSchemaValidator{
		has: map[string]bool{"spec_envelope.v1": true},
	}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.schemaRegistry = validator

	cpcID := "ccp_pass"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID:             cpcID,
		Status:         persistence.CPCStatusRunning,
		ExpectedSchema: "spec_envelope.v1",
	}
	envelope := []byte(`{"schema":"spec_envelope.v1","status":"ok","data":{"kpis":["a"]}}`)
	callee := &persistence.Task{ID: "t-callee", CrossProjectCallID: &cpcID, ResultEnvelope: envelope}

	e.resolveCrossProjectCallForTask(context.Background(), callee, true)
	if validator.calls != 1 {
		t.Errorf("Validate should be called once, got %d", validator.calls)
	}
	if cpc.rows[cpcID].Status != persistence.CPCStatusCompleted {
		t.Errorf("status = %q, want completed", cpc.rows[cpcID].Status)
	}
}

// TestResolveCPC_SchemaValidationFails asserts a schema-
// validation failure rejects the CPC with a useful reason
// message that includes the validator's error.
func TestResolveCPC_SchemaValidationFails(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	validator := &stubSchemaValidator{
		has:    map[string]bool{"spec_envelope.v1": true},
		failOn: map[string]error{"spec_envelope.v1": errors.New("data.kpis: required")},
	}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.schemaRegistry = validator

	cpcID := "ccp_fail"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID:             cpcID,
		Status:         persistence.CPCStatusRunning,
		ExpectedSchema: "spec_envelope.v1",
	}
	envelope := []byte(`{"schema":"spec_envelope.v1","status":"ok"}`)
	callee := &persistence.Task{ID: "t-callee", CrossProjectCallID: &cpcID, ResultEnvelope: envelope}

	e.resolveCrossProjectCallForTask(context.Background(), callee, true)

	stored := cpc.rows[cpcID]
	if stored.Status != persistence.CPCStatusRejected {
		t.Errorf("status = %q, want rejected", stored.Status)
	}
	if stored.ErrorMessage == nil || !contains(*stored.ErrorMessage, "JSON-Schema validation") {
		t.Errorf("error_message should mention schema validation, got %v", stored.ErrorMessage)
	}
	if stored.ErrorMessage == nil || !contains(*stored.ErrorMessage, "data.kpis: required") {
		t.Errorf("error_message should include validator error, got %v", stored.ErrorMessage)
	}
}

// TestResolveCPC_NoSchemaPassesThroughShapeOnly asserts that
// when no schema is registered for the expected_schema id, the
// resolve hook uses envelope-shape validation only. This is
// the "deployments without configs/schemas/" path — must keep
// working for backward compatibility.
func TestResolveCPC_NoSchemaPassesThroughShapeOnly(t *testing.T) {
	withInterProjectEnabled(t)
	cpc := newMockCPCRepo()
	validator := &stubSchemaValidator{
		has: map[string]bool{}, // empty — no schemas registered
	}
	e, _ := newCallProjectExecutor(&MockWorkflowResolver{}, cpc)
	e.schemaRegistry = validator

	cpcID := "ccp_nosch"
	cpc.rows[cpcID] = &persistence.CrossProjectCall{
		ID:             cpcID,
		Status:         persistence.CPCStatusRunning,
		ExpectedSchema: "spec_envelope.v1",
	}
	// Envelope passes shape check (schema + status present);
	// schema-validation path skipped because HasSchema returns
	// false. Resolves as completed.
	envelope := []byte(`{"schema":"spec_envelope.v1","status":"ok"}`)
	callee := &persistence.Task{ID: "t-callee", CrossProjectCallID: &cpcID, ResultEnvelope: envelope}

	e.resolveCrossProjectCallForTask(context.Background(), callee, true)

	if validator.calls != 0 {
		t.Errorf("Validate should NOT be called when HasSchema=false, got %d calls", validator.calls)
	}
	if cpc.rows[cpcID].Status != persistence.CPCStatusCompleted {
		t.Errorf("status = %q, want completed (shape passed, schema skipped)", cpc.rows[cpcID].Status)
	}
}
