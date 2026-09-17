package slack

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

// stubAssistant records what the entrypoint handed the engine.
type stubAssistant struct {
	mu                           sync.Mutex
	calls                        int
	projectID, intent, accountID string
	reply                        string
}

func (s *stubAssistant) ProposeFromChat(_ context.Context, projectID, intent, accountID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.projectID, s.intent, s.accountID = projectID, intent, accountID
	if s.reply == "" {
		return "Filed proposal cap_1 (class A)."
	}
	return s.reply
}

func (s *stubAssistant) seen() (int, string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.projectID, s.intent, s.accountID
}

// assistChannel builds a channel whose resolver knows `linked` and whose
// legacy allowlist admits `listed`. Both populations matter: the entrypoint
// must serve the first and refuse the second.
func assistChannel(t *testing.T, a ConfigAssistant, linked []string, listed []string, resolveErr error) (*Channel, *installation) {
	t.Helper()
	principals := map[string]*authz.Principal{}
	for _, u := range linked {
		principals["slack:"+u] = &authz.Principal{UserID: "user_" + u}
	}
	senders := map[string]struct{}{}
	for _, u := range listed {
		senders[u] = struct{}{}
	}
	inst := &installation{teamID: "T1", projectID: "assistant", senders: senders}
	c := &Channel{
		logger:            zerolog.Nop(),
		installations:     []*installation{inst},
		installationsByID: map[string]*installation{"T1": inst},
		identityShim: chatauth.New(
			assistStubResolver{principals: principals, err: resolveErr},
			chatauth.NewMetrics(nil), zerolog.Nop(),
		),
	}
	c.SetConfigAssistant(a)
	return c, inst
}

type assistStubResolver struct {
	principals map[string]*authz.Principal
	err        error
}

func (s assistStubResolver) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	if s.err != nil {
		return nil, s.err
	}
	if p, ok := s.principals[channel+":"+externalID]; ok {
		return p, nil
	}
	return nil, authz.ErrUnknownIdentity
}

// TestConfigEntrypoint_ConsumesARealRequest is the POSITIVE case — the
// behaviour §6.3.3 accepts as a residual and the first test list never
// asserted (review-20260915-2353 F5). It pinned what must not match and left
// what must match to inference.
func TestConfigEntrypoint_ConsumesARealRequest(t *testing.T) {
	a := &stubAssistant{}
	c, inst := assistChannel(t, a, []string{"U1"}, nil, nil)

	if !c.tryConfigAssistant(httptest.NewRecorder(), inst, "U1", "config raise the retry budget") {
		t.Fatal("a real config request reached the dispatcher instead of the entrypoint")
	}
	waitForAssist(t, a)
	calls, project, intent, account := a.seen()
	if calls != 1 {
		t.Fatalf("engine called %d times, want 1", calls)
	}
	if project != "assistant" {
		t.Errorf("project = %q, want the installation's", project)
	}
	if intent != "raise the retry budget" {
		t.Errorf("intent = %q — the keyword must be stripped and the request kept whole", intent)
	}
	if account != "user_U1" {
		t.Errorf("account = %q, want the RESOLVED account id, not the Slack user id", account)
	}
}

// TestConfigEntrypoint_NearMissesReachTheDispatcher — the recogniser has no
// field-count rule to lean on, so the letters-only reduce is the whole thing
// and it must stay strict.
//
// The anagram is the case that matters: "configure" fails against a letter-BAG
// reduce too, so it does not distinguish one implementation from the other.
// "ocnfig" does (review-20260915-2353 F3).
func TestConfigEntrypoint_NearMissesReachTheDispatcher(t *testing.T) {
	for _, text := range []string{
		"configure something",      // superstring
		"ocnfig something",         // ANAGRAM — the letter-bag detector
		"reconfigure the retries",  // contains the letters, not the word
		"what is the config for x", // not the first field
		"summarise the backlog",    //
		"",                         //
	} {
		a := &stubAssistant{}
		c, inst := assistChannel(t, a, []string{"U1"}, nil, nil)
		if c.tryConfigAssistant(httptest.NewRecorder(), inst, "U1", text) {
			t.Errorf("%q was taken as a config request; it must reach the dispatcher", text)
		}
		if calls, _, _, _ := a.seen(); calls != 0 {
			t.Errorf("%q reached the engine", text)
		}
	}
}

// TestConfigEntrypoint_DecorationDoesNotDefeatTheKeyword — the same class the
// link recogniser was fixed for. A decorated keyword that fails to match does
// not fail safely: the text dispatches onward.
func TestConfigEntrypoint_DecorationDoesNotDefeatTheKeyword(t *testing.T) {
	for _, text := range []string{
		"`config` raise the retry budget",
		"*config* raise the retry budget",
		"“config” raise the retry budget",
		"config, raise the retry budget",
	} {
		a := &stubAssistant{}
		c, inst := assistChannel(t, a, []string{"U1"}, nil, nil)
		if !c.tryConfigAssistant(httptest.NewRecorder(), inst, "U1", text) {
			t.Errorf("%q was not recognised; a config request went to a model instead", text)
		}
	}
}

