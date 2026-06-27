package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/httpx/realip"
)

// withCtxIP returns a request whose context carries ip, as the
// outermost realip.Middleware would have stored it.
func withCtxIP(method, remoteAddr, ip string) *http.Request {
	req := httptest.NewRequest(method, "/", nil)
	req.RemoteAddr = remoteAddr
	if ip != "" {
		req = req.WithContext(realip.WithClientIP(req.Context(), ip))
	}
	return req
}

// TestPerIPLimiter_AllowDrainsBucket — the wrapper preserves the
// inner APIKeyLimiter's burst contract: burst consecutive Allow
// calls pass; the burst+1th call blocks with a non-zero
// RetryAfter.
func TestPerIPLimiter_AllowDrainsBucket(t *testing.T) {
	l := NewPerIPLimiter()
	now := time.Now()
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		d, ip := l.Allow(req, 10, 5, now)
		assert.Equal(t, "203.0.113.5", ip)
		assert.False(t, d.Blocked, "burst[%d] should pass", i)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	d, _ := l.Allow(req, 10, 5, now)
	assert.True(t, d.Blocked, "post-burst call must block")
	assert.Greater(t, d.RetryAfter, time.Duration(0))
}

// TestPerIPLimiter_DisabledByZeroRPS — both rps == 0 and burst
// == 0 short-circuit to an un-blocked decision regardless of
// traffic shape. Mirrors APIKeyLimiter's contract so the daemon
// can swap out the field without changing call sites.
func TestPerIPLimiter_DisabledByZeroRPS(t *testing.T) {
	l := NewPerIPLimiter()
	now := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	for i := 0; i < 100; i++ {
		d, _ := l.Allow(req, 0, 10, now)
		assert.False(t, d.Blocked, "rps==0 must never block")
	}
	for i := 0; i < 100; i++ {
		d, _ := l.Allow(req, 10, 0, now)
		assert.False(t, d.Blocked, "burst==0 must never block")
	}
}

// TestPerIPLimiter_ClientIP_UsesContextValue — the limiter now
// keys on the IP the realip middleware resolved into the request
// context, NOT on any request header.
func TestPerIPLimiter_ClientIP_UsesContextValue(t *testing.T) {
	l := NewPerIPLimiter()
	req := withCtxIP(http.MethodGet, "10.0.0.5:54321", "203.0.113.7")
	assert.Equal(t, "203.0.113.7", l.ClientIP(req))
}

// TestPerIPLimiter_ClientIP_FallsBackToRemoteAddr — when no
// context value is present (non-middleware path / unit test) the
// limiter strips RemoteAddr's host.
func TestPerIPLimiter_ClientIP_FallsBackToRemoteAddr(t *testing.T) {
	l := NewPerIPLimiter()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.99:443"
	assert.Equal(t, "203.0.113.99", l.ClientIP(req))
}

// TestPerIPLimiter_ForgedHeaderDoesNotMoveKey is the consumer
// regression for the Cloudflare tunnel real-IP spoof: leftmost-XFF
// was attacker-controllable. A forged X-Forwarded-For / CF-Connecting-IP
// header MUST NOT change the per-IP rate-limit key — the limiter
// keys on the context value (or RemoteAddr), never the header.
// Pre-fix the limiter read leftmost-XFF, so a single attacker could
// trip another customer's lockout (customer-blocking) or rotate the
// header to evade the limit.
func TestPerIPLimiter_ForgedHeaderDoesNotMoveKey(t *testing.T) {
	l := NewPerIPLimiter()
	now := time.Now()
	// No middleware in front (untrusted path): the forged headers
	// are present but must be ignored — the key is RemoteAddr.
	drain := func() (string, bool) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "198.51.100.9:1"
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("CF-Connecting-IP", "203.0.113.8")
		d, ip := l.Allow(req, 5, 3, now)
		return ip, d.Blocked
	}
	var lastIP string
	blocked := false
	for i := 0; i < 4; i++ {
		ip, b := drain()
		lastIP = ip
		blocked = blocked || b
	}
	assert.Equal(t, "198.51.100.9", lastIP, "key must be RemoteAddr, not the forged header")
	assert.True(t, blocked, "forged headers must not let the attacker dodge the bucket")
}

// TestPerIPLimiter_NilSafe — nil receiver returns zero decisions
// and empty IPs rather than panicking. Production wires the
// limiter optionally; nil means "feature disabled".
func TestPerIPLimiter_NilSafe(t *testing.T) {
	var l *PerIPLimiter
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	d, ip := l.Allow(req, 10, 5, time.Now())
	assert.False(t, d.Blocked)
	assert.Empty(t, ip)
	assert.Empty(t, l.ClientIP(req))
	_, ok := l.SnapshotFor("any")
	assert.False(t, ok)
}

// TestPerIPLimiter_SnapshotFor_ReadOnly — Snapshot must not
// consume; two consecutive Snapshots return the same level.
func TestPerIPLimiter_SnapshotFor_ReadOnly(t *testing.T) {
	l := NewPerIPLimiter()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	l.Allow(req, 10, 5, time.Now())
	l.Allow(req, 10, 5, time.Now())

	s1, ok := l.SnapshotFor("203.0.113.5")
	require.True(t, ok)
	s2, ok := l.SnapshotFor("203.0.113.5")
	require.True(t, ok)
	assert.InDelta(t, s1.Tokens, s2.Tokens, 0.001)
}
