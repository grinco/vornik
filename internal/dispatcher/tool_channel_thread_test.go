package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

type stubThreadReader struct {
	threads   map[string][]chat.Message
	requested []string
	err       error
}

func (s *stubThreadReader) ReadThread(_ context.Context, sessionID string) ([]chat.Message, error) {
	s.requested = append(s.requested, sessionID)
	if s.err != nil {
		return nil, s.err
	}
	return s.threads[sessionID], nil
}

func callerCtx(sessionID string) context.Context {
	return withOriginatingChannel(context.Background(), "slack", sessionID)
}

// The containment property, and the reason it must key on the full
// <team>/<channel># prefix rather than the channel name: two workspaces
// routinely both have a #general, so channel-name equality would let one
// workspace's lead read the other's conversation (companion design review
// finding 2, task_20260728162340_c506418c456d72ea).
func TestGetChannelThread_RefusesOtherWorkspaceSameChannelName(t *testing.T) {
	reader := &stubThreadReader{threads: map[string][]chat.Message{
		"T_B/C_general#1700000010.000100": {{Role: "user", Content: "secret from workspace B"}},
	}}
	te := &ToolExecutor{channelThreads: reader}

	res := te.getChannelThread(
		callerCtx("T_A/C_general#main"),
		`{"thread_key":"T_B/C_general#1700000010.000100"}`,
	)

	if !strings.Contains(res.Content, "different channel or workspace") {
		t.Errorf("expected refusal, got: %q", res.Content)
	}
	if strings.Contains(res.Content, "secret from workspace B") {
		t.Fatal("LEAKED another workspace's conversation")
	}
	if len(reader.requested) != 0 {
		t.Errorf("reader was consulted despite refusal: %v", reader.requested)
	}
}

func TestGetChannelThread_RefusesOtherChannelSameWorkspace(t *testing.T) {
	reader := &stubThreadReader{threads: map[string][]chat.Message{
		"T_A/C_private#1700000010.000100": {{Role: "user", Content: "private channel content"}},
	}}
	te := &ToolExecutor{channelThreads: reader}

	res := te.getChannelThread(
		callerCtx("T_A/C_general#main"),
		`{"thread_key":"T_A/C_private#1700000010.000100"}`,
	)

	if strings.Contains(res.Content, "private channel content") {
		t.Fatal("LEAKED another channel's conversation")
	}
	if len(reader.requested) != 0 {
		t.Errorf("reader was consulted despite refusal: %v", reader.requested)
	}
}

// The happy path: a bare thread_key is resolved inside the caller's own channel.
func TestGetChannelThread_ResolvesBareKeyInOwnChannel(t *testing.T) {
	reader := &stubThreadReader{threads: map[string][]chat.Message{
		"T_A/C_general#1700000010.000100": {
			{Role: "user", Content: "what did we decide about the offsite budget?"},
			{Role: "assistant", Content: "the 12k cap was approved, catering excluded"},
		},
	}}
	te := &ToolExecutor{channelThreads: reader}

	res := te.getChannelThread(callerCtx("T_A/C_general#main"), `{"thread_key":"1700000010.000100"}`)

	for _, want := range []string{"offsite budget", "12k cap was approved"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q in transcript; got:\n%s", want, res.Content)
		}
	}
	// Roles are rendered from the reader's perspective so the lead does not
	// mistake its own past words for a third party's.
	if !strings.Contains(res.Content, "you:") || !strings.Contains(res.Content, "them:") {
		t.Errorf("expected them:/you: role labels; got:\n%s", res.Content)
	}
	if len(reader.requested) != 1 || reader.requested[0] != "T_A/C_general#1700000010.000100" {
		t.Errorf("resolved session = %v, want the caller's own channel prefix + key", reader.requested)
	}
}

