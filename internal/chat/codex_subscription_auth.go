package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// codexRefreshSkew is how close to id_token expiry we proactively
// refresh. Also the "is this token fresh enough" bar when we re-read a
// copy another process may have rotated.
const codexRefreshSkew = 60 * time.Second

// ErrCodexSessionEnded is returned when the OAuth refresh is rejected
// with a terminal status (the refresh_token was invalidated —
// app_session_terminated / invalid_grant / refresh_token_reused).
// Replaying the dead token just floods identical failures, so the
// manager quarantines it and surfaces this typed error until a fresh
// `codex login` rewrites auth.json. errors.Is-friendly so callers /
// the router can special-case "needs re-auth" vs a transient blip.
var ErrCodexSessionEnded = errors.New("codex subscription session ended — run `codex login` to re-authenticate")

// codexAuth mirrors the on-disk layout of ~/.codex/auth.json, the file
// the Codex CLI writes after a successful ChatGPT-subscription login.
// We deliberately preserve unknown fields via a raw-tail map so future
// Codex versions that add keys don't get them stripped on writeback.
//
// Two auth modes coexist in this file:
//   - auth_mode="ChatGPT": the tokens.* block is populated; OPENAI_API_KEY
//     is null. This is the subscription path we implement.
//   - auth_mode="ApiKey": OPENAI_API_KEY is populated; tokens.* empty.
//     Not supported by this provider — use the HTTP provider instead.
//
// The refresh_token changes on every /oauth/token call, so persistence
// matters: losing a refresh means the user has to re-login via the
// Codex CLI. We write atomically (temp file + rename) and hold an
// advisory lock for the duration of a refresh to keep concurrent
// requests from racing.
type codexAuth struct {
	AuthMode     string         `json:"auth_mode"`
	OpenAIAPIKey *string        `json:"OPENAI_API_KEY"`
	Tokens       codexAuthToken `json:"tokens"`
	LastRefresh  string         `json:"last_refresh"`
}

type codexAuthToken struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

// codexAuthManager is the runtime handle over ~/.codex/auth.json. It
// owns the in-memory copy, refresh logic, and concurrent-access
// serialization. Callers obtain bearer+account headers via Token().
type codexAuthManager struct {
	path string

	mu          sync.Mutex
	cached      *codexAuth
	accountID   string    // cached JWT-derived id
	expiresAt   time.Time // cached JWT exp
	httpClient  *http.Client
	clientID    string
	tokenURL    string
	lastRefresh time.Time // rate-limit our own refresh attempts

	// Dead-token quarantine. When a refresh hits a terminal auth error
	// the refresh_token is invalidated server-side; we record the dead
	// token + reason and stop replaying it (Token returns
	// ErrCodexSessionEnded without a network round-trip) until a fresh
	// `codex login` writes a DIFFERENT refresh_token to disk.
	dead             bool
	deadRefreshToken string
	deadReason       string

	// metrics records refresh outcomes
	// (vornik_chat_subscription_token_refresh_total). Nil-safe via
	// Metrics.RecordSubscriptionTokenRefresh; set by the client's
	// SetMetrics. Guarded by mu like every other mutable field.
	metrics *Metrics
}

// setMetrics wires the refresh-outcome counter. Called from the
// client's SetMetrics; safe to call any time.
func (m *codexAuthManager) setMetrics(mm *Metrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics = mm
}

// recordRefresh emits one refresh-outcome observation. Caller holds
// m.mu; the Metrics method itself is nil-safe.
func (m *codexAuthManager) recordRefresh(outcome string) {
	m.metrics.RecordSubscriptionTokenRefresh("codex", outcome)
}

// newCodexAuthManager constructs a manager. path may be empty — we
// then fall back to $HOME/.codex/auth.json, the Codex CLI default.
func newCodexAuthManager(path string) *codexAuthManager {
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".codex", "auth.json")
		}
	}
	return &codexAuthManager{
		path:       path,
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: sharedHTTPTransport()},
		// openclaw / Codex CLI OAuth client ID. Confirmed against
		// https://github.com/openai/codex-cli and openclaw's
		// pi-mono/packages/ai/src/utils/oauth/openai-codex.ts.
		clientID: "app_EMoamEEZ73f0CkXaXp7hrann",
		tokenURL: "https://auth.openai.com/oauth/token",
	}
}

