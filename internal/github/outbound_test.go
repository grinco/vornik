package github

import (
	"context"
	"crypto"
	"crypto/ed25519"
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
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/conversation"
)

// rsaTestKey returns a freshly generated 2048-bit RSA key for use
// in JWT / outbound tests. Generated per-test so leak windows
// across goroutines don't matter.
func rsaTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return key
}

// pemEncodePKCS1 returns the PEM-encoded PKCS#1 form of a key. The
// shape GitHub hands operators when they download an App key.
func pemEncodePKCS1(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// pemEncodePKCS8 returns the PEM-encoded PKCS#8 form. Some
// operators preprocess App keys into PKCS#8 for k8s Secret
// compatibility; LoadPrivateKeyPEM must accept both.
func pemEncodePKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})
}

// TestLoadPrivateKeyPEM_PKCS1 — the common case.
func TestLoadPrivateKeyPEM_PKCS1(t *testing.T) {
	original := rsaTestKey(t)
	got, err := LoadPrivateKeyPEM(pemEncodePKCS1(t, original))
	if err != nil {
		t.Fatalf("LoadPrivateKeyPEM: %v", err)
	}
	if got.N.Cmp(original.N) != 0 {
		t.Error("PKCS#1 round-trip: modulus mismatch")
	}
}

// TestLoadPrivateKeyPEM_PKCS8 — the operator-preprocessed case.
func TestLoadPrivateKeyPEM_PKCS8(t *testing.T) {
	original := rsaTestKey(t)
	got, err := LoadPrivateKeyPEM(pemEncodePKCS8(t, original))
	if err != nil {
		t.Fatalf("LoadPrivateKeyPEM: %v", err)
	}
	if got.N.Cmp(original.N) != 0 {
		t.Error("PKCS#8 round-trip: modulus mismatch")
	}
}

// TestLoadPrivateKeyPEM_NoBlock — junk input.
func TestLoadPrivateKeyPEM_NoBlock(t *testing.T) {
	_, err := LoadPrivateKeyPEM([]byte("not a pem"))
	if err == nil {
		t.Fatal("expected error on non-PEM input")
	}
}

// TestLoadPrivateKeyPEM_NonRSAKey — an EC key wrapped in PKCS#8 is
// a valid PEM block but not an RSA key. Must surface a clear
// operator-facing error rather than nil-deref later when signJWT
// fires.
func TestLoadPrivateKeyPEM_NonRSAKey(t *testing.T) {
	// Build a synthetic PKCS#8 PEM that decodes but isn't RSA. The
	// easiest way: take the PKCS#8 of an RSA key and corrupt the
	// algorithm identifier. Simpler: encode an unparseable block
	// labelled PRIVATE KEY.
	bad := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: []byte("not-a-valid-pkcs8-key"),
	})
	_, err := LoadPrivateKeyPEM(bad)
	if err == nil {
		t.Fatal("expected error parsing malformed PKCS#8")
	}
}

// TestLoadPrivateKeyPEM_PKCS8ButNotRSA — when PKCS#8 parses but
// the inner key is not RSA (e.g. an Ed25519 key), the function
// returns the "non-RSA key" error rather than silently passing
// an unsupported key to the signer.
func TestLoadPrivateKeyPEM_PKCS8ButNotRSA(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8: %v", err)
	}
	blob := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	_, err = LoadPrivateKeyPEM(blob)
	if err == nil || !strings.Contains(err.Error(), "non-RSA") {
		t.Errorf("err = %v, want non-RSA failure", err)
	}
}

// TestSignJWT_StructureRoundTrip — produces a valid RS256 JWT that
// validates against the public key, with the expected claims.
func TestSignJWT_StructureRoundTrip(t *testing.T) {
	key := rsaTestKey(t)
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	tok, err := signJWT(99, key, now)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d, want 3", len(parts))
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if string(headerBytes) != `{"alg":"RS256","typ":"JWT"}` {
		t.Errorf("header = %q, want RS256/JWT", headerBytes)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var p struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(payloadBytes, &p); err != nil {
		t.Fatalf("payload parse: %v", err)
	}
	if p.Iss != 99 {
		t.Errorf("iss = %d, want 99", p.Iss)
	}
	if p.Iat != now.Add(-jwtClockSkew).Unix() {
		t.Errorf("iat = %d, want now-60s", p.Iat)
	}
	if p.Exp != now.Add(jwtLifetime).Unix() {
		t.Errorf("exp = %d, want now+10min", p.Exp)
	}

	// Verify the signature against the public key.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	signingInput := parts[0] + "." + parts[1]
	hashed := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
		t.Errorf("signature verify failed: %v", err)
	}
}

