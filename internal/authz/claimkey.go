package authz

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

// Key → account mapping — oidc-identity-permissions-design §5.4.
//
// A key is mapped by a `user_identities` row with channel "api_key" and
// external_id = the key's id. No owner column is added to `api_keys`: the
// observed actor stays what was observed, and the person is resolved at read
// time, so the moment a key is claimed its whole recorded history rolls up to
// its owner with no backfill and the mapping stays reversible.

// keyChannel is the user_identities channel a key mapping lives on. It
// matches actor.KindAPIKey's string deliberately — one vocabulary across the
// actor model and the identity tables, since two spellings for one concept is
// the likelier bug.
const keyChannel = "api_key"

// ErrKeyClaimRefused is the ONLY error a non-admin claim failure produces.
//
// Wrong secret, unknown key id, and a key someone else already claimed all
// answer this, with identical text (§5.4). A caller holding a valid secret
// must not learn from the response whether the key exists or whether it is
// already claimed — and unlike the link-code oracle, the probe input here is
// a real credential.
var ErrKeyClaimRefused = errors.New("authz: key claim refused")

// ErrKeyClaimRateLimited is the §5.4 attempt bound. It is DISTINCT from
// ErrKeyClaimRefused so a caller can answer 429 rather than a flat refusal,
// and it is raised identically for a wrong secret, an unknown key id and an
// already-claimed key — so the bound itself reveals nothing the refusals do
// not (audit 2026-09-15 CA-09).
var ErrKeyClaimRateLimited = errors.New("authz: too many claim attempts for this key")

// ErrKeysUnavailable is returned when no API-key repository is wired.
var ErrKeysUnavailable = errors.New("authz: key claims not available")

// WithAPIKeys attaches the key store used by the claim path.
func (a *Accounts) WithAPIKeys(keys persistence.APIKeyRepository) *Accounts {
	a.keys = keys
	return a
}

// ClaimKey maps a key to userID after the caller proves possession of its
// secret. Proving possession is what makes a self-service claim more than an
// assertion: without it any user could attribute any key's spend to anyone.
//
// BOTH lookups always run, and the decision is taken afterwards on their
// results. That ordering is the contract, not an implementation detail: an
// early return on a missing key would answer faster than a wrong-secret
// refusal and leak the cause through latency even with identical text
// (§5.4, round-3 finding R2-3).
func (a *Accounts) ClaimKey(ctx context.Context, userID, keyID, secret string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if a.keys == nil {
		return ErrKeysUnavailable
	}
	// The attempt bound is checked HERE, before the key repository is
	// touched, because this is the one function every claim door calls. It
	// used to sit in the REST handlers only, which the browser form went
	// around entirely (audit 2026-09-15 CA-09).
	if !a.claims.allow(keyID) {
		return ErrKeyClaimRateLimited
	}

	key, keyErr := a.keys.GetByID(ctx, keyID)
	ownerID, ownerErr := a.keyOwner(ctx, keyID)
	if ownerErr != nil {
		return fmt.Errorf("authz: claim key: %w", ownerErr)
	}
	// The CLAIMANT's own account, read unconditionally like the two above so
	// the lookup profile does not depend on the outcome. A disabled account
	// may not take a new key: §5.0 retains existing rows through a disable
	// (they are records, and the resolver refuses them), but recording a NEW
	// grant for an account that is disabled would write an audit row saying
	// someone acted who, by the deployment's own decision, may not
	// (review-20260914-3c36 F8). AssignKey already looked its target up;
	// ClaimKey did not look up anyone.
	claimant, claimantErr := a.Get(ctx, userID)

	// Now branch, on results only.
	switch {
	case keyErr != nil:
		// Unknown key id.
		return ErrKeyClaimRefused
	case claimantErr != nil || claimant.Disabled:
		// Same refusal as the rest: the claimant learns nothing about the
		// key from the state of their own account, and nothing about their
		// account they do not already know.
		return ErrKeyClaimRefused
	case !secretMatches(key.KeyHash, secret):
		return ErrKeyClaimRefused
	case ownerID == userID:
		// Already theirs: nothing to change, and nothing to refuse either.
		return nil
	case ownerID != "":
		// Someone else holds it. Silent repointing would move another
		// person's recorded spend without either of them seeing it.
		return ErrKeyClaimRefused
	}

	return a.bindKey(ctx, userID, keyID, actor, "account.key.claim")
}

