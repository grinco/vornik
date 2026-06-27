package auth

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// APIKeyLookup is the narrow lookup slice DBKeysBackend needs.
// Structurally identical to internal/api.APIKeyLookup so the same
// repository satisfies both — duplicated here (rather than imported)
// to keep internal/auth free of an api-package dependency.
type APIKeyLookup interface {
	LookupActiveByHash(ctx context.Context, keyHash string) (*persistence.APIKey, error)
}

// APIKeyToucher fires the async last_used_at update after a
// successful auth. Mirrors internal/api.APIKeyToucher.
type APIKeyToucher interface {
	TouchLastUsed(ctx context.Context, keyID string) error
}

// ExtraDBKeyRow is the Identity.Extra key under which DBKeysBackend
// stores the matched *persistence.APIKey row. The middleware reads
// it to enforce per-key rate limits and companion-key confinement —
// both write HTTP responses, so they cannot live inside the backend.
const ExtraDBKeyRow = "db_key_row"

// DBKeysBackend authenticates DB-backed `sk-vornik-*` bearer keys.
// Wraps the lookup + toucher pair from the inline middleware path
// (internal/api/middleware.go:260-313). Expiry / revocation are
// enforced by LookupActiveByHash's SQL predicate — an expired or
// revoked key simply has no active row and falls through.
//
// Sentinels: wrong shape or lookup miss → ErrNoCredential (the
// legacy migration window allows mixed static + DB keys, so a miss
// must reach the static backend); a FOUND row whose project doesn't
// match the prefix-embedded project → ErrUnauthorized (tampered
// prefix or poisoned row — never fall through).
type DBKeysBackend struct {
	Lookup  APIKeyLookup
	Toucher APIKeyToucher // nil disables last_used_at touches
}

// NewDBKeysBackend constructs the backend. A nil lookup yields a
// backend that returns ErrNoCredential on every call.
func NewDBKeysBackend(lookup APIKeyLookup, toucher APIKeyToucher) *DBKeysBackend {
	return &DBKeysBackend{Lookup: lookup, Toucher: toucher}
}

// Name returns the audit-trail identifier for this backend.
func (b *DBKeysBackend) Name() string { return "db-keys" }

// Authenticate resolves a sk-vornik-* bearer against the api_keys
// table. See the type comment for the sentinel contract.
func (b *DBKeysBackend) Authenticate(ctx context.Context, cred Credential) (*Identity, error) {
	if b == nil || b.Lookup == nil {
		return nil, ErrNoCredential
	}
	if cred.BearerToken == "" || !strings.HasPrefix(cred.BearerToken, apikey.Prefix+"-") {
		return nil, ErrNoCredential
	}
	row, err := b.Lookup.LookupActiveByHash(ctx, apikey.Hash(cred.BearerToken))
	if err != nil {
		// Lookup miss or transient DB error — fall through to the
		// static map, matching the legacy migration-window flow.
		return nil, ErrNoCredential
	}
	// Defense-in-depth: the prefix-embedded project tag (or the legacy
	// raw-projectID prefix) must match the row's project via
	// MatchesProject. A mismatch (key prefix tampered or DB poisoned)
	// is a hard reject — identical to the inline path's 401.
	claimedProject, _, parseErr := apikey.Parse(cred.BearerToken)
	if parseErr != nil || !apikey.MatchesProject(claimedProject, row.ProjectID) {
		return nil, ErrUnauthorized
	}
	// Async last_used_at touch — never blocks the hot path. A DB
	// hiccup means the column stays stale; auth still succeeded.
	// One goroutine per authenticated request, bounded only by
	// request rate — same trait as the legacy inline path.
	if b.Toucher != nil {
		go func(id string) {
			_ = b.Toucher.TouchLastUsed(context.Background(), id)
		}(row.ID)
	}
	id := &Identity{
		Subject:        row.ID,
		Projects:       []string{row.ProjectID},
		BoundProjectID: row.ProjectID,
		DisplayName:    row.Name,
		IssuedAt:       row.CreatedAt,
		Extra:          map[string]any{ExtraDBKeyRow: row},
	}
	if row.ExpiresAt != nil {
		id.ExpiresAt = *row.ExpiresAt
	}
	return id, nil
}
