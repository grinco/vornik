package telegram

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The §5.2 redemption half of `/link`. §5.0 settled a command collision
// rather than inventing a verb: one `/link`, two code TYPES, disambiguated AT
// REDEMPTION — link_codes first, the in-memory operator-profile OTP second.
//
// Only the second half was wired. Codes could be issued from the UI, the CLI
// and the API, and redeemed nowhere: the feature was a one-way street, and
// the chat answer to a perfectly good code was "that code isn't valid".

// stubLinker records what it was asked to bind and answers with whatever the
// test wants. The recorded (channel, externalID) is the point of most of
// these tests — binding the wrong key writes a row that can never authorize.
type stubLinker struct {
	gotChannel, gotExternal, gotCode string
	user                             *persistence.User
	err                              error
	calls                            int
}

func (s *stubLinker) RedeemLinkCode(_ context.Context, code, channel, externalID, _ string) (*persistence.User, error) {
	s.calls++
	s.gotCode, s.gotChannel, s.gotExternal = code, channel, externalID
	return s.user, s.err
}

func linkTestBot(t *testing.T, rec *telegramRecorder, linker AccountLinker) *Bot {
	t.Helper()
	bot, err := NewBot(BotConfig{Token: "tok", AllowUnlistedUsers: true},
		chat.NewClient("https://example.com", "k", "m"),
		WithHTTPClient(rec.server.Client()))
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = rec.server.URL
	bot.logger = zerolog.Nop()
	if linker != nil {
		bot.SetAccountLinker(linker)
	}
	return bot
}

// TestLink_RedeemsAnAccountCodeAgainstTheUserID — the binding MUST key on the
// Telegram user id, not the chat id. They are equal in a DM and differ in a
// group, and the resolver looks up the user id, so a chat-id binding is a row
// that authorizes nobody and fails silently for exactly the senders who are
// hardest to debug.
func TestLink_RedeemsAnAccountCodeAgainstTheUserID(t *testing.T) {
	rec := newTelegramRecorder(t)
	linker := &stubLinker{user: &persistence.User{ID: "user_1", DisplayName: "Vadim"}}
	bot := linkTestBot(t, rec, linker)

	const groupChat, senderUser = int64(-1001234), int64(42)
	if err := bot.handleLinkCommand(context.Background(), groupChat, senderUser, []string{"/link", "ACDE2345"}); err != nil {
		t.Fatalf("handleLinkCommand: %v", err)
	}

	if linker.gotChannel != "telegram" || linker.gotExternal != "42" {
		t.Errorf("bound (%q, %q), want (telegram, 42) — the USER id, not the chat id",
			linker.gotChannel, linker.gotExternal)
	}
	if linker.gotCode != "ACDE2345" {
		t.Errorf("redeemed code %q, want the one typed", linker.gotCode)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Vadim") {
		t.Errorf("reply = %+v, want one message naming the account joined", msgs)
	}
}

// TestLink_FallsThroughToTheOTPStore — §5.0's disambiguation. A code that is
// not an account link code must reach the operator-profile OTP path, NOT be
// answered by the account path. Asserted through the reply text, because the
// user-visible failure of getting this wrong is a working OTP being refused.
func TestLink_FallsThroughToTheOTPStore(t *testing.T) {
	rec := newTelegramRecorder(t)
	linker := &stubLinker{err: authz.ErrLinkCodeInvalid}
	bot := linkTestBot(t, rec, linker)

	// No operator-profile repos wired, so the OTP path reports that — which
	// is proof the fall-through happened rather than the account path
	// answering.
	if err := bot.handleLinkCommand(context.Background(), 42, 42, []string{"/link", "XYZ"}); err != nil {
		t.Fatalf("handleLinkCommand: %v", err)
	}
	if linker.calls != 1 {
		t.Errorf("the account store was consulted %d times, want exactly 1 (first, per §5.0)", linker.calls)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}
	if strings.Contains(msgs[0].Text, "Linked to") {
		t.Error("an invalid account code was reported as a successful link")
	}
	// And it must not blame the deployment for a mistyped code when the
	// account half IS wired.
	if strings.Contains(msgs[0].Text, "not configured") {
		t.Errorf("a wired deployment answered %q — that tells the user to go fix the daemon over a typo", msgs[0].Text)
	}
}

