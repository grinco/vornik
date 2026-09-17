package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
)

func accountsServer(repo *stubUsersIdentityRepo, audit *stubAdminAuditRepo) *Server {
	return NewServer(
		WithAccountsService(authz.NewAccounts(repo, audit)),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
		WithProjectRegistry(registry.New()),
	)
}

func withAuthOff(r *http.Request) *http.Request {
	return r.WithContext(api.ContextWithAuthEnabled(r.Context(), false))
}

func TestOperatorAccounts_NotWired(t *testing.T) {
	s := NewServer()
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "not wired") {
		t.Fatalf("unwired page: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/u/disable", nil)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired POST: want 503, got %d", rec.Code)
	}
}

// Review R2: the CE shell refuses a caller without an explicit capability
// even though the key is unrestricted; the break-glass key passes.
func TestOperatorAccounts_CapabilityGate(t *testing.T) {
	s := accountsServer(&stubUsersIdentityRepo{users: twoUserFixture()}, &stubAdminAuditRepo{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)
	req = req.WithContext(api.ContextWithAPIKeyForTesting(api.ContextWithAuthEnabled(req.Context(), true), "sk-plain"))
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain key: want 403, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)
	req = req.WithContext(api.ContextWithAPIKeyForTesting(api.ContextWithAuthEnabled(req.Context(), true), "sk-operator"))
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Vadim") {
		t.Fatalf("break-glass key: want 200 with the accounts, got %d", rec.Code)
	}
}

func TestOperatorAccounts_GrantAndRevokeAudited(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: twoUserFixture()}
	audit := &stubAdminAuditRepo{}
	s := accountsServer(repo, audit)

	form := strings.NewReader("role=user&projects=proj-a")
	req := withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/grant", form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "access+updated") {
		t.Fatalf("grant: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "account.grant" || audit.rows[0].Source != "ui" {
		t.Fatalf("audit rows: %+v", audit.rows)
	}

	// A user-role grant with no project is refused before the repository.
	req = withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/grant", strings.NewReader("role=user")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "pick+at+least+one") {
		t.Fatalf("empty projects must be refused: %s", rec.Header().Get("Location"))
	}

	// Create.
	req = withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts", strings.NewReader("display_name=Ada&role=admin")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "account+created") {
		t.Fatalf("create: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Unknown action → 404; GET on an action → 405.
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodPost, "/operator/accounts/user_await/explode", nil)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown action: want 404, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts/user_await/disable", nil)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET action: want 405, got %d", rec.Code)
	}
	_ = context.Background()
}

// §5.5's operator key surface. The design's table promised "operator → Keys &
// access" for key assignment; the API and CLI had it and this page did not,
// which made "reachable from all three" true of the design and false of the
// build. These pin the three actions and, above all, that the link code the
// page mints never reaches a URL.

// operatorKeysStubKeys is the minimal APIKeyRepository the key actions need:
// AssignKey confirms the key exists before binding it.
type operatorKeysStubKeys struct {
	persistence.APIKeyRepository
	known map[string]bool
}

func (k operatorKeysStubKeys) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	if !k.known[id] {
		return nil, persistence.ErrAPIKeyNotFound
	}
	return &persistence.APIKey{ID: id}, nil
}

func operatorKeysServer() (*Server, *stubUsersIdentityRepo) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Vadim", Role: "user",
	}}}
	acc := authz.NewAccounts(repo, &stubAdminAuditRepo{}).
		WithAPIKeys(operatorKeysStubKeys{known: map[string]bool{"akey_1": true}}).
		WithLinkCodes(newOperatorStubLinkCodes())
	return NewServer(
		WithAccountsService(acc),
		WithAPIKeyRepository(operatorKeysStubKeys{known: map[string]bool{"akey_1": true}}),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-operator"}}),
		WithProjectRegistry(registry.New()),
	), repo
}

// TestOperatorAccounts_IssuedLinkCodeNeverEntersAURL — the incident is
// already in the ledger: the self-service panel's first cut redirected to
// ?notice=Link+code+ACDE2345, putting a one-time credential into browser
// history, the Referer header of everything the page loads, and every access
// log on the path. This page mints the same kind of code, so it gets the same
// assertion rather than the same lesson twice.
func TestOperatorAccounts_IssuedLinkCodeNeverEntersAURL(t *testing.T) {
	s, _ := operatorKeysServer()

	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(
		httptest.NewRequest(http.MethodPost, "/operator/accounts/user_1/link-code", nil)))

	if rec.Code == http.StatusSeeOther {
		t.Fatalf("the page REDIRECTED after issuing a one-time code; Location=%q", rec.Header().Get("Location"))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 with the code rendered inline", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store: the response carries a credential", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if !strings.Contains(rec.Body.String(), "Link code for this account") {
		t.Error("the page did not render the issued code inline")
	}
}

