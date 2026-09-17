package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

// The configuration assistant's chat entrypoint on Telegram — design §6.3.3,
// plan §9. These pin the two properties that make this entrypoint safe to give
// a chat sender: it authorises on a RESOLVED identity rather than the legacy
// allowlist, and it refuses rather than guessing which project a change is for.

type stubTgAssistant struct {
	mu                           sync.Mutex
	calls                        int
	projectID, intent, accountID string
}

func (s *stubTgAssistant) ProposeFromChat(_ context.Context, projectID, intent, accountID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.projectID, s.intent, s.accountID = projectID, intent, accountID
	return "Filed proposal cap_1 (class A)."
}

func (s *stubTgAssistant) seen() (int, string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.projectID, s.intent, s.accountID
}

// assistBot wires a bot whose resolver knows `linked` and whose LEGACY list
// admits `listed`. Both populations matter: the entrypoint serves the first
// and must refuse the second.
func assistBot(t *testing.T, a ConfigAssistant, linked map[string]*authz.Principal, listed map[int64]UserAccess, resolveErr error) *Bot {
	t.Helper()
	b := &Bot{config: BotConfig{AllowedUsers: listed}, logger: zerolog.Nop()}
	b.SetIdentityShim(chatauth.New(
		erroringResolver{linked: linked, err: resolveErr},
		chatauth.NewMetrics(prometheus.NewRegistry()),
		zerolog.Nop(),
	))
	b.SetConfigAssistant(a)
	return b
}

type erroringResolver struct {
	linked map[string]*authz.Principal
	err    error
}

func (f erroringResolver) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.linked[channel+":"+externalID]; ok {
		return p, nil
	}
	return nil, authz.ErrUnknownIdentity
}

// TestConfigCommand_RefusesWithExactlyOneProjectRegistered is the case the
// design's prose defends and its first test list did not pin
// (review-20260915-2353 F1).
//
// With zero or many projects, refusing is the only sane behaviour and a test
// passes trivially against an implementation that guesses. ONE project is the
// only setup where guessing is tempting — "there is only one, they must mean
// that one" — so it is the only setup that distinguishes the two
// implementations.
func TestConfigCommand_RefusesWithExactlyOneProjectRegistered(t *testing.T) {
	a := &stubTgAssistant{}
	b := assistBot(t, a, map[string]*authz.Principal{"telegram:42": {UserID: "u1"}}, nil, nil)

	// Exactly one project exists, and the chat has not chosen it.
	b.activeProjects = map[int64]string{}

	reply := b.handleConfigCommand(context.Background(), 100, 42, []string{"/config", "raise", "the", "retry", "budget"})

	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("the assistant was called with a guessed project; a change proposed against a " +
			"project the person did not name is what the class ceiling exists to bound")
	}
	if !strings.Contains(reply, "/project") {
		t.Errorf("the refusal does not name the remedy: %q", reply)
	}
}

// TestConfigCommand_RefusesALegacyOnlySender — the deny-path rule of identity
// §5.3, on the second channel. The sender is on the legacy allowlist and NOT
// linked; the shim admits them for ordinary chat, and this entrypoint must
// refuse them anyway.
//
// The allowlist is WIRED, which is what makes the assertion bite: without it
// the test cannot tell "refused because unlinked" from "refused even though
// the shim would have admitted them".
func TestConfigCommand_RefusesALegacyOnlySender(t *testing.T) {
	a := &stubTgAssistant{}
	b := assistBot(t, a,
		map[string]*authz.Principal{},            // nobody linked
		map[int64]UserAccess{7: {Allowed: true}}, // but 7 IS on the legacy list
		nil)
	b.activeProjects = map[int64]string{100: "assistant"}

	// Precondition: the shim really would admit this sender for ordinary chat.
	if d := b.authorize(7); !d.Allowed || !d.ViaLegacyOnly {
		t.Fatalf("precondition failed: the legacy list does not admit 7 (%+v) — this test "+
			"cannot distinguish the two reasons for refusing", d)
	}

	reply := b.handleConfigCommand(context.Background(), 100, 7, []string{"/config", "raise", "the", "budget"})
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("a sender admitted ONLY by the legacy allowlist reached the configuration " +
			"assistant; the shim is a grace period for reading, not for editing the deployment")
	}
	if !strings.Contains(reply, "/link") {
		t.Errorf("the refusal does not name the remedy: %q", reply)
	}
}

