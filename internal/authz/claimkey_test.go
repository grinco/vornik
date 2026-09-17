package authz

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// recordingKeys is an APIKeyRepository double that records every GetByID it
// served. The call record is what the indistinguishability test inspects:
// §5.4 requires both refusal paths to perform the SAME lookups and branch only
// on the results, and an implementation that short-circuits would leak the
// answer through latency even with identical error text.
type recordingKeys struct {
	persistence.APIKeyRepository
	rows  map[string]*persistence.APIKey
	calls []string
}

func newRecordingKeys() *recordingKeys {
	return &recordingKeys{rows: map[string]*persistence.APIKey{}}
}

func (r *recordingKeys) GetByID(_ context.Context, keyID string) (*persistence.APIKey, error) {
	r.calls = append(r.calls, "GetByID:"+keyID)
	k, ok := r.rows[keyID]
	if !ok {
		return nil, persistence.ErrAPIKeyNotFound
	}
	cp := *k
	return &cp, nil
}

// claimFixture wires the account service with a key store.
type claimFixture struct {
	accounts *Accounts
	keys     *recordingKeys
	repo     *memIdentityRepo
	audit    *memAudit
	owner    *persistence.User
	other    *persistence.User
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	f := &claimFixture{repo: newMemRepo(), audit: &memAudit{}, keys: newRecordingKeys()}
	f.accounts = NewAccounts(f.repo, f.audit).WithAPIKeys(f.keys)
	ctx := context.Background()
	f.owner = &persistence.User{ID: "user_owner", DisplayName: "Owner"}
	f.other = &persistence.User{ID: "user_other", DisplayName: "Other"}
	for _, u := range []*persistence.User{f.owner, f.other} {
		if err := f.repo.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	return f
}

// key registers a key with the given raw secret and returns its id.
func (f *claimFixture) key(t *testing.T, id, secret string) string {
	t.Helper()
	f.keys.rows[id] = &persistence.APIKey{
		ID:        id,
		ProjectID: "proj",
		KeyHash:   apikey.Hash(secret),
		CreatedAt: time.Now().UTC(),
	}
	return id
}

// TestClaimKey_BindsOnCorrectSecret is the happy path: possession of the
// secret is what makes a self-claim more than an assertion.
func TestClaimKey_BindsOnCorrectSecret(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_1", "sk-vornik-secret-1")

	if err := f.accounts.ClaimKey(context.Background(), f.owner.ID, id, "sk-vornik-secret-1", Actor{Principal: "owner", Source: "api"}); err != nil {
		t.Fatalf("ClaimKey: %v", err)
	}
	if got := f.repo.bindings["api_key:"+id]; got != f.owner.ID {
		t.Errorf("binding = %q, want %q — the mapping is a user_identities row on channel api_key", got, f.owner.ID)
	}
}

// TestClaimKey_RefusalsAreIndistinguishable is §5.4's oracle rule. A caller
// holding a valid secret must not learn from the response whether the key
// exists, or whether it is already claimed.
func TestClaimKey_RefusalsAreIndistinguishable(t *testing.T) {
	f := newClaimFixture(t)
	liveKey := f.key(t, "akey_real", "sk-vornik-real")
	mapped := f.key(t, "akey_mapped", "sk-vornik-mapped")
	if err := f.accounts.ClaimKey(context.Background(), f.other.ID, mapped, "sk-vornik-mapped", Actor{Principal: "other"}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	cases := []struct {
		name, keyID, secret string
	}{
		{"wrong secret against a real key", liveKey, "sk-vornik-WRONG"},
		{"any secret against a key id that does not exist", "akey_ghost", "sk-vornik-real"},
		{"correct secret against a key someone else already claimed", mapped, "sk-vornik-mapped"},
	}
	var errs []string
	var callShapes []int
	for _, tc := range cases {
		f.keys.calls = nil
		f.repo.calls = nil
		err := f.accounts.ClaimKey(context.Background(), f.owner.ID, tc.keyID, tc.secret, Actor{Principal: "owner"})
		if err == nil {
			t.Fatalf("%s: expected a refusal", tc.name)
		}
		if !errors.Is(err, ErrKeyClaimRefused) {
			t.Errorf("%s: err = %v, want ErrKeyClaimRefused", tc.name, err)
		}
		errs = append(errs, err.Error())
		// BOTH stores count. Recording only the key lookup would miss the
		// short-circuit that matters: a missing key id must still cost the
		// identity lookup, or it answers faster than a wrong-secret refusal.
		callShapes = append(callShapes, len(f.keys.calls)+len(f.repo.calls))
	}
	for i := 1; i < len(errs); i++ {
		if errs[i] != errs[0] {
			t.Errorf("refusal text differs: %q (%s) vs %q (%s) — that is the oracle §5.4 forbids",
				errs[i], cases[i].name, errs[0], cases[0].name)
		}
	}
	// The operation-set assertion: an implementation that returned early on a
	// missing key would do fewer lookups and answer faster, leaking the cause
	// through latency even with identical text.
	for i := 1; i < len(callShapes); i++ {
		if callShapes[i] != callShapes[0] {
			t.Errorf("%s performed %d key lookups, %s performed %d — both paths must do the same work",
				cases[i].name, callShapes[i], cases[0].name, callShapes[0])
		}
	}
}

// TestClaimKey_OwnerReclaimIsANoOp: the claimant is already the owner, so
// there is nothing to refuse and nothing to change.
func TestClaimKey_OwnerReclaimIsANoOp(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_2", "sk-vornik-2")
	ctx := context.Background()
	if err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-2", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-2", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("re-claim by the same owner must be a no-op, got %v", err)
	}
}

// TestClaimKey_RevokedKeyStaysClaimableAndStaysRevoked pins both halves of
// §5.4: the history of a revoked key is exactly what the mapping exists to
// attribute, and claiming it must not re-enable it for authentication.
func TestClaimKey_RevokedKeyStaysClaimableAndStaysRevoked(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_revoked", "sk-vornik-revoked")
	revoked := time.Now().UTC()
	f.keys.rows[id].RevokedAt = &revoked

	if err := f.accounts.ClaimKey(context.Background(), f.owner.ID, id, "sk-vornik-revoked", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("a revoked key must stay claimable: %v", err)
	}
	if f.keys.rows[id].RevokedAt == nil {
		t.Error("claiming must not clear revoked_at — claim is not reactivation")
	}
}

// TestAssignKey_AdminCanCorrectAMapping is the other half of the operator
// decision: self-claim cannot fix a wrong mapping or attribute a leaver's key.
func TestAssignKey_AdminCanCorrectAMapping(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_3", "sk-vornik-3")
	ctx := context.Background()
	if err := f.accounts.ClaimKey(ctx, f.other.ID, id, "sk-vornik-3", Actor{Principal: "other"}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// No secret is presented: the admin's authority replaces proof of
	// possession, which is the whole reason this path exists.
	if err := f.accounts.AssignKey(ctx, f.owner.ID, id, Actor{Principal: "admin", Source: "ui"}); err != nil {
		t.Fatalf("AssignKey: %v", err)
	}
	if got := f.repo.bindings["api_key:"+id]; got != f.owner.ID {
		t.Errorf("after admin re-assign the binding is %q, want %q", got, f.owner.ID)
	}
}

// TestClaimAndAssign_AreAudited: both move attribution and, for keys, spend.
func TestClaimAndAssign_AreAudited(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_4", "sk-vornik-4")
	ctx := context.Background()
	if err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-4", Actor{Principal: "owner"}); err != nil {
		t.Fatalf("ClaimKey: %v", err)
	}
	if err := f.accounts.AssignKey(ctx, f.other.ID, id, Actor{Principal: "admin"}); err != nil {
		t.Fatalf("AssignKey: %v", err)
	}
	var actions []string
	for _, e := range f.audit.rows {
		actions = append(actions, e.Action)
	}
	joined := strings.Join(actions, ",")
	for _, want := range []string{"account.key.claim", "account.key.assign"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit actions %v missing %q", actions, want)
		}
	}
	// The secret must never reach the audit trail, which would make the log a
	// credential store.
	for _, e := range f.audit.rows {
		if strings.Contains(e.After, "sk-vornik-4") {
			t.Fatalf("the raw key secret reached an audit row: %s", e.After)
		}
	}
}