// A fully-qualified key inside the caller's own channel is accepted, since the
// digest block could legitimately be quoted back either way.
func TestGetChannelThread_AcceptsQualifiedKeyInOwnChannel(t *testing.T) {
	reader := &stubThreadReader{threads: map[string][]chat.Message{
		"T_A/C_general#1700000010.000100": {{Role: "user", Content: "budget question"}},
	}}
	te := &ToolExecutor{channelThreads: reader}

	res := te.getChannelThread(
		callerCtx("T_A/C_general#main"),
		`{"thread_key":"T_A/C_general#1700000010.000100"}`,
	)
	if !strings.Contains(res.Content, "budget question") {
		t.Errorf("qualified own-channel key should resolve; got: %q", res.Content)
	}
}

func TestGetChannelThread_Guards(t *testing.T) {
	tests := []struct {
		name    string
		te      *ToolExecutor
		ctx     context.Context
		args    string
		wantSub string
	}{
		{
			name:    "no reader wired",
			te:      &ToolExecutor{},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{"thread_key":"1700000010.000100"}`,
			wantSub: "not available",
		},
		{
			name:    "missing thread_key",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{}},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{}`,
			wantSub: "thread_key is required",
		},
		{
			name:    "malformed args",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{}},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{`,
			wantSub: "Invalid arguments",
		},
		{
			// A channel whose session ids carry no container (webchat cookie
			// hash, email message-id) must get no cross-thread access at all,
			// rather than an accidentally-wide one.
			name:    "caller session has no channel container",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{}},
			ctx:     callerCtx("some-webchat-cookie-hash"),
			args:    `{"thread_key":"1700000010.000100"}`,
			wantSub: "no channel context",
		},
		{
			name:    "asking for the current conversation",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{}},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{"thread_key":"main"}`,
			wantSub: "already in context",
		},
		{
			name:    "unknown thread",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{}},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{"thread_key":"1700000099.000100"}`,
			wantSub: "No stored history",
		},
		{
			name:    "reader error surfaces without leaking internals",
			te:      &ToolExecutor{channelThreads: &stubThreadReader{err: errors.New("db down")}},
			ctx:     callerCtx("T_A/C_general#main"),
			args:    `{"thread_key":"1700000010.000100"}`,
			wantSub: "Could not read that thread",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.te.getChannelThread(tc.ctx, tc.args)
			if !strings.Contains(res.Content, tc.wantSub) {
				t.Errorf("got %q, want it to contain %q", res.Content, tc.wantSub)
			}
		})
	}
}

func TestChatContainerPrefix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"T_A/C_general#main", "T_A/C_general#"},
		{"T_A/C_general#1700000010.000100", "T_A/C_general#"},
		{"webchat-cookie-hash", ""},             // no separator → no access
		{"owner/repo#issues/42", "owner/repo#"}, // github shape parses; scoping still per-container
		{"#main", ""},                           // empty container
		{"/C_general#main", ""},                 // empty team
		{"T_A/#main", ""},                       // empty channel
	}
	for _, tc := range tests {
		if got := chatContainerPrefix(tc.in); got != tc.want {
			t.Errorf("chatContainerPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A very long thread must be truncated from the FRONT: the tail carries the
// conclusion, which is what a follow-up is about.
func TestGetChannelThread_TruncatesKeepingTail(t *testing.T) {
	long := make([]chat.Message, 0, 400)
	for i := 0; i < 200; i++ {
		long = append(long,
			chat.Message{Role: "user", Content: strings.Repeat("filler question ", 8)},
			chat.Message{Role: "assistant", Content: strings.Repeat("filler answer ", 8)},
		)
	}
	long = append(long, chat.Message{Role: "assistant", Content: "FINAL DECISION: ship on Friday"})
	reader := &stubThreadReader{threads: map[string][]chat.Message{
		"T_A/C_general#1700000010.000100": long,
	}}
	te := &ToolExecutor{channelThreads: reader}

	res := te.getChannelThread(callerCtx("T_A/C_general#main"), `{"thread_key":"1700000010.000100"}`)

	if !strings.Contains(res.Content, "FINAL DECISION: ship on Friday") {
		t.Error("truncation dropped the tail; the conclusion must survive")
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Error("truncation should be disclosed to the model")
	}
	if len(res.Content) > maxChannelThreadBytes*2 {
		t.Errorf("output %d bytes, want bounded near %d", len(res.Content), maxChannelThreadBytes)
	}
}