// TestLink_ReportsAFaultAsAFaultAndKeepsTheCode — redemption stopped
// flattening transport faults into "invalid" so this path could tell the
// difference. Sending someone to generate a new code during an outage burns
// their good one and does not help.
func TestLink_ReportsAFaultAsAFaultAndKeepsTheCode(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, &stubLinker{err: context.DeadlineExceeded})

	if err := bot.handleLinkCommand(context.Background(), 42, 42, []string{"/link", "ACDE2345"}); err != nil {
		t.Fatalf("handleLinkCommand: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}
	if !strings.Contains(msgs[0].Text, "has not been used") {
		t.Errorf("reply = %q, want one that says the code survives the fault", msgs[0].Text)
	}
}

// TestLink_WithoutALinkerIsUnchanged — every deployment that has not wired
// the identity core must see the pre-Phase-4 `/link` exactly.
func TestLink_WithoutALinkerIsUnchanged(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, nil)

	if err := bot.handleLinkCommand(context.Background(), 42, 42, []string{"/link", "ACDE2345"}); err != nil {
		t.Fatalf("handleLinkCommand: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "not configured") {
		t.Errorf("reply = %+v, want the pre-Phase-4 unconfigured message", msgs)
	}
}

// TestLink_NoArgNeverConsultsTheAccountStore — bare `/link` issues an OTP for
// the chat→chat flow. There is no code to redeem, and consulting the account
// store with an empty one would be a lookup for nothing.
func TestLink_NoArgNeverConsultsTheAccountStore(t *testing.T) {
	rec := newTelegramRecorder(t)
	linker := &stubLinker{}
	bot := linkTestBot(t, rec, linker)

	if err := bot.handleLinkCommand(context.Background(), 42, 42, []string{"/link"}); err != nil {
		t.Fatalf("handleLinkCommand: %v", err)
	}
	if linker.calls != 0 {
		t.Errorf("the account store was consulted %d times for a bare /link, want 0", linker.calls)
	}
}

// TestUnauthorizedMessage_NamesTheRemedyOnlyWhenItExists — §5.7's ledger
// requires the refusal to name the `/link` remedy. A dead-end "not
// authorized" is the control that stops the next person looking, and during
// the §5.3 migration window the likeliest person to see it is someone who has
// an account and has never linked this chat.
//
// But only when redemption is wired: naming a command that answers "not
// configured" is the documented-behaviour-nothing-implements failure in
// miniature.
func TestUnauthorizedMessage_NamesTheRemedyOnlyWhenItExists(t *testing.T) {
	rec := newTelegramRecorder(t)

	wired := linkTestBot(t, rec, &stubLinker{})
	if got := wired.unauthorizedMessage(); !strings.Contains(got, "/link") {
		t.Errorf("wired refusal = %q, want it to name the /link remedy", got)
	}

	bare := linkTestBot(t, rec, nil)
	if got := bare.unauthorizedMessage(); strings.Contains(got, "/link") {
		t.Errorf("unwired refusal = %q, must not name a command that cannot work here", got)
	}
	if !strings.Contains(bare.unauthorizedMessage(), "not authorized") {
		t.Error("the refusal stopped saying it was a refusal")
	}
}

// TestLink_IsReachableByAnUNAUTHORIZEDSpeaker — the defect this test exists
// for made the entire redemption path dead on arrival.
//
// HandleMessage refused unauthorized senders before reaching the command
// switch, and `/link` lived in that switch. A speaker who has not linked yet
// is BY DEFINITION not authorized — that is what they are trying to fix — so
// every issue surface worked, redemption was implemented, and nobody who
// needed it could reach it. §5.1 names this as the single exception to
// deny-by-default: "`/link <code>` is processed from unknown senders — that
// is how a new binding is created."
func TestLink_IsReachableByAnUNAUTHORIZEDSpeaker(t *testing.T) {
	rec := newTelegramRecorder(t)
	linker := &stubLinker{user: &persistence.User{ID: "user_1", DisplayName: "Vadim"}}
	bot := linkTestBot(t, rec, linker)
	// An empty allowlist with allow-unlisted OFF: nobody is authorized, which
	// is the state of every deployment the moment identity ships.
	bot.config.AllowUnlistedUsers = false
	bot.config.AllowedUsers = nil
	if bot.IsAllowed(42) {
		t.Fatal("fixture error: this speaker must be unauthorized for the test to mean anything")
	}

	err := bot.HandleMessage(context.Background(), &Message{ChatID: 42, UserID: 42, Text: "/link ACDE2345"})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if linker.calls != 1 {
		t.Fatalf("the account store was consulted %d times; an unauthorized speaker could not redeem, "+
			"which is the only population redemption exists for", linker.calls)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Linked to") {
		t.Errorf("reply = %+v, want the link confirmation", msgs)
	}
}

// TestLink_BareLinkStaysBehindTheGate — the exception is for the WITH-CODE
// form only. Bare `/link` MINTS an OTP, and an unknown sender who can mint is
// a spam surface that creates no binding to anything.
func TestLink_BareLinkStaysBehindTheGate(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, &stubLinker{})
	bot.config.AllowUnlistedUsers = false
	bot.config.AllowedUsers = nil

	if err := bot.HandleMessage(context.Background(), &Message{ChatID: 42, UserID: 42, Text: "/link"}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}
	if strings.Contains(msgs[0].Text, "Link code:") {
		t.Error("an unauthorized speaker minted an OTP; only the with-code form is exempt from the gate")
	}
	if !strings.Contains(msgs[0].Text, "not authorized") {
		t.Errorf("reply = %q, want the refusal", msgs[0].Text)
	}
}

