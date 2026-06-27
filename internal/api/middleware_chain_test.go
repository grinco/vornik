package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// chainAuthConfig builds an AuthConfig with one static key and one
// DB-backed key, reusing the stub types from middleware_db_keys_test.go.
func chainAuthConfig(t *testing.T) (AuthConfig, string, string) {
	t.Helper()
	dbKey, err := apikey.Generate("proj-db")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	row := &persistence.APIKey{
		ID: "01CHAINKEY", ProjectID: "proj-db", Name: "chain-test",
		KeyHash:   apikey.Hash(dbKey),
		CreatedAt: time.Now().UTC(),
	}
	cfg := AuthConfig{
		Enabled: true,
		StaticAPIKeys: map[string][]string{
			"static-key-1": {"proj-static"},
		},
		// stubAPIKeyLookup returns its row for any hash; the DB
		// backend re-derives the hash from the bearer and the row's
		// ProjectID matches the prefix-embedded project, so the
		// defense-in-depth check passes.
		APIKeyLookup: &stubAPIKeyLookup{row: row},
	}
	return cfg, "static-key-1", dbKey
}

func TestChainPath_IdentityStamped(t *testing.T) {
	cfg, staticKey, dbKey := chainAuthConfig(t)

	var gotIdentity *auth.Identity
	var gotAPIKeyID string
	handler := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = IdentityFromContext(r.Context())
		gotAPIKeyID = APIKeyIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// DB key → identity from db-keys backend + legacy keys stamped.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-db/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+dbKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("db key: status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if gotIdentity == nil || gotIdentity.Backend != "db-keys" {
		t.Fatalf("identity = %+v, want backend db-keys", gotIdentity)
	}
	if gotAPIKeyID != "01CHAINKEY" {
		t.Errorf("legacy apiKeyIDKey = %q, want 01CHAINKEY", gotAPIKeyID)
	}

	// Static key → identity from static-keys backend.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-static/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+staticKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("static key: status = %d", rec.Code)
	}
	if gotIdentity == nil || gotIdentity.Backend != "static-keys" {
		t.Fatalf("identity = %+v, want backend static-keys", gotIdentity)
	}
}

func TestChainPath_InvalidKey401(t *testing.T) {
	cfg, _, _ := chainAuthConfig(t)
	// A non-sk-vornik bearer that isn't in the static map: the DB
	// backend declines (wrong prefix), the static backend declines
	// (no match) → chain returns ErrUnauthorized.
	handler := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p/tasks", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestChainPath_WebhookSignaturePassThrough(t *testing.T) {
	cfg, _, _ := chainAuthConfig(t)
	var gotIdentity *auth.Identity
	handler := AuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/proj/src", nil)
	req.Header.Set("X-Vornik-Signature", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if gotIdentity == nil || gotIdentity.Backend != "hmac-webhook" {
		t.Fatalf("identity = %+v, want backend hmac-webhook", gotIdentity)
	}
}

func TestChainPath_IdentityFromContextNilSafe(t *testing.T) {
	//nolint:staticcheck // SA1012: nil is deliberate — this test PINS nil-safety of the helper.
	if IdentityFromContext(nil) != nil {
		t.Fatal("nil ctx must yield nil identity")
	}
}