// TestSignJWT_ZeroAppID — defensive: signing with AppID 0 fails
// before producing a meaningless token.
func TestSignJWT_ZeroAppID(t *testing.T) {
	_, err := signJWT(0, rsaTestKey(t), time.Now())
	if err == nil {
		t.Error("signJWT with AppID 0 returned nil error")
	}
}

// TestSignJWT_NilKey — defensive: nil key is a config bug, not a
// runtime nil-deref.
func TestSignJWT_NilKey(t *testing.T) {
	_, err := signJWT(99, nil, time.Now())
	if err == nil {
		t.Error("signJWT with nil key returned nil error")
	}
}

// TestParseGitHubSessionID_Cases — every success and failure
// branch in one table.
func TestParseGitHubSessionID_Cases(t *testing.T) {
	cases := []struct {
		in       string
		owner    string
		repo     string
		number   int
		wantErr  bool
		errMatch string
	}{
		{"acme/api#issues/42", "acme", "api", 42, false, ""},
		{"acme/api#pulls/7", "acme", "api", 7, false, ""},
		{"acme/api", "", "", 0, true, "missing '#'"},
		{"acme#issues/1", "", "", 0, true, "repo part"},
		{"/api#issues/1", "", "", 0, true, "empty owner or repo"},
		{"acme/#issues/1", "", "", 0, true, "empty owner or repo"},
		{"acme/api#issues", "", "", 0, true, "kind part"},
		{"acme/api#fork/1", "", "", 0, true, "unknown kind"},
		{"acme/api#issues/notanumber", "", "", 0, true, "not integer"},
		{"acme/api#issues/0", "", "", 0, true, "must be positive"},
		{"acme/api#issues/-5", "", "", 0, true, "must be positive"},
	}
	for _, c := range cases {
		o, r, n, err := parseGitHubSessionID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseGitHubSessionID(%q) returned nil error, want %q", c.in, c.errMatch)
				continue
			}
			if !strings.Contains(err.Error(), c.errMatch) {
				t.Errorf("parseGitHubSessionID(%q) err = %q, want match of %q", c.in, err.Error(), c.errMatch)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGitHubSessionID(%q) err = %v", c.in, err)
			continue
		}
		if o != c.owner || r != c.repo || n != c.number {
			t.Errorf("parseGitHubSessionID(%q) = (%q, %q, %d), want (%q, %q, %d)",
				c.in, o, r, n, c.owner, c.repo, c.number)
		}
	}
}

// githubAPIStub is an httptest server that imitates the two GitHub
// endpoints the channel calls: installation-token POST and
// issue-comment POST.
type githubAPIStub struct {
	server *httptest.Server

	mintToken       string
	mintExpires     time.Time
	mintStatus      int    // override the 201 default
	mintRawBody     string // override the JSON body
	mintCalls       atomic.Int64
	commentStatus   int
	commentRawBody  string
	commentID       int64
	commentCalls    atomic.Int64
	lastCommentBody string
}

func newGithubAPIStub() *githubAPIStub {
	s := &githubAPIStub{
		mintToken:   "ghs_testtoken123",
		mintExpires: time.Now().Add(1 * time.Hour),
		commentID:   555,
	}
	s.server = httptest.NewServer(s)
	return s
}

func (s *githubAPIStub) Close()      { s.server.Close() }
func (s *githubAPIStub) URL() string { return s.server.URL }

