package slack

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/persistence"
)

// §5.2 redemption on Slack. The design says a code is redeemed on "any chat
// channel"; Telegram had it and Slack did not, which made that sentence false
// on half the channels it names.

type stubSlackLinker struct {
	gotChannel, gotExternal, gotCode string
	user                             *persistence.User
	err                              error
	calls                            int
}

func (s *stubSlackLinker) RedeemLinkCode(_ context.Context, code, channel, externalID, _ string) (*persistence.User, error) {
	s.calls++
	s.gotCode, s.gotChannel, s.gotExternal = code, channel, externalID
	return s.user, s.err
}

func linkChannel(l AccountLinker) *Channel {
	c := &Channel{logger: zerolog.Nop()}
	c.SetAccountLinker(l)
	return c
}

// TestSlackLink_RedeemsAgainstTheSlackUserID — the binding must key on the
// Slack user id, which is exactly what resolveSpeakerForInstallation and the
// resolver use. Agreement by construction, not by coincidence.
func TestSlackLink_RedeemsAgainstTheSlackUserID(t *testing.T) {
	linker := &stubSlackLinker{user: &persistence.User{ID: "user_1", DisplayName: "Vadim"}}
	c := linkChannel(linker)
	rec := httptest.NewRecorder()

	if !c.tryAccountLinkCode(context.Background(), rec, "U123", "link ACDE2345") {
		t.Fatal("a redemption was not recognised")
	}
	if linker.gotChannel != "slack" || linker.gotExternal != "U123" {
		t.Errorf("bound (%q, %q), want (slack, U123)", linker.gotChannel, linker.gotExternal)
	}
	if linker.gotCode != "ACDE2345" {
		t.Errorf("redeemed %q, want the code typed", linker.gotCode)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Vadim") {
		t.Errorf("reply = %q, want the account named", body)
	}
	// A link confirmation names an account: it must be ephemeral, not posted
	// to the channel everyone can read.
	if !strings.Contains(body, "ephemeral") {
		t.Errorf("reply = %q, want response_type ephemeral", body)
	}
}

// TestSlackLink_OrdinaryPromptsReachTheDispatcher — the recogniser is strict
// on purpose. Swallowing a prompt into an authorization command is much worse
// than a missed shortcut: the user's question would silently vanish.
func TestSlackLink_OrdinaryPromptsReachTheDispatcher(t *testing.T) {
	for _, text := range []string{
		"link the two designs and summarise", // begins with the word
		"link",                               // no code
		"link ACDE2345 extra",                // more than one argument
		"please link ACDE2345",               // not the first word
		"summarise the backlog",
		"",
	} {
		linker := &stubSlackLinker{}
		c := linkChannel(linker)
		if c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", text) {
			t.Errorf("%q was taken as a link-code redemption; it must reach the dispatcher", text)
		}
		if linker.calls != 0 {
			t.Errorf("%q consulted the account store %d times, want 0", text, linker.calls)
		}
	}
}

// TestSlackLink_WithoutALinkerIsUnchanged — a deployment with no identity
// core must see `link <code>` as an ordinary prompt, exactly as before.
func TestSlackLink_WithoutALinkerIsUnchanged(t *testing.T) {
	c := &Channel{logger: zerolog.Nop()}
	if c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", "link ACDE2345") {
		t.Error("an unwired deployment answered a redemption it cannot perform")
	}
}

// TestSlackLink_FaultIsNotReportedAsABadCode — redemption stopped flattening
// transport faults into "invalid" so this reply could tell the difference.
func TestSlackLink_FaultIsNotReportedAsABadCode(t *testing.T) {
	c := linkChannel(&stubSlackLinker{err: context.DeadlineExceeded})
	rec := httptest.NewRecorder()
	if !c.tryAccountLinkCode(context.Background(), rec, "U1", "link ACDE2345") {
		t.Fatal("a redemption was not recognised")
	}
	if !strings.Contains(rec.Body.String(), "has not been used") {
		t.Errorf("reply = %q, want one that says the code survives the fault", rec.Body.String())
	}
}

// TestSlackLink_InvalidCodeSaysSo — and does not blame the deployment.
func TestSlackLink_InvalidCodeSaysSo(t *testing.T) {
	c := linkChannel(&stubSlackLinker{err: authz.ErrLinkCodeInvalid})
	rec := httptest.NewRecorder()
	if !c.tryAccountLinkCode(context.Background(), rec, "U1", "link BADCODE1") {
		t.Fatal("a redemption was not recognised")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "isn't valid") || strings.Contains(body, "configured") {
		t.Errorf("reply = %q, want a mistyped-code message rather than a deployment complaint", body)
	}
}

