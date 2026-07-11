package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AuthTestURL is Slack's auth.test endpoint. Package var (same idiom
// as telegram.telegramAPIBaseURL / mcpProbeConnect) so tests point it at an
// httptest server without a live network call.
var AuthTestURL = "https://slack.com/api/auth.test"

// maxProbeResponseBytes caps how much of an auth.test response is read.
const maxProbeResponseBytes = 64 * 1024

// slackKnownInvalidAuthCodes are the auth.test `error` values that mean
// "this specific credential is bad" — as opposed to a transient condition
// like rate-limiting. Sourced from Slack's documented auth.test error
// codes. Anything not in this set (including codes Slack might add later)
// classifies conservatively as OutcomeError, not OutcomeFail — per the
// design's classification rule, "malformed"/unrecognized responses are
// reachability signal, not proof the credential is invalid.
var slackKnownInvalidAuthCodes = map[string]bool{
	"invalid_auth":     true,
	"not_authed":       true,
	"token_revoked":    true,
	"token_expired":    true,
	"account_inactive": true,
}

// AuthTestError is returned by ProbeToken when Slack responds with
// ok:false. Code is the machine-readable Slack error string (e.g.
// "invalid_auth", "ratelimited") the caller (internal/integrations)
// classifies against slackKnownInvalidAuthCodes.
type AuthTestError struct {
	Code string
}

func (e *AuthTestError) Error() string {
	return "slack: auth.test rejected the token: " + e.Code
}

// IsKnownInvalidAuth reports whether code is one of Slack's documented
// "this credential is bad" auth.test error codes, exported so
// internal/integrations doesn't have to duplicate Slack's error-code
// vocabulary.
func IsKnownInvalidAuth(code string) bool {
	return slackKnownInvalidAuthCodes[code]
}

type authTestResponse struct {
	OK    bool   `json:"ok"`
	Team  string `json:"team"`
	User  string `json:"user"`
	Error string `json:"error"`
}

// ProbeToken validates a candidate bot token against Slack's auth.test
// endpoint, WITHOUT joining any channel or persisting anything. Returns
// the authenticated team/user on success (the caller builds a secret-free
// "Connected to <team> as <user>" Summary from it).
//
// Slack returns HTTP 200 even for a rejected token (ok:false with an
// `error` code) — ProbeToken surfaces that as *AuthTestError so the caller
// can distinguish "invalid_auth" (OutcomeFail) from "ratelimited"
// (OutcomeError) without string-matching.
//
// httpClient is caller-supplied so the integrations probe layer can pass a
// DialGuard-wrapped client (design §6).
func ProbeToken(ctx context.Context, httpClient *http.Client, token string) (team, user string, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if strings.TrimSpace(token) == "" {
		return "", "", errors.New("slack: bot token is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AuthTestURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("slack: build auth.test request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := httpClient.Do(req)
	if err != nil {
		// The token travels in the Authorization header, never the URL, so
		// err (a *url.Error wrapping the request URL) carries no secret here.
		return "", "", fmt.Errorf("slack: auth.test request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes))

	if resp.StatusCode == http.StatusTooManyRequests {
		return "", "", fmt.Errorf("slack: auth.test rate limited (HTTP 429)")
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return "", "", fmt.Errorf("slack: auth.test upstream error (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("slack: auth.test unexpected HTTP status %d", resp.StatusCode)
	}

	var parsed authTestResponse
	if jsonErr := json.Unmarshal(body, &parsed); jsonErr != nil {
		return "", "", fmt.Errorf("slack: auth.test response parse: %w", jsonErr)
	}
	if !parsed.OK {
		return "", "", &AuthTestError{Code: parsed.Error}
	}
	return parsed.Team, parsed.User, nil
}