func (s *githubAPIStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		s.mintCalls.Add(1)
		if s.mintStatus != 0 {
			w.WriteHeader(s.mintStatus)
			if s.mintRawBody != "" {
				_, _ = io.WriteString(w, s.mintRawBody)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		payload, _ := json.Marshal(installationTokenResponse{
			Token:     s.mintToken,
			ExpiresAt: s.mintExpires,
		})
		_, _ = w.Write(payload)
	case strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
		s.commentCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		s.lastCommentBody = string(body)
		if s.commentStatus != 0 {
			w.WriteHeader(s.commentStatus)
			if s.commentRawBody != "" {
				_, _ = io.WriteString(w, s.commentRawBody)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id": %d}`, s.commentID)
	default:
		http.NotFound(w, r)
	}
}

// outboundChannel constructs a Channel configured to talk to the
// given API stub, with a real RSA key + AppID + InstallationID.
func outboundChannel(t *testing.T, stub *githubAPIStub) *Channel {
	t.Helper()
	cfg := validConfig()
	cfg.AppID = 12345
	cfg.PrivateKey = rsaTestKey(t)
	cfg.InstallationID = 99
	cfg.APIBaseURL = stub.URL()
	cfg.HTTPClient = stub.server.Client()
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ch
}

// TestSend_HappyPath_PostsCommentAndReturnsID — full round-trip:
// JWT minted, token cached, comment POSTed, ID returned.
func TestSend_HappyPath_PostsCommentAndReturnsID(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	id, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/42",
		Text:      "hello from vornik",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "555" {
		t.Errorf("Send returned id = %q, want 555", id)
	}
	if stub.mintCalls.Load() != 1 {
		t.Errorf("mint called %d times, want 1", stub.mintCalls.Load())
	}
	if stub.commentCalls.Load() != 1 {
		t.Errorf("comment POSTed %d times, want 1", stub.commentCalls.Load())
	}
	// Body must be a JSON {"body":"..."} envelope.
	if !strings.Contains(stub.lastCommentBody, "hello from vornik") {
		t.Errorf("comment body did not include text: %q", stub.lastCommentBody)
	}
}

// TestSend_PRSession_PostsToIssuesEndpoint — pulls/N SessionID
// resolves to the same `/issues/N/comments` REST URL on GitHub's
// API (GitHub uses the issues endpoint for both).
func TestSend_PRSession_PostsToIssuesEndpoint(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#pulls/9",
		Text:      "PR reply",
	})
	if err != nil {
		t.Fatalf("Send PR: %v", err)
	}
	if stub.commentCalls.Load() != 1 {
		t.Errorf("comment POSTed %d times, want 1", stub.commentCalls.Load())
	}
}

// TestSend_TokenCacheReused_AcrossSends — second Send within the
// token's TTL doesn't trigger a re-mint.
func TestSend_TokenCacheReused_AcrossSends(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	for i := 0; i < 3; i++ {
		if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
			SessionID: "acme/api#issues/42",
			Text:      "again",
		}); err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
	}
	if stub.mintCalls.Load() != 1 {
		t.Errorf("mint called %d times across 3 Sends, want 1 (cache miss)", stub.mintCalls.Load())
	}
}

// TestSend_TokenCacheRefreshes_OnExpiry — when the cached token's
// remaining TTL drops below the buffer, the next Send re-mints.
func TestSend_TokenCacheRefreshes_OnExpiry(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	// First Send mints a token.
	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/1",
		Text:      "first",
	}); err != nil {
		t.Fatalf("Send #1: %v", err)
	}

	// Force the cached token to look about to expire.
	inst := ch.installations[0]
	inst.tokenMu.Lock()
	inst.tokenExpires = time.Now().Add(1 * time.Minute) // below installationTokenTTLBuffer
	inst.tokenMu.Unlock()

	if _, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/2",
		Text:      "second",
	}); err != nil {
		t.Fatalf("Send #2: %v", err)
	}
	if stub.mintCalls.Load() != 2 {
		t.Errorf("mint called %d times, want 2 (expiry refresh)", stub.mintCalls.Load())
	}
}

// TestSend_MintFailure_PropagatesError — installation-token
// endpoint returning 401 surfaces an actionable error.
func TestSend_MintFailure_PropagatesError(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.mintStatus = http.StatusUnauthorized
	stub.mintRawBody = `{"message":"bad credentials"}`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/1",
		Text:      "boom",
	})
	if err == nil {
		t.Fatal("Send returned nil error after 401, want error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %q, want to include HTTP 401", err.Error())
	}
}

// TestSend_MintReturnsInvalidJSON_PropagatesParseError — when the
// upstream answers 201 but the body isn't JSON, the receiver
// learns about it.
func TestSend_MintReturnsInvalidJSON_PropagatesParseError(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.mintStatus = http.StatusCreated
	stub.mintRawBody = `not json at all`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{
		SessionID: "acme/api#issues/1",
		Text:      "boom",
	})
	if err == nil {
		t.Fatal("Send returned nil error on invalid mint JSON, want parse error")
	}
}

