// Coverage for the codex subscription auth manager — the on-disk
// auth.json loader and JWT decoder. Stops short of exercising
// refresh against the real OAuth endpoint (no network in unit
// tests); the no-refresh-needed path is exercised by writing a
// future-dated id_token so Token() returns from cache.
//
// The previous test file covered only newCodexAuthManager and the
// pure decodeJWTClaims helper. This sweep adds Token + loadLocked +
// decodeTokensLocked across happy / corrupt / wrong-auth-mode /
// missing-tokens / missing-account-id branches.

package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildCodexJWT returns a valid (but unsigned) JWT whose payload
// carries the supplied claims. Codec auth never verifies the
// signature so a placeholder is fine. exp is set to expiresAt's Unix
// epoch.
func buildCodexJWT(t *testing.T, accountID string, expiresAt time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := map[string]any{
		"exp": expiresAt.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return header + "." + payload + ".sig"
}

// writeCodexAuth writes an auth.json to dir/auth.json with the given
// fields. Returns the file path.
func writeCodexAuth(t *testing.T, dir, mode string, tokens codexAuthToken) string {
	t.Helper()
	auth := codexAuth{AuthMode: mode, Tokens: tokens}
	out, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	return path
}

func TestCodexAuth_Token_HappyPath_FreshTokenSkipsRefresh(t *testing.T) {
	dir := t.TempDir()
	// Token expires well into the future so the refresh path is
	// skipped — that's what we want to assert here.
	jwt := buildCodexJWT(t, "acct-123", time.Now().Add(time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken:      jwt,
		AccessToken:  "access-abc",
		RefreshToken: "refresh-abc",
		AccountID:    "acct-fallback",
	})
	mgr := newCodexAuthManager(path)

	access, accountID, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if access != "access-abc" {
		t.Errorf("access token: got %q, want access-abc", access)
	}
	if accountID != "acct-123" {
		t.Errorf("accountID: got %q, want acct-123 (from JWT claim)", accountID)
	}
}

func TestCodexAuth_Token_FallbackAccountIDFromTokens(t *testing.T) {
	dir := t.TempDir()
	// JWT carries NO chatgpt_account_id claim — manager must fall
	// back to tokens.account_id.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := map[string]any{"exp": time.Now().Add(time.Hour).Unix()}
	body, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(body)
	jwt := header + "." + payload + ".sig"

	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken:      jwt,
		AccessToken:  "access-x",
		RefreshToken: "refresh-x",
		AccountID:    "acct-from-tokens",
	})
	mgr := newCodexAuthManager(path)
	_, accountID, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if accountID != "acct-from-tokens" {
		t.Errorf("expected fallback account id; got %q", accountID)
	}
}

func TestCodexAuth_Token_MissingFile(t *testing.T) {
	mgr := newCodexAuthManager("/no/such/path/auth.json")
	_, _, err := mgr.Token(context.Background())
	if err == nil {
		t.Error("missing auth.json: expected error, got nil")
	}
}

func TestCodexAuth_Token_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mgr := newCodexAuthManager(path)
	_, _, err := mgr.Token(context.Background())
	if err == nil {
		t.Error("corrupt JSON: expected error, got nil")
	}
}

func TestCodexAuth_Token_WrongAuthMode(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuth(t, dir, "ApiKey", codexAuthToken{
		IDToken: "id", AccessToken: "a", RefreshToken: "r", AccountID: "acct",
	})
	mgr := newCodexAuthManager(path)
	_, _, err := mgr.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth_mode") {
		t.Errorf("wrong auth_mode: expected auth_mode error, got %v", err)
	}
}

func TestCodexAuth_Token_MissingTokens(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		// All three required tokens missing.
	})
	mgr := newCodexAuthManager(path)
	_, _, err := mgr.Token(context.Background())
	if err == nil {
		t.Error("missing tokens: expected error, got nil")
	}
}

func TestCodexAuth_Token_NoAccountIDAnywhere(t *testing.T) {
	dir := t.TempDir()
	// JWT without account claim AND tokens.account_id empty.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := map[string]any{"exp": time.Now().Add(time.Hour).Unix()}
	body, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(body)
	jwt := header + "." + payload + ".sig"
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: jwt, AccessToken: "a", RefreshToken: "r",
	})
	mgr := newCodexAuthManager(path)
	_, _, err := mgr.Token(context.Background())
	if err == nil {
		t.Error("no account id anywhere: expected error, got nil")
	}
}

