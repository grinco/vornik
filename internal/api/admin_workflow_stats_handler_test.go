package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

// stubWorkflowTelemetry mirrors api.WorkflowTelemetry for the
// handler tests. Captures the call so we can assert the parsed
// `since` value reached the service correctly.
type stubWorkflowTelemetry struct {
	lastWorkflowID string
	lastSince      time.Time
	result         any
	err            error
}

func (s *stubWorkflowTelemetry) ForWorkflow(_ context.Context, workflowID string, since time.Time) (any, error) {
	s.lastWorkflowID = workflowID
	s.lastSince = since
	return s.result, s.err
}

func TestAdminWorkflowStats_Disabled(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: false}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil)
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_NoTelemetryWired(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{
		Enabled: true, AllowedKeys: []string{"sk-admin"},
	}))
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no telemetry: want 503, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_NoKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil)
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: want 401, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_NonAdmin(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil),
		"sk-user")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_MissingWorkflow(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing workflow: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_BadSince(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a&since=banana", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since: want 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "since") {
		t.Errorf("error body should mention 'since', got %q", rec.Body.String())
	}
}

func TestAdminWorkflowStats_NegativeDurationRejected(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a&since=-7d", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative since: want 400, got %d", rec.Code)
	}
}

func TestAdminWorkflowStats_Admin_HappyPath(t *testing.T) {
	stub := &stubWorkflowTelemetry{
		result: map[string]any{
			"workflow_id": "wf-a",
			"run_count":   42,
		},
	}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(stub),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a&since=24h", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy path: want 200, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	// Service received the parsed `since` (24h ago).
	if stub.lastWorkflowID != "wf-a" {
		t.Errorf("workflow_id not threaded: %q", stub.lastWorkflowID)
	}
	ago := time.Since(stub.lastSince)
	if ago < 23*time.Hour || ago > 25*time.Hour {
		t.Errorf("since not ~24h ago: ago=%v", ago)
	}
	// Response body is the service's opaque any value, JSON-encoded.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["workflow_id"] != "wf-a" {
		t.Errorf("body shape unexpected: %v", body)
	}
}

func TestAdminWorkflowStats_DefaultSince_7d(t *testing.T) {
	stub := &stubWorkflowTelemetry{}
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(stub),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	ago := time.Since(stub.lastSince)
	if ago < 6*24*time.Hour || ago > 8*24*time.Hour {
		t.Errorf("default since should be ~7d ago, got %v", ago)
	}
}

func TestAdminWorkflowStats_RepoError(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{err: errors.New("db down")}),
	)
	req := withAdminKeyContext(
		httptest.NewRequest(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a", nil),
		"sk-admin")
	rec := httptest.NewRecorder()
	s.AdminWorkflowStats(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("repo error: want 500, got %d", rec.Code)
	}
}

// parseSinceParam table — pins the duration / RFC3339 acceptance.
func TestParseSinceParam_TableDriven(t *testing.T) {
	now := time.Now()
	cases := []struct {
		raw       string
		wantClose time.Duration // expected age (now - returned)
		wantErr   bool
	}{
		{"", 7 * 24 * time.Hour, false}, // default
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"-1h", 0, true},                   // negative rejected
		{"banana", 0, true},                // unparseable
		{"2026-05-20T00:00:00Z", 0, false}, // RFC3339 accepted (age varies; skip age check)
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseSinceParam(tc.raw, 7*24*time.Hour)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if tc.wantClose > 0 {
				ago := now.Sub(got)
				// Wide tolerance (5m) — the test isn't pinning
				// exact clock arithmetic, just confirming the
				// rough lookback distance.
				if ago < tc.wantClose-5*time.Minute || ago > tc.wantClose+5*time.Minute {
					t.Errorf("%q: ago=%v, want close to %v", tc.raw, ago, tc.wantClose)
				}
			}
		})
	}
}
