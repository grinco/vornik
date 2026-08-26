package mcpconnect

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// countingTokens wraps a repository and counts reads, so a test can assert
// that the hot path does NO I/O — the property that makes per-call credential
// binding affordable (design §3.1).
type countingTokens struct {
	persistence.MCPOAuthTokenRepository
	mu     sync.Mutex
	tok    *persistence.MCPOAuthToken
	gets   int64
	locks  int64
	onLock func()
}

func (c *countingTokens) Get(_ context.Context, _, _ string) (*persistence.MCPOAuthToken, error) {
	atomic.AddInt64(&c.gets, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok == nil {
		return nil, persistence.ErrNotFound
	}
	cp := *c.tok
	return &cp, nil
}

func (c *countingTokens) WithRefreshLock(ctx context.Context, _, _ string, fn func(context.Context) error) error {
	atomic.AddInt64(&c.locks, 1)
	if c.onLock != nil {
		c.onLock()
	}
	return fn(ctx)
}

func (c *countingTokens) MarkNeedsReconnect(context.Context, string, string) error { return nil }

func newCachedConnector(t *testing.T, tok *persistence.MCPOAuthToken) (*Connector, *countingTokens) {
	t.Helper()
	repo := &countingTokens{tok: tok}
	return &Connector{Tokens: repo, Logger: zerolog.Nop()}, repo
}

func futureToken(d time.Duration) *persistence.MCPOAuthToken {
	exp := time.Now().Add(d)
	return &persistence.MCPOAuthToken{
		ProjectID: "vornik-marketing", ServerName: "atlassian",
		AccessToken: "live-token", RefreshToken: "r1",
		Resource: "https://mcp.atlassian.com/v1/mcp/authv2", ExpiresAt: &exp,
	}
}

func atlassianRef() ServerRef {
	return ServerRef{
		ProjectID: "vornik-marketing", ServerName: "atlassian",
		URL: "https://mcp.atlassian.com/v1/mcp/authv2",
	}
}

// The steady state must be zero I/O. A credential is now resolved on EVERY
// tool call rather than once per reload, so a repository read per call would
// put a database round trip in front of every agent action.
func TestCachedAccessTokenDoesNoRepeatIO(t *testing.T) {
	c, repo := newCachedConnector(t, futureToken(time.Hour))
	ref := atlassianRef()

	for i := 0; i < 5; i++ {
		got, err := c.CachedAccessToken(context.Background(), ref)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "live-token" {
			t.Fatalf("call %d: got %q", i, got)
		}
	}
	if n := atomic.LoadInt64(&repo.gets); n != 1 {
		t.Fatalf("want exactly 1 repository read across 5 calls, got %d", n)
	}
	if n := atomic.LoadInt64(&repo.locks); n != 0 {
		t.Fatalf("the hot path must not take the refresh lock, took it %d times", n)
	}
}

// Review round 1, finding C, rule 1: an entry whose expiry is within
// refreshSkew is a cache MISS. Without this the cache could serve a token past
// the point the uncached path would have refreshed it — widening the very
// window this design closes.
func TestNearExpiryEntryIsACacheMiss(t *testing.T) {
	c, repo := newCachedConnector(t, futureToken(refreshSkew/2))
	ref := atlassianRef()

	// Refresh will fail (no metadata wired), but what this asserts is that the
	// cache did not answer: the call must reach the repository every time.
	_, _ = c.CachedAccessToken(context.Background(), ref)
	_, _ = c.CachedAccessToken(context.Background(), ref)

	if n := atomic.LoadInt64(&repo.gets); n < 2 {
		t.Fatalf("a near-expiry entry must not be served from cache; got %d reads", n)
	}
}

// Review round 1, finding C, rule 3 — and the precondition for the one-shot
// retry in §5: a 401 evicts, or the replay presents the credential that just
// failed and fails identically.
func TestInvalidateEvictsTheCachedToken(t *testing.T) {
	c, repo := newCachedConnector(t, futureToken(time.Hour))
	ref := atlassianRef()

	if _, err := c.CachedAccessToken(context.Background(), ref); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if n := atomic.LoadInt64(&repo.gets); n != 1 {
		t.Fatalf("warm read count %d", n)
	}

	c.InvalidateToken(ref.ProjectID, ref.ServerName)

	repo.mu.Lock()
	repo.tok.AccessToken = "rotated-token"
	repo.mu.Unlock()

	got, err := c.CachedAccessToken(context.Background(), ref)
	if err != nil {
		t.Fatalf("after invalidate: %v", err)
	}
	if got != "rotated-token" {
		t.Fatalf("eviction did not force a re-read: got %q", got)
	}
}

// A server the operator never connected is ("", nil) — not an error. It
// registers and calls unauthenticated so its tools stay visible (auth design
// §8), and that answer must not be cached as though it were a credential.
func TestUnconnectedServerIsNotAnError(t *testing.T) {
	c, _ := newCachedConnector(t, nil)
	got, err := c.CachedAccessToken(context.Background(), atlassianRef())
	if err != nil {
		t.Fatalf("an unconnected server must not error: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty token, got %q", got)
	}
}

// needs_reconnect must never be served from cache: it is the flag every
// operator surface reads, and a cached token that outlives it would keep the
// deployment silently degraded — exactly the P0.
func TestNeedsReconnectIsNeverCached(t *testing.T) {
	tok := futureToken(time.Hour)
	c, repo := newCachedConnector(t, tok)
	ref := atlassianRef()

	if _, err := c.CachedAccessToken(context.Background(), ref); err != nil {
		t.Fatalf("warm: %v", err)
	}

	repo.mu.Lock()
	repo.tok.NeedsReconnect = true
	repo.mu.Unlock()
	c.InvalidateToken(ref.ProjectID, ref.ServerName)

	_, err := c.CachedAccessToken(context.Background(), ref)
	if !errors.Is(err, ErrNeedsReconnect) {
		t.Fatalf("want ErrNeedsReconnect, got %v", err)
	}
}

// Review round 1, finding F: concurrent callers outside the skew must all be
// served from cache without any of them taking the refresh lock.
func TestConcurrentCachedReadsDoNotContend(t *testing.T) {
	c, repo := newCachedConnector(t, futureToken(time.Hour))
	ref := atlassianRef()

	if _, err := c.CachedAccessToken(context.Background(), ref); err != nil {
		t.Fatalf("warm: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.CachedAccessToken(context.Background(), ref); err != nil {
				t.Errorf("concurrent read: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt64(&repo.locks); n != 0 {
		t.Fatalf("32 concurrent cached reads must take no refresh lock, took %d", n)
	}
	if n := atomic.LoadInt64(&repo.gets); n != 1 {
		t.Fatalf("32 concurrent cached reads must do no extra I/O, did %d reads", n)
	}
}

// Grants are keyed per (project, server). A daemon-scope grant lives at "" and
// must not be handed to a project that owns its own credential, or vice versa.
func TestCacheIsKeyedPerProjectAndServer(t *testing.T) {
	c, _ := newCachedConnector(t, futureToken(time.Hour))
	ref := atlassianRef()

	if _, err := c.CachedAccessToken(context.Background(), ref); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// Evicting a DIFFERENT key must not disturb this one.
	c.InvalidateToken("", "atlassian")
	c.InvalidateToken("vornik-marketing", "google")

	if _, ok := c.tokenCache.Load(cacheKey("vornik-marketing", "atlassian")); !ok {
		t.Fatal("evicting a different key dropped this entry — the cache is not keyed correctly")
	}
}
