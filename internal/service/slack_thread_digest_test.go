package service

import (
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/sessionstore"
)

func TestChannelPrefixFromSessionID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"T_A/C_general#main", "T_A/C_general#"},
		{"T_A/C_general#1700000010.000100", "T_A/C_general#"},
		{"no-separator", ""},
		{"#main", ""},
		{"/C_general#main", ""},
		{"T_A/#main", ""},
	}
	for _, tc := range tests {
		if got := channelPrefixFromSessionID(tc.in); got != tc.want {
			t.Errorf("channelPrefixFromSessionID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestThreadKeyFromSessionID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"T_A/C_general#main", "main"},
		{"T_A/C_general#1700000010.000100", "1700000010.000100"},
		{"T_A/C_general#", ""},
		{"nohash", ""},
	}
	for _, tc := range tests {
		if got := threadKeyFromSessionID(tc.in); got != tc.want {
			t.Errorf("threadKeyFromSessionID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDeriveThreadDigest(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	d := deriveThreadDigest("T_A/C_general#1700000010.000100", []chat.Message{
		{Role: "user", Content: "what did we decide about the offsite budget?"},
		{Role: "assistant", Content: "checking the thread"},
		{Role: "user", Content: "thanks"},
		{Role: "assistant", Content: "the 12k cap was approved, catering excluded"},
	}, now)

	if d.ThreadKey != "1700000010.000100" {
		t.Errorf("ThreadKey = %q", d.ThreadKey)
	}
	if d.TurnCount != 4 {
		t.Errorf("TurnCount = %d, want 4", d.TurnCount)
	}
	// First user turn is the question that identifies the thread…
	if !strings.Contains(d.Asked, "offsite budget") {
		t.Errorf("Asked = %q, want the FIRST user turn", d.Asked)
	}
	// …and the LAST assistant turn is the answer a follow-up refers to.
	if !strings.Contains(d.Answered, "12k cap") {
		t.Errorf("Answered = %q, want the LAST assistant turn", d.Answered)
	}
}

func TestDigestExcerptFlattensAndBounds(t *testing.T) {
	got := digestExcerpt("first line\n\nsecond line\nthird")
	if strings.Contains(got, "\n") {
		t.Errorf("excerpt must be one line; got %q", got)
	}
	if !strings.Contains(got, "·") {
		t.Errorf("expected newline-joined marker; got %q", got)
	}

	long := digestExcerpt(strings.Repeat("x", maxDigestExcerpt*3))
	if len([]rune(long)) > maxDigestExcerpt {
		t.Errorf("excerpt = %d runes, want <= %d", len([]rune(long)), maxDigestExcerpt)
	}
	if digestExcerpt("   ") != "" {
		t.Error("whitespace-only excerpt should be empty")
	}
}

func TestRenderThreadDigests(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	digests := []threadDigest{
		{ThreadKey: "older", TurnCount: 2, LastActive: base, Asked: "old question", Answered: "old answer"},
		{ThreadKey: "newer", TurnCount: 2, LastActive: base.Add(48 * time.Hour), Asked: "new question", Answered: "new answer"},
	}

	got := renderThreadDigests(digests)

	if !strings.Contains(got, "RECENT THREADS IN THIS CHANNEL") {
		t.Errorf("missing block header; got:\n%s", got)
	}
	// Most recent first — a follow-up almost always refers to recent activity.
	if strings.Index(got, "newer") > strings.Index(got, "older") {
		t.Errorf("expected most-recent-first ordering; got:\n%s", got)
	}
	// The block must tell the lead these are its OWN past conversations, so it
	// doesn't repeat the email-channel failure of reading itself as a stranger.
	if !strings.Contains(got, "YOUR earlier conversations") {
		t.Errorf("block should mark threads as the lead's own; got:\n%s", got)
	}
	if !strings.Contains(got, "get_channel_thread") {
		t.Errorf("block should name the drill-down tool; got:\n%s", got)
	}
	for _, want := range []string{"thread_key=newer", "asked: new question", "answered: new answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q; got:\n%s", want, got)
		}
	}
}

// Empty / contentless input must render nothing at all, so the prompt is
// byte-identical when there is no channel history to describe.
func TestRenderThreadDigestsEmpty(t *testing.T) {
	cases := [][]threadDigest{
		nil,
		{},
		{{ThreadKey: "k", TurnCount: 0}}, // no turns
		{{ThreadKey: "", TurnCount: 3, Asked: "x"}},               // no key to drill into
		{{ThreadKey: "k", TurnCount: 3, Asked: "", Answered: ""}}, // nothing to say
	}
	for i, c := range cases {
		if got := renderThreadDigests(c); got != "" {
			t.Errorf("case %d: expected empty block, got:\n%s", i, got)
		}
	}
}

func TestRenderThreadDigestsCapsThreadCount(t *testing.T) {
	base := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	many := make([]threadDigest, 0, maxDigestThreads*2)
	for i := 0; i < maxDigestThreads*2; i++ {
		many = append(many, threadDigest{
			ThreadKey:  string(rune('a' + i)),
			TurnCount:  2,
			LastActive: base.Add(time.Duration(i) * time.Hour),
			Asked:      "q",
			Answered:   "a",
		})
	}
	got := renderThreadDigests(many)
	// Count entry headers specifically: the block's instructions also mention
	// get_channel_thread(thread_key=...), so a bare "thread_key=" over-counts.
	if n := strings.Count(got, "[thread_key="); n != maxDigestThreads {
		t.Errorf("rendered %d threads, want the cap of %d", n, maxDigestThreads)
	}
}

