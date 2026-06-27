package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultAPIBaseURL is GitHub's public REST endpoint. Tests
// override this via Config.APIBaseURL to point at an httptest stub.
const defaultAPIBaseURL = "https://api.github.com"

// installationTokenTTLBuffer is the minimum remaining lifetime
// before getInstallationToken triggers a refresh. GitHub
// installation tokens last 1 hour; refreshing 5 minutes early
// gives a safe margin without thrashing the JWT exchange.
const installationTokenTTLBuffer = 5 * time.Minute

// jwtLifetime is how long the signed JWT remains valid. GitHub
// caps at 10 minutes; signing for the full window avoids
// unnecessary re-signing when getInstallationToken loops on a
// retry.
const jwtLifetime = 10 * time.Minute

// jwtClockSkew is subtracted from `iat` to absorb minor clock
// drift between vornik and GitHub's auth service. 60s mirrors
// GitHub's recommendation.
const jwtClockSkew = 60 * time.Second

// maxOutboundResponseBytes caps how much of a GitHub response we
// read. Comment-create responses are < 4 KiB in practice; the cap
// protects against a misbehaving upstream returning a giant body.
const maxOutboundResponseBytes = 64 * 1024

// errorBodyExcerpt limits how much of an error response body is
// echoed into the returned error message. Long error bodies in
// logs make grep painful.
const errorBodyExcerpt = 512

// ErrOutboundNotConfigured is returned by Send when the channel
// was constructed without complete GitHub App credentials
// (AppID + PrivateKey + InstallationID). The inbound webhook path
// remains fully usable in that mode; only outbound replies fail
// with this sentinel.
var ErrOutboundNotConfigured = errors.New("github-app channel: outbound credentials not configured (set AppID + PrivateKey + InstallationID)")

// LoadPrivateKeyPEM parses a PEM-encoded RSA private key. Accepts
// both PKCS#1 (`BEGIN RSA PRIVATE KEY`) and PKCS#8 (`BEGIN PRIVATE
// KEY`) — GitHub App keys download in PKCS#1, but some operators
// pre-process them into PKCS#8 for k8s Secret compatibility.
// Returned errors are operator-facing; the underlying x509 error
// is wrapped, not surfaced, to avoid leaking PEM internals.
func LoadPrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github-app: no PEM block found in private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github-app: parse private key: %w", err)
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github-app: PEM contains a non-RSA key")
	}
	return rsaKey, nil
}