// TestConfigEntrypoint_RefusesALegacyOnlySender is the deny-path mirror of
// test 23, and the pin the identity design's §5.3 rule was missing
// (review-20260915-931b F-A).
//
// The sender is on the legacy allowlist and NOT linked. The shim admits them
// for ordinary chat — that is its entire purpose during the migration — and
// this entrypoint must refuse them anyway, because the shim is a grace period
// for surfaces that read and ask, and this one edits the deployment.
//
// The allowlist is WIRED, which is what makes the test bite: without it the
// assertion cannot tell "refused because unlinked" from "refused even though
// the shim would have admitted them", and an implementation that falls back to
// the shim would pass.
func TestConfigEntrypoint_RefusesALegacyOnlySender(t *testing.T) {
	a := &stubAssistant{}
	c, inst := assistChannel(t, a, nil /* nobody linked */, []string{"U-listed"}, nil)

	// Precondition: the shim really would admit this sender for ordinary chat.
	// Without this the test could pass because nothing admits anyone.
	if d := c.authorize("U-listed", c.legacyForInstallation(inst, "U-listed")); !d.Allowed || !d.ViaLegacyOnly {
		t.Fatalf("precondition failed: the legacy allowlist does not admit U-listed (%+v) — "+
			"this test cannot distinguish the two reasons for refusing", d)
	}

	rec := httptest.NewRecorder()
	if !c.tryConfigAssistant(rec, inst, "U-listed", "config raise the retry budget") {
		t.Fatal("the entrypoint declined to answer at all")
	}
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("a sender admitted ONLY by the legacy allowlist reached the configuration " +
			"assistant; the shim is a grace period for reading, not for editing the deployment")
	}
	if !strings.Contains(rec.Body.String(), "link") {
		t.Errorf("the refusal does not name the remedy: %s", rec.Body.String())
	}
}

// TestConfigEntrypoint_RefusesWhileResolutionIsUnavailable is design test 23.
// The allowlist is wired and WOULD admit, so the assertion distinguishes
// "refused" from "refused because nothing would have admitted them".
//
// The message must also not send the person to /link: during an outage that is
// advice to do something that cannot work.
func TestConfigEntrypoint_RefusesWhileResolutionIsUnavailable(t *testing.T) {
	a := &stubAssistant{}
	c, inst := assistChannel(t, a, []string{"U1"}, []string{"U1"}, context.DeadlineExceeded)

	rec := httptest.NewRecorder()
	if !c.tryConfigAssistant(rec, inst, "U1", "config raise the retry budget") {
		t.Fatal("the entrypoint declined to answer at all")
	}
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("the entrypoint served a request while identity resolution was unavailable; " +
			"it fell back to the allowlist, which is what test 23 forbids")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "identity service") {
		t.Errorf("the refusal does not say resolution is unavailable: %s", body)
	}
	if strings.Contains(body, "Generate a code") {
		t.Errorf("an outage was reported as an unlinked account, sending the person to do "+
			"something that cannot work: %s", body)
	}
}

// TestConfigEntrypoint_RefusesWithNoProjectBound is F6's symmetric case: Slack
// must fail the same way Telegram does on the same missing input, rather than
// asserting the project is always there.
func TestConfigEntrypoint_RefusesWithNoProjectBound(t *testing.T) {
	a := &stubAssistant{}
	c, inst := assistChannel(t, a, []string{"U1"}, nil, nil)
	inst.projectID = ""

	rec := httptest.NewRecorder()
	if !c.tryConfigAssistant(rec, inst, "U1", "config raise the retry budget") {
		t.Fatal("the entrypoint declined to answer at all")
	}
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("the engine was called with no project")
	}
	if !strings.Contains(rec.Body.String(), "project") {
		t.Errorf("the refusal does not name what is missing: %s", rec.Body.String())
	}
}

// TestConfigEntrypoint_BareKeywordExplains — "config" alone is a person who
// knows the entrypoint exists and not what to type. Sending that to a model
// spends a run on nothing.
func TestConfigEntrypoint_BareKeywordExplains(t *testing.T) {
	a := &stubAssistant{}
	c, inst := assistChannel(t, a, []string{"U1"}, nil, nil)
	rec := httptest.NewRecorder()
	if !c.tryConfigAssistant(rec, inst, "U1", "config") {
		t.Fatal("the bare keyword reached the dispatcher")
	}
	if calls, _, _, _ := a.seen(); calls != 0 {
		t.Fatal("the bare keyword spent an engine run")
	}
	if !strings.Contains(rec.Body.String(), "what you want changed") {
		t.Errorf("the reply does not say what to type: %s", rec.Body.String())
	}
}

// TestConfigEntrypoint_UnwiredIsAnOrdinaryPrompt — a deployment that has not
// opened the entrypoint must see "config …" exactly as it did before, or
// shipping this feature would silently change what every existing deployment's
// prompts mean.
func TestConfigEntrypoint_UnwiredIsAnOrdinaryPrompt(t *testing.T) {
	c := &Channel{logger: zerolog.Nop()}
	if c.tryConfigAssistant(httptest.NewRecorder(), &installation{projectID: "p"}, "U1", "config raise the retry budget") {
		t.Error("an unwired deployment answered a request it cannot serve")
	}
}

// waitForAssist waits for the detached engine goroutine. The entrypoint
// answers Slack inside three seconds and runs the engine after, so a test that
// asserted immediately would race the thing it is asserting.
func waitForAssist(t *testing.T, a *stubAssistant) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if calls, _, _, _ := a.seen(); calls > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the engine was never called; the detached run did not happen")
}
