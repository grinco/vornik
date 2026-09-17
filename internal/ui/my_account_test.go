package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// /ui/account is the SELF-SERVICE identity surface (§5.5's "Linked
// identities" panel). It is the one identity page that needs no operator
// capability and no admin surface, so it works identically in Community and
// Enterprise — which is why it, rather than an admin page, is where a person
// manages their own bindings.

// serverAsUser builds a UI server whose "who is signed in" resolver answers
// userID. The resolver is injected rather than read from a context key
// because api's identity key is unexported — and injecting it also makes the
// page testable without standing up the whole auth chain.
func serverAsUser() *Server {
	const userID = "user_1"
	repo, audit := &stubUsersIdentityRepo{}, &stubAdminAuditRepo{}
	return NewServer(
		WithSessionUserResolver(func(*http.Request) string { return userID }),
		WithAccountsService(authz.NewAccounts(repo, audit)),
	)
}

// TestMyAccount_RequiresASignedInUser — the page is about YOUR bindings, so
// without a session there is nothing to render and nothing to guess at.
//
// The resolver IS wired here and returns nobody. That distinction was
// previously invisible: an unwired resolver and an anonymous visitor both
// produced "sign in to see your account", so a Community deployment with no
// session backend at all told its users to sign in somewhere that does not
// exist (audit 2026-09-15 CA-12). The unwired case is now its own answer,
// asserted by TestMyAccount_SaysSoWhenThereIsNoPersonalLogin.
func TestMyAccount_RequiresASignedInUser(t *testing.T) {
	s := NewServer(WithSessionUserResolver(func(*http.Request) string { return "" }))
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	rec := httptest.NewRecorder()
	s.MyAccount(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestMyAccount_IsNotAdminGated is the CE/EE property: a page that required
// the admin surface would be absent in Community, where identity ships.
func TestMyAccount_IsNotAdminGated(t *testing.T) {
	s := serverAsUser()
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	rec := httptest.NewRecorder()
	s.MyAccount(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("a signed-in non-admin was refused; self-service must not need an operator capability")
	}
}

// TestMyAccount_NavEntryExistsAndIsNotAdminOnly — the config-assistant door
// shipped wired and unlinked, and nobody found it. This page must not repeat
// that, and its entry must be visible to ordinary users, not just admins.
func TestMyAccount_NavEntryExistsAndIsNotAdminOnly(t *testing.T) {
	var found *navDest
	for _, area := range navModel() {
		for i := range area.Dests {
			if area.Dests[i].Href == "/ui/account" {
				found = &area.Dests[i]
			}
		}
	}
	if found == nil {
		t.Fatal("no nav destination links to /ui/account — the panel would be reachable only by typing the URL")
	}
	if found.AdminOnly {
		t.Error("My account is self-service; marking it AdminOnly hides it from exactly the people it is for")
	}
}

// TestOperatorAccounts_NavEntryExists closes the same gap on the CE account
// page, which has been mounted and invisible since it shipped.
func TestOperatorAccounts_NavEntryExists(t *testing.T) {
	for _, area := range navModel() {
		for _, d := range area.Dests {
			if d.Href == "/ui/operator/accounts" {
				if !d.AdminOnly {
					t.Error("managing everyone's accounts needs the operator capability; the entry should be AdminOnly")
				}
				return
			}
		}
	}
	t.Fatal("no nav destination links to /ui/operator/accounts — CE operators cannot find account management")
}

// TestMyAccount_RendersTheLinkedIdentitiesPanel — the substance.
func TestMyAccount_RendersTheLinkedIdentitiesPanel(t *testing.T) {
	s := serverAsUser()
	req := httptest.NewRequest(http.MethodGet, "/account", nil)
	rec := httptest.NewRecorder()
	s.MyAccount(rec, req)
	body := rec.Body.String()
	for _, want := range []string{"Linked identities", "link code"} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("the page does not mention %q; body was:\n%s", want, truncateForTest(body))
		}
	}
}

func truncateForTest(s string) string {
	if len(s) > 600 {
		return s[:600] + "…"
	}
	return s
}