// AssignKey maps a key to userID on an admin's authority, with no secret.
//
// This is what self-claim cannot do: correct a wrong mapping, and attribute
// the key of someone who has left. The caller is responsible for having
// established that the actor is an admin — this service does not re-derive
// authorization, it records the decision.
func (a *Accounts) AssignKey(ctx context.Context, userID, keyID string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if a.keys == nil {
		return ErrKeysUnavailable
	}
	if _, err := a.keys.GetByID(ctx, keyID); err != nil {
		// An admin may be told the key does not exist: they are already
		// authenticated and authorized to know, and the oracle §5.4 closes
		// is one for unauthenticated probing.
		return fmt.Errorf("authz: assign key: %w", err)
	}
	if _, err := a.Get(ctx, userID); err != nil {
		return fmt.Errorf("authz: assign key: %w", err)
	}
	return a.rebindKey(ctx, userID, keyID, actor, "account.key.assign")
}

// UnassignKey removes a key mapping, returning the key to unattributed.
func (a *Accounts) UnassignKey(ctx context.Context, keyID string, actor Actor) error {
	if err := a.ready(); err != nil {
		return err
	}
	if err := a.repo.RevokeIdentity(ctx, keyChannel, keyID); err != nil {
		return fmt.Errorf("authz: unassign key: %w", err)
	}
	return a.record(ctx, actor, "account.key.unassign", keyID, map[string]any{"channel": keyChannel})
}

// bindKey writes the mapping and audits it. The audit names the key id and
// never the secret, which would make the audit log a credential store.
//
// The bind is the GUARDED one: if another user has taken the key between the
// keyOwner read above and this write, it does not fire, and the claim is
// refused rather than audited as done. That window is small and the refusal
// is the same one the owner check produces, so the race adds no oracle.
func (a *Accounts) bindKey(ctx context.Context, userID, keyID string, actor Actor, action string) error {
	bound, err := a.repo.BindIdentity(ctx, a.keyIdentity(userID, keyID))
	if err != nil {
		return fmt.Errorf("authz: bind key: %w", err)
	}
	if !bound {
		return ErrKeyClaimRefused
	}
	return a.record(ctx, actor, action, userID, map[string]any{"channel": keyChannel, "key_id": keyID})
}

// rebindKey is bindKey on admin authority: it repoints a key already held by
// someone else, which is the whole point of AssignKey and the one thing
// self-claim must not be able to do.
//
// It used to share bindKey, and so shared its guard — the upsert declined to
// move an active binding, returned no error, and AssignKey audited a
// correction that had not happened (review-20260914-3c36 F11).
func (a *Accounts) rebindKey(ctx context.Context, userID, keyID string, actor Actor, action string) error {
	if err := a.repo.RebindIdentity(ctx, a.keyIdentity(userID, keyID)); err != nil {
		return fmt.Errorf("authz: bind key: %w", err)
	}
	return a.record(ctx, actor, action, userID, map[string]any{"channel": keyChannel, "key_id": keyID})
}

// keyIdentity builds the mapping row. Display is deliberately EMPTY.
//
// §3.1 calls that column operator-friendly ("@grinco"); it used to be stamped
// with the key id, a verbatim copy of ExternalID that told a reader nothing
// and displaced the key's real name on every surface that preferred Display
// (operator report 2026-09-16 — see keyidentity_display_test.go).
//
// The name is NOT copied here either: §5.4's governing decision is that the
// observed fact is stored and the person is resolved at read time. A key's
// name lives in api_keys and changes when it is renamed, so a copy taken at
// claim time would be stale from the first rename.
func (a *Accounts) keyIdentity(userID, keyID string) *persistence.UserIdentity {
	return &persistence.UserIdentity{
		ID:         persistence.GenerateID("uident"),
		UserID:     userID,
		Channel:    keyChannel,
		ExternalID: keyID,
		CreatedAt:  a.now(),
	}
}

