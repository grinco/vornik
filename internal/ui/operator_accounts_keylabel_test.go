package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Operator report, 2026-09-16: "I'd like in the operator/accounts surface to
// see API key names mapped to users, not the key IDs — otherwise I can't
// understand from the dropdown list if the key is already assigned to someone
// or it's dangling."
//
// Two halves, both true of the page as it stood:
//
//  1. The LINKED IDENTITIES line rendered `api_key:<ExternalID>` — the opaque
//     akey_<ts>_<hex>. The identity store has no idea what a key is called
//     (the name lives in api_keys, a different table), so the row showed the
//     one identifier §5.4 guarantees the operator cannot recognise.
//  2. The PICKER annotated a held key with "held by X" and left an
//     unattributed one with no marker at all. Telling "assigned" from
//     "dangling" meant scanning for the ABSENCE of a suffix, which is the
//     hardest thing to see in a list and the exact question being asked.
//
// The key list is already loaded for the picker, so both are labelling, not
// lookups.
func TestOperatorAccounts_KeyRowsShowNamesNotOpaqueIds(t *testing.T) {
	s, repo := operatorKeysServer()
	// The account holds akey_1, which the key repository calls "slava/codex".
	repo.users[0].Identities = []persistence.UserIdentityRef{
		{Channel: "api_key", ExternalID: "akey_1"},
	}

	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	body := rec.Body.String()

	// The row must name the key the way the operator knows it.
	if !strings.Contains(body, "slava/codex") {
		t.Error("the linked-identity row does not name the key; an operator cannot tell which key is attributed here")
	}
	// The id may still appear (support needs it), but must not be the only
	// thing identifying the key on that row.
	idx := strings.Index(body, "Linked identities")
	if idx < 0 {
		t.Fatal("no linked identities block rendered")
	}
	row := body[idx:min(idx+1200, len(body))]
	if !strings.Contains(row, "slava/codex") {
		t.Errorf("the key's name is missing from the identity row:\n%s", row)
	}
}

// A DANGLING key must say so positively, rather than being identifiable only
// by the absence of an owner.
func TestOperatorAccounts_PickerMarksUnassignedKeys(t *testing.T) {
	s, repo := operatorKeysServer()
	repo.users[0].Identities = []persistence.UserIdentityRef{
		{Channel: "api_key", ExternalID: "akey_1"},
	}

	rec := httptest.NewRecorder()
	s.operatorAccountsRouter(rec, withAuthOff(httptest.NewRequest(http.MethodGet, "/operator/accounts", nil)))
	body := rec.Body.String()

	// akey_1 is held by Vadim; akey_2 is held by nobody.
	if !strings.Contains(body, "held by Vadim") {
		t.Error("an attributed key must name its holder")
	}
	if !strings.Contains(body, "unassigned") {
		t.Error("an unattributed key must SAY it is unassigned; absence of a suffix is not a signal an operator can scan for")
	}
	// Every key stays in the list — moving one between assignees is the
	// operator's stated workflow, so held keys must remain selectable.
	if !strings.Contains(body, `value="akey_1"`) {
		t.Error("an already-assigned key must remain in the list so it can be moved")
	}
}