// TestLinkCode_NeverTravelsInAURL is a regression test for a real defect in
// the first cut of this page (found by automated security review,
// 2026-09-14): the POST handler put the freshly issued code into a redirect
// as ?notice=Link+code+ACDE2345…
//
// A secret in a URL is not "shown once". It lands in browser history, in the
// Referer header of every subsequent request the page makes, in proxy and
// server access logs, and on the screen of anyone glancing at the address
// bar. The design's §5.2 hygiene — the code exists exactly once, on the
// operator's screen — was defeated by the transport, not by the storage, and
// an earlier test asserting the code never reaches an AUDIT row passed
// happily while it leaked through the query string.
func TestLinkCode_NeverTravelsInAURL(t *testing.T) {
	s := serverAsUser()
	req := httptest.NewRequest(http.MethodPost, "/account/link-code", nil)
	rec := httptest.NewRecorder()
	s.MyAccountAction(rec, req)

	if loc := rec.Header().Get("Location"); loc != "" {
		// A redirect at all is suspicious here; a redirect carrying anything
		// code-shaped is the defect.
		if strings.Contains(loc, "notice=Link") || strings.Contains(loc, "code") {
			t.Fatalf("the link code (or its label) travelled in a redirect URL: %s", loc)
		}
	}
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the code rendered inline with 200, got %d: %s", rec.Code, truncateForTest(body))
	}
	// The page that displays a one-time secret must not be cached or leak a
	// referrer to anything it loads.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on a page showing a one-time code", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
}

// TestNonSecretNoticesMayStillRedirect — the fix is scoped to the secret. An
// unlink or a claim result carries nothing sensitive, so those keep the
// post-redirect-get that stops a refresh re-submitting the form.
func TestNonSecretNoticesMayStillRedirect(t *testing.T) {
	s := serverAsUser()
	req := httptest.NewRequest(http.MethodPost, "/account/unlink", strings.NewReader("channel=telegram&externalId=7"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.MyAccountAction(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("unlink status = %d, want 303 (post-redirect-get is still right for a non-secret result)", rec.Code)
	}
}

// TestMyAccount_EscapesAHostileExternalID — the panel renders the external id
// in two contexts: an HTML table cell, and a JS string literal inside the
// unlink form's confirm(). An email address is a legitimate external id
// (§5.0's mapping table makes email a speaker channel) and may contain a
// quote, which in the JS context is not a display bug but a break out of the
// literal.
//
// review-20260914-3c36 F10 reasoned that html/template's contextual escaper
// handles both and asked for confirmation rather than a change. This is the
// confirmation: reasoning that a framework escapes something is how the
// framework turns out to have been text/template.
func TestMyAccount_EscapesAHostileExternalID(t *testing.T) {
	const hostile = `a'); alert('xss`
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID:      "user_1",
		DisplayName: "Test User",
		Identities: []persistence.UserIdentityRef{
			{Channel: "email", ExternalID: hostile, Display: hostile},
		},
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{})),
	)

	rec := httptest.NewRecorder()
	s.MyAccount(rec, httptest.NewRequest(http.MethodGet, "/account", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The raw sequence must appear nowhere: neither the HTML cell nor the JS
	// literal may carry an unescaped quote-paren-semicolon.
	if strings.Contains(body, hostile) {
		t.Error("the external id is rendered raw — it breaks out of the confirm() string literal")
	}
	// And it must still be PRESENT, escaped: a page that dropped the row
	// would pass the check above while hiding a binding from its owner.
	if !strings.Contains(body, "alert") {
		t.Error("the identity is missing from the page entirely; the owner cannot see what resolves to them")
	}
}

// TestMyAccount_IssuedCodeCarriesTheObservationHandles — §5.5's redemption
// observation contract: redemption happens in chat, nothing pushes it back to
// the open browser session, so the panel polls for ITS code's id to leave the
// caller's outstanding list.
//
// The design forbids the obvious alternative — watching the identity list
// grow — because a second outstanding code, or an admin assigning a key
// during the window, both add an identity the poller did not cause. So the
// page needs a handle on its own code, and the handle must not be the code.
func TestMyAccount_IssuedCodeCarriesTheObservationHandles(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Test User",
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
			WithLinkCodes(newOperatorStubLinkCodes())),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/account/link-code", nil)
	s.MyAccountAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 with the code rendered inline", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="link-watch"`) {
		t.Fatal("the page carries no watcher; a person must reload by hand to see their own link land")
	}
	if !strings.Contains(body, "data-code-id=") || !strings.Contains(body, "data-expires-at=") {
		t.Error("the watcher has no code id or expiry; it cannot tell redemption from expiry")
	}
	// The handle must be a hash prefix, never the code: it travels in a
	// polling context, and "shown once" is a transport rule.
	if strings.Contains(body, `data-code-id=""`) {
		t.Error("the code id is empty")
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "data-code-id=") {
			continue
		}
		// Extract the attribute value and assert it is hex — a code from the
		// §5.2 alphabet contains letters the hex set does not.
		i := strings.Index(line, `data-code-id="`) + len(`data-code-id="`)
		val := line[i:]
		val = val[:strings.IndexByte(val, '"')]
		if len(val) != 16 {
			t.Errorf("code id = %q, want a 16-char hash prefix", val)
		}
		for _, r := range val {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Errorf("code id %q is not hex; it may be the code itself, which must never leave the page body", val)
				break
			}
		}
	}
}

