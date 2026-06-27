package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Negative-path tests for the API-key CRUD surface. The happy paths
// are well-covered in api_key_handlers_test.go; these target the
// refusal branches that decide what an attacker (or a buggy client)
// actually sees when they push the surface in malformed ways.
//
// All four CRUD handlers must:
//
//  1. Refuse a wrong HTTP method with 405.
//  2. Refuse a missing apiKeyRepo with 503 (no panic).
//  3. Refuse a blank projectId with 400 + VALIDATION_ERROR.
//  4. Map repo errors to 500 + DB_ERROR (no leak of raw error text).

// --- shared scaffolding ---------------------------------------------

// failingAPIKeyRepo satisfies persistence.APIKeyRepository and forces
// every operation to error. Used to pin the DB_ERROR mapping on each
// handler — the test verifies the body says "DB_ERROR" and not the
// raw error text (defense in depth: error messages can leak schema
// or path information to an attacker).
type failingAPIKeyRepo struct {
	mu      sync.Mutex
	listErr error
}

func (f *failingAPIKeyRepo) Create(context.Context, *persistence.APIKey) error {
	return errors.New("create failed: secret detail")
}
func (f *failingAPIKeyRepo) LookupActiveByHash(context.Context, string) (*persistence.APIKey, error) {
	return nil, persistence.ErrAPIKeyNotFound
}
func (f *failingAPIKeyRepo) ListByProject(context.Context, string) ([]*persistence.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return nil, errors.New("list failed: secret detail")
}
func (f *failingAPIKeyRepo) ListCompanionByProject(context.Context, string) ([]*persistence.APIKey, error) {
	return nil, errors.New("list companion failed: secret detail")
}
func (f *failingAPIKeyRepo) TouchLastUsed(context.Context, string) error { return nil }
func (f *failingAPIKeyRepo) Revoke(context.Context, string) error {
	return errors.New("revoke failed: secret detail")
}
func (f *failingAPIKeyRepo) UpdateAllowedWorkflows(context.Context, string, []string) error {
	return errors.New("update workflows failed: secret detail")
}
func (f *failingAPIKeyRepo) UpdateAllowPush(context.Context, string, bool) error {
	return errors.New("update allow_push failed: secret detail")
}
func (f *failingAPIKeyRepo) RevokeByName(context.Context, string) error {
	return errors.New("revoke by name failed: secret detail")
}

// --- CreateAPIKey ---------------------------------------------------

func TestCreateAPIKey_RejectsWrongMethod(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/keys", nil)
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCreateAPIKey_RejectsMissingProjectID(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	// extractProjectID looks for /projects/<id>/ — a URL with no
	// segment after "projects" returns "".
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "VALIDATION_ERROR") {
		t.Fatalf("expected VALIDATION_ERROR, got %s", rec.Body.String())
	}
}

func TestCreateAPIKey_RejectsMalformedJSON(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/proj/keys", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "INVALID_JSON") {
		t.Fatalf("expected INVALID_JSON, got %s", rec.Body.String())
	}
}