// TestSend_MintReturnsEmptyToken — defensive: an upstream that
// answers 201 with `{"token":""}` would otherwise authenticate the
// next call as Bearer "", which would 401 anyway. Detect it up
// front for a clearer error.
func TestSend_MintReturnsEmptyToken(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.mintStatus = http.StatusCreated
	stub.mintRawBody = `{"token":"","expires_at":"2030-01-01T00:00:00Z"}`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Errorf("err = %v, want missing-token failure", err)
	}
}

// TestSend_CommentFailure_PropagatesError — 422 on POST surfaces
// as a Send error.
func TestSend_CommentFailure_PropagatesError(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.commentStatus = http.StatusUnprocessableEntity
	stub.commentRawBody = `{"message":"validation failed"}`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Errorf("err = %v, want HTTP 422 failure", err)
	}
}

// TestSend_CommentReturnsInvalidJSON_PropagatesParseError —
// upstream answers 201 with junk.
func TestSend_CommentReturnsInvalidJSON_PropagatesParseError(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.commentStatus = http.StatusCreated
	stub.commentRawBody = `not json`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want parse failure", err)
	}
}

// TestSend_CommentReturnsZeroID_Defensive — an upstream that
// answers 201 with `{"id":0}` would let the dispatcher store a
// useless id; detect up front.
func TestSend_CommentReturnsZeroID_Defensive(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	stub.commentStatus = http.StatusCreated
	stub.commentRawBody = `{}`
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing id") {
		t.Errorf("err = %v, want missing-id failure", err)
	}
}

// TestSend_BadSessionID_NeverHitsUpstream — malformed SessionID
// short-circuits before any network call.
func TestSend_BadSessionID_NeverHitsUpstream(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "totally bogus", Text: "x"})
	if err == nil {
		t.Fatal("Send returned nil on bogus SessionID")
	}
	if stub.mintCalls.Load() != 0 || stub.commentCalls.Load() != 0 {
		t.Errorf("upstream hit on bogus SessionID: mint=%d comment=%d",
			stub.mintCalls.Load(), stub.commentCalls.Load())
	}
}

// TestSend_EmptyText_Rejected — sending an empty body would either
// fail upstream or post a blank comment; reject up front.
func TestSend_EmptyText_Rejected(t *testing.T) {
	stub := newGithubAPIStub()
	defer stub.Close()
	ch := outboundChannel(t, stub)

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: ""})
	if err == nil {
		t.Fatal("Send returned nil on empty text")
	}
}

// TestSend_NetworkFailure_PropagatesError — when the HTTP client
// can't reach the server (stub closed), Send surfaces the
// transport error.
func TestSend_NetworkFailure_PropagatesError(t *testing.T) {
	stub := newGithubAPIStub()
	ch := outboundChannel(t, stub)
	stub.Close() // close before the call so Dial fails

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil {
		t.Fatal("Send returned nil on closed server, want transport error")
	}
}

// TestSend_CommentNetworkFailure_PropagatesError — mint succeeds
// against the real stub; the second call fails because we close
// the server BETWEEN mint and comment.
func TestSend_CommentNetworkFailure_PropagatesError(t *testing.T) {
	stub := newGithubAPIStub()
	ch := outboundChannel(t, stub)

	// Mint a token first by triggering Send once successfully...
	// Actually that's hard to interleave. Easier: pre-seed the
	// token cache, then close the stub so only the comment POST
	// fails.
	inst := ch.installations[0]
	inst.tokenMu.Lock()
	inst.token = "cached-token"
	inst.tokenExpires = time.Now().Add(30 * time.Minute)
	inst.tokenMu.Unlock()
	stub.Close()

	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil {
		t.Fatal("Send returned nil after stub close, want transport error")
	}
}

// TestSend_UnconfiguredAppID_ReturnsSentinel — outbound disabled
// via missing AppID. Path through getInstallationToken hits the
// ErrOutboundNotConfigured branch.
func TestSend_UnconfiguredAppID_ReturnsSentinel(t *testing.T) {
	cfg := validConfig()
	cfg.AppID = 0 // validConfig pre-fills it for slice 4A+B; override here
	cfg.PrivateKey = rsaTestKey(t)
	cfg.InstallationID = 1
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("err = %v, want ErrOutboundNotConfigured", err)
	}
}