// TestSlackLink_IsReachableByAnUNALLOWLISTEDSpeaker — the Telegram twin's
// defect, in Slack's shape: slashCommandActorAllowed ran before the text was
// even read, so a speaker who is not on the sender allowlist could not
// redeem — and not being on the allowlist is exactly the state linking
// exists to fix. §5.1 names redemption as the single exception to
// deny-by-default.
//
// Asserted through handleSlashCommandWebhook rather than tryAccountLinkCode,
// because the defect was the ORDER of two calls, and a unit test on the inner
// one cannot see it.
func TestSlackLink_IsReachableByAnUNALLOWLISTEDSpeaker(t *testing.T) {
	linker := &stubSlackLinker{user: &persistence.User{ID: "user_1", DisplayName: "Vadim"}}
	inst := &installation{
		teamID:  "T1",
		senders: map[string]struct{}{"U-somebody-else": {}}, // U123 is NOT allowlisted
	}
	c := &Channel{
		logger:            zerolog.Nop(),
		installations:     []*installation{inst},
		installationsByID: map[string]*installation{"T1": inst},
		cfg:               Config{SlashCommand: "/vornik"},
	}
	c.SetAccountLinker(linker)

	form := url.Values{
		"team_id":    {"T1"},
		"command":    {"/vornik"},
		"channel_id": {"C1"},
		"user_id":    {"U123"},
		"text":       {"link ACDE2345"},
		"trigger_id": {"trig-1"},
	}.Encode()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.handleSlashCommandWebhook(rec, req, []byte(form), time.Now())

	if linker.calls != 1 {
		t.Fatalf("the account store was consulted %d times; an un-allowlisted speaker could not redeem, "+
			"which is the only population redemption exists for", linker.calls)
	}
	if !strings.Contains(rec.Body.String(), "Vadim") {
		t.Errorf("reply = %q, want the link confirmation", rec.Body.String())
	}
}

// TestSlackLink_OrdinaryPromptsStillNeedTheAllowlist — the exception is
// redemption and nothing else. An un-allowlisted speaker's ordinary prompt
// must still be refused, or hoisting the check would have opened the channel.
func TestSlackLink_OrdinaryPromptsStillNeedTheAllowlist(t *testing.T) {
	linker := &stubSlackLinker{}
	inst := &installation{teamID: "T1", senders: map[string]struct{}{"U-else": {}}}
	c := &Channel{
		logger:            zerolog.Nop(),
		installations:     []*installation{inst},
		installationsByID: map[string]*installation{"T1": inst},
		cfg:               Config{SlashCommand: "/vornik"},
	}
	c.SetAccountLinker(linker)

	form := url.Values{
		"team_id": {"T1"}, "command": {"/vornik"}, "channel_id": {"C1"},
		"user_id": {"U123"}, "text": {"summarise the backlog"}, "trigger_id": {"trig-2"},
	}.Encode()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/slack/commands", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.handleSlashCommandWebhook(rec, req, []byte(form), time.Now())

	if linker.calls != 0 {
		t.Errorf("an ordinary prompt reached the account store %d times", linker.calls)
	}
	if body := rec.Body.String(); strings.Contains(body, "Vadim") || strings.Contains(body, "Linked") {
		t.Errorf("an un-allowlisted prompt was answered: %q", body)
	}
}

// TestSlackLink_RecognisesAPastedFormattedCommand — 2026-09-15. The panel
// rendered the instruction inside a <code> element; pasting it into Slack's
// rich-text composer kept the formatting, and Slack serialised it back as
// literal backticks in the slash payload.
//
// The keyword half matters more than the code half. A decorated CODE
// ("link `ACDE2345`") merely failed to redeem, which the operator saw. A
// decorated KEYWORD ("`link ACDE2345`") did not match the recogniser at all,
// so the text fell through to the dispatcher — sending a live one-time
// credential to a model and into a conversation transcript. That is the
// failure worth pinning.
func TestSlackLink_RecognisesAPastedFormattedCommand(t *testing.T) {
	for _, text := range []string{
		"`link ACDE2345`",   // whole command in one code span — reached the model
		"link `ACDE2345`",   // only the code formatted
		"*link ACDE2345*",   // bold
		"_link ACDE2345_",   // italic
		"~link ACDE2345~",   // strikethrough
		"  link   ACDE2345", // stray whitespace from a wrapped copy
		// Decorations a CHARACTER BLACKLIST misses. The first fix trimmed a
		// fixed cutset of markup characters; these three declined and went to
		// the dispatcher with a live code in them (review-20260915-3d5b,
		// finding 8). They are the reason the keyword is now reduced to its
		// letters rather than stripped of a list.
		"“link ACDE2345”", // smart double quotes
		"‘link ACDE2345’", // smart single quotes
		"link, ACDE2345",  // a comma from prose wrapping
	} {
		linker := &stubSlackLinker{user: &persistence.User{DisplayName: "Ada"}}
		c := linkChannel(linker)
		if !c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", text) {
			t.Errorf("%q was not recognised as a redemption; a one-time code would reach the dispatcher", text)
			continue
		}
		if linker.calls != 1 {
			t.Errorf("%q consulted the account store %d times, want 1", text, linker.calls)
		}
	}
}