func TestCodexAuth_Token_RefreshOnExpiredHitsTokenURL(t *testing.T) {
	// Token is already expired so the manager goes into refreshLocked,
	// which POSTs to the OAuth endpoint. We swap the endpoint for an
	// httptest server that returns a fresh token set, so the refresh
	// completes without leaving the test process.
	dir := t.TempDir()
	expiredJWT := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expiredJWT, AccessToken: "old", RefreshToken: "old-refresh",
		AccountID: "acct-1",
	})

	// Server returns a rotated token set with a new JWT in the future.
	newJWT := buildCodexJWT(t, "acct-1", time.Now().Add(time.Hour))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the rotated tokens.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id_token":      newJWT,
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
		})
	}))
	defer ts.Close()

	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL // swap to test server

	access, accountID, err := mgr.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if access != "new-access" {
		t.Errorf("access: got %q, want new-access", access)
	}
	if accountID != "acct-1" {
		t.Errorf("accountID: got %q", accountID)
	}
}

func TestCodexAuth_Token_RefreshNon200Surfaces(t *testing.T) {
	dir := t.TempDir()
	expiredJWT := buildCodexJWT(t, "acct-1", time.Now().Add(-time.Hour))
	path := writeCodexAuth(t, dir, "chatgpt", codexAuthToken{
		IDToken: expiredJWT, AccessToken: "old", RefreshToken: "old-refresh",
		AccountID: "acct-1",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer ts.Close()

	mgr := newCodexAuthManager(path)
	mgr.tokenURL = ts.URL

	_, _, err := mgr.Token(context.Background())
	if err == nil {
		t.Error("oauth 401: expected error, got nil")
	}
}

func TestCodexAuth_decodeJWTClaims_BadStructure(t *testing.T) {
	if _, err := decodeJWTClaims("not.a.valid.token"); err == nil {
		t.Error("4-segment string: want error")
	}
	if _, err := decodeJWTClaims("only.two"); err == nil {
		t.Error("2-segment string: want error")
	}
	// Valid 3 segment but garbage in the payload — base64 decode fails.
	if _, err := decodeJWTClaims("h.@@@.s"); err == nil {
		t.Error("garbled payload: want error")
	}
	// Valid base64 but garbage JSON in the claims.
	bad := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	if _, err := decodeJWTClaims("h." + bad + ".s"); err == nil {
		t.Error("garbled claims: want error")
	}
}

// TestCodexAuth_readAllCapped_Truncates exercises the cap path that
// keeps error-body logging from buffering megabytes.
func TestCodexAuth_readAllCapped_Truncates(t *testing.T) {
	r := strings.NewReader(strings.Repeat("a", 5000))
	got, err := readAllCapped(r, 1024)
	if err != nil {
		t.Fatalf("readAllCapped: %v", err)
	}
	if len(got) != 1024 {
		t.Errorf("len = %d, want 1024", len(got))
	}
}

func TestCodexAuth_readAllCapped_SmallerThanLimit(t *testing.T) {
	r := strings.NewReader("hello")
	got, err := readAllCapped(r, 1024)
	if err != nil {
		t.Fatalf("readAllCapped: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want hello", string(got))
	}
}

// errReader feeds a Read error after one chunk so we can hit the
// non-EOF error branch of readAllCapped.
type errReader struct {
	served bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.served {
		return 0, errors.New("disk bad")
	}
	e.served = true
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestCodexAuth_readAllCapped_ReadError(t *testing.T) {
	got, err := readAllCapped(&errReader{}, 4096)
	if err != nil {
		t.Errorf("readAllCapped should not bubble Read error; got %v", err)
	}
	if len(got) == 0 {
		t.Error("readAllCapped should still return whatever bytes it got")
	}
}

// Sanity test: building a fake JWT works end-to-end so the other
// tests that rely on buildCodexJWT aren't silently failing.
func TestCodexAuth_BuildFakeJWT_RoundTrip(t *testing.T) {
	jwt := buildCodexJWT(t, "acct-x", time.Now().Add(time.Hour))
	claims, err := decodeJWTClaims(jwt)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := claims["exp"]; !ok {
		t.Error("decoded claims missing exp")
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil || auth["chatgpt_account_id"] != "acct-x" {
		t.Errorf("decoded claims missing chatgpt_account_id; got %+v", claims)
	}
}
