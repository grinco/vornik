package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// The self-service surface of §5.5. Unlike the operator routes, these act on
// the CALLER'S OWN account, identified by the session — so they need no
// operator capability, and they must not be reachable by a caller who has no
// session at all.

func newSelfServer(t *testing.T) (*Server, *accountsStubRepo, *keysStubRepo, *stubLinkCodes) {
	t.Helper()
	repo := newAccountsStubRepo()
	keys := &keysStubRepo{rows: map[string]*persistence.APIKey{}}
	codes := newStubLinkCodes()
	cfg := config.DefaultConfig()
	acc := authz.NewAccounts(repo, &accountsStubAudit{}).WithAPIKeys(keys).WithLinkCodes(codes)
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAccountsService(acc))
	return s, repo, keys, codes
}

// asUser stamps an authenticated browser session for userID.
func asUser(req *http.Request, userID string) *http.Request {
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{
		Subject: userID,
		Extra:   map[string]any{auth.ExtraSessionUserID: userID},
	})
	return req.WithContext(ctx)
}

// TestAccountSelf_RequiresASession is the gate. These routes derive WHOSE
// account to act on from the session, so a caller without one has no account
// to act on — and must not fall back to acting on somebody else's.
func TestAccountSelf_RequiresASession(t *testing.T) {
	s, _, _, _ := newSelfServer(t)
	for _, path := range []string{"/api/v1/account/identities", "/api/v1/account/link-codes"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), authEnabledKey, true))
		rec := httptest.NewRecorder()
		s.AccountSelf(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", path, rec.Code)
		}
	}
}

// TestAccountSelf_ListsOnlyTheCallersIdentities — the panel shows everything
// that resolves to you, and nothing that resolves to anyone else.
func TestAccountSelf_ListsOnlyTheCallersIdentities(t *testing.T) {
	s, repo, _, _ := newSelfServer(t)
	repo.users["me"] = &persistence.User{ID: "me", DisplayName: "Me"}
	repo.users["them"] = &persistence.User{ID: "them", DisplayName: "Them"}
	repo.bindings["telegram:1"] = "me"
	repo.bindings["api_key:akey_mine"] = "me"
	repo.bindings["telegram:2"] = "them"

	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/account/identities", nil), "me")
	rec := httptest.NewRecorder()
	s.AccountSelf(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "telegram") || !strings.Contains(body, "api_key") {
		t.Errorf("the caller's own bindings are missing: %s", body)
	}
	if strings.Contains(body, `"externalId":"2"`) {
		t.Errorf("another account's binding leaked into the response: %s", body)
	}
}

// TestAccountSelf_IssuesALinkCodeForTheCallerOnly: the code binds a channel to
// whoever redeems it, so issuing one for somebody else would be a way to
// capture their channel.
func TestAccountSelf_IssuesALinkCodeForTheCallerOnly(t *testing.T) {
	s, repo, _, codes := newSelfServer(t)
	repo.users["me"] = &persistence.User{ID: "me", DisplayName: "Me"}

	req := asUser(httptest.NewRequest(http.MethodPost, "/api/v1/account/link-codes", strings.NewReader(`{"userId":"them"}`)), "me")
	rec := httptest.NewRecorder()
	s.AccountSelf(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	for _, lc := range codes.codes {
		if lc.UserID != "me" {
			t.Fatalf("a body-supplied userId steered the code to %q; it must always be the session's user", lc.UserID)
		}
	}
}

// TestAccountSelf_ClaimUsesTheSessionUserAndIsRateLimited — same claim
// semantics as the operator route, bound to the caller.
func TestAccountSelf_ClaimUsesTheSessionUserAndIsRateLimited(t *testing.T) {
	s, repo, keys, _ := newSelfServer(t)
	repo.users["me"] = &persistence.User{ID: "me", DisplayName: "Me"}
	keys.rows["akey_x"] = &persistence.APIKey{ID: "akey_x", ProjectID: "p", KeyHash: apikey.Hash("sk-right"), CreatedAt: time.Now().UTC()}

	post := func(keyID, secret string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"keyId": keyID, "keySecret": secret})
		req := asUser(httptest.NewRequest(http.MethodPost, "/api/v1/account/keys/claim", strings.NewReader(string(b))), "me")
		rec := httptest.NewRecorder()
		s.AccountSelf(rec, req)
		return rec
	}
	if rec := post("akey_x", "sk-right"); rec.Code != http.StatusOK {
		t.Fatalf("claim: status %d body %s", rec.Code, rec.Body.String())
	}
	if repo.bindings["api_key:akey_x"] != "me" {
		t.Errorf("binding = %q, want the session user", repo.bindings["api_key:akey_x"])
	}

	// The same per-key-id bucket applies here as on the operator route: the
	// attack does not care which door the guesses arrive through.
	var last *httptest.ResponseRecorder
	for i := 0; i < authz.ClaimAttemptLimit+1; i++ {
		last = post("akey_ghost", "sk-guess")
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("self-service claim was not rate limited: got %d", last.Code)
	}
}
