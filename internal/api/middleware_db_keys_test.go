package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// stubAPIKeyLookup implements APIKeyLookup for middleware tests
// without spinning up sqlmock. The fields are set per test case to
// pin the (lookup-result, error) pair the middleware will receive.
type stubAPIKeyLookup struct {
	gotHash string
	row     *persistence.APIKey
	err     error
}

func (s *stubAPIKeyLookup) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	s.gotHash = h
	if s.err != nil {
		return nil, s.err
	}
	return s.row, nil
}

// stubAPIKeyToucher records that TouchLastUsed fired (and which id).
// AuthMiddleware fires the touch in a goroutine, so the test must
// block on the WaitGroup before asserting.
type stubAPIKeyToucher struct {
	wg sync.WaitGroup
	mu sync.Mutex
	id string
}

func (s *stubAPIKeyToucher) TouchLastUsed(_ context.Context, id string) error {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
	s.wg.Done()
	return nil
}

// TestAuthMiddleware_DBKey_AcceptsValidAndStashesProject — the
// headline contract: a DB-backed key authenticates, its bound
// project lands on context under projectIDFromKeyKey (and the
// legacy projectIDKey slice for the existing ProjectAuthMiddleware
// path).
func TestAuthMiddleware_DBKey_AcceptsValidAndStashesProject(t *testing.T) {
	key, err := apikey.Generate("assistant")
	require.NoError(t, err)

	row := &persistence.APIKey{
		ID:        "akey-1",
		ProjectID: "assistant",
		Name:      "ha-key",
		KeyHash:   apikey.Hash(key),
		KeyPrefix: apikey.DisplayPrefix(key),
		CreatedAt: time.Now(),
	}
	lookup := &stubAPIKeyLookup{row: row}
	toucher := &stubAPIKeyToucher{}
	toucher.wg.Add(1)

	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  lookup,
		APIKeyToucher: toucher,
	})

	var ctxKeyID, ctxBoundProject, ctxClientKind string
	var ctxProjectList []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxKeyID, _ = r.Context().Value(apiKeyIDKey).(string)
		ctxBoundProject, _ = r.Context().Value(projectIDFromKeyKey).(string)
		ctxClientKind = APIKeyClientKindFromContext(r.Context())
		ctxProjectList, _ = r.Context().Value(projectIDKey).([]string)
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "akey-1", ctxKeyID)
	require.Equal(t, "assistant", ctxBoundProject)
	require.Empty(t, ctxClientKind)
	require.Equal(t, []string{"assistant"}, ctxProjectList)
	require.Equal(t, apikey.Hash(key), lookup.gotHash)

	// Touch fires async; wait for it before asserting.
	toucher.wg.Wait()
	toucher.mu.Lock()
	require.Equal(t, "akey-1", toucher.id)
	toucher.mu.Unlock()
}

// TestAuthMiddleware_DBKey_StashesCompanionClientKind ensures
// ProjectAuthMiddleware can distinguish companion keys from normal
// project keys without re-querying the database.
func TestAuthMiddleware_DBKey_StashesCompanionClientKind(t *testing.T) {
	key, err := apikey.Generate("assistant")
	require.NoError(t, err)
	row := &persistence.APIKey{
		ID:         "akey-1",
		ProjectID:  "assistant",
		KeyHash:    apikey.Hash(key),
		CreatedAt:  time.Now(),
		ClientKind: "claude-code",
	}
	mw := AuthMiddleware(AuthConfig{Enabled: true, APIKeyLookup: &stubAPIKeyLookup{row: row}})

	var got string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = APIKeyClientKindFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := newAuthRequest(t, "/api/v1/mcp/companion")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "claude-code", got)
}

// TestAuthMiddleware_DBKey_RejectsTamperedProjectPrefix — defense
// in depth: an attacker who steals a key for project A and rewrites
// the embedded prefix to "B" must NOT pass auth, even if the DB
// row's hash still matches. The middleware's Parse-and-compare
// step closes the loop.
func TestAuthMiddleware_DBKey_RejectsTamperedProjectPrefix(t *testing.T) {
	realKey, _ := apikey.Generate("assistant")
	// Pretend the attacker swaps the prefix; the hash now WON'T
	// match the row's stored hash. To exercise the prefix-mismatch
	// branch specifically we stage a hash-match against a tampered
	// prefix by lying about the row's project_id.
	tampered := "sk-vornik-attacker." + realKey[len("sk-vornik-assistant."):]
	row := &persistence.APIKey{
		ID:        "akey-1",
		ProjectID: "assistant", // row's TRUE project
		KeyHash:   apikey.Hash(tampered),
		CreatedAt: time.Now(),
	}
	lookup := &stubAPIKeyLookup{row: row}

	mw := AuthMiddleware(AuthConfig{Enabled: true, APIKeyLookup: lookup})

	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/attacker/tasks")
	req.Header.Set("Authorization", "Bearer "+tampered)
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, reached, "handler must not be reached on tampered prefix")
}