// TestMyAccount_NamesOnlyChatCommandsThatExistHere — the Slack slash command
// is CONFIGURABLE (slack.slash_command), so a hardcoded "/vornik link" tells
// some operators to type something their workspace does not answer, and every
// operator with no Slack at all to use a channel they do not have.
//
// Telegram's /link is fixed in code, so it is always named.
func TestMyAccount_NamesOnlyChatCommandsThatExistHere(t *testing.T) {
	newPanel := func(slackCmds ...string) string {
		repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
			UserID: "user_1", DisplayName: "Test User",
		}}}
		opts := []ServerOption{
			WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
			WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
				WithLinkCodes(newOperatorStubLinkCodes())),
		}
		if len(slackCmds) > 0 {
			opts = append(opts, WithSlackLinkCommands(slackCmds))
		}
		rec := httptest.NewRecorder()
		NewServer(opts...).MyAccountAction(rec, httptest.NewRequest(http.MethodPost, "/account/link-code", nil))
		return rec.Body.String()
	}

	noSlack := newPanel()
	if !strings.Contains(noSlack, "/link ") {
		t.Error("Telegram's command is fixed in code and must always be named")
	}
	if strings.Contains(noSlack, "on Slack") {
		t.Error("a deployment with no Slack was told to redeem in Slack")
	}

	custom := newPanel("/vk")
	if !strings.Contains(custom, "/vk link ") {
		t.Errorf("the configured Slack command was not named; panel said:\n%s",
			custom[max(0, strings.Index(custom, "Telegram")-80):])
	}
	if strings.Contains(custom, "/vornik link") {
		t.Error("the panel hardcoded /vornik over the deployment's configured command")
	}

	// EVERY command, not the first. An operator running two Slack apps was
	// told to use whichever project happened to load first, which is wrong
	// for everyone in the other workspace.
	both := newPanel("/t800", "/holly")
	for _, want := range []string{"/t800 link ", "/holly link "} {
		if !strings.Contains(both, want) {
			t.Errorf("a deployment answering two Slack commands did not name %q; "+
				"whoever uses the other app is told to type something that does not work", want)
		}
	}

	// And the code is instance-scoped: another instance's chat app cannot
	// redeem it, which is the failure an operator hit and could not explain.
	if !strings.Contains(both, "another instance") {
		t.Error("the panel does not say the code belongs to this instance")
	}
}

// TestMyAccount_WatcherTreatsAnAbsentCodeListAsTransient — the endpoint omits
// outstandingCodes entirely when the link-code store errors, deliberately, so
// a failing store cannot break the identity list. The watcher read a missing
// key as an empty list, which made a store blip indistinguishable from a
// redemption: the page announced "Redeemed" for a code nobody had used
// (review-20260915-c6a0 F1).
//
// Asserted on the rendered script rather than by driving a browser: the guard
// is one line and its absence is what shipped, so the assertion that matters
// is that the distinction is made at all.
func TestMyAccount_WatcherTreatsAnAbsentCodeListAsTransient(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Test User",
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
			WithLinkCodes(newOperatorStubLinkCodes())),
	)
	rec := httptest.NewRecorder()
	s.MyAccountAction(rec, httptest.NewRequest(http.MethodPost, "/account/link-code", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "Array.isArray(body.outstandingCodes)") {
		t.Error("the watcher does not distinguish an ABSENT code list from an empty one; " +
			"a link-code store blip will be reported to the user as a redemption")
	}
	if strings.Contains(body, "body.outstandingCodes || []") {
		t.Error("the watcher still coerces a missing list to empty")
	}
}

