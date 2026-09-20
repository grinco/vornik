// Package session mints, validates, and revokes browser login
// sessions (design §4.3). The raw 256-bit token lives only in the
// cookie; the store persists its sha256 (api_keys hygiene). The
// store owns clock checks (expiry, idle); revocation filtering is
// in the repository's SQL.
package authsession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/httpx/realip"
	"vornik.io/vornik/internal/persistence"
)

// ErrNoSession means the token resolves to no usable session
// (unknown, expired, idle-timed-out, or revoked). Callers treat
// all four identically: the cookie is dead.
var ErrNoSession = errors.New("session: no usable session")

// lastSeenTouchInterval coalesces last_seen_at writes. Pre-2026-06-15
// Validate fired one async TouchSession per validated request, so the
// ui_sessions write rate tracked request rate (a hot spot under load /
// HTMX poll loops). We now touch at most once per session per interval.
// When an idle timeout is configured the effective interval is clamped to
// idleTimeout/2 so the coalescing window can never let an otherwise-active
// session drift past the idle bound and be falsely timed out.
const lastSeenTouchInterval = 5 * time.Minute

// Store wraps the repository with token handling and TTL policy.
type Store struct {
	repo        persistence.UISessionRepository
	lifetime    time.Duration // fixed expiry from creation
	idleTimeout time.Duration // 0 = disabled
	now         func() time.Time
}

// Option customises a Store at construction.
type Option func(*Store)

// WithNow injects a clock source. Test seam — construction-time
// only so a live Store is never mutated (it is shared across
// request goroutines).
func WithNow(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// New constructs a Store. lifetime must be >0 (validated at config
// load); idleTimeout 0 disables the idle check.
func New(repo persistence.UISessionRepository, lifetime, idleTimeout time.Duration, opts ...Option) *Store {
	s := &Store{repo: repo, lifetime: lifetime, idleTimeout: idleTimeout, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// hashToken is the cookie→row mapping. Same construction as
// apikey.Hash (sha256 hex) without importing that package.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create mints a session for userID and returns the RAW token for
// the cookie. The raw token is unrecoverable after this call.
// Create mints a session and returns its raw token.
//
// originCredentialID names the credential that minted it, or "" for a login
// that had none — an OIDC sign-in. It is a REQUIRED PARAMETER rather than an
// optional second constructor, so every mint path has to answer the question
// "what authority is this session capped by" rather than inherit an answer by
// omission. The per-request capping rule reads it
// (ce-human-login-design §5), and a session attributed to nothing is a session
// no key revocation can ever end.
func (s *Store) Create(ctx context.Context, userID, provider, ip, userAgent, originCredentialID string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("session: entropy: %w", err)
	}
	raw := base64.RawURLEncoding.EncodeToString(b[:])
	now := s.now().UTC()
	err := s.repo.CreateSession(ctx, &persistence.UISession{
		ID:         persistence.GenerateID("sess"),
		TokenHash:  hashToken(raw),
		UserID:     userID,
		Provider:   provider,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.lifetime),
		IP:         ip,
		UserAgent:  userAgent,

		OriginCredentialID: originCredentialID,
	})
	if err != nil {
		return "", fmt.Errorf("session: create: %w", err)
	}
	return raw, nil
}

// Validate maps a raw cookie token to its active session. Fires an
// async last_seen touch on success (best-effort, mirrors the
// api_keys pattern: one goroutine per validated request, bounded
// only by request rate).
func (s *Store) Validate(ctx context.Context, raw string) (*persistence.UISession, error) {
	if raw == "" {
		return nil, ErrNoSession
	}
	sess, err := s.repo.GetActiveByTokenHash(ctx, hashToken(raw))
	if errors.Is(err, persistence.ErrSessionNotFound) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("session: lookup: %w", err)
	}
	now := s.now().UTC()
	if now.After(sess.ExpiresAt) {
		return nil, ErrNoSession
	}
	if s.idleTimeout > 0 && now.Sub(sess.LastSeenAt) > s.idleTimeout {
		return nil, ErrNoSession
	}
	// Coalesced last_seen touch: only when the row is older than the
	// touch interval, capping the write rate independent of request rate.
	// Capture the spoof-safe client IP from the REQUEST ctx here — the
	// async touch runs on context.Background(), so reading it inside the
	// goroutine would always see "". A non-empty IP refreshes the stored
	// value (kills the "stale IP" in the admin viewer, 2026-06-23); an
	// empty one (non-middleware path) leaves it untouched.
	if now.Sub(sess.LastSeenAt) >= s.touchInterval() {
		ip := realip.ClientIPFromContext(ctx)
		go func(id, ip string) { _ = s.repo.TouchSession(context.Background(), id, ip) }(sess.ID, ip)
	}
	return sess, nil
}

// touchInterval is the coalescing window for last_seen_at writes, clamped
// below idleTimeout/2 when an idle timeout is set so coalescing can never
// cause a false idle-out of an active session.
func (s *Store) touchInterval() time.Duration {
	iv := lastSeenTouchInterval
	if s.idleTimeout > 0 && s.idleTimeout/2 < iv {
		iv = s.idleTimeout / 2
	}
	return iv
}

// Revoke kills one session (logout). ErrSessionNotFound from the
// repo maps to ErrNoSession (already dead — the caller clears the
// cookie either way).
func (s *Store) Revoke(ctx context.Context, id string) error {
	err := s.repo.RevokeSession(ctx, id)
	if errors.Is(err, persistence.ErrSessionNotFound) {
		return ErrNoSession
	}
	return err
}
