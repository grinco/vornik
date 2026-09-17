package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// Operator report, 2026-09-16, part two: the key identifiers on screen read
// akey_20260627092635_6d9fd4db0d825d42, "which I expect to be the name that I
// set when minting one, not its id — same in the accounts, linked identities
// list".
//
// WHY THE FIRST FIX (2c50fe255) DID NOT REACH THE DEPLOYED BOX. It labelled
// identities whose Display was EMPTY, and the test seeded exactly that shape.
// Production rows are not that shape: authz.keyIdentity stamped the key id
// into Display, so every real row arrived with a non-empty, uninformative
// Display and the labeller skipped it. The test passed against a state the
// database never held — the same "asserted the broken state" pattern as CA-19.
//
// The write is fixed at its seam (authz/keyidentity_display_test.go), but the
// rows already written keep their stamped id, on this operator's box and every
// other deployment. So the READ side must treat a Display that merely repeats
// the ExternalID as carrying no information — which is what these two tests
// pin, in the production shape.

// keyNamesStub answers both the picker listing and the per-key lookup the
// personal page makes.
type keyNamesStub struct {
	persistence.APIKeyRepository
	rows []*persistence.APIKey
	gets []string
}

func (k *keyNamesStub) ListAttributable(context.Context) ([]*persistence.APIKey, error) {
	return k.rows, nil
}

func (k *keyNamesStub) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	k.gets = append(k.gets, id)
	for _, r := range k.rows {
		if r.ID == id {
			cp := *r
			return &cp, nil
		}
	}
	return nil, persistence.ErrAPIKeyNotFound
}

func namedKeys() *keyNamesStub {
	return &keyNamesStub{rows: []*persistence.APIKey{
		{ID: "akey_1", Name: "slava/codex", ProjectID: "slava-companion", KeyPrefix: "sk-vornik-08"},
	}}
}

// The accounts page, with the row shape production actually stores.
func TestOperatorAccounts_NamesAKeyWhoseStoredDisplayIsItsOwnId(t *testing.T) {
	keys := namedKeys()
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Vadim", Role: "user",
		// The shape authz wrote until 2026-09-16: Display == ExternalID.
		Identities: []persistence.UserIdentityRef{
			{Channel: "api_key", ExternalID: "akey_1", Display: "akey_1"},
		},
	}}}
	s := NewServer(
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).WithAPIKeys(keys)),
		WithAPIKeyRepository(keys),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
	)

	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	// Scoped to the identity line: the PICKER lists every key by name too, so
	// an unscoped substring search passes on a page whose identity row still
	// shows the id — which is how the first fix was believed to work.
	row := identityLine(t, rec.Body.String())
	if !strings.Contains(row, "slava/codex") {
		t.Errorf("the linked-identity row still shows the opaque id: a stored Display that repeats "+
			"the ExternalID must not displace the key's name:\n%s", row)
	}
}

// identityLine returns the rendered "api key: ..." fragment, so an assertion
// about that row cannot be satisfied by text elsewhere on the page.
func identityLine(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, "api key:")
	if i < 0 {
		t.Fatal("no api_key identity row rendered")
	}
	return body[i:min(i+300, len(body))]
}

// The personal page — the surface the operator was actually looking at, and
// the one the first fix never touched at all.
func TestMyAccount_NamesTheKeysBoundToYou(t *testing.T) {
	keys := namedKeys()
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Vadim", Role: "admin",
		Identities: []persistence.UserIdentityRef{
			{Channel: "api_key", ExternalID: "akey_1", Display: "akey_1"},
		},
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{})),
		WithAPIKeyRepository(keys),
	)

	rec := httptest.NewRecorder()
	s.MyAccount(rec, httptest.NewRequest(http.MethodGet, "/account", nil))
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "slava/codex") {
		t.Error("your own API key is listed by its id; the page that exists to show you every " +
			"identity that resolves to you must name them the way you named them")
	}
	// Resolved per identity, not by listing every key in the deployment: a
	// personal page has no business enumerating other people's keys.
	if len(keys.gets) != 1 || keys.gets[0] != "akey_1" {
		t.Errorf("GetByID calls = %v, want exactly [akey_1]", keys.gets)
	}
}

// A key the repository cannot resolve (deleted, or no key repository wired at
// all on a Community box) must still render a row. Falling back to the id is
// worse than a name and better than dropping the binding, which is the one
// outcome the page must never produce: §5.5 makes this the place a person
// sees EVERY identity that resolves to them.
func TestMyAccount_KeepsTheRowWhenTheKeyCannotBeNamed(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Vadim", Role: "admin",
		Identities: []persistence.UserIdentityRef{
			{Channel: "api_key", ExternalID: "akey_gone", Display: "akey_gone"},
		},
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{})),
	)

	rec := httptest.NewRecorder()
	s.MyAccount(rec, httptest.NewRequest(http.MethodGet, "/account", nil))
	if body := rec.Body.String(); !strings.Contains(body, "akey_gone") {
		t.Error("an unresolvable key binding vanished from the page; it must fall back to the id")
	}
}
