package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// D4 (audit 2026-06-10): the five CLI-mirror admin handlers used an
// inline IsAdminKey check that bypassed requireAdminGate's
// auth-disabled override. On an auth-OFF deployment (with admin.enabled)
// the trusted local operator was 401'd because no API key is present.
// Each handler now routes through requireAdminGate; these tests pin that
// an auth-disabled request WITHOUT any API key is admitted (i.e. NOT
// 401/403). They fail pre-fix (the inline path 401s) and pass post-fix.

// authOffNoKeyReq builds an auth-disabled request carrying no API key —
// exactly the shape a trusted local operator hits on an auth-OFF box.
func authOffNoKeyReq(method, target string) *http.Request {
	return authDisabledReq(httptest.NewRequest(method, target, nil))
}

func TestD4_AdminAuditList_AuthOff_AdmittedWithoutKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithAdminAuditRepository(&stubAdminAuditRepo{}),
	)
	rec := httptest.NewRecorder()
	s.AdminAuditList(rec, authOffNoKeyReq(http.MethodGet, "/api/v1/admin/audit"))
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth-off operator must be admitted, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for auth-off admin audit, got %d", rec.Code)
	}
}

func TestD4_AdminChatAuditList_AuthOff_AdmittedWithoutKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithChatAuditRepository(&stubChatAuditAdminRepo{}),
	)
	rec := httptest.NewRecorder()
	s.AdminChatAuditList(rec, authOffNoKeyReq(http.MethodGet, "/api/v1/admin/chat-audit"))
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth-off operator must be admitted, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for auth-off chat audit, got %d", rec.Code)
	}
}

func TestD4_AdminWorkflowStats_AuthOff_AdmittedWithoutKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowTelemetry(&stubWorkflowTelemetry{result: map[string]any{"ok": true}}),
	)
	rec := httptest.NewRecorder()
	// workflow param required so we reach past the gate to a 200.
	s.AdminWorkflowStats(rec, authOffNoKeyReq(http.MethodGet, "/api/v1/admin/workflow-stats?workflow=wf-a"))
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth-off operator must be admitted, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for auth-off workflow stats, got %d", rec.Code)
	}
}

func TestD4_AdminWorkflowArchitect_AuthOff_AdmittedWithoutKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowArchitect(&stubArchitect{}),
	)
	rec := httptest.NewRecorder()
	req := authDisabledReq(newProposeRequest(`{"workflow_id":"wf-a"}`, ""))
	s.AdminWorkflowArchitectPropose(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth-off operator must be admitted, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for auth-off architect propose, got %d", rec.Code)
	}
}

func TestD4_AdminWorkflowProposalsList_AuthOff_AdmittedWithoutKey(t *testing.T) {
	s := NewServer(
		WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}),
		WithWorkflowProposals(&stubProposalRepo{}),
	)
	rec := httptest.NewRecorder()
	s.AdminWorkflowProposalsList(rec, authOffNoKeyReq(http.MethodGet, "/api/v1/admin/workflow-proposals"))
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("auth-off operator must be admitted, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for auth-off proposals list, got %d", rec.Code)
	}
}