// TestLink_AttemptsAreBounded — redemption is reachable before
// authorization, so it is a surface anyone who can message the bot can reach.
// The code space makes a blind search hopeless, but that is an arithmetic
// argument and this is a mechanism.
func TestLink_AttemptsAreBounded(t *testing.T) {
	rec := newTelegramRecorder(t)
	linker := &stubLinker{err: authz.ErrLinkCodeInvalid}
	bot := linkTestBot(t, rec, linker)

	for i := 0; i < 40; i++ {
		if err := bot.handleLinkCommand(context.Background(), 42, 42, []string{"/link", "GUESS" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if linker.calls >= 40 {
		t.Errorf("the account store was consulted %d times in 40 attempts; the guessing surface is unbounded", linker.calls)
	}
	last := rec.snapshot()
	if !strings.Contains(last[len(last)-1].Text, "Too many link attempts") {
		t.Errorf("final reply = %q, want the rate-limit refusal", last[len(last)-1].Text)
	}
}

// TestIsLinkCommand_HandlesTheBotSuffix — Telegram appends @botname when
// several bots share a group, and a group is exactly where linking matters
// (it is where the chat id and the user id differ). Without this the person
// is refused outright rather than merely ignored.
func TestIsLinkCommand_HandlesTheBotSuffix(t *testing.T) {
	for _, in := range []string{"/link", "/link@vornikbot", "/link@anything"} {
		if !isLinkCommand(in) {
			t.Errorf("isLinkCommand(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"/links", "/linkcode", "link", "/unlink", "/li@nk", ""} {
		if isLinkCommand(in) {
			t.Errorf("isLinkCommand(%q) = true, want false", in)
		}
	}
}

// TestLink_OTPCodeIsAlsoReachableUnauthenticated — §5.1's exception covers
// BOTH code types: "/link <code> is processed from unknown senders". The
// account half was asserted; the OTP half was routed and unasserted
// (review-20260915-8284). If the OTP path ever re-applied the authorization
// check, the pre-gate hoist would be silently defeated for half the feature
// and no test would notice.
func TestLink_OTPCodeIsAlsoReachableUnauthenticated(t *testing.T) {
	rec := newTelegramRecorder(t)
	// The account store says "not mine", so the fall-through to the OTP path
	// is what answers — which is §5.0's disambiguation working.
	bot := linkTestBot(t, rec, &stubLinker{err: authz.ErrLinkCodeInvalid})
	bot.config.AllowUnlistedUsers = false
	bot.config.AllowedUsers = nil
	bot.operatorProfiles = &noopProfiles{}
	bot.operatorIdentityLinks = &noopLinks{}
	if bot.IsAllowed(42) {
		t.Fatal("fixture error: this speaker must be unauthorized")
	}

	if err := bot.HandleMessage(context.Background(), &Message{ChatID: 42, UserID: 42, Text: "/link OTP12345"}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}
	// The OTP path answered — its own vocabulary, not the authorization
	// refusal and not the account path's.
	if strings.Contains(msgs[0].Text, "not authorized") {
		t.Errorf("the OTP half was refused by the authorization gate: %q", msgs[0].Text)
	}
	if strings.Contains(msgs[0].Text, "not configured") {
		t.Errorf("the OTP path reported itself unwired though both repositories are set: %q", msgs[0].Text)
	}
}

// TestLink_UnwiredDeploymentDoesNotDiscloseItselfToStrangers — the hoist
// routes /link <code> before the authorization gate, so on a deployment with
// NOTHING wired an unauthenticated stranger was told about the operator-
// profile wiring state. §5.1's exception asks for redemption to be reachable,
// not for the deployment to introduce itself (review-20260915-8284).
func TestLink_UnwiredDeploymentDoesNotDiscloseItselfToStrangers(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, nil) // no account linker, no profile repos
	bot.config.AllowUnlistedUsers = false
	bot.config.AllowedUsers = nil

	if err := bot.HandleMessage(context.Background(), &Message{ChatID: 42, UserID: 42, Text: "/link ACDE2345"}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 {
		t.Fatalf("messages = %+v, want 1", msgs)
	}
	if strings.Contains(msgs[0].Text, "operator-profile repositories") {
		t.Errorf("an unauthenticated stranger was told the deployment's wiring state: %q", msgs[0].Text)
	}
	if !strings.Contains(msgs[0].Text, "not authorized") {
		t.Errorf("reply = %q, want the refusal they would have got before the hoist", msgs[0].Text)
	}
}

// TestLink_BareLinkWithBotSuffixStillMints — in a group Telegram appends
// @botname, and bare /link@botname fell through the command switch to the
// dispatcher: redeem worked with the suffix and mint did not, for an
// authorized user, in exactly the setting linking matters most.
func TestLink_BareLinkWithBotSuffixStillMints(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := linkTestBot(t, rec, &stubLinker{})
	bot.operatorProfiles = &noopProfiles{}
	bot.operatorIdentityLinks = &noopLinks{}
	bot.config.AllowUnlistedUsers = true

	if err := bot.HandleMessage(context.Background(), &Message{ChatID: 42, UserID: 42, Text: "/link@vornikbot"}); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	msgs := rec.snapshot()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Link code:") {
		t.Errorf("reply = %+v, want an OTP minted for the suffixed bare form", msgs)
	}
}

// noopProfiles and noopLinks satisfy the operator-profile repositories so the
// OTP half of /link is WIRED in these tests. They store nothing: these tests
// assert which path answered and with what vocabulary, never what it stored —
// the storage contract is pinned in internal/persistence.
type noopProfiles struct {
	persistence.OperatorProfileRepository
}

func (noopProfiles) Get(context.Context, string) (*persistence.OperatorProfile, error) {
	return nil, persistence.ErrNotFound
}
func (noopProfiles) Upsert(context.Context, *persistence.OperatorProfile) error { return nil }

type noopLinks struct {
	persistence.OperatorIdentityLinkRepository
}

func (noopLinks) Get(context.Context, string) (*persistence.OperatorIdentityLink, error) {
	return nil, persistence.ErrNotFound
}
func (noopLinks) ListForOperator(context.Context, string) ([]*persistence.OperatorIdentityLink, error) {
	return nil, nil
}
func (noopLinks) Upsert(context.Context, *persistence.OperatorIdentityLink) error { return nil }
func (noopLinks) Delete(context.Context, string) error                            { return nil }
func (noopLinks) DeleteAllForOperator(context.Context, string) error              { return nil }

// TestNoopRepos_SatisfyTheMissContract holds this file's doubles to the same
// miss contract the real repositories obey. A double that answered a miss
// differently would make the "which path answered" assertions above rest on a
// lookup the daemon never performs.
func TestNoopRepos_SatisfyTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "OperatorProfileRepository.Get", noopProfiles{}.Get)
	repotest.AssertMissRepo(t, "OperatorIdentityLinkRepository.Get", noopLinks{}.Get)
}

// TestIsLinkCommand_ToleratesDecorationButKeepsTheSlash — the Slack twin's
// defect in Telegram's shape (review-20260915-3d5b, finding 1 generalised).
//
// A command word that declines does not merely fail to link: the text
// continues to the dispatcher, so a message carrying a live one-time code
// reaches a model and a transcript with nothing reporting it.
//
// The second loop is the half that matters more. Telegram, unlike Slack, has
// no leading-slash-free command form, so relaxing the slash would turn every
// ordinary message beginning "link …" into a redemption attempt — the exact
// prompt-swallowing §5.2 calls the worse failure.
func TestIsLinkCommand_ToleratesDecorationButKeepsTheSlash(t *testing.T) {
	for _, in := range []string{
		"`/link",            // markdown code span
		"`/link`",           //
		"*/link*",           // bold
		"_/link_",           // italic
		"“/link”",           // smart quotes
		"(/link)",           // parenthesised in prose
		"`/link@vornikbot`", // decorated AND group-suffixed
	} {
		if !isLinkCommand(in) {
			t.Errorf("isLinkCommand(%q) = false; the message carries on to the dispatcher "+
				"with a live code in it", in)
		}
	}

	for _, in := range []string{
		"link",     // no slash: on Telegram a bare word is not a command
		"link@bot", //
		"/linkx",   // a different command
		"/unlink",  // contains link, is not it
		"//link",   // not a command word
		"",         //
	} {
		if isLinkCommand(in) {
			t.Errorf("isLinkCommand(%q) = true; the recogniser has stopped requiring a real command word "+
				"and will swallow ordinary messages", in)
		}
	}
}