// TestOperatorAccounts_AssignsAndUnassignsAKey — the two authority actions.
// Assign is precisely what self-claim cannot do (correct a wrong mapping,
// attribute the key of someone who has left), so it exists ONLY here.
func TestOperatorAccounts_AssignsAndUnassignsAKey(t *testing.T) {
	s, repo := operatorKeysServer()

	post := func(action, keyID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/operator/accounts/user_1/"+action,
			strings.NewReader("key_id="+keyID))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.operatorAccountsRouter(rec, withAuthOff(req))
		return rec
	}

	if rec := post("assign-key", "akey_1"); rec.Code != http.StatusSeeOther {
		t.Fatalf("assign-key status = %d (%s), want a redirect back to the page", rec.Code, rec.Body.String())
	}
	if len(repo.rebound) != 1 || repo.rebound[0].ExternalID != "akey_1" || repo.rebound[0].UserID != "user_1" {
		t.Errorf("after assign the repository saw %+v, want one rebind of akey_1 to user_1", repo.rebound)
	}

	if rec := post("unassign-key", "akey_1"); rec.Code != http.StatusSeeOther {
		t.Fatalf("unassign-key status = %d, want a redirect", rec.Code)
	}
	if len(repo.revoked) != 1 || repo.revoked[0].externalID != "akey_1" {
		t.Errorf("after unassign the repository saw %+v, want one revoke of akey_1", repo.revoked)
	}
}

// operatorStubLinkCodes stores codes by hash. It obeys the miss contract the
// real repositories obey, so the page's behaviour on a miss is the page's and
// not the double's.
type operatorStubLinkCodes struct {
	codes map[string]*persistence.LinkCode
}

func newOperatorStubLinkCodes() *operatorStubLinkCodes {
	return &operatorStubLinkCodes{codes: map[string]*persistence.LinkCode{}}
}

func (m *operatorStubLinkCodes) CreateLinkCode(_ context.Context, lc *persistence.LinkCode) error {
	cp := *lc
	m.codes[lc.CodeHash] = &cp
	return nil
}

func (m *operatorStubLinkCodes) GetLinkCode(_ context.Context, hash string) (*persistence.LinkCode, error) {
	lc, ok := m.codes[hash]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return lc, nil
}

func (m *operatorStubLinkCodes) ConsumeLinkCode(_ context.Context, hash, channel, externalID string) (*persistence.LinkCode, error) {
	lc, ok := m.codes[hash]
	if !ok || lc.UsedAt != nil {
		return nil, persistence.ErrNotFound
	}
	now := time.Now().UTC()
	lc.UsedAt, lc.UsedByChannel, lc.UsedByExternalID = &now, channel, externalID
	return lc, nil
}

func (m *operatorStubLinkCodes) OutstandingLinkCodes(_ context.Context, userID string) ([]*persistence.LinkCode, error) {
	var out []*persistence.LinkCode
	for _, lc := range m.codes {
		if lc.UserID == userID && lc.UsedAt == nil {
			out = append(out, lc)
		}
	}
	return out, nil
}

// TestOperatorStubLinkCodes_MissContract holds this file's double to the same
// miss contract the real repositories obey.
func TestOperatorStubLinkCodes_MissContract(t *testing.T) {
	m := newOperatorStubLinkCodes()
	repotest.AssertMissRepo(t, "LinkCodeRepository.GetLinkCode", m.GetLinkCode)
	repotest.AssertMiss(t, "LinkCodeRepository.ConsumeLinkCode", func() (*persistence.LinkCode, error) {
		return m.ConsumeLinkCode(context.Background(), "absent", "telegram", "1")
	})
}

// TestOperatorKeyActions_AreNotAdminGated is the CE/EE property for the
// surface added in this build. Identity is a COMMUNITY feature: its operator
// routes live in the CE operator shell behind requireOperatorCapability, not
// behind the Enterprise admin gate, because /ui/admin/* answers 501
// EDITION_UNSUPPORTED in Community.
//
// A key action that required the admin surface would be absent in exactly the
// edition this feature ships in — and would be absent silently, since the
// page would still render with the rest of the block missing.
func TestOperatorKeyActions_AreNotAdminGated(t *testing.T) {
	s, _ := operatorKeysServer()

	// An operator key, no admin edition gate and no admin session.
	for _, action := range []string{"assign-key", "unassign-key", "link-code"} {
		t.Run(action, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/operator/accounts/user_1/"+action,
				strings.NewReader("key_id=akey_1"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			s.operatorAccountsRouter(rec, withAuthOff(req))

			if rec.Code == http.StatusForbidden || rec.Code == http.StatusNotImplemented {
				t.Errorf("%s answered %d — a Community operator cannot reach it, and identity "+
					"is a Community feature", action, rec.Code)
			}
		})
	}

	// And the page itself renders the block for a non-admin operator.
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	if !strings.Contains(rec.Body.String(), "Keys &amp; access") {
		t.Error("the Keys & access block is missing for a Community operator")
	}
}