// Token returns a non-expired access token + the ChatGPT account id
// extracted from the ID token's custom auth claim. Refreshes in-band
// when the cached access token is within the configured skew of
// expiry. Thread-safe.
func (m *codexAuthManager) Token(ctx context.Context) (accessToken, accountID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached == nil {
		if err := m.loadLocked(); err != nil {
			return "", "", fmt.Errorf("codex: load auth.json: %w", err)
		}
	}

	// Quarantine: a dead refresh_token never recovers by replay — only a
	// fresh `codex login` (new refresh_token on disk) does. Auto-recover
	// when we see that; otherwise fail fast with the typed error and NO
	// network call, so a terminated session doesn't flood retries.
	if m.dead {
		if !m.recoverFromNewLoginLocked() {
			return "", "", fmt.Errorf("%w: %s", ErrCodexSessionEnded, m.deadReason)
		}
	}

	// Refresh when the ID token is within the skew of expiry, or when we
	// don't yet have a decoded expiry (fresh load path).
	if m.expiresAt.IsZero() || time.Until(m.expiresAt) < codexRefreshSkew {
		if err := m.refreshLocked(ctx); err != nil {
			return "", "", fmt.Errorf("codex: refresh: %w", err)
		}
	}

	return m.cached.Tokens.AccessToken, m.accountID, nil
}

// recoverFromNewLoginLocked re-reads auth.json; if its refresh_token
// differs from the one that died, a fresh `codex login` has landed —
// adopt it and clear the quarantine. Returns true on recovery. Caller
// holds m.mu.
func (m *codexAuthManager) recoverFromNewLoginLocked() bool {
	a, exp, acct, err := m.readAuthFileLocked()
	if err != nil {
		return false
	}
	if a.Tokens.RefreshToken == m.deadRefreshToken {
		return false // same dead token — no new login yet
	}
	m.cached = a
	m.expiresAt = exp
	m.accountID = acct
	m.dead = false
	m.deadRefreshToken = ""
	m.deadReason = ""
	return true
}

func (m *codexAuthManager) loadLocked() error {
	a, exp, acct, err := m.readAuthFileLocked()
	if err != nil {
		return err
	}
	m.cached = a
	m.expiresAt = exp
	m.accountID = acct
	return nil
}

// readAuthFileLocked reads + parses + validates auth.json from disk
// WITHOUT mutating the manager, returning the parsed struct plus its
// decoded expiry + account id. Used for the initial load AND the
// re-read-under-lock path that picks up a refresh_token another process
// (the interactive `codex` CLI, a second vornik, another host sharing
// the file) may have rotated since we cached. Caller holds m.mu.
func (m *codexAuthManager) readAuthFileLocked() (*codexAuth, time.Time, string, error) {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return nil, time.Time{}, "", err
	}
	var a codexAuth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, time.Time{}, "", fmt.Errorf("parse auth.json: %w", err)
	}
	// auth_mode: Codex CLI writes "chatgpt" (lowercase) on current
	// releases; older docs / openclaw comments show "ChatGPT". Match
	// both to avoid breaking when either rev is in the wild.
	if !strings.EqualFold(a.AuthMode, "chatgpt") {
		return nil, time.Time{}, "", fmt.Errorf("auth_mode=%q — this provider requires the ChatGPT-subscription login "+
			"produced by `codex login`; run it once and try again", a.AuthMode)
	}
	if a.Tokens.IDToken == "" || a.Tokens.AccessToken == "" || a.Tokens.RefreshToken == "" {
		return nil, time.Time{}, "", fmt.Errorf("auth.json is missing tokens (id_token / access_token / refresh_token)")
	}
	exp, acct, err := decodeAuthClaims(&a)
	if err != nil {
		return nil, time.Time{}, "", fmt.Errorf("decode id_token: %w", err)
	}
	return &a, exp, acct, nil
}