// TestClaimKey_RefusesWithoutAudit — same service-wide rule as every other
// mutation here.
func TestClaimKey_RefusesWithoutAudit(t *testing.T) {
	f := newClaimFixture(t)
	id := f.key(t, "akey_5", "sk-vornik-5")
	f.accounts.audit = nil
	if err := f.accounts.ClaimKey(context.Background(), f.owner.ID, id, "sk-vornik-5", Actor{Principal: "owner"}); !errors.Is(err, ErrUnauditable) {
		t.Fatalf("ClaimKey without an audit sink = %v, want ErrUnauditable", err)
	}
	if _, bound := f.repo.bindings["api_key:"+id]; bound {
		t.Error("no binding may be written when the mutation is refused")
	}
}

// TestClaimKey_RefusesADisabledClaimant — §5.0 retains an identity's rows
// through a disable, because they are records and the resolver refuses them
// anyway. Writing a NEW one is different: it produces an audit row saying a
// disabled account acted (review-20260914-3c36 F8).
//
// AssignKey already resolved its target; ClaimKey looked up nobody.
func TestClaimKey_RefusesADisabledClaimant(t *testing.T) {
	f := newClaimFixture(t)
	ctx := context.Background()
	id := f.key(t, "akey_disabled", "sk-vornik-disabled")

	if err := f.repo.SetUserDisabled(ctx, f.owner.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	err := f.accounts.ClaimKey(ctx, f.owner.ID, id, "sk-vornik-disabled", Actor{Principal: "owner"})
	if !errors.Is(err, ErrKeyClaimRefused) {
		t.Errorf("ClaimKey by a disabled account = %v, want ErrKeyClaimRefused", err)
	}
	if got, ok := f.repo.bindings["api_key:"+id]; ok {
		t.Errorf("the refused claim bound the key to %q", got)
	}
	for _, e := range f.audit.rows {
		if e.Action == "account.key.claim" {
			t.Error("the refused claim wrote an audit row saying a disabled account claimed a key")
		}
	}
}