func TestCreateAPIKey_RejectsOverlongName(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	// 129-char name (limit is 128). Pin the boundary so a future
	// off-by-one in the validation doesn't slip through.
	longName := strings.Repeat("x", 129)
	body := `{"name":"` + longName + `"}`
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/proj/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAPIKey_MapsRepoFailureTo500WithoutLeakingDetail(t *testing.T) {
	s := newAPIKeyServer(&failingAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/projects/proj/keys", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	s.CreateAPIKey(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "DB_ERROR") {
		t.Fatalf("expected DB_ERROR code, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("error message leaked raw repo error text: %s", rec.Body.String())
	}
}

// --- ListAPIKeys ----------------------------------------------------

func TestListAPIKeys_RejectsWrongMethod(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestListAPIKeys_ReturnsServiceUnavailableWhenRepoNil(t *testing.T) {
	s := newAPIKeyServer(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "API_KEYS_DISABLED") {
		t.Fatalf("expected API_KEYS_DISABLED, got %s", rec.Body.String())
	}
}

func TestListAPIKeys_RejectsMissingProjectID(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestListAPIKeys_MapsRepoFailureTo500(t *testing.T) {
	s := newAPIKeyServer(&failingAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj/keys", nil)
	rec := httptest.NewRecorder()
	s.ListAPIKeys(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("leaked raw repo error: %s", rec.Body.String())
	}
}

// --- RotateAPIKey ---------------------------------------------------

func TestRotateAPIKey_RejectsWrongMethod(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/keys/k1/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "k1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestRotateAPIKey_ReturnsServiceUnavailableWhenRepoNil(t *testing.T) {
	s := newAPIKeyServer(nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/p/keys/k1/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "k1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRotateAPIKey_RejectsBlankKeyID(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/keys//rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestRotateAPIKey_RejectsNotFound(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/keys/no-such-key/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "no-such-key")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestRotateAPIKey_MapsListFailureTo500(t *testing.T) {
	s := newAPIKeyServer(&failingAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/keys/k1/rotate", nil)
	rec := httptest.NewRecorder()
	s.RotateAPIKey(rec, req, "k1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
}

// --- RevokeAPIKey ---------------------------------------------------

func TestRevokeAPIKey_RejectsWrongMethod(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/keys/k1/revoke", nil)
	rec := httptest.NewRecorder()
	s.RevokeAPIKey(rec, req, "k1")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestRevokeAPIKey_ReturnsServiceUnavailableWhenRepoNil(t *testing.T) {
	s := newAPIKeyServer(nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/p/keys/k1", nil)
	rec := httptest.NewRecorder()
	s.RevokeAPIKey(rec, req, "k1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRevokeAPIKey_RejectsBlankKeyID(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj/keys/", nil)
	rec := httptest.NewRecorder()
	s.RevokeAPIKey(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestRevokeAPIKey_RejectsNotFound(t *testing.T) {
	repo := &memAPIKeyRepo{}
	s := newAPIKeyServer(repo)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj/keys/missing", nil)
	rec := httptest.NewRecorder()
	s.RevokeAPIKey(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// --- UpdateAllowPushHandler -----------------------------------------

func TestUpdateAllowPush_ReturnsServiceUnavailableWhenRepoNil(t *testing.T) {
	s := newAPIKeyServer(nil)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/p/keys/k1/allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "k1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestUpdateAllowPush_RejectsBlankKeyID(t *testing.T) {
	s := newAPIKeyServer(&memAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj/keys//allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateAllowPush_MapsListFailureTo500(t *testing.T) {
	s := newAPIKeyServer(&failingAPIKeyRepo{})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj/keys/k1/allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "k1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("leaked raw repo error: %s", rec.Body.String())
	}
}

// allowPushUpdateFailRepo returns one row from List but errors on
// UpdateAllowPush, letting us pin the 500 branch in the handler.
type allowPushUpdateFailRepo struct {
	*memAPIKeyRepo
}

func (r *allowPushUpdateFailRepo) UpdateAllowPush(_ context.Context, _ string, _ bool) error {
	return errors.New("update allow_push failed: secret detail")
}

func TestUpdateAllowPush_MapsUpdateFailureTo500(t *testing.T) {
	inner := &memAPIKeyRepo{}
	_ = inner.Create(context.Background(), &persistence.APIKey{
		ID: "k1", ProjectID: "proj", Name: "x",
	})
	spy := &allowPushUpdateFailRepo{memAPIKeyRepo: inner}
	s := newAPIKeyServer(spy)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj/keys/k1/allow-push",
		strings.NewReader(`{"allow_push":true}`))
	rec := httptest.NewRecorder()
	s.UpdateAllowPushHandler(rec, req, "k1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("leaked raw repo error: %s", rec.Body.String())
	}
}
