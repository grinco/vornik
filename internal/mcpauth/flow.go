package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The OAuth 2.1 authorization-code flow with PKCE, plus RFC 7591 dynamic client registration.
//
// DELIBERATE DEVIATION from design §5, which says golang.org/x/oauth2 is used "for the
// token-exchange and refresh half only". Implemented with plain net/http instead, for three
// reasons worth stating because they are a judgement call rather than an oversight:
//
//  1. Both requests must carry the RFC 8707 `resource` indicator (F5), which x/oauth2 has no
//     first-class support for — it would arrive via an extra-params escape hatch anyway.
//  2. The refresh path is rotation-aware and persists through a conditional UPDATE under an
//     advisory lock (design §6). x/oauth2's TokenSource wants to own refresh scheduling, which
//     fights that design rather than helping it.
//  3. It is a form POST and a JSON parse. The design's own reasoning for writing discovery and
//     DCR by hand — "plain HTTP against documented JSON shapes; a dependency there would buy
//     nothing and hide F2/F3" — applies unchanged here, and it keeps the dependency surface
//     unchanged in a repo that audits its dependencies.
//
// If the operator prefers the dependency, swapping this file is a contained change.

var (
	// ErrNoDCR reports that the authorization server offers no dynamic client registration
	// and no client_id was configured (F1 — Slack, GitHub, Box).
	ErrNoDCR = errors.New("authorization server requires a pre-registered OAuth client")

	// ErrAuthorizationServer reports an error RESPONSE from the authorization server (an
	// OAuth error object), as opposed to a transport failure.
	ErrAuthorizationServer = errors.New("authorization server rejected the request")

	// ErrInvalidGrant is the specific OAuth error that means a refresh token is no longer
	// usable — the grant was revoked, or a rotated token was replayed. It is the signal to
	// stop retrying and ask a human to re-consent.
	ErrInvalidGrant = errors.New("authorization server rejected the grant (invalid_grant)")
)