// TestMyAccount_WatcherDoesNotReSubmitTheIssuePost — the panel that shows a
// freshly issued code is rendered from a POST to the issue endpoint (inline,
// so the code never enters a URL). location.reload() therefore re-submits
// that POST and mints a fresh code the person never asked for — once every
// time a redemption is observed (review-20260915-80fa F3).
//
// It must navigate to the GET page instead. And it must not call the outcome
// "redeemed": a code also leaves the outstanding list when the SERVER expires
// it, and the server's clock is not this browser's, so a client running
// behind would announce a redemption for a code that merely expired.
func TestMyAccount_WatcherDoesNotReSubmitTheIssuePost(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Test User",
	}}}
	s := NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
			WithLinkCodes(newOperatorStubLinkCodes())),
	)
	rec := httptest.NewRecorder()
	s.MyAccountAction(rec, httptest.NewRequest(http.MethodPost, "/account/link-code", nil))
	body := rec.Body.String()

	if strings.Contains(body, "location.reload()") {
		t.Error("the watcher reloads a page rendered from a POST; every observed redemption re-issues a code")
	}
	if !strings.Contains(body, "location.assign('/ui/account')") {
		t.Error("the watcher does not navigate to the GET page")
	}
	// The outcome must not be asserted as a redemption the client cannot know.
	if strings.Contains(body, "Redeemed —") {
		t.Error("the watcher announces a redemption for what is only 'no longer outstanding'")
	}
	// A dead session must end the loop rather than wait out the TTL and then
	// tell the person to act on a page they can no longer reach.
	// Both statuses that cannot recover on their own must stop the loop. The
	// 403 branch shipped with no test, so narrowing the condition back to 401
	// alone would have been silent (review-20260915-6d99 F3).
	for _, status := range []string{"401", "403"} {
		if !strings.Contains(body, "res.status === "+status) {
			t.Errorf("the watcher treats a %s as transient; it cannot recover on its own, so the "+
				"person waits out the TTL and is then told to act on a page they cannot reach", status)
		}
	}
}

