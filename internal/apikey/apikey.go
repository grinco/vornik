// Package apikey generates, parses, and hashes per-project bearer
// tokens used by AuthMiddleware. Replaces the static YAML
// `api.api_keys` map for new deployments while leaving that path
// intact for legacy single-tenant installs.
//
// Key format:
//
//	sk-vornik-<projectTag>.<32 url-safe random chars>
//
// projectTag is a NON-REVERSIBLE short digest of the project ID
// (ShortProjectTag = first 12 hex of sha256(projectID)). It still
// serves the original debuggability purpose — operators can correlate
// a key to its project by computing the tag — but, unlike the legacy
// raw-projectID prefix, it does NOT leak the project name into curl
// traces / proxy logs (security LLD review batch 3: "projectID
// recoverable from the key → enumeration"). It is cross-checked against
// the DB row on every auth via MatchesProject, which also accepts the
// legacy raw-projectID prefix so keys minted before this change keep
// working. The random tail carries ~190 bits of entropy from
// crypto/rand, far more than needed to defeat brute force; we use
// sha256 (not bcrypt) for storage because the entropy is already high
// enough that a memory-hard KDF buys nothing.
//
// Two-property invariants enforced by the tests:
//
//  1. Generate → Parse round-trips: the produced key parses back
//     to the same (projectID, random) pair.
//  2. Hash is deterministic + collision-free for distinct inputs:
//     same key → same hash; different keys → different hashes.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	// Prefix marks every vornik-issued bearer token. Lets operators
	// grep for "is this key vornik-shape?" without parsing.
	Prefix = "sk-vornik"

	// separator splits the human-readable project ID from the
	// random tail. base64.RawURLEncoding never emits '.', and
	// project path IDs accepted by the API are alnum / '-' / '_',
	// so this keeps parsing unambiguous while supporting the
	// repo's existing hyphenated project IDs.
	separator = "."

	// randomBytes feeds the random tail. 24 raw bytes encode to 32
	// url-safe characters (base64.RawURLEncoding, no padding).
	randomBytes = 24

	// PrefixDisplayLen is how many chars the UI / DB row stores as
	// `key_prefix` for human recognition. 12 covers
	// "sk-vornik-Ab" — enough to differentiate a few keys in a UI
	// table without exposing the secret.
	PrefixDisplayLen = 12
)

// Errors surfaced when a presented key doesn't have the expected
// shape. AuthMiddleware maps any of these to a single
// `UNAUTHORIZED` response so the caller can't enumerate which
// shape mismatch fired.
var (
	ErrMalformed    = errors.New("apikey: malformed")
	ErrWrongPrefix  = errors.New("apikey: missing sk-vornik prefix")
	ErrEmptyProject = errors.New("apikey: empty project segment")
	ErrEmptyRandom  = errors.New("apikey: empty random segment")
)

// Generate returns a freshly-minted key for the supplied project.
// The caller should display the returned secret exactly once and
// store only Hash(secret) in the database — the raw key MUST
// NEVER be persisted. The project is embedded as a non-reversible
// ShortProjectTag (not the raw ID) so the key doesn't leak the
// project name.
func Generate(projectID string) (string, error) {
	if projectID == "" {
		return "", ErrEmptyProject
	}
	raw := make([]byte, randomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("apikey: read crypto/rand: %w", err)
	}
	random := base64.RawURLEncoding.EncodeToString(raw)
	return fmt.Sprintf("%s-%s%s%s", Prefix, ShortProjectTag(projectID), separator, random), nil
}

// ShortProjectTag is the non-reversible project identifier embedded in
// freshly-minted keys: the first 12 hex chars of sha256(projectID). It
// is stable per project (so operators can correlate keys to a project by
// recomputing it) but does not reveal the project name.
//
// Collision domain: 12 hex = 48 bits, so a birthday collision needs
// ~2^24 (~16M) distinct projects for a 50% chance — far beyond any
// single-tenant or small-fleet deployment. A collision is NOT an
// auth-bypass anyway: the authoritative binding is the DB row found by
// the key's sha256 hash (LookupActiveByHash), and that row's ProjectID
// is what scopes the request; the tag is only a defense-in-depth cross
// -check via MatchesProject. A large multi-tenant operator that wants
// belt-and-braces can add a project-creation uniqueness check on the
// tag. (Review 2026-06-15.)
func ShortProjectTag(projectID string) string {
	sum := sha256.Sum256([]byte(projectID))
	return hex.EncodeToString(sum[:])[:12]
}

// MatchesProject reports whether the project segment parsed from a key
// belongs to projectID. It accepts BOTH the new non-reversible tag
// (ShortProjectTag) and the legacy raw-projectID prefix, so keys minted
// before the tag change still pass the defense-in-depth cross-check.
func MatchesProject(claimedSegment, projectID string) bool {
	if claimedSegment == "" {
		return false
	}
	return claimedSegment == projectID || claimedSegment == ShortProjectTag(projectID)
}

// Parse splits a presented key into its (projectID, random)
// components and validates the prefix. Returns ErrMalformed for
// any shape divergence — the caller maps every error to a
// single UNAUTHORIZED response.
func Parse(key string) (projectID, random string, err error) {
	if key == "" {
		return "", "", ErrMalformed
	}
	// Expected shape: sk-vornik-<project>.<random>. The first two
	// hyphens are part of the prefix; '.' separates project from
	// random. base64url random tails can contain '-' and '_' but
	// not '.', so hyphenated project IDs remain round-trippable.
	if !strings.HasPrefix(key, Prefix+"-") {
		return "", "", ErrWrongPrefix
	}
	rest := strings.TrimPrefix(key, Prefix+"-")
	sep := strings.LastIndex(rest, separator)
	if sep < 0 {
		return "", "", ErrMalformed
	}
	projectID = rest[:sep]
	random = rest[sep+1:]
	if projectID == "" {
		return "", "", ErrEmptyProject
	}
	if random == "" {
		return "", "", ErrEmptyRandom
	}
	return projectID, random, nil
}

// Hash returns the hex-encoded sha256 digest of the supplied key.
// This is what AuthMiddleware compares against `api_keys.key_hash`.
// The lookup is constant-time via a single equality on a 64-char
// hex string, so the timing-oracle property the legacy
// `lookupAPIKey` provides is preserved without the constant-time
// walk over every row.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix extracts the first PrefixDisplayLen characters of
// the key for storage in `api_keys.key_prefix`. The UI renders this
// alongside the row so operators can recognise which key they're
// about to revoke without exposing the secret.
func DisplayPrefix(key string) string {
	if len(key) <= PrefixDisplayLen {
		return key
	}
	return key[:PrefixDisplayLen]
}