// PKCE is a proof-key pair for one authorization attempt.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates an S256 proof key. Every surveyed vendor advertises S256 and none advertises
// `plain`, so there is no downgrade path to get wrong.
func NewPKCE() (PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return PKCE{}, fmt.Errorf("mcpauth: generate pkce verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// NewState generates an unguessable CSRF state value.
func NewState() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mcpauth: generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ClientCredentials identifies the OAuth client. Secret is empty for a public client.
type ClientCredentials struct {
	ID     string
	Secret string
}

// Confidential reports whether the token endpoint must be called with client authentication.
func (c ClientCredentials) Confidential() bool { return c.Secret != "" }

// Register performs RFC 7591 dynamic client registration and returns the issued credentials.
//
// The registration pins redirectURI, which is why a client is scoped to a (deployment, server)
// pair rather than to a project: projects on one daemon share a redirect URI, so a second
// registration would be pure duplication at the authorization server (design §7.2a).
func Register(ctx context.Context, client *http.Client, md Metadata, redirectURI string, scopes []string) (ClientCredentials, error) {
	if !md.SupportsDCR() {
		return ClientCredentials{}, fmt.Errorf("%w: configure auth.client_id (and auth.client_secret_from when the server issues a secret)", ErrNoDCR)
	}
	if client == nil {
		client = http.DefaultClient
	}

	authMethod := "none"
	if md.RequiresClientSecret() {
		authMethod = "client_secret_post"
	}
	body, err := json.Marshal(map[string]any{
		"client_name":                "Vornik",
		"client_uri":                 "https://vornik.io",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": authMethod,
		"scope":                      strings.Join(scopes, " "),
	})
	if err != nil {
		return ClientCredentials{}, fmt.Errorf("mcpauth: marshal registration: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return ClientCredentials{}, fmt.Errorf("mcpauth: build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setUserAgent(req)

	resp, err := client.Do(req)
	if err != nil {
		return ClientCredentials{}, fmt.Errorf("mcpauth: dynamic client registration: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return ClientCredentials{}, fmt.Errorf("mcpauth: read registration response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return ClientCredentials{}, fmt.Errorf("%w: registration returned %d: %s",
			ErrAuthorizationServer, resp.StatusCode, oauthErrorSummary(raw))
	}

	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ClientCredentials{}, fmt.Errorf("mcpauth: parse registration response: %w", err)
	}
	if out.ClientID == "" {
		return ClientCredentials{}, fmt.Errorf("%w: registration succeeded but returned no client_id", ErrAuthorizationServer)
	}
	return ClientCredentials{ID: out.ClientID, Secret: out.ClientSecret}, nil
}

// AuthorizationURL builds the URL the operator opens in a browser.
//
// `resource` rides here AND on the token request (F5): Trello and Jira share one authorization
// server, so without it a consent for one product would mint a token the other would reject —
// and one Atlassian consent does not cover both.
func AuthorizationURL(md Metadata, creds ClientCredentials, redirectURI string, scopes []string, state string, pkce PKCE) (string, error) {
	if md.AuthorizationEndpoint == "" {
		return "", errors.New("mcpauth: no authorization endpoint")
	}
	u, err := url.Parse(md.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("mcpauth: parse authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", creds.ID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", md.Resource)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// TokenResponse is the authorization server's token payload, normalised.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt is nil when the server issued no expires_in.
	ExpiresAt *time.Time
	// Scopes is the GRANTED set, which may be narrower than requested. Stored so a silently
	// reduced grant is visible rather than surfacing later as a puzzling 403.
	Scopes string
}

// ExchangeCode trades an authorization code for tokens.
func ExchangeCode(ctx context.Context, client *http.Client, md Metadata, creds ClientCredentials, redirectURI, code string, pkce PKCE) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {creds.ID},
		"code_verifier": {pkce.Verifier},
		"resource":      {md.Resource},
	}
	return postToken(ctx, client, md.TokenEndpoint, creds, form)
}

// Refresh exchanges a refresh token for a new access token.
//
// `resource` rides here too: an authorization server that issues per-resource tokens must be
// told which one is being refreshed, or the refreshed token can come back scoped to a different
// audience than the one it replaces.
func Refresh(ctx context.Context, client *http.Client, md Metadata, creds ClientCredentials, refreshToken string) (TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {creds.ID},
		"resource":      {md.Resource},
	}
	return postToken(ctx, client, md.TokenEndpoint, creds, form)
}

// postToken performs a token-endpoint request.
//
// Client authentication is client_secret_post rather than HTTP Basic: Slack's authorization
// server accepts only the former (F1), and every server that takes Basic also takes post.
func postToken(ctx context.Context, client *http.Client, endpoint string, creds ClientCredentials, form url.Values) (TokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if endpoint == "" {
		return TokenResponse{}, errors.New("mcpauth: no token endpoint")
	}
	if creds.Confidential() {
		form.Set("client_secret", creds.Secret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("mcpauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	setUserAgent(req)

	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("mcpauth: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("mcpauth: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// invalid_grant is called out separately because it is the one error
		// where retrying is guaranteed useless: the grant is gone and a human
		// must consent again. Everything else may be transient.
		if oauthErrorCode(raw) == "invalid_grant" {
			return TokenResponse{}, fmt.Errorf("%w: status %d", ErrInvalidGrant, resp.StatusCode)
		}
		return TokenResponse{}, fmt.Errorf("%w: token endpoint returned %d: %s",
			ErrAuthorizationServer, resp.StatusCode, oauthErrorSummary(raw))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return TokenResponse{}, fmt.Errorf("mcpauth: parse token response: %w", err)
	}
	if out.AccessToken == "" {
		return TokenResponse{}, fmt.Errorf("%w: token response carried no access_token", ErrAuthorizationServer)
	}
	tr := TokenResponse{
		AccessToken:  out.AccessToken,
		RefreshToken: out.RefreshToken,
		Scopes:       out.Scope,
	}
	if out.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).UTC()
		tr.ExpiresAt = &exp
	}
	return tr, nil
}

// oauthErrorCode extracts the RFC 6749 `error` field, or "" when the body is not an OAuth error.
func oauthErrorCode(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return ""
	}
	return e.Error
}

// oauthErrorSummary renders a SHORT, bounded description of an error response for a log line or
// an operator-facing message.
//
// Never the raw body: it comes from an upstream we do not control, it can be arbitrarily large,
// and on some servers it echoes request parameters — which on a token request would mean echoing
// the client secret or the refresh token into a log.
func oauthErrorSummary(raw []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Error == "" {
		return "(no OAuth error object in the response)"
	}
	out := e.Error
	if e.Description != "" {
		desc := e.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		out += ": " + desc
	}
	return out
}
