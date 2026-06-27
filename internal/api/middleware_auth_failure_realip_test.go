package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/httpx/realip"
)

// These tests characterise the SHIPPED wiring of the brute-force auth
// lockout (authFailureLimiter) through the production middleware chain:
//
//	realip.Middleware  →  AuthMiddleware (with AuthFailures)
//
// The unit tests in auth_failure_limiter_test.go pin the limiter in
// isolation; tradingauth/middleware_per_ip_auth_test.go pin realip's
// effect on the per-IP RATE limiter. Neither proves that the lockout
// keys on the realip-resolved client IP rather than the connection IP.
// That is the spoof-safety contract AuthConfig.AuthFailures documents
// ("a forged header can't trip another client's lockout"), and it is
// what these two tests exercise end-to-end over httptest.

// buildAuthFailureChain wires the real outermost realip.Middleware in
// front of a real AuthMiddleware carrying a default-shaped
// authFailureLimiter (15 fails / 5 min → 15 min lockout) and a static
// key set, so a wrong bearer drives RecordFailure. rcfg controls which
// peer RemoteAddr is trusted to supply the CF-Connecting-IP header.
func buildAuthFailureChain(t *testing.T, rcfg realip.Config) http.Handler {
	t.Helper()
	limiter := newAuthFailureLimiter(15, 5*time.Minute, 15*time.Minute)
	auth := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"the-real-key": nil},
		AuthFailures:  limiter,
	})
	sink := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return realip.Middleware(rcfg, nil)(auth(sink))
}

// badAuthReq is a POST to a project-scoped (non-public) route carrying a
// WRONG bearer. realip resolves the client IP from CF-Connecting-IP when
// the peer is trusted; remoteAddr is the connection peer.
func badAuthReq(t *testing.T, remoteAddr, cfConnectingIP string) *http.Request {
	t.Helper()
	req := newAuthRequest(t, "/api/v1/projects/p/tasks")
	req.RemoteAddr = remoteAddr
	req.Header.Set("Authorization", "Bearer wrong-key")
	if cfConnectingIP != "" {
		req.Header.Set("CF-Connecting-IP", cfConnectingIP)
	}
	return req
}

// TestAuthMiddleware_RealIPDrivesBruteForceLockout — test #1.
//
// With realip trusting the httptest connection peer (127.0.0.0/8) and
// honouring CF-Connecting-IP, 15 bad-auth requests carrying
// CF-Connecting-IP: 203.0.113.41 lock THAT resolved IP out: the 16th is
// 429 TOO_MANY_AUTH_FAILURES. A request from the same connection but a
// DIFFERENT header IP (203.0.113.42), and one with NO header at all, are
// NOT locked — proving the lockout keys on the resolved header IP, not
// on the shared connection RemoteAddr.
func TestAuthMiddleware_RealIPDrivesBruteForceLockout(t *testing.T) {
	rcfg, err := realip.NewConfig(true, []string{"127.0.0.0/8"}, "CF-Connecting-IP")
	require.NoError(t, err)
	chain := buildAuthFailureChain(t, rcfg)

	hit := func(cfIP string) int {
		rec := httptest.NewRecorder()
		// Same trusted connection peer every time; only the resolved
		// header IP varies.
		chain.ServeHTTP(rec, badAuthReq(t, "127.0.0.1:5000", cfIP))
		return rec.Code
	}

	// 15 bad attempts for the victim IP — each is a 401 (wrong key), and
	// the 15th crosses the threshold but is still answered as the auth
	// failure (the lockout takes effect on the NEXT request).
	for i := 0; i < 15; i++ {
		code := hit("203.0.113.41")
		require.Equalf(t, http.StatusUnauthorized, code,
			"attempt %d for victim IP should be 401 (wrong key), not yet locked", i+1)
	}

	// 16th request for the SAME resolved IP → locked out (429).
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, badAuthReq(t, "127.0.0.1:5000", "203.0.113.41"))
	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"16th attempt for the victim resolved IP must be locked out")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"), "lockout 429 must carry Retry-After")
	assert.Contains(t, rec.Body.String(), "TOO_MANY_AUTH_FAILURES")

	// A DIFFERENT resolved header IP over the SAME connection is NOT
	// locked — it keys on its own bucket, so it gets the plain 401.
	assert.Equal(t, http.StatusUnauthorized, hit("203.0.113.42"),
		"a different resolved IP must not inherit the victim's lockout")

	// A request with NO forwarding header falls back to the connection
	// peer (127.0.0.1) as the key — a distinct bucket from 203.0.113.41,
	// so it too is only 401, not locked.
	rec3 := httptest.NewRecorder()
	chain.ServeHTTP(rec3, badAuthReq(t, "127.0.0.1:5000", ""))
	assert.Equal(t, http.StatusUnauthorized, rec3.Code,
		"no-header request keys on the connection IP, a separate bucket from the locked header IP")
}

