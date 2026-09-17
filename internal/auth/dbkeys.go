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
	// OwnerAccess reports what this key's MAPPED owner may currently reach.
	//
	// This is the API-key door of oidc-identity-permissions-design §5.0 and
	// review amendment R2. Until it existed, the invariant "revoking the
	// account revokes every door that authorizes" named this door with no
	// enforcement path behind it: this backend authenticates from key_hash
	// alone and knows nothing about any person, so a claimed key whose owner
	// was disabled kept working (round-3 finding F1).
	//
	// It reports the owner's GRANTS and not merely their disabled flag,
	// because those are two different revocations and only one of them was
	// enforced. Removing a person's project access through
	// Accounts.RemoveUserAccess leaves disabled_at unset, so a check that
	// asked only "is this owner disabled?" let the mapped key carry on
	// reaching a project its owner had lost (audit 2026-09-15 CA-04).
	//
	// Nil on a deployment that never maps keys to people — behaviour is then
	// exactly as before. An unmapped key reports Mapped=false and costs one
	// indexed lookup; the lookup is unconditional because deciding WHETHER a
	// key is mapped is itself the lookup (round-4 finding F4-2).
	OwnerAccess OwnerAccessCheck
}

// OwnerAccess is what a mapped key's owner may currently reach.
type OwnerAccess struct {
	// Mapped is false when no person has claimed this key. The other
	// fields are then meaningless and the key keeps its own authority.
	Mapped bool
	// Disabled is the §5.0 door: a disabled owner denies.
	Disabled bool
	// AllProjects is true for an admin owner, who narrows nothing.
	AllProjects bool
	// Projects is the owner's granted project set. EMPTY on a mapped,
	// enabled, non-admin owner means "awaiting access", which denies —
	// authenticated is not authorized (R2).
	Projects []string
	// UserID is the owning account (users.id). It is stamped onto the
	// APIKey row this backend puts in Identity.Extra, which is what makes
	// persistence.APIKey.OwnerUserID true: the field was documented as the
	// key's owner and every consumer read it, but the mapping lives in
	// user_identities and no driver ever populated the column, so
	// attribution silently fell back to "no person" (audit 2026-09-15
	// CA-11).
	UserID string
}

// OwnerAccessCheck resolves keyID's mapped account access. Implemented over
// the identity store by authz.Accounts.KeyOwnerAccess, injected here so
// internal/auth keeps no dependency on the identity layer.
type OwnerAccessCheck func(ctx context.Context, keyID string) (OwnerAccess, error)

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
	// The owner check runs AFTER the key is authenticated and validated, and
	// its refusal is ErrUnauthorized rather than ErrNoCredential — so it
	// SHORT-CIRCUITS rather than falling through to the static break-glass
	// backend. A request carrying both a disabled-owner key and valid Basic
	// Auth still fails: break-glass exists for an IdP outage, not for a
	// disabled account (§5.0, ledger bullet F5-4).
	//
	// The refusal is the ordinary 401, indistinguishable from a wrong or
	// rotated key, so an unauthenticated caller learns nothing about a
	// person. The cost is that a legitimate key-only principal cannot tell
	// the two apart either; the audit row the disable wrote is the operator's
	// diagnostic channel (§5.0, F5-5).
	//
	// A check that ERRORS refuses too. This is an access control, so a
	// database hiccup must fail closed — treating an error as "not disabled"
	// would make the door openable by breaking the lookup it depends on.
	if b.OwnerAccess != nil {
		access, err := b.OwnerAccess(ctx, row.ID)
		if err != nil {
			return nil, ErrUnauthorized
		}
		if !ownerAdmits(access, row.ProjectID) {
			return nil, ErrUnauthorized
		}
		// Resolve the documented owner onto the row every downstream
		// consumer reads, so attribution can name the person behind a
		// claimed key (audit 2026-09-15 CA-11). Unowned keys keep "".
		if access.Mapped && access.UserID != "" {
			owned := *row
			owned.OwnerUserID = access.UserID
			row = &owned
		}
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
	// The principal is built from the KEY's project, never from the owner's
	// grant set: mapping narrows a credential, it never promotes one. An
	// admin's claim on a single-project machine key does not turn that key
	// into an admin key.
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

// ownerAdmits applies the R2 narrowing rules to a mapped key.
//
// The order matters and each branch is a separate revocation:
//   - not mapped        → the key is nobody's; it keeps its own authority
//   - disabled owner    → deny (§5.0)
//   - admin owner       → admit; an admin narrows nothing
//   - zero grants       → deny; "awaiting access" is not access
//   - project not granted → deny; the person lost this project, so the
//     credential mapped to them loses it too
func ownerAdmits(access OwnerAccess, keyProject string) bool {
	if !access.Mapped {
		return true
	}
	if access.Disabled {
		return false
	}
	if access.AllProjects {
		return true
	}
	for _, p := range access.Projects {
		if p == keyProject || p == "*" {
			return true
		}
	}
	return false
}