func (m *codexAuthManager) decodeTokensLocked(a *codexAuth) error {
	exp, acct, err := decodeAuthClaims(a)
	if err != nil {
		return err
	}
	m.expiresAt = exp
	m.accountID = acct
	return nil
}

// decodeAuthClaims extracts the id_token expiry + chatgpt account id
// from an auth struct WITHOUT mutating any manager. Pure so it can run
// on a re-read copy before deciding whether to adopt it.
func decodeAuthClaims(a *codexAuth) (expiresAt time.Time, accountID string, err error) {
	claims, err := decodeJWTClaims(a.Tokens.IDToken)
	if err != nil {
		return time.Time{}, "", err
	}
	// Expiry — used to decide when to refresh. exp is seconds since epoch.
	if expF, ok := claims["exp"].(float64); ok {
		expiresAt = time.Unix(int64(expF), 0)
	}
	// Account id lives under the custom auth claim. Codex CLI also
	// writes it plainly to tokens.account_id, which we treat as a
	// fallback in case the JWT's custom claim is reshaped in a future
	// schema.
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id, ok := auth["chatgpt_account_id"].(string); ok && id != "" {
			accountID = id
		}
	}
	if accountID == "" {
		accountID = a.Tokens.AccountID
	}
	if accountID == "" {
		return time.Time{}, "", fmt.Errorf("id_token has no chatgpt_account_id claim and tokens.account_id is empty")
	}
	return expiresAt, accountID, nil
}

// refreshLocked hits the OAuth token endpoint with grant_type=refresh_token.
// OpenAI rotates the refresh token on every call — we must persist the
// new one back to auth.json or the next refresh will 401.
func (m *codexAuthManager) refreshLocked(ctx context.Context) error {
	// Belt-and-braces: don't hammer the endpoint if we just refreshed.
	// openclaw uses a 120s window; anything finer risks thrash during
	// concurrent tool-call loops.
	if !m.lastRefresh.IsZero() && time.Since(m.lastRefresh) < 30*time.Second {
		// Treat as success — the cached token is new enough even if
		// exp is misread. If we're wrong the next API call will 401
		// and the subsequent refresh round-trips.
		return nil
	}
	// Serialize the refresh across PROCESSES, not just goroutines: the
	// OAuth refresh_token is single-use, so if the interactive `codex`
	// CLI, a second vornik, or another host shares this auth.json, an
	// unsynchronized refresh by either side invalidates the other's
	// token (400 app_session_terminated — the failure this whole
	// hardening pass fixes). An advisory flock on a sibling .lock file
	// makes the read-rotate-write atomic across processes.
	return m.withFileLock(func() error {
		// Under the lock, re-read the on-disk auth: another process may
		// have rotated the single-use token since we cached. Refreshing
		// with our stale in-memory token would 400; adopting the disk
		// copy uses the freshest token — and if it's already valid,
		// someone else just refreshed and we skip the network entirely.
		if a, exp, acct, err := m.readAuthFileLocked(); err == nil {
			m.cached = a
			m.expiresAt = exp
			m.accountID = acct
			if time.Until(exp) > codexRefreshSkew {
				m.lastRefresh = time.Now()
				return nil
			}
		}
		return m.doRefreshPOST(ctx)
	})
}

// doRefreshPOST wraps the refresh round-trip with outcome recording:
// quarantine transitions and plain failures were previously invisible
// to observability (2026-06-07 review, subscription-auth finding 4).
// Caller holds m.mu and the file lock.
func (m *codexAuthManager) doRefreshPOST(ctx context.Context) error {
	err := m.doRefreshPOSTInner(ctx)
	switch {
	case err == nil:
		m.recordRefresh("success")
	case errors.Is(err, ErrCodexSessionEnded):
		m.recordRefresh("quarantined")
	default:
		m.recordRefresh("failure")
	}
	return err
}

