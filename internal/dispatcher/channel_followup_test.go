// Tests for the per-channel follow-up wiring on the dispatcher.
// Pin: Request.OriginatingChannel + OriginatingSessionID propagate
// to create_task via context; create_task routes the follow-up to
// the matching ChannelFollowupRegistrar instead of the legacy
// chatID-keyed FollowupRegistrar.
package dispatcher

import (
	"context"
	"sync"
	"testing"
)

// stubChannelFollowupRegistrar captures every RegisterFollowup
// call so tests can pin the routing decisions create_task makes.
type stubChannelFollowupRegistrar struct {
	mu    sync.Mutex
	calls []stubFollowupCall
}

type stubFollowupCall struct {
	SessionID string
	TaskID    string
	ProjectID string
}

func (s *stubChannelFollowupRegistrar) RegisterFollowup(sessionID, taskID, projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubFollowupCall{sessionID, taskID, projectID})
}

// TestOriginatingChannelContext_Roundtrip — context-key plumbing
// returns what was set. Empty-string set is a no-op (defensive:
// don't pollute the context with empties from synthesised turns).
func TestOriginatingChannelContext_Roundtrip(t *testing.T) {
	ctx := context.Background()
	if ch, sid := originatingChannelFromContext(ctx); ch != "" || sid != "" {
		t.Errorf("bare ctx: ch=%q sid=%q, want both empty", ch, sid)
	}

	ctx2 := withOriginatingChannel(ctx, "email", "thread-1")
	ch, sid := originatingChannelFromContext(ctx2)
	if ch != "email" || sid != "thread-1" {
		t.Errorf("after set: ch=%q sid=%q, want email/thread-1", ch, sid)
	}

	// Empty values must not stash anything — the parent ctx
	// passes through unchanged.
	ctx3 := withOriginatingChannel(ctx, "", "")
	if ch, sid := originatingChannelFromContext(ctx3); ch != "" || sid != "" {
		t.Errorf("empty set: ch=%q sid=%q, want both empty", ch, sid)
	}
}

// TestSetChannelFollowupRegistrar_StoresAndUnwires — the
// SetChannelFollowupRegistrar setter records the registrar in
// the per-channel map, and passing nil removes it.
func TestSetChannelFollowupRegistrar_StoresAndUnwires(t *testing.T) {
	a := &Agent{toolExecutor: &ToolExecutor{}}
	stub := &stubChannelFollowupRegistrar{}

	a.SetChannelFollowupRegistrar("email", stub)
	if got := a.toolExecutor.channelFollowupRegistrars["email"]; got != stub {
		t.Errorf("registrar not stored: got %v, want %v", got, stub)
	}

	a.SetChannelFollowupRegistrar("email", nil)
	if _, ok := a.toolExecutor.channelFollowupRegistrars["email"]; ok {
		t.Error("nil registrar should remove the key, but it's still present")
	}
}

// TestSetChannelFollowupRegistrar_NilGuards — nil agent +
// missing toolExecutor + empty channel name all return cleanly
// without panicking.
func TestSetChannelFollowupRegistrar_NilGuards(t *testing.T) {
	var a *Agent
	a.SetChannelFollowupRegistrar("email", &stubChannelFollowupRegistrar{}) // no panic
	(&Agent{}).SetChannelFollowupRegistrar("", &stubChannelFollowupRegistrar{})
	(&Agent{toolExecutor: nil}).SetChannelFollowupRegistrar("email", &stubChannelFollowupRegistrar{})
}