// TestSlackLink_NearMissesAreStillPrompts — the keyword normalisation must not
// become "anything containing the letters of link". These are the shapes that
// prove it stayed strict; every one must reach the dispatcher.
//
// Added with the normalisation (review-20260915-3d5b): widening a recogniser
// without pinning what it still refuses is how a redemption keyword quietly
// starts swallowing prompts, which §5.2 calls the worse failure.
func TestSlackLink_NearMissesAreStillPrompts(t *testing.T) {
	for _, text := range []string{
		"linkx ACDE2345",  // a longer word
		"unlink ACDE2345", // contains "link" but is not it
		"relink ACDE2345", // prefixed
		// Anagrams. chatauth.KeywordLetters keeps letters IN ORDER, and the substring
		// cases above pass under a letter-BAG implementation too — so they
		// do not actually pin the invariant this test claims
		// (review-20260915-beff F3). These fail the moment someone
		// "simplifies" the reduce into a set comparison.
		"klin ACDE2345",
		"ilnk ACDE2345",
		"link ACDE2345 now", // three fields
		"ACDE2345",          // the code alone, no keyword
	} {
		linker := &stubSlackLinker{user: &persistence.User{DisplayName: "Ada"}}
		c := linkChannel(linker)
		if c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", text) {
			t.Errorf("%q was taken as a redemption; the recogniser has stopped being strict", text)
		}
	}

	// And the widening, asserted rather than left for someone to discover:
	// letters-only normalisation cannot tell a decorated keyword from a
	// separated one, so these ARE redemptions now. Nobody types them, and the
	// exactly-two-fields rule bounds what they can swallow. They are here so
	// the cost of the normalisation is written down next to its benefit.
	for _, text := range []string{
		"l-i-n-k ACDE2345",
		"link. ACDE2345",
	} {
		linker := &stubSlackLinker{user: &persistence.User{DisplayName: "Ada"}}
		c := linkChannel(linker)
		if !c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", text) {
			t.Errorf("%q: the stated widening no longer holds; if that is deliberate, "+
				"update the comment in linkCodeFromSlashText rather than this list", text)
		}
	}
}

// TestSlackLink_UnrecognisedLinkAttemptIsNoted — the review's minimum
// proposal, built (review-20260915-3d5b finding 1, review-20260915-beff F1).
//
// A text naming the link keyword that is NOT shaped like a redemption is about
// to be dispatched to a model. It may be carrying a live one-time code. This
// does not change that — the strictness rule is deliberate — it stops the
// boundary being silent, keyed on the keyword this package already computed
// and without ever asking the code store anything.
//
// The second half is the one that matters most: the text must NOT appear in
// the log line. A log is the second place a one-time credential escapes to
// after a URL, and this path exists precisely because the text may hold one.
func TestSlackLink_UnrecognisedLinkAttemptIsNoted(t *testing.T) {
	var buf bytes.Buffer
	c := &Channel{logger: zerolog.New(&buf)}
	c.SetAccountLinker(&stubSlackLinker{user: &persistence.User{DisplayName: "Ada"}})

	// Shaped like an attempt, wrong field count — dispatched, and noted.
	if c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", "link ACDE2345 please") {
		t.Fatal("a three-field text was taken as a redemption")
	}
	logged := buf.String()
	if !strings.Contains(logged, "link keyword") {
		t.Errorf("a probable redemption attempt was dispatched with nothing logged; log was:\n%s", logged)
	}
	if strings.Contains(logged, "ACDE2345") {
		t.Errorf("THE CODE REACHED THE LOG. That is the leak this path exists to report, "+
			"committed by the report itself:\n%s", logged)
	}
	if strings.Contains(logged, "please") {
		t.Errorf("the message text was logged; it may carry a credential:\n%s", logged)
	}

	// An ordinary prompt is dispatched in silence — the note must not become
	// a line on every message.
	buf.Reset()
	if c.tryAccountLinkCode(context.Background(), httptest.NewRecorder(), "U1", "summarise the backlog") {
		t.Fatal("an ordinary prompt was taken as a redemption")
	}
	if buf.Len() != 0 {
		t.Errorf("an ordinary prompt produced a log line; the note is not keyed on the keyword:\n%s", buf.String())
	}
}
