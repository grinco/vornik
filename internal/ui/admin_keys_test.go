package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// adminKeysRepoStub maps project_id → keys so the daemon-wide
// admin view can be exercised with a multi-project fixture
// without duplicating every method on the live APIKeyRepository.
type adminKeysRepoStub struct {
	byProject   map[string][]*persistence.APIKey
	lastCapsID  string
	lastCaps    persistence.APIKeyCapabilities
	capsUpdated bool
}

func (s *adminKeysRepoStub) Create(context.Context, *persistence.APIKey) error {
	panic("unused")
}
func (s *adminKeysRepoStub) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	panic("unused")
}
func (s *adminKeysRepoStub) ListByProject(_ context.Context, projectID string) ([]*persistence.APIKey, error) {
	return s.byProject[projectID], nil
}
func (s *adminKeysRepoStub) ListCompanionByProject(_ context.Context, projectID string) ([]*persistence.APIKey, error) {
	var out []*persistence.APIKey
	for _, k := range s.byProject[projectID] {
		if k != nil && k.ClientKind != "" {
			out = append(out, k)
		}
	}
	return out, nil
}
func (s *adminKeysRepoStub) TouchLastUsed(context.Context, string) error { return nil }
func (s *adminKeysRepoStub) Revoke(context.Context, string) error        { return nil }
func (s *adminKeysRepoStub) UpdateAllowedWorkflows(context.Context, string, []string) error {
	return nil
}
func (s *adminKeysRepoStub) UpdateAllowPush(context.Context, string, bool) error { return nil }
func (s *adminKeysRepoStub) RevokeByName(context.Context, string) error          { return nil }
func (s *adminKeysRepoStub) UpdateCapabilities(_ context.Context, id string, caps persistence.APIKeyCapabilities) error {
	s.lastCapsID = id
	s.lastCaps = caps
	s.capsUpdated = true
	return nil
}

// TestAdminKeys_SetCaps covers the capability-grant form POST: skill
// caps get set (skill_write normalises skill_read on too).
func TestAdminKeys_SetCaps(t *testing.T) {
	repo := &adminKeysRepoStub{byProject: map[string][]*persistence.APIKey{}}
	reg := registry.New()
	s := NewServer(WithAPIKeyRepository(repo), WithProjectRegistry(reg))

	form := url.Values{"id": {"k_x"}, "skill_write": {"on"}, "skill_admin": {"on"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.AdminKeys(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303, got %d", rec.Code)
	}
	if !repo.capsUpdated || repo.lastCapsID != "k_x" {
		t.Fatalf("UpdateCapabilities not called for k_x: %+v", repo)
	}
	if !repo.lastCaps.SkillWrite || !repo.lastCaps.SkillAdmin {
		t.Fatalf("skill caps not set: %+v", repo.lastCaps)
	}
	if !repo.lastCaps.SkillRead {
		t.Fatalf("skill_write must normalise skill_read on: %+v", repo.lastCaps)
	}
}

// TestAdminKeys_RendersAcrossProjects covers the happy path:
// two projects with one key each → the page renders both rows.
func TestAdminKeys_RendersAcrossProjects(t *testing.T) {
	now := time.Now()
	repo := &adminKeysRepoStub{byProject: map[string][]*persistence.APIKey{
		"alpha": {
			{ID: "k_a1", ProjectID: "alpha", Name: "alpha admin", KeyPrefix: "sk-vornik-alpha.ab12", CreatedAt: now.Add(-2 * time.Hour)},
		},
		"beta": {
			{ID: "k_b1", ProjectID: "beta", Name: "beta CI", KeyPrefix: "sk-vornik-beta.cd34", CreatedAt: now.Add(-1 * time.Hour), RevokedAt: &now},
		},
	}}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"alpha": {ID: "alpha"}, "beta": {ID: "beta"},
	})

	s := NewServer(WithAPIKeyRepository(repo), WithProjectRegistry(reg))
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "alpha admin") {
		t.Errorf("alpha key missing from page")
	}
	if !strings.Contains(body, "beta CI") {
		t.Errorf("beta key missing from page")
	}
	// Header summary should report 2 total + 1 active + 1 revoked.
	if !strings.Contains(body, "2 total") {
		excerpt := body
		if len(excerpt) > 800 {
			excerpt = excerpt[:800]
		}
		t.Errorf("header should report 2 total; body excerpt: %s", excerpt)
	}
	if !strings.Contains(body, "1 active") || !strings.Contains(body, "1 revoked") {
		t.Errorf("header should distinguish active vs revoked")
	}
}