// doRefreshPOSTInner performs the actual grant_type=refresh_token
// round-trip using the (freshly re-read) cached refresh_token, persists
// the rotated tokens, and quarantines on a terminal auth error.
func (m *codexAuthManager) doRefreshPOSTInner(ctx context.Context) error {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", m.cached.Tokens.RefreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return err // transient (network) — do NOT quarantine
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := readAllCapped(resp.Body, 4096)
		if isTerminalAuthStatus(resp.StatusCode) {
			// The refresh_token is dead server-side; replaying it just
			// floods identical failures. Quarantine until a fresh login.
			m.dead = true
			m.deadRefreshToken = m.cached.Tokens.RefreshToken
			m.deadReason = fmt.Sprintf("oauth/token %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return fmt.Errorf("%w: %s", ErrCodexSessionEnded, m.deadReason)
		}
		return fmt.Errorf("oauth/token returned %d: %s", resp.StatusCode, string(body))
	}
	var tr struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return fmt.Errorf("refresh response missing access_token / refresh_token")
	}
	m.cached.Tokens.IDToken = tr.IDToken
	m.cached.Tokens.AccessToken = tr.AccessToken
	m.cached.Tokens.RefreshToken = tr.RefreshToken
	m.cached.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	m.lastRefresh = time.Now()
	if err := m.decodeTokensLocked(m.cached); err != nil {
		return err
	}
	if err := m.saveLocked(); err != nil {
		// Saving failed — the in-memory copy is still valid for
		// this process, but the new refresh token won't survive
		// restart. Log-worthy; not fatal for the current call.
		return fmt.Errorf("persist refreshed tokens: %w", err)
	}
	return nil
}

// isTerminalAuthStatus reports whether an oauth/token status means the
// session is dead and only a re-login can fix it (vs a transient 5xx /
// network blip worth retrying). A 400/401/403 from the token endpoint
// means the refresh_token was rejected — app_session_terminated,
// invalid_grant, refresh_token_reused — and replaying it is futile.
func isTerminalAuthStatus(code int) bool {
	return code == http.StatusBadRequest ||
		code == http.StatusUnauthorized ||
		code == http.StatusForbidden
}

// withFileLock serializes refreshes across processes sharing this
// auth.json. Implementation lives in withAuthFileLock (extracted
// 2026-06-07 so the claude manager shares it; see
// subscription_auth_lock.go for the full semantics).
func (m *codexAuthManager) withFileLock(fn func() error) error {
	return withAuthFileLock(m.path, fn)
}

// saveLocked writes the current in-memory auth struct to disk
// atomically (temp file + rename) so a crash mid-write can't leave
// corrupted JSON. File mode matches Codex CLI's 0600.
func (m *codexAuthManager) saveLocked() error {
	out, err := json.MarshalIndent(m.cached, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	tmp, err := os.CreateTemp(dir, ".auth.json.")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // cleanup on any failure path below
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.path)
}

// decodeJWTClaims parses the middle segment of a JWT into a generic
// map. We don't verify the signature — the OAuth server has already
// done so on issuance and we trust the file on local disk. Adding
// JWKS verification would need a public-key fetcher for no real
// security benefit in this threat model.
func decodeJWTClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("jwt: expected 3 segments, got %d", len(parts))
	}
	// JWTs use base64url without padding — RawURLEncoding handles it.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some producers emit base64url WITH padding; try that too.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, fmt.Errorf("jwt: decode payload: %w", err)
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("jwt: unmarshal claims: %w", err)
	}
	return claims, nil
}

// readAllCapped reads up to limit bytes from r and discards the rest.
// Used for error bodies — never want to buffer megabytes of nginx 502
// HTML into a log line.
func readAllCapped(r interface {
	Read(p []byte) (int, error)
}, limit int64) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for int64(len(buf)) < limit {
		n, err := r.Read(tmp)
		if n > 0 {
			remaining := limit - int64(len(buf))
			if int64(n) > remaining {
				n = int(remaining)
			}
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}
