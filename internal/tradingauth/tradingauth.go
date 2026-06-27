// Package tradingauth implements the HMAC request-authentication
// scheme guarding the daemon's /api/v1/internal/trading-* endpoints.
//
// Backlog (https://docs.vornik.io, batch-2 Financials PRE-LIVE blockers):
// "HMAC/mTLS on /internal/trading-*". These endpoints carry the
// broker's order/fill/safety audit stream and serve the boot-time
// rate-limit state replay. They were previously guarded by the
// VORNIK_API_KEY bearer alone; a leaked key (or a same-host process
// on the daemon's network) could forge audit rows or read another
// project's turnover. This package adds a second, integrity-bound
// factor: an HMAC-SHA256 signature over method + path + timestamp +
// nonce + SHA-256(body), with a freshness window and an in-memory
// replay guard.
//
// Why HMAC and not mTLS: the codebase has no mTLS plumbing (no
// ClientCAs / tls.Config.VerifyConnection on any server listener),
// and the broker already authenticates to the daemon with a shared
// bearer over plain HTTP to host.containers.internal. A shared-secret
// HMAC reuses the existing secret-distribution channel (an env var /
// secrets file, same as the bearer) and the codebase's existing
// crypto idiom (crypto/hmac + hmac.Equal, as in verifyWebhookSignature)
// with zero new infrastructure. The webhook verifier signs body-only
// with no nonce; trading is a financial write surface, so this scheme
// additionally binds the method+path (stops cross-endpoint replay) and
// adds a nonce cache (stops same-window replay).
package tradingauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// HeaderSignature carries the hex-encoded HMAC-SHA256 signature.
	HeaderSignature = "X-Vornik-Trading-Signature"
	// HeaderTimestamp carries the Unix-seconds signing time.
	HeaderTimestamp = "X-Vornik-Trading-Timestamp"
	// HeaderNonce carries a per-request random nonce (replay guard).
	HeaderNonce = "X-Vornik-Trading-Nonce"

	// DefaultClockSkew is the default freshness window: a request
	// whose timestamp is more than this far from the verifier's
	// clock (in either direction) is rejected as stale or skewed.
	DefaultClockSkew = 5 * time.Minute
)

// canonicalString builds the byte string fed to the MAC. The body is
// hashed (not inlined) so the signed string stays bounded and we never
// hold two copies of a large payload. Fields are newline-joined; none
// of the inputs can contain a newline (method/path are HTTP tokens,
// timestamp is digits, nonce is hex) so the join is unambiguous.
func canonicalString(method, path, timestamp, nonce string, body []byte) []byte {
	bodyHash := sha256.Sum256(body)
	var b strings.Builder
	b.Grow(len(method) + len(path) + len(timestamp) + len(nonce) + 2*sha256.Size + 4)
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(path)
	b.WriteByte('\n')
	b.WriteString(timestamp)
	b.WriteByte('\n')
	b.WriteString(nonce)
	b.WriteByte('\n')
	b.WriteString(hex.EncodeToString(bodyHash[:]))
	return []byte(b.String())
}

func computeMAC(secret string, canonical []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(canonical)
	return mac.Sum(nil)
}

// Signer stamps the trading-auth headers on outbound requests. A
// Signer with an empty secret is an inert no-op so the in-daemon
// client can be constructed unconditionally and stay silent when the
// feature is disabled (backward-compatible with an unsigned rollout).
type Signer struct {
	secret string
}

// NewSigner returns a Signer keyed on secret. Empty secret → no-op.
func NewSigner(secret string) *Signer { return &Signer{secret: secret} }

// Enabled reports whether this signer will stamp headers.
func (s *Signer) Enabled() bool { return s != nil && s.secret != "" }

// Sign stamps timestamp, nonce, and signature headers on req. body
// MUST be the exact bytes that will be transmitted (the verifier
// recomputes the body hash from the bytes it reads off the wire). A
// no-op when the signer has no secret.
func (s *Signer) Sign(req *http.Request, body []byte, now time.Time) error {
	if !s.Enabled() {
		return nil
	}
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	// Sign the URL path, matching what the verifier reads from
	// r.URL.Path on the server side.
	mac := computeMAC(s.secret, canonicalString(req.Method, req.URL.Path, ts, nonce, body))
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, hex.EncodeToString(mac))
	return nil
}

func newNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// Verifier validates trading-auth headers fail-closed: a missing,
// malformed, forged, stale, or replayed signature returns a non-nil
// error and the caller MUST reject the request. It is safe for
// concurrent use.
type Verifier struct {
	secret string
	skew   time.Duration

	mu     sync.Mutex
	nonces map[string]time.Time // nonce → signing time, pruned past skew
}

// NewVerifier returns a Verifier keyed on secret with the given
// freshness window. A non-positive skew falls back to DefaultClockSkew.
func NewVerifier(secret string, skew time.Duration) *Verifier {
	if skew <= 0 {
		skew = DefaultClockSkew
	}
	return &Verifier{
		secret: secret,
		skew:   skew,
		nonces: make(map[string]time.Time),
	}
}

// VerifyError is returned for every rejection. Reason is a stable,
// non-secret token safe to log and surface in an audit metric; the
// secret and the computed/presented MACs are never included.
type VerifyError struct{ Reason string }

func (e *VerifyError) Error() string { return "trading auth: " + e.Reason }

// Verify checks the signature on req against body. now is the
// verifier's clock (injectable for tests). Returns nil iff the
// signature is present, well-formed, fresh, unreplayed, and valid.
func (v *Verifier) Verify(req *http.Request, body []byte, now time.Time) error {
	sig := strings.TrimSpace(req.Header.Get(HeaderSignature))
	tsRaw := strings.TrimSpace(req.Header.Get(HeaderTimestamp))
	nonce := strings.TrimSpace(req.Header.Get(HeaderNonce))
	if sig == "" || tsRaw == "" || nonce == "" {
		return &VerifyError{Reason: "missing signature headers"}
	}

	tsSec, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return &VerifyError{Reason: "malformed timestamp"}
	}
	signedAt := time.Unix(tsSec, 0)
	delta := now.Sub(signedAt)
	if delta < 0 {
		delta = -delta
	}
	if delta > v.skew {
		return &VerifyError{Reason: "timestamp outside freshness window"}
	}

	presented, err := hex.DecodeString(sig)
	if err != nil {
		return &VerifyError{Reason: "malformed signature"}
	}
	want := computeMAC(v.secret, canonicalString(req.Method, req.URL.Path, tsRaw, nonce, body))
	// Constant-time compare. hmac.Equal also guards against a
	// length-leak oracle.
	if !hmac.Equal(presented, want) {
		return &VerifyError{Reason: "signature mismatch"}
	}

	// Replay guard: a (valid) nonce may be used at most once within
	// the freshness window. Past the window the timestamp check
	// already rejects, so pruning stale nonces here is safe.
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pruneLocked(now)
	if _, seen := v.nonces[nonce]; seen {
		return &VerifyError{Reason: "replayed nonce"}
	}
	v.nonces[nonce] = signedAt
	return nil
}

// pruneLocked drops nonces whose signing time is older than the skew
// window — they can no longer pass the freshness check, so retaining
// them buys nothing and the map would grow unbounded.
func (v *Verifier) pruneLocked(now time.Time) {
	cutoff := now.Add(-v.skew)
	for n, t := range v.nonces {
		if t.Before(cutoff) {
			delete(v.nonces, n)
		}
	}
}

// nonceCount returns the number of cached nonces (test introspection).
func (v *Verifier) nonceCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.nonces)
}