// keyOwner returns the user a key is currently mapped to, or "" when it is
// unmapped. It is called on EVERY claim path, including the ones that will
// refuse, so the work done does not vary with the outcome.
func (a *Accounts) keyOwner(ctx context.Context, keyID string) (string, error) {
	rows, err := a.repo.ResolvePrincipalRows(ctx, keyChannel, keyID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].UserID, nil
}

// secretMatches compares the presented secret against the stored hash in
// constant time. The hash comparison is constant-time even though the
// surrounding lookups are not: §5.4 is honest that the residual is a
// cardinality-dependent latency signal, and this removes the one axis that
// would otherwise be trivially exploitable.
func secretMatches(storedHash, secret string) bool {
	if storedHash == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(apikey.Hash(secret)), []byte(storedHash)) == 1
}

// KeyOwnerDisabled reports whether the key's mapped owner is disabled.
//
// This is the API-key door of §5.0's door set. Before it existed the
// invariant "revoking the account revokes every door that authorizes" named
// the API-key door with no enforcement path behind it — the auth backend
// authenticates from key_hash alone and knows nothing about any person
// (round-3 finding F1).
//
// An UNMAPPED key reports false and costs one indexed lookup. That is every
// key on a deployment where nobody has claimed one, so the check changes no
// behaviour until someone does — but the lookup is unconditional, because
// deciding WHETHER a key is mapped is itself the lookup (round-4 F4-2).
func (a *Accounts) KeyOwnerDisabled(ctx context.Context, keyID string) (bool, error) {
	rows, err := a.repo.ResolvePrincipalRows(ctx, keyChannel, keyID)
	if err != nil {
		return false, fmt.Errorf("authz: key owner disabled: %w", err)
	}
	return UserDisabled(rows), nil
}

// KeyOwnerAccess reports what the key's mapped owner may currently reach:
// whether the key is mapped at all, whether that owner is disabled, and the
// owner's granted project set.
//
// It exists because "disabled" and "has no grant for this project" are two
// different revocations, and only the first was enforced at the API-key
// door. Removing a person's project access leaves disabled_at unset, so a
// disabled-only check let their claimed key keep reaching a project they had
// lost (audit 2026-09-15 CA-04, normative amendment R2).
//
// An UNMAPPED key reports Mapped=false — the same single indexed lookup
// KeyOwnerDisabled costs, since deciding whether a key is mapped IS the
// lookup.
func (a *Accounts) KeyOwnerAccess(ctx context.Context, keyID string) (auth.OwnerAccess, error) {
	rows, err := a.repo.ResolvePrincipalRows(ctx, keyChannel, keyID)
	if err != nil {
		return auth.OwnerAccess{}, fmt.Errorf("authz: key owner access: %w", err)
	}
	if len(rows) == 0 {
		return auth.OwnerAccess{Mapped: false}, nil
	}
	if UserDisabled(rows) {
		return auth.OwnerAccess{Mapped: true, Disabled: true, UserID: rows[0].UserID}, nil
	}
	p, err := principalFromRows(rows)
	if err != nil {
		// A mapped owner we cannot resolve is not an owner we may admit:
		// this is an access control, so the unresolvable case fails closed
		// with no grants rather than falling through ungoverned.
		return auth.OwnerAccess{Mapped: true, UserID: rows[0].UserID}, nil
	}
	if p.AllProjects() {
		return auth.OwnerAccess{Mapped: true, AllProjects: true, UserID: p.UserID}, nil
	}
	return auth.OwnerAccess{Mapped: true, Projects: p.Projects, UserID: p.UserID}, nil
}