// TestAuthMiddleware_UntrustedForgedHeaderIgnoredByLockout — test #2.
//
// The flip side: when the connection peer is NOT in trusted_proxies, the
// CF-Connecting-IP header is ignored and the lockout keys on the peer
// RemoteAddr. An attacker on a single untrusted RemoteAddr who rotates a
// forged CF-Connecting-IP on every request still collapses to ONE bucket
// (their real peer IP), so:
//   - their own 15 forged-header attempts lock THEM out (16th = 429),
//     proving the forged header did not spread the count across buckets;
//   - a forged header naming a victim IP cannot pre-lock that victim:
//     a fresh untrusted peer presenting CF-Connecting-IP: <victim> is
//     keyed on its OWN RemoteAddr, never the victim's.
func TestAuthMiddleware_UntrustedForgedHeaderIgnoredByLockout(t *testing.T) {
	// Trust ONLY the cloudflared host; the attacker peer below is not it,
	// so its CF-Connecting-IP is never honoured.
	rcfg, err := realip.NewConfig(true, []string{"10.0.0.5/32"}, "CF-Connecting-IP")
	require.NoError(t, err)
	chain := buildAuthFailureChain(t, rcfg)

	const attackerPeer = "198.51.100.9:5000"

	// 15 attempts from the untrusted attacker peer, each forging a
	// DIFFERENT CF-Connecting-IP. Because the header is ignored, all 15
	// land in the single bucket keyed on 198.51.100.9.
	forged := []string{
		"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4", "203.0.113.5",
		"203.0.113.6", "203.0.113.7", "203.0.113.8", "203.0.113.9", "203.0.113.10",
		"203.0.113.11", "203.0.113.12", "203.0.113.13", "203.0.113.14", "203.0.113.15",
	}
	for i, ip := range forged {
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, badAuthReq(t, attackerPeer, ip))
		require.Equalf(t, http.StatusUnauthorized, rec.Code,
			"attempt %d (forged %s) should be a plain 401 — rotating the header must not dodge the count", i+1, ip)
	}

	// 16th from the same peer (any forged header) → locked, proving all
	// the rotated headers were ignored and counted into one bucket.
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, badAuthReq(t, attackerPeer, "203.0.113.99"))
	require.Equal(t, http.StatusTooManyRequests, rec.Code,
		"the attacker's own peer must be locked after 15 attempts despite rotating the forged header")

	// A DIFFERENT untrusted peer forging CF-Connecting-IP: <a victim it
	// wants pre-locked> is keyed on ITS OWN RemoteAddr — the forged
	// victim header is ignored, so the victim is never locked and this
	// fresh peer just gets a 401.
	rec2 := httptest.NewRecorder()
	chain.ServeHTTP(rec2, badAuthReq(t, "198.51.100.77:6000", "203.0.113.50"))
	assert.Equal(t, http.StatusUnauthorized, rec2.Code,
		"an untrusted peer's forged victim header must not lock the victim out")
}
