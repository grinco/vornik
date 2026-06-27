package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// claudeOAuthTokenURL is the token-exchange endpoint Claude Code posts
// to for both the initial code→token swap and subsequent refreshes.
// Confirmed via the leaked CLI source under
// src/services/oauth/client.ts.
const claudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"

// claudeOAuthClientID is the OAuth client id Claude Code identifies as.
// Baked into the CLI; the OAuth server keys its refresh-token rotation
// off this value, so we must send the exact same string.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// claudeOAuthScope is the space-joined scope string Claude Code
// requests. Sent on refresh so the rotated token has the same scopes
// the access token was issued with.
const claudeOAuthScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

// claudeCredsFile is the on-disk format of ~/.claude/.credentials.json.
// Only the fields we touch are typed out; unknown keys are preserved
// via the embedded raw-tail map so a future CLI revision that adds keys
// doesn't lose them on our writeback.
//
// expiresAt is milliseconds since epoch (JavaScript's Date.now()),
// matching what the CLI writes. That bit of weirdness is the easiest
// footgun here — Go's time.Unix() takes seconds, not millis.
type claudeCreds struct {
	Oauth claudeOAuth    `json:"claudeAiOauth"`
	Extra map[string]any `json:"-"` // preserved on writeback
}

type claudeOAuth struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // ms since epoch
	Scopes           []string `json:"scopes,omitempty"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    any      `json:"rateLimitTier,omitempty"`
}

// claudeAuthManager owns the in-memory view of the credentials file
// and serialises concurrent refresh attempts behind a single mutex.
// One instance is shared by every CLIClient derived from WithModel so
// the refresh-token rotation is visible across model clones.
type claudeAuthManager struct {
	path string

	mu          sync.Mutex
	cached      *claudeCreds
	http        *http.Client
	lastRefresh time.Time // guard against refresh storms under bursty load

	// tokenURL is the OAuth token-exchange endpoint; defaults to
	// claudeOAuthTokenURL. A field (mirroring codexAuthManager) so
	// tests can point refreshes at an httptest server.
	tokenURL string

	// metrics records refresh outcomes
	// (vornik_chat_subscription_token_refresh_total). Nil-safe via
	// Metrics.RecordSubscriptionTokenRefresh; set by the client's
	// SetMetrics. Guarded by mu like every other mutable field.
	metrics *Metrics
}

// newClaudeAuthManager constructs a manager. path may be empty — we
// fall back to $CLAUDE_CONFIG_DIR/.credentials.json (env override) or
// $HOME/.claude/.credentials.json (the CLI default on Linux / Windows).
// The file is read lazily on the first Token() call so a misconfigured
// path surfaces with a clear error rather than failing at construction.
func newClaudeAuthManager(path string) *claudeAuthManager {
	if path == "" {
		path = defaultClaudeCredentialsPath()
	}
	return &claudeAuthManager{
		path:     path,
		http:     &http.Client{Timeout: 30 * time.Second, Transport: sharedHTTPTransport()},
		tokenURL: claudeOAuthTokenURL,
	}
}

// setMetrics wires the refresh-outcome counter. Called from the
// client's SetMetrics; safe to call any time.
func (m *claudeAuthManager) setMetrics(mm *Metrics) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics = mm
}

// recordRefresh emits one refresh-outcome observation. Caller holds
// m.mu (metrics field access); the Metrics method itself is nil-safe.
func (m *claudeAuthManager) recordRefresh(outcome string) {
	m.metrics.RecordSubscriptionTokenRefresh("claude", outcome)
}

// defaultClaudeCredentialsPath resolves the path Claude Code writes to
// on this platform. Mirrors getClaudeConfigHomeDir() in the leaked CLI
// source (src/utils/envUtils.ts): CLAUDE_CONFIG_DIR env var wins, then
// $HOME/.claude.
//
// Note: on macOS the CLI stashes tokens in the Keychain rather than
// .credentials.json — we don't support that path here. Operators on
// Mac can export CLAUDE_CODE_OAUTH_TOKEN (see Token() below) or point
// this provider at an alternate file they manage themselves.
func defaultClaudeCredentialsPath() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".credentials.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude", ".credentials.json")
	}
	return ".credentials.json"
}

// Token returns a non-expired access token. If the cached token is
// within 60s of expiry it is refreshed synchronously; refresh failures
// propagate to the caller so a truly dead login doesn't silently hang
// on a 401 loop. Thread-safe.
//
// Coordinates with a concurrently-running `claude` CLI that may rotate
// the same refresh_token out from under us: we always reload the file
// from disk when the cached access token is near expiry, and if a
// refresh returns invalid_grant (the Claude OAuth server's response
// to a rotated refresh_token) we reload once more and retry. This
// lets vornik and the interactive CLI share `~/.claude/.credentials.json`
// without one of them stepping on the other.
//
// If CLAUDE_CODE_OAUTH_TOKEN is set, it bypasses the file entirely —
// useful for CI, container images that can't mount the user's ~/.claude,
// and macOS where the CLI writes to Keychain instead of disk. Tokens
// sourced this way have no refresh path (we don't know the refresh
// token), so the caller owns rotation.
func (m *claudeAuthManager) Token(ctx context.Context) (accessToken string, err error) {
	if env := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); env != "" {
		return env, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached == nil {
		if err := m.loadLocked(); err != nil {
			return "", fmt.Errorf("claude: load credentials: %w", err)
		}
	}

	if m.tokenExpiringLocked() {
		// Reload first — the interactive CLI may have refreshed while
		// we were sleeping. This is cheap (a ~500-byte JSON read) and
		// eliminates most invalid_grant races.
		if err := m.loadLocked(); err != nil {
			return "", fmt.Errorf("claude: reload credentials before refresh: %w", err)
		}
	}
	if m.tokenExpiringLocked() {
		// Serialize the refresh across PROCESSES, not just goroutines
		// (2026-06-07 review, subscription-auth finding 2 — the same
		// single-use-refresh-token race the codex hardening fixed): a
		// sibling vornik process sharing this credentials file must
		// not refresh concurrently. The interactive CLI does NOT take
		// this lock; that race stays covered by the invalid_grant
		// reload below.
		err := withAuthFileLock(m.path, func() error {
			// Under the lock, re-read: another lock-taking process
			// may have rotated the token while we waited. If the disk
			// copy is already fresh, adopt it and skip the network.
			if reloadErr := m.loadLocked(); reloadErr == nil && !m.tokenExpiringLocked() {
				return nil
			}
			return m.refreshLocked(ctx)
		})
		if err != nil {
			// On invalid_grant the CLI almost certainly won the race
			// between our load and its refresh. Reload one more time
			// and see if the on-disk copy is now valid.
			if isClaudeInvalidGrant(err) {
				if reloadErr := m.loadLocked(); reloadErr == nil && !m.tokenExpiringLocked() {
					m.recordRefresh("invalid_grant_recovered")
					return m.cached.Oauth.AccessToken, nil
				}
			}
			m.recordRefresh("failure")
			return "", fmt.Errorf("claude: refresh: %w", err)
		}
	}
	return m.cached.Oauth.AccessToken, nil
}

// tokenExpiringLocked reports whether the cached access token is
// within 60s of expiry (or has no known expiry at all). Caller must
// hold m.mu.
func (m *claudeAuthManager) tokenExpiringLocked() bool {
	expiry := time.UnixMilli(m.cached.Oauth.ExpiresAt)
	return expiry.IsZero() || time.Until(expiry) < 60*time.Second
}

// isClaudeInvalidGrant is true when an OAuth error indicates the
// refresh token has been rotated or revoked — the exact marker the
// Claude platform returns when a concurrent CLI session refreshed
// ahead of us.
func isClaudeInvalidGrant(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "Refresh token not found")
}

func (m *claudeAuthManager) loadLocked() error {
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}

	// Parse twice: once into our typed view, once into a generic map
	// that preserves unknown top-level keys (e.g. other auth kinds the
	// CLI may add later) for faithful writeback.
	var creds claudeCreds
	if err := json.Unmarshal(raw, &creds); err != nil {
		return fmt.Errorf("parse credentials.json: %w", err)
	}
	if creds.Oauth.AccessToken == "" || creds.Oauth.RefreshToken == "" {
		return fmt.Errorf("credentials.json is missing claudeAiOauth.accessToken / refreshToken " +
			"— run `claude login` on this host, or set CLAUDE_CODE_OAUTH_TOKEN")
	}

	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err == nil {
		delete(extra, "claudeAiOauth")
		creds.Extra = extra
	}
	m.cached = &creds
	return nil
}

// refreshLocked posts the refresh_token grant and persists the rotated
// pair back to disk. The Claude OAuth server rotates refresh tokens on
// every call — losing one means the user has to re-login via `claude`.
func (m *claudeAuthManager) refreshLocked(ctx context.Context) error {
	// Belt-and-braces against concurrent burst traffic racing each
	// other into back-to-back refreshes. 30s is long enough that a
	// request in-flight behind a newly-refreshed token will still see
	// a valid one when it reaches the header layer.
	if !m.lastRefresh.IsZero() && time.Since(m.lastRefresh) < 30*time.Second {
		return nil
	}

	payload := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": m.cached.Oauth.RefreshToken,
		"client_id":     claudeOAuthClientID,
		"scope":         claudeOAuthScope,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := readAllCapped(resp.Body, 4096)
		return fmt.Errorf("oauth/token returned %d: %s", resp.StatusCode, string(errBody))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"` // seconds
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("decode refresh response: %w", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return fmt.Errorf("refresh response missing access_token / refresh_token")
	}

	m.cached.Oauth.AccessToken = tr.AccessToken
	m.cached.Oauth.RefreshToken = tr.RefreshToken
	// expires_in is seconds from now; the on-disk format stores an
	// absolute millis-since-epoch expiry, so convert.
	m.cached.Oauth.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).UnixMilli()
	m.lastRefresh = time.Now()

	if err := m.saveLocked(); err != nil {
		return fmt.Errorf("persist refreshed tokens: %w", err)
	}
	m.recordRefresh("success")
	return nil
}

// saveLocked writes the current in-memory credentials back to disk,
// atomically via temp file + rename. File mode 0600 matches what the
// CLI writes. Unknown top-level keys captured in Extra are merged back
// so we don't strip anything the CLI might rely on in a future rev.
func (m *claudeAuthManager) saveLocked() error {
	out := make(map[string]any, 1+len(m.cached.Extra))
	for k, v := range m.cached.Extra {
		out[k] = v
	}
	out["claudeAiOauth"] = m.cached.Oauth

	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials.json.")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(buf); err != nil {
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
