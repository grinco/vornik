package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memetic"
	"vornik.io/vornik/internal/persistence"
)

// stubArchitect satisfies the WorkflowArchitect interface for
// handler tests. Captures the call so we can assert the parsed
// workflow_id reached the architect; returns the configured
// result/error.
type stubArchitect struct {
	lastWorkflowID string
	result         any
	err            error
}

func (s *stubArchitect) Propose(_ context.Context, workflowID string) (any, error) {
	s.lastWorkflowID = workflowID
	return s.result, s.err
}

func newProposeRequest(body string, key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/admin/workflow-architect/propose",
		bytes.NewReader([]byte(body)))
	if key != "" {
		req = withAdminKeyContext(req, key)
	}
	return req
}

func TestAdminWorkflowArchitect_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"x"}`, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_NotWired(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{
		Enabled: true, AllowedKeys: []string{"sk-admin"},
	}))
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"x"}`, "sk-admin"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not wired: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_NoKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"x"}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_NonAdmin(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"x"}`, "sk-user"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_WrongMethod(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/workflow-architect/propose", nil)
	s.AdminWorkflowArchitectPropose(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_BadBody(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`not json`, "sk-admin"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_MissingWorkflow(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":""}`, "sk-admin"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty workflow: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowArchitect_HappyPath(t *testing.T) {
	stub := &stubArchitect{
		result: map[string]any{
			"id":          "wpr-1",
			"workflow_id": "research",
			"status":      "pending",
		},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(stub),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"research"}`, "sk-admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if stub.lastWorkflowID != "research" {
		t.Errorf("workflow_id not threaded: %q", stub.lastWorkflowID)
	}
}

// TestAdminWorkflowArchitect_ErrorMapping pins the sentinel → HTTP
// status table. A future "let's bump to 200 on low-confidence so
// the UI shows it" change has to update this case AND the design
// doc.
func TestAdminWorkflowArchitect_ErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  int
		errCode string
	}{
		{"low confidence", memetic.ErrLowConfidence, http.StatusNoContent, ""}, // empty body — no error envelope
		{"insufficient evidence", fmt.Errorf("wrap: %w", memetic.ErrInsufficientEvidence), http.StatusUnprocessableEntity, "ARCHITECT_VALIDATION_FAILED"},
		{"invalid evidence", memetic.ErrEvidenceInvalid, http.StatusUnprocessableEntity, "ARCHITECT_VALIDATION_FAILED"},
		{"workflow mismatch", memetic.ErrWorkflowMismatch, http.StatusUnprocessableEntity, "ARCHITECT_VALIDATION_FAILED"},
		{"invalid yaml", memetic.ErrProposalYAMLInvalid, http.StatusUnprocessableEntity, "ARCHITECT_VALIDATION_FAILED"},
		{"malformed output", memetic.ErrMalformedOutput, http.StatusBadGateway, "ARCHITECT_OUTPUT_INVALID"},
		{"rate limited", persistence.ErrProposalRateLimited, http.StatusTooManyRequests, "PROPOSAL_RATE_LIMITED"},
		{"workflow missing", os.ErrNotExist, http.StatusNotFound, "WORKFLOW_NOT_FOUND"},
		{"architect paused", memetic.ErrArchitectPaused, http.StatusServiceUnavailable, "ARCHITECT_PAUSED"},
		{"disabled for workflow", memetic.ErrArchitectDisabledForWorkflow, http.StatusServiceUnavailable, "ARCHITECT_DISABLED_FOR_WORKFLOW"},
		{"proposal kind disabled", memetic.ErrProposalKindDisabled, http.StatusConflict, "PROPOSAL_KIND_DISABLED"},
		{"generic error", errors.New("boom"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(
				WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
				WithWorkflowArchitect(&stubArchitect{err: tc.err}),
			)
			rec := httptest.NewRecorder()
			s.AdminWorkflowArchitectPropose(rec, newProposeRequest(`{"workflow_id":"x"}`, "sk-admin"))
			if rec.Code != tc.status {
				t.Fatalf("%s: want %d, got %d (body=%q)", tc.name, tc.status, rec.Code, rec.Body.String())
			}
			if tc.errCode != "" {
				if !bytes.Contains(rec.Body.Bytes(), []byte(tc.errCode)) {
					t.Errorf("%s: error code %q missing from body %q",
						tc.name, tc.errCode, rec.Body.String())
				}
			}
		})
	}
}