// TestConfigCommand_RefusesWhileResolutionIsUnavailable is design test 23 on
// this channel. The legacy list WOULD admit, so the assertion distinguishes
// "refused" from "refused because nothing would have admitted them"; and the
// message must not send the person to /link, which cannot work during an
// outage.
func TestConfigCommand_RefusesWhileResolutionIsUnavailable(t *testing.T) {
	a := &stubTgAssistant{}
	b := assistBot(t, a, nil, map[int64]UserAccess{7: {Allowed: true}}, errors.New("identity store unreachable"))
	b.activeProjects = map[int64]string{100: "assistant"}

	reply := b.handleConfigCommand(context.Background(), 100, 7, []string{"/config", "raise", "the", "budget"})
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("the entrypoint served a request while identity resolution was unavailable; " +
			"it fell back to the allowlist, which is what test 23 forbids")
	}
	if !strings.Contains(reply, "identity service") {
		t.Errorf("the refusal does not say resolution is unavailable: %q", reply)
	}
	if strings.Contains(reply, "Generate a code") {
		t.Errorf("an outage was reported as an unlinked account, sending the person to do "+
			"something that cannot work: %q", reply)
	}
}

// TestConfigCommand_ServesALinkedSender is the positive case: the entrypoint
// runs, and hands the engine the RESOLVED account id rather than the Telegram
// user id — without which the proposal's actor is a chat handle and the
// ledger cannot say who asked.
func TestConfigCommand_ServesALinkedSender(t *testing.T) {
	a := &stubTgAssistant{}
	b := assistBot(t, a, map[string]*authz.Principal{"telegram:42": {UserID: "u1"}}, nil, nil)
	b.activeProjects = map[int64]string{100: "assistant"}

	reply := b.handleConfigCommand(context.Background(), 100, 42, []string{"/config", "raise", "the", "retry", "budget"})
	if !strings.Contains(reply, "Working on it") {
		t.Errorf("the person was not told the work had started: %q", reply)
	}
	waitForTgAssist(t, a)
	_, project, intent, account := a.seen()
	if project != "assistant" {
		t.Errorf("project = %q, want the chat's active project", project)
	}
	if intent != "raise the retry budget" {
		t.Errorf("intent = %q — the command word must be stripped and the request kept whole", intent)
	}
	if account != "u1" {
		t.Errorf("account = %q, want the RESOLVED account id, not the Telegram user id", account)
	}
}

// TestConfigCommand_BareAndUnwired — the two cases that must not spend an
// engine run.
func TestConfigCommand_BareAndUnwired(t *testing.T) {
	a := &stubTgAssistant{}
	b := assistBot(t, a, map[string]*authz.Principal{"telegram:42": {UserID: "u1"}}, nil, nil)
	b.activeProjects = map[int64]string{100: "assistant"}

	if reply := b.handleConfigCommand(context.Background(), 100, 42, []string{"/config"}); !strings.Contains(reply, "what you want changed") {
		t.Errorf("the bare command does not say what to type: %q", reply)
	}
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Error("the bare command spent an engine run")
	}

	unwired := &Bot{logger: zerolog.Nop()}
	if reply := unwired.handleConfigCommand(context.Background(), 100, 42, []string{"/config", "do", "something"}); !strings.Contains(reply, "isn't enabled") {
		t.Errorf("a deployment without the assistant does not say so: %q", reply)
	}
}

// TestIsConfigCommand_MatchesTheGroupAndDecoratedForms — /config@botname is the
// group form, and a group is where this matters most. The near-misses must
// still miss, and the leading slash must still be required: on Telegram a bare
// word is not a command, and accepting one would swallow ordinary messages.
func TestIsConfigCommand_MatchesTheGroupAndDecoratedForms(t *testing.T) {
	for _, in := range []string{"/config", "/config@vornikbot", "`/config`", "*/config*", "(/config)"} {
		if !isConfigCommand(in) {
			t.Errorf("isConfigCommand(%q) = false; the request goes to a model instead", in)
		}
	}
	for _, in := range []string{"config", "/configure", "/reconfig", "/unconfig", "//config", ""} {
		if isConfigCommand(in) {
			t.Errorf("isConfigCommand(%q) = true; the recogniser has stopped requiring a real command word", in)
		}
	}
}

func waitForTgAssist(t *testing.T, a *stubTgAssistant) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if calls, _, _, _ := a.seen(); calls > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the engine was never called; the detached run did not happen")
}