// operatorKeysStubKeys gains ListAll so the picker has something to offer.
func (k operatorKeysStubKeys) ListAttributable(context.Context) ([]*persistence.APIKey, error) {
	return []*persistence.APIKey{
		{ID: "akey_1", Name: "slava/codex", ProjectID: "slava-companion", KeyPrefix: "sk-vornik-08"},
		{ID: "akey_2", Name: "", ProjectID: "orphaned", KeyPrefix: "sk-vornik-zz"},
	}, nil
}

// TestOperatorAccounts_KeyPickerOffersNamesNotIds — the defect this closes was
// reported by the operator: they tried to attribute a key called
// "slava/codex", which is the only identifier they had, into a field asking
// for an opaque akey_<ts>_<hex> id. §5.4 guarantees they do not hold the
// secret either, so the form asked for the one identifier nobody has, and a
// wrong value answered "assign-key failed — see the daemon log".
func TestOperatorAccounts_KeyPickerOffersNamesNotIds(t *testing.T) {
	s, _ := operatorKeysServer()
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	body := rec.Body.String()

	if strings.Contains(body, `placeholder="key id"`) {
		t.Error("the attribution form still asks an operator to type an opaque key id")
	}
	if !strings.Contains(body, `<select name="key_id"`) {
		t.Fatal("no key picker rendered")
	}
	// The picker must show what a person recognises...
	for _, want := range []string{"slava/codex", "slava-companion", "sk-vornik-08"} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker does not show %q; an operator cannot recognise the key they mean", want)
		}
	}
	// ...and still submit the id the service needs.
	if !strings.Contains(body, `value="akey_1"`) {
		t.Error("the picker does not submit the key id")
	}
	// A key with no name must not render as a blank, unpickable row.
	if !strings.Contains(body, "(unnamed)") {
		t.Error("an unnamed key renders as a blank option")
	}
}

// TestOperatorAccounts_UnknownKeyIdSaysWhatIsWrong — the operator's wrong
// value produced "assign-key failed — see the daemon log", because no branch
// matched and the curated default swallowed it. Curating a default is right;
// leaving the case that actually happens to fall into it is not.
func TestOperatorAccounts_UnknownKeyIdSaysWhatIsWrong(t *testing.T) {
	s, _ := operatorKeysServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/operator/accounts/user_1/assign-key",
		strings.NewReader("key_id=slava/codex")) // the NAME, as the operator typed
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.operatorAccountsRouter(rec, withAuthOff(req))

	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "no+key+with+that+id") {
		t.Errorf("notice = %q, want one that says the id is unknown and points at the list", loc)
	}
	if strings.Contains(loc, "see+the+daemon+log") {
		t.Error("the operator is still told to read the daemon log for a value they mistyped")
	}
}

// TestOperatorAccounts_NamesOnlyChatCommandsThatExist — the page's own blurb
// told operators to "link channels with /account link <code>". There is no
// /account command on either channel: Telegram answers /link <code> and Slack
// answers "<slack.slash_command> link <code>". A documented behaviour nothing
// implements is worse than an absent one, because it stops the next person
// looking — and this one sat directly above the button that issues the code.
//
// Found 2026-09-15 while fixing the sibling defect on "My account".
func TestOperatorAccounts_NamesOnlyChatCommandsThatExist(t *testing.T) {
	s, _ := operatorKeysServer()
	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(
		httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	body := rec.Body.String()

	if strings.Contains(body, "/account link") {
		t.Error("the page names an /account chat command; no channel implements one")
	}
	if !strings.Contains(body, "/link") {
		t.Error("the page no longer names Telegram's redemption command")
	}
	// Both halves, not just Telegram's. Asserting only "/link" left a
	// regression that dropped the Slack instruction entirely passing, while
	// the test's name promised it covered what exists (review-20260915-3d5b,
	// finding 6).
	if !strings.Contains(body, "slash command") {
		t.Error("the page no longer tells operators how redemption works on Slack")
	}
}
