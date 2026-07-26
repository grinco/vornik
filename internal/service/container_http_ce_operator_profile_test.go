package service

// Regression test for the Community operator-profile UI wiring.
//
// Operator profiles are a Community feature, but their UI source was
// accidentally appended inside adminUIOptions. That helper is only called
// when providers.Admin is true, so CE rendered "repository not wired" even
// though the SQLite repository and dispatcher tool were both available.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewContainer_CommunityWiresOperatorProfileUI(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)

	c, err := NewContainer(cfg, "")
	if err != nil {
		t.Fatalf("NewContainer: unexpected error: %v", err)
	}
	if c.providers.Admin {
		t.Fatal("precondition: Community providers must have Admin=false")
	}
	if c.repos == nil || c.repos.OperatorProfiles == nil {
		t.Fatal("precondition: Community storage must provide OperatorProfiles")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/memory/operators", nil)
	c.HTTPServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/memory/operators: status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "Operator profile repository not wired") {
		t.Fatal("Community operator-profile UI must receive the repository")
	}
	if !strings.Contains(body, "No operator profiles yet") {
		t.Fatalf("empty Community repository should render the normal empty state; body:\n%s", body)
	}
}