// TestMyAccount_DestinationPageDistinguishesRedeemedFromExpired — the watcher
// stopped claiming "Redeemed" (it cannot know: a code leaves the outstanding
// list on redemption OR on server-side expiry) and instead navigates to
// /ui/account, on the stated grounds that "a redemption puts the binding in
// the identity list".
//
// That claim is now the whole resolution, and nothing proved it
// (review-20260915-ba13 F2). Deferring an ambiguity to a page that cannot
// resolve it is not a fix, it is a relocation.
func TestMyAccount_DestinationPageDistinguishesRedeemedFromExpired(t *testing.T) {
	page := func(identities []persistence.UserIdentityRef) string {
		repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
			UserID: "user_1", DisplayName: "Test User", Identities: identities,
		}}}
		s := NewServer(
			WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
			WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{})),
		)
		rec := httptest.NewRecorder()
		s.MyAccount(rec, httptest.NewRequest(http.MethodGet, "/account", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	// The display value is what a PERSON reads. Asserting the channel string
	// alone would pass on a data- attribute or a CSS class — present in the
	// HTML and invisible on the screen — which is bytes, not visibility, and
	// visibility is what this test claims (review-20260915-6d99 F1).
	const displayed = "speaker-42-display"

	// Expired: the code is gone and nothing was bound. This arm is the
	// control: without it the redeemed arm could pass off static page chrome.
	expired := page(nil)
	if strings.Contains(expired, displayed) {
		t.Error("the page shows a binding for an account that has none")
	}

	// Redeemed: the binding the redemption wrote is on the page, in the text
	// a person reads.
	redeemed := page([]persistence.UserIdentityRef{
		{Channel: "telegram", ExternalID: "42", Display: displayed},
	})
	if !strings.Contains(redeemed, displayed) {
		t.Error("a redeemed binding is not rendered on the page the watcher navigates to; " +
			"the person cannot tell redemption from expiry, which is the distinction " +
			"the watcher now defers to this page")
	}
	if !strings.Contains(redeemed, "telegram") {
		t.Error("the rendered binding does not name its channel, so a person with several " +
			"outstanding links cannot tell WHICH one landed")
	}
}

// TestMyAccount_InstructionIsPlainAndSaysTheSlackCommandIsDeploymentSpecific
// covers the two halves of the 2026-09-15 report, which are one defect seen
// from both ends.
//
// The instruction was rendered inside a <code> element. Copying it into
// Slack's rich-text composer carried that formatting along, and Slack
// serialised it back into the payload as literal backticks — so a freshly
// minted code was refused until the operator stripped the formatting by hand.
// The redeemers now normalise markup away (authz.hashLinkCode and
// slack.linkCodeFromSlashText), but the page must also stop PRODUCING the
// problem: a copy that is already plain is the fix a person experiences.
//
// The second half: the panel names this deployment's configured Slack command
// and nothing said it was configured, so "/t800" read as a Vornik-wide
// instruction. An operator with a second Slack app answering a different
// command has no way to tell which of the two the line refers to.
func TestMyAccount_InstructionIsPlainAndSaysTheSlackCommandIsDeploymentSpecific(t *testing.T) {
	repo := &stubUsersIdentityRepo{users: []persistence.UserAdminView{{
		UserID: "user_1", DisplayName: "Test User",
	}}}
	rec := httptest.NewRecorder()
	NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
			WithLinkCodes(newOperatorStubLinkCodes())),
		WithSlackLinkCommands([]string{"/t800"}),
	).MyAccountAction(rec, httptest.NewRequest(http.MethodPost, "/account/link-code", nil))
	body := rec.Body.String()

	// No <code> element anywhere: it is what made the copy carry formatting,
	// and it exists on this page only to style the instruction.
	if strings.Contains(body, "<code") {
		t.Error("the link instruction is inside a <code> element again; copying it into a " +
			"rich-text chat composer carries the formatting into the payload")
	}
	// The instruction must still BE there — a test that only forbids <code>
	// passes just as well on a page that stopped telling anyone what to type.
	if !strings.Contains(body, "/t800 link ") {
		t.Fatalf("the Slack instruction is gone entirely; panel said:\n%s", body)
	}
	if !strings.Contains(body, "configured on this deployment") {
		t.Error("the panel names a Slack command without saying it is this deployment's " +
			"configured one; an operator running a second Slack app cannot tell which it means")
	}

	// The negative branch. A deployment with no Slack must show neither the
	// instruction nor the per-app sentence — a qualification about a command
	// that is not named is noise, and naming one that is not wired is the
	// defect this whole surface keeps producing (review-20260915-3d5b,
	// finding 5: the present branch was tested and the absent one was not).
	rec = httptest.NewRecorder()
	NewServer(
		WithSessionUserResolver(func(*http.Request) string { return "user_1" }),
		WithAccountsService(authz.NewAccounts(repo, &stubAdminAuditRepo{}).
			WithLinkCodes(newOperatorStubLinkCodes())),
	).MyAccountAction(rec, httptest.NewRequest(http.MethodPost, "/account/link-code", nil))
	noSlack := rec.Body.String()

	// Asserted on "Slack", not on a phrase. The qualification's wording is
	// prose and will be reworded; a negative test pinned to prose stops
	// biting the moment someone edits it, and then silently passes on the
	// very regression it was added for (review-20260915-beff, suggestion 4).
	// The word "Slack" survives any rewording that still mentions Slack, and
	// a page with no Slack configured has no business mentioning it at all.
	//
	// CASE MATTERS here, and not by accident: a bound Slack identity renders
	// in the identities table as the raw channel string "slack", lower case,
	// while the instruction says "Slack". Matching the capitalised form is
	// what keeps this assertion about the INSTRUCTION rather than about
	// whether the person happens to have a Slack identity already. If the
	// identities table is ever changed to title-case its channel column, this
	// test will fail and the fix is to narrow it to the instruction block,
	// not to drop it.
	if strings.Contains(noSlack, "Slack") {
		t.Errorf("a deployment with no Slack configured mentions Slack; it was told to "+
			"redeem on a channel it does not have. Panel said:\n%s", noSlack)
	}
	if !strings.Contains(noSlack, "/link ") {
		t.Error("Telegram's command is fixed in code and must still be named")
	}
}