// signJWT builds a GitHub App JWT (RS256). Inlines header +
// payload JSON to avoid an external JWT dependency — the surface
// is well-defined and tested against the round-trip Verify path.
func signJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	if appID == 0 {
		return "", errors.New("github-app: cannot sign JWT with zero AppID")
	}
	if key == nil {
		return "", errors.New("github-app: cannot sign JWT with nil PrivateKey")
	}
	header := `{"alg":"RS256","typ":"JWT"}`
	payload := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%d}`,
		now.Add(-jwtClockSkew).Unix(),
		now.Add(jwtLifetime).Unix(),
		appID,
	)
	headerB64 := base64.RawURLEncoding.EncodeToString([]byte(header))
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signingInput := headerB64 + "." + payloadB64
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("github-app: JWT sign: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// installationTokenResponse models the relevant subset of
// GitHub's POST /app/installations/{id}/access_tokens response.
// Other fields (permissions, repositories, etc.) are ignored —
// json.Unmarshal silently drops unknown keys.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// getInstallationToken returns a valid installation access token
// for the given installation, minting a new one via JWT exchange
// when the cached one is missing or near expiry. Concurrent-safe
// via the per-installation tokenMu.
//
// The mutex covers the full mint cycle so two concurrent Sends
// after expiry trigger exactly one JWT exchange — the second
// waits, then sees the freshly populated cache and short-
// circuits. Each installation has its own mutex so Sends across
// different installations don't serialise.
func (c *Channel) getInstallationToken(ctx context.Context, inst *installation) (string, error) {
	inst.tokenMu.Lock()
	defer inst.tokenMu.Unlock()

	if inst.token != "" && time.Until(inst.tokenExpires) > installationTokenTTLBuffer {
		return inst.token, nil
	}

	token, expires, err := MintInstallationToken(ctx, c.httpClient, c.apiBaseURL, inst.appID, inst.installationID, inst.privateKey)
	if err != nil {
		return "", err
	}
	inst.token = token
	inst.tokenExpires = expires
	return inst.token, nil
}

// MintInstallationToken exchanges a GitHub App JWT for an installation access
// token via POST /app/installations/{id}/access_tokens. It is standalone (no
// *Channel) so out-of-band callers — notably `vornikctl github-token`, which
// hands the agent a short-lived GH_TOKEN for git push / gh pr — can mint
// without constructing a channel or its token cache. Returns the token and its
// expiry. Returns ErrOutboundNotConfigured when any of appID / installationID /
// key is missing.
func MintInstallationToken(ctx context.Context, httpClient *http.Client, apiBaseURL string, appID, installationID int64, key *rsa.PrivateKey) (string, time.Time, error) {
	if appID == 0 || key == nil || installationID == 0 {
		return "", time.Time{}, ErrOutboundNotConfigured
	}
	jwt, err := signJWT(appID, key, time.Now())
	if err != nil {
		return "", time.Time{}, err
	}

	url := apiBaseURL + fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github-app: build installation-token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github-app: installation-token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("github-app: installation-token HTTP %d: %s",
			resp.StatusCode, truncateErrorBody(string(body)))
	}

	var tok installationTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", time.Time{}, fmt.Errorf("github-app: installation-token parse: %w", err)
	}
	if tok.Token == "" {
		return "", time.Time{}, errors.New("github-app: installation-token response missing token field")
	}
	return tok.Token, tok.ExpiresAt, nil
}

// installationPermissions models the `permissions` map GitHub returns alongside
// a minted installation token. Only the keys vornik cares about are listed;
// json.Unmarshal drops the rest.
type installationPermissions struct {
	Permissions struct {
		Contents     string `json:"contents"`
		PullRequests string `json:"pull_requests"`
	} `json:"permissions"`
}

// CheckContentsWrite mints an installation token and reports whether the
// installation grants `contents: write` — the permission forge.PushBranch needs
// to push a branch. Returns the granted contents level for an actionable log.
// Standalone (no *Channel) so the service container can call it at boot. Errors
// are surfaced so the caller can log; a false result with nil error means the
// App is installed but under-permissioned (likely issues:write only).
func CheckContentsWrite(ctx context.Context, httpClient *http.Client, apiBaseURL string, appID, installationID int64, key *rsa.PrivateKey) (ok bool, contents string, err error) {
	if appID == 0 || key == nil || installationID == 0 {
		return false, "", ErrOutboundNotConfigured
	}
	jwt, err := signJWT(appID, key, time.Now())
	if err != nil {
		return false, "", err
	}
	url := apiBaseURL + fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return false, "", fmt.Errorf("github-app: build permission-check request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("github-app: permission-check request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusCreated {
		return false, "", fmt.Errorf("github-app: permission-check HTTP %d: %s", resp.StatusCode, truncateErrorBody(string(body)))
	}
	var perms installationPermissions
	if err := json.Unmarshal(body, &perms); err != nil {
		return false, "", fmt.Errorf("github-app: permission-check parse: %w", err)
	}
	return perms.Permissions.Contents == "write", perms.Permissions.Contents, nil
}

// sendIssueComment posts an issue or PR comment via the GitHub
// REST API. GitHub uses the same endpoint
// (`POST /repos/.../issues/N/comments`) for both — the SessionID
// kind (`issues` vs `pulls`) only affects the operator-facing
// URL, not the API call.
//
// Returns the comment ID as a decimal string on success — the
// dispatcher's downstream wiring can stash it as `InReplyTo` for
// future threading.
//
// Installation resolution: the SessionID alone doesn't carry an
// installation_id, so the channel reads the per-session
// installation pin recorded by recordSession on the first inbound
// event for this issue/PR. Multi-installation deployments
// therefore route every Send to the same installation that
// originally produced the session — preventing cross-installation
// posts. Single-installation deployments fall through to the one
// configured route.
func (c *Channel) sendIssueComment(ctx context.Context, sessionID, text string) (string, error) {
	owner, repo, number, err := parseGitHubSessionID(sessionID)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New("github-app: cannot send empty comment")
	}
	inst, err := c.installationForSession(sessionID)
	if err != nil {
		return "", err
	}
	token, err := c.getInstallationToken(ctx, inst)
	if err != nil {
		return "", err
	}
	url := c.apiBaseURL + fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	body, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("github-app: build comment request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("github-app: comment request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutboundResponseBytes))
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("github-app: comment POST HTTP %d: %s",
			resp.StatusCode, truncateErrorBody(string(respBody)))
	}

	var commentResp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &commentResp); err != nil {
		return "", fmt.Errorf("github-app: comment response parse: %w", err)
	}
	if commentResp.ID == 0 {
		return "", errors.New("github-app: comment response missing id field")
	}
	return strconv.FormatInt(commentResp.ID, 10), nil
}

// installationForSession resolves which installation owns the
// given session. Reads the per-session pin recorded by
// recordSession on the first inbound event; falls back to the
// single configured route in single-installation mode.
//
// Returns ErrUnknownSession when multi-installation mode can't
// resolve the session — the dispatcher's reply lands on an
// unknown issue, which is a logic bug (the session must have been
// recorded by the inbound delivery that triggered the reply).
func (c *Channel) installationForSession(sessionID string) (*installation, error) {
	c.sessionsMu.Lock()
	entry, ok := c.sessions[sessionID]
	c.sessionsMu.Unlock()
	if ok && entry.installation != nil {
		return entry.installation, nil
	}
	// Single-installation back-compat: outbound Send to a session
	// the channel hasn't seen yet (e.g. a task creator replying
	// before any inbound delivery has landed) routes through the
	// only configured installation.
	if len(c.installations) == 1 {
		return c.installations[0], nil
	}
	return nil, fmt.Errorf("github-app: no installation pinned for session %q (multi-installation mode requires inbound delivery first)", sessionID)
}

// parseGitHubSessionID splits a session ID in the form
// `owner/repo#issues/N` or `owner/repo#pulls/N` into its
// components. Defensive on every separator so a malformed
// SessionID surfaces a routing error rather than constructing a
// nonsense URL.
func parseGitHubSessionID(s string) (owner, repo string, number int, err error) {
	hashIdx := strings.Index(s, "#")
	if hashIdx < 0 {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q missing '#'", s)
	}
	repoPart := s[:hashIdx]
	rest := s[hashIdx+1:]
	slashIdx := strings.Index(repoPart, "/")
	if slashIdx < 0 {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q repo part %q missing '/'", s, repoPart)
	}
	owner = repoPart[:slashIdx]
	repo = repoPart[slashIdx+1:]
	if owner == "" || repo == "" {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q has empty owner or repo", s)
	}
	typeSlash := strings.Index(rest, "/")
	if typeSlash < 0 {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q kind part %q missing '/'", s, rest)
	}
	kind := rest[:typeSlash]
	if kind != "issues" && kind != "pulls" {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q unknown kind %q (want issues|pulls)", s, kind)
	}
	number, err = strconv.Atoi(rest[typeSlash+1:])
	if err != nil {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q number not integer: %w", s, err)
	}
	if number <= 0 {
		return "", "", 0, fmt.Errorf("github-app: SessionID %q number must be positive", s)
	}
	return owner, repo, number, nil
}

// truncateErrorBody caps GitHub API response excerpts in error
// messages.
func truncateErrorBody(s string) string {
	if len(s) > errorBodyExcerpt {
		return s[:errorBodyExcerpt] + "..."
	}
	return s
}