// digestsForChannel must exclude the caller itself plus anything that isn't a
// thread — a sibling channel-scoped session or a slash invocation is not a
// "thread that was discussed".
func TestDigestsForChannelExcludesNonThreads(t *testing.T) {
	caller := "T_A/C_general#main"
	siblings := []sessionstore.SiblingSession{
		{SessionID: caller, History: []chat.Message{{Role: "user", Content: "current"}}},
		{SessionID: "T_A/C_general#slash:U_alice", History: []chat.Message{{Role: "user", Content: "slash"}}},
		{SessionID: "T_A/C_general#1700000010.000100", History: []chat.Message{
			{Role: "user", Content: "real thread question"},
			{Role: "assistant", Content: "real thread answer"},
		}},
	}

	got := digestsForChannel(siblings, caller)

	if len(got) != 1 {
		t.Fatalf("got %d digests, want 1 (only the real thread)", len(got))
	}
	if got[0].ThreadKey != "1700000010.000100" {
		t.Errorf("ThreadKey = %q, want the real thread", got[0].ThreadKey)
	}
}

// The digest block is only for channel-scoped turns: inside a thread the lead
// already has that thread's history and sibling threads would be pure noise.
func TestThreadDigestBlockOnlyForChannelScopedSession(t *testing.T) {
	store := newSlackSessionStore(nil, "proj")
	store.history["T_A/C_general#1700000010.000100"] = []chat.Message{
		{Role: "user", Content: "thread question"},
		{Role: "assistant", Content: "thread answer"},
	}

	if got := store.threadDigestBlock(t.Context(), "T_A/C_general#1700000020.000200"); got != "" {
		t.Errorf("in-thread session should get no digest block; got:\n%s", got)
	}
	if got := store.threadDigestBlock(t.Context(), "malformed-session-id"); got != "" {
		t.Errorf("malformed session id should get no digest block; got:\n%s", got)
	}

	block := store.threadDigestBlock(t.Context(), "T_A/C_general#main")
	if !strings.Contains(block, "thread question") {
		t.Errorf("channel-scoped session should see sibling threads; got:\n%s", block)
	}
}

// Siblings must be scoped by the full <team>/<channel># prefix — the in-memory
// fallback path must not leak another workspace's threads into the block.
func TestThreadDigestBlockScopedToOwnChannel(t *testing.T) {
	store := newSlackSessionStore(nil, "proj")
	store.history["T_B/C_general#1700000010.000100"] = []chat.Message{
		{Role: "user", Content: "OTHER WORKSPACE SECRET"},
		{Role: "assistant", Content: "other answer"},
	}
	store.history["T_A/C_other#1700000030.000300"] = []chat.Message{
		{Role: "user", Content: "OTHER CHANNEL SECRET"},
		{Role: "assistant", Content: "other answer"},
	}
	store.history["T_A/C_general#1700000020.000200"] = []chat.Message{
		{Role: "user", Content: "own channel question"},
		{Role: "assistant", Content: "own channel answer"},
	}

	block := store.threadDigestBlock(t.Context(), "T_A/C_general#main")

	if strings.Contains(block, "OTHER WORKSPACE SECRET") {
		t.Error("LEAKED another workspace's thread into the digest block")
	}
	if strings.Contains(block, "OTHER CHANNEL SECRET") {
		t.Error("LEAKED another channel's thread into the digest block")
	}
	if !strings.Contains(block, "own channel question") {
		t.Errorf("own channel thread missing; got:\n%s", block)
	}
}

// ReadThread serves the in-memory map; the tool layer owns access scoping.
func TestSlackSessionStoreReadThread(t *testing.T) {
	store := newSlackSessionStore(nil, "proj")
	want := []chat.Message{{Role: "user", Content: "hello"}}
	store.history["T_A/C_general#1700000010.000100"] = want

	got, err := store.ReadThread(t.Context(), "T_A/C_general#1700000010.000100")
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hello" {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Unknown session with no persister: a miss, not an error.
	missing, err := store.ReadThread(t.Context(), "T_A/C_general#nope")
	if err != nil {
		t.Errorf("unknown session should not error, got %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("expected no history, got %+v", missing)
	}
}

// The composite reader must consult every wired store, since each store's
// in-memory map only holds its own channel's threads.
func TestSlackThreadReaderSetFansOut(t *testing.T) {
	a := newSlackSessionStore(nil, "proj-a")
	b := newSlackSessionStore(nil, "proj-b")
	b.history["T_B/C_general#1700000010.000100"] = []chat.Message{{Role: "user", Content: "in b"}}

	set := slackThreadReaderSet{a, b}
	got, err := set.ReadThread(t.Context(), "T_B/C_general#1700000010.000100")
	if err != nil {
		t.Fatalf("ReadThread: %v", err)
	}
	if len(got) != 1 || got[0].Content != "in b" {
		t.Errorf("got %+v, want the second store's thread", got)
	}

	miss, err := set.ReadThread(t.Context(), "T_C/C_general#nope")
	if err != nil || len(miss) != 0 {
		t.Errorf("miss should be (nil, nil); got %+v / %v", miss, err)
	}
}