// TestAuthMiddleware_DBKey_NotFoundFallsThroughToStatic — during
// migration we may have a mix of DB + static keys. ErrAPIKeyNotFound
// must not short-circuit the static-map path so legacy clients keep
// working.
func TestAuthMiddleware_DBKey_NotFoundFallsThroughToStatic(t *testing.T) {
	lookup := &stubAPIKeyLookup{err: persistence.ErrAPIKeyNotFound}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  lookup,
		StaticAPIKeys: map[string][]string{"legacy-static-key": {"assistant"}},
	})

	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	// Use a sk-vornik-prefixed key so we hit the DB branch, then
	// the static map MUST also be checked (and reject) — the test
	// is that we DON'T short-circuit on the DB miss.
	notInDB, _ := apikey.Generate("assistant")
	req.Header.Set("Authorization", "Bearer "+notInDB)
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)

	// notInDB isn't in the static map either, so we still expect 401.
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.False(t, reached)

	// Now confirm the static key DOES authenticate (proving the
	// static path is still reachable when the DB returns nothing).
	req2 := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req2.Header.Set("Authorization", "Bearer legacy-static-key")
	rec2 := httptest.NewRecorder()
	var reached2 bool
	mw(authMiddlewareTestHandler(&reached2)).ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)
	require.True(t, reached2)
}

// TestAuthMiddleware_DBKey_AttributionEvenWithAuthDisabled — when
// auth_enabled=false the daemon shouldn't reject anyone, BUT when a
// DB-backed bearer token is presented we still want to resolve it
// so downstream cost attribution credits the right project. Without
// this pass every external-API call lands under "_external" even
// when the caller authenticates with a real per-project key.
func TestAuthMiddleware_DBKey_AttributionEvenWithAuthDisabled(t *testing.T) {
	key, _ := apikey.Generate("assistant")
	row := &persistence.APIKey{
		ID:        "akey-1",
		ProjectID: "assistant",
		KeyHash:   apikey.Hash(key),
		CreatedAt: time.Now(),
	}
	lookup := &stubAPIKeyLookup{row: row}
	mw := AuthMiddleware(AuthConfig{
		Enabled:      false, // auth disabled
		APIKeyLookup: lookup,
	})

	var ctxBoundProject string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxBoundProject, _ = r.Context().Value(projectIDFromKeyKey).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := newAuthRequest(t, "/api/v1/projects/assistant/tasks")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	mw(handler).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "assistant", ctxBoundProject,
		"DB-key attribution must run even when auth_enabled=false")
}

// TestAuthMiddleware_DBKey_AuthDisabledNoKeyStillPasses — the
// disabled-auth path must still pass through unauthenticated
// requests. The attribution lookup is opportunistic, not gating.
func TestAuthMiddleware_DBKey_AuthDisabledNoKeyStillPasses(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:      false,
		APIKeyLookup: &stubAPIKeyLookup{err: persistence.ErrAPIKeyNotFound},
	})
	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/p/tasks")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, reached)
}

// TestAuthMiddleware_DBKey_NonVornikPrefixSkipsLookup — keys that
// don't start with the sk-vornik prefix never get hashed against the
// DB. Avoids hashing legacy random strings and saves a DB round
// trip on the static-key path.
func TestAuthMiddleware_DBKey_NonVornikPrefixSkipsLookup(t *testing.T) {
	lookup := &stubAPIKeyLookup{err: errors.New("lookup should not be called")}
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  lookup,
		StaticAPIKeys: map[string][]string{"legacy-key": {"p"}},
	})

	var reached bool
	req := newAuthRequest(t, "/api/v1/projects/p/tasks")
	req.Header.Set("Authorization", "Bearer legacy-key")
	rec := httptest.NewRecorder()
	mw(authMiddlewareTestHandler(&reached)).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, reached)
	require.Empty(t, lookup.gotHash, "lookup must not be hit for non-sk-vornik keys")
}
