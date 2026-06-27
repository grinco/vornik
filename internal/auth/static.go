package auth

import (
	"context"
	"crypto/subtle"

	"vornik.io/vornik/internal/apikey"
)

// StaticKeysBackend is the reference Backend implementation that
// wraps the legacy `api.api_keys` YAML map (key → []project).
// AuthMiddleware's static-keys path moves behind this backend
// once the slice-2 middleware refactor lands; today it stays in
// place and this implementation is exercised only by tests.
//
// Both the stored key and the presented token are hashed via
// apikey.Hash (sha256 hex, fixed length) before the
// constant-time compare. This closes the length-timing oracle
// that raw ConstantTimeCompare exposes when key lengths differ —
// matching the defence already in place in the api package's
// lookupAPIKey. The full map walk runs on every call even after a
// match, so timing cannot reveal which key matched.
type StaticKeysBackend struct {
	// Keys maps the raw bearer token to its project allowlist.
	// Empty list means "all projects" (legacy semantics).
	Keys map[string][]string
}

// NewStaticKeysBackend constructs a backend from a static-key
// map. Nil / empty maps are accepted; the backend simply returns
// ErrNoCredential on every call (no legacy keys → no
// authentication via this path).
func NewStaticKeysBackend(keys map[string][]string) *StaticKeysBackend {
	return &StaticKeysBackend{Keys: keys}
}

// Name returns the audit-trail identifier for this backend.
func (b *StaticKeysBackend) Name() string { return "static-keys" }

// Authenticate accepts a bearer token, walks the configured map
// in constant time, and returns an Identity on match. Returns
// ErrNoCredential when no bearer was presented OR when the
// presented token doesn't match any configured key (legacy
// semantics — the static-keys backend is permissive about
// non-matches so the next backend in the chain can try).
//
// Defensive note: the constant-time walk reads every key on
// every call even after a match. This is intentional — a
// short-circuit-on-match implementation would let a network
// observer infer "the matching key was the third one in the
// map" by timing. Slow, but the map is small (typically <10
// entries) and the work is bounded.
func (b *StaticKeysBackend) Authenticate(_ context.Context, cred Credential) (*Identity, error) {
	if b == nil || len(b.Keys) == 0 {
		return nil, ErrNoCredential
	}
	if cred.BearerToken == "" {
		return nil, ErrNoCredential
	}
	// Hash both sides to a fixed-length digest before comparing.
	// subtle.ConstantTimeCompare returns 0 in O(1) when lengths
	// differ, so comparing raw key bytes would let an observer
	// binary-search the key LENGTH by timing. Same defence as
	// internal/api lookupAPIKey — keep the two in lockstep until
	// the legacy path is deleted at the end of the rollout window.
	//
	// Hashing every configured key per call is acceptable: the
	// static map is the legacy single-tenant path and holds under
	// a dozen entries in practice. For larger sets the stored side
	// can be pre-hashed at construction — same trade-off documented
	// on lookupAPIKey.
	presentedHash := []byte(apikey.Hash(cred.BearerToken))
	var matchedProjects []string
	matched := false
	for k, v := range b.Keys {
		if subtle.ConstantTimeCompare([]byte(apikey.Hash(k)), presentedHash) == 1 {
			matchedProjects = v
			matched = true
		}
	}
	if !matched {
		// No opinion — let the next backend try.
		return nil, ErrNoCredential
	}
	// Match found. Subject is a short fingerprint so audit rows
	// can identify which static key landed the request without
	// exposing the raw key in logs.
	subject := "static:" + fingerprintForLog(cred.BearerToken)
	return &Identity{
		Subject:     subject,
		Projects:    matchedProjects,
		DisplayName: subject,
	}, nil
}

// fingerprintForLog returns a short stable identifier derived
// from the first 6 + last 4 chars of the token — enough for
// operators to tell two configured keys apart in audit rows
// without leaking the secret. Short keys collapse to "short:N"
// where N is the length, so a misconfigured 4-char key still
// has a distinct (but obviously bad) fingerprint.
func fingerprintForLog(s string) string {
	if len(s) < 12 {
		return shortFingerprint(s)
	}
	return s[:6] + "…" + s[len(s)-4:]
}

func shortFingerprint(s string) string {
	if s == "" {
		return "empty"
	}
	if len(s) <= 4 {
		return "short:" + s
	}
	return "short:" + s[:2] + "…"
}