// TestAdminKeys_FilterByProject pins the ?project= filter
// scopes to one row.
func TestAdminKeys_FilterByProject(t *testing.T) {
	now := time.Now()
	repo := &adminKeysRepoStub{byProject: map[string][]*persistence.APIKey{
		"alpha": {{ID: "k1", ProjectID: "alpha", Name: "alpha-key", KeyPrefix: "ka", CreatedAt: now}},
		"beta":  {{ID: "k2", ProjectID: "beta", Name: "beta-key", KeyPrefix: "kb", CreatedAt: now}},
	}}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"alpha": {ID: "alpha"}, "beta": {ID: "beta"},
	})
	s := NewServer(WithAPIKeyRepository(repo), WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/admin/keys?project=alpha", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	body := rec.Body.String()
	if !strings.Contains(body, "alpha-key") {
		t.Errorf("alpha row should be present")
	}
	if strings.Contains(body, "beta-key") {
		t.Errorf("beta row should be filtered out")
	}
}

// TestAdminKeys_FilterByStatusRevoked covers the status filter
// path — operators often want to see just the revoked keys when
// auditing rotation hygiene.
func TestAdminKeys_FilterByStatusRevoked(t *testing.T) {
	now := time.Now()
	revoked := now.Add(-1 * time.Hour)
	repo := &adminKeysRepoStub{byProject: map[string][]*persistence.APIKey{
		"alpha": {
			{ID: "k_active", ProjectID: "alpha", Name: "active", KeyPrefix: "k1", CreatedAt: now.Add(-2 * time.Hour)},
			{ID: "k_rev", ProjectID: "alpha", Name: "revoked", KeyPrefix: "k2", CreatedAt: now.Add(-3 * time.Hour), RevokedAt: &revoked},
		},
	}}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"alpha": {ID: "alpha"}})
	s := NewServer(WithAPIKeyRepository(repo), WithProjectRegistry(reg))

	req := httptest.NewRequest(http.MethodGet, "/admin/keys?status=revoked", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	body := rec.Body.String()
	if !strings.Contains(body, ">revoked<") {
		t.Errorf("revoked status pill missing from filtered view")
	}
}

// TestAdminKeys_Unwired_RendersFriendly502 sanity: a deployment
// without the apiKeyRepo wired still gets a clean empty-state
// page rather than 500.
func TestAdminKeys_Unwired(t *testing.T) {
	s := NewServer()
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	rec := httptest.NewRecorder()
	s.adminRouter(rec, withAdminUI(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not wired on this deployment") {
		t.Errorf("body should explain unwired state")
	}
}

// TestAdminKeyToRow_StatusTransitions pins the active/revoked/
// expired computation since the template colour-codes on the
// returned Status string.
func TestAdminKeyToRow_StatusTransitions(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	future := now.Add(24 * time.Hour)

	cases := []struct {
		name string
		k    *persistence.APIKey
		want string
	}{
		{"active no expiry", &persistence.APIKey{CreatedAt: now}, "active"},
		{"active future expiry", &persistence.APIKey{CreatedAt: now, ExpiresAt: &future}, "active"},
		{"revoked wins over expiry", &persistence.APIKey{CreatedAt: now, RevokedAt: &past, ExpiresAt: &future}, "revoked"},
		{"expired no revoke", &persistence.APIKey{CreatedAt: now, ExpiresAt: &past}, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := adminKeyToRow(tc.k, now)
			if r.Status != tc.want {
				t.Errorf("status = %q, want %q", r.Status, tc.want)
			}
		})
	}
}