// TestSend_UnconfiguredInstallationID_ReturnsSentinel — same, but
// missing InstallationID.
func TestSend_UnconfiguredInstallationID_ReturnsSentinel(t *testing.T) {
	cfg := validConfig()
	cfg.AppID = 12345
	cfg.PrivateKey = rsaTestKey(t)
	// InstallationID intentionally zero.
	ch, _ := New(cfg)
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if !errors.Is(err, ErrOutboundNotConfigured) {
		t.Errorf("err = %v, want ErrOutboundNotConfigured", err)
	}
}

// TestTruncateErrorBody_LongInput — bodies over the cap get a
// trailing ellipsis; short bodies pass through unchanged.
func TestTruncateErrorBody_LongInput(t *testing.T) {
	short := "small body"
	if got := truncateErrorBody(short); got != short {
		t.Errorf("short = %q, want unchanged", got)
	}
	long := strings.Repeat("x", errorBodyExcerpt+200)
	got := truncateErrorBody(long)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("long body missing ellipsis: ...%q", got[len(got)-10:])
	}
	if len(got) <= errorBodyExcerpt {
		t.Errorf("long body len = %d, want > %d", len(got), errorBodyExcerpt)
	}
}

// TestNew_DefaultsAPIBaseURLAndHTTPClient — leaving APIBaseURL /
// HTTPClient unset wires in the package defaults. Important for
// production code paths that may not set them explicitly.
func TestNew_DefaultsAPIBaseURLAndHTTPClient(t *testing.T) {
	cfg := validConfig()
	ch, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ch.apiBaseURL != defaultAPIBaseURL {
		t.Errorf("apiBaseURL = %q, want %q", ch.apiBaseURL, defaultAPIBaseURL)
	}
	if ch.httpClient != http.DefaultClient {
		t.Errorf("httpClient = %p, want http.DefaultClient (%p)", ch.httpClient, http.DefaultClient)
	}
}

// TestGetInstallationToken_MalformedURL_NewRequestFails — covers
// the defensive http.NewRequestWithContext error branch by
// feeding a URL with a control character that url.Parse rejects.
func TestGetInstallationToken_MalformedURL_NewRequestFails(t *testing.T) {
	cfg := validConfig()
	cfg.AppID = 12345
	cfg.PrivateKey = rsaTestKey(t)
	cfg.InstallationID = 1
	cfg.APIBaseURL = "http://example.com\x7f" // DEL byte → url.Parse rejects
	ch, _ := New(cfg)
	_, err := ch.getInstallationToken(context.Background(), ch.installations[0])
	if err == nil {
		t.Fatal("getInstallationToken with malformed URL returned nil, want NewRequest error")
	}
}

// TestSendIssueComment_MalformedURL_NewRequestFails — same
// defensive path for the comment POST. Pre-seeds the token cache
// so the comment POST's NewRequest is the failing call, not the
// upstream mint.
func TestSendIssueComment_MalformedURL_NewRequestFails(t *testing.T) {
	cfg := validConfig()
	cfg.AppID = 12345
	cfg.PrivateKey = rsaTestKey(t)
	cfg.InstallationID = 1
	cfg.APIBaseURL = "http://example.com\x7f"
	ch, _ := New(cfg)
	inst := ch.installations[0]
	inst.tokenMu.Lock()
	inst.token = "preseeded"
	inst.tokenExpires = time.Now().Add(30 * time.Minute)
	inst.tokenMu.Unlock()
	_, err := ch.Send(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1", Text: "x"})
	if err == nil {
		t.Fatal("Send with malformed URL returned nil, want NewRequest error")
	}
}

// TestGetInstallationToken_BuildRequestPathIsSafe — defensive:
// the URL the channel hits matches the GitHub API shape exactly.
// Captures the URL the stub receives.
func TestGetInstallationToken_BuildRequestPathIsSafe(t *testing.T) {
	gotURLs := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURLs = append(gotURLs, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"x","expires_at":"2030-01-01T00:00:00Z"}`)
	}))
	defer server.Close()

	cfg := validConfig()
	cfg.AppID = 12345
	cfg.PrivateKey = rsaTestKey(t)
	cfg.InstallationID = 77
	cfg.APIBaseURL = server.URL
	cfg.HTTPClient = server.Client()
	ch, _ := New(cfg)

	if _, err := ch.getInstallationToken(context.Background(), ch.installations[0]); err != nil {
		t.Fatalf("getInstallationToken: %v", err)
	}
	want := "/app/installations/77/access_tokens"
	if len(gotURLs) != 1 || gotURLs[0] != want {
		t.Errorf("upstream paths = %v, want one of %q", gotURLs, want)
	}
}
