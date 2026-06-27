package service

import (
	"context"
	"os"
	"sync"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/registry"
)

// osMkdirAll / osWriteFile alias os.MkdirAll / os.WriteFile so the
// inlined writeDirFile helper below stays readable without
// re-importing os in the helper's eye-line.
var (
	osMkdirAll  = os.MkdirAll
	osWriteFile = os.WriteFile
)

// TestGitHubSessionStore_Load_EmptyHistory — a freshly constructed
// store returns an empty Session with ActiveProject populated.
// Nil registry means no lead prompt / project list resolution.
func TestGitHubSessionStore_Load_EmptyHistory(t *testing.T) {
	store := newGitHubSessionStore(nil, "p-1")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "acme/api#issues/1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.ActiveProject != "p-1" {
		t.Errorf("ActiveProject = %q, want p-1", sess.ActiveProject)
	}
	if len(sess.History) != 0 {
		t.Errorf("History len = %d, want 0", len(sess.History))
	}
	if sess.LeadSystemPrompt != "" {
		t.Errorf("LeadSystemPrompt = %q, want empty (no registry)", sess.LeadSystemPrompt)
	}
	if len(sess.AllowedProjects) != 0 {
		t.Errorf("AllowedProjects = %v, want empty (no registry)", sess.AllowedProjects)
	}
}

// TestGitHubSessionStore_Load_NoProject — nil registry OR empty
// projectID skips the lookup path and still returns a usable
// session.
func TestGitHubSessionStore_Load_NoProject(t *testing.T) {
	store := newGitHubSessionStore(nil, "")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "x"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.ActiveProject != "" || sess.LeadSystemPrompt != "" {
		t.Errorf("unexpected fields: %+v", sess)
	}
}

// TestGitHubSessionStore_Append_ReplacesHistory — the Append
// contract: result.Messages becomes the new authoritative state.
func TestGitHubSessionStore_Append_ReplacesHistory(t *testing.T) {
	store := newGitHubSessionStore(nil, "p-1")
	sessionID := "acme/api#issues/7"

	// Pre-seed via Append.
	first := dispatcher.Result{Messages: []chat.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}}
	if err := store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID}, first); err != nil {
		t.Fatalf("Append #1: %v", err)
	}
	if h := store.snapshotHistory(sessionID); len(h) != 2 {
		t.Fatalf("history after first Append = %d, want 2", len(h))
	}

	// Second Append replaces with a new turn.
	second := dispatcher.Result{Messages: []chat.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "follow up"},
		{Role: "assistant", Content: "reply"},
	}}
	if err := store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID}, second); err != nil {
		t.Fatalf("Append #2: %v", err)
	}
	h := store.snapshotHistory(sessionID)
	if len(h) != 4 {
		t.Errorf("history after second Append = %d, want 4", len(h))
	}
	if h[3].Content != "reply" {
		t.Errorf("last message = %q, want reply", h[3].Content)
	}
}

// TestGitHubSessionStore_Append_EmptyMessagesIsNoop — defensive:
// an empty result.Messages (dispatcher error / early-return path)
// must not wipe the session's existing history.
func TestGitHubSessionStore_Append_EmptyMessagesIsNoop(t *testing.T) {
	store := newGitHubSessionStore(nil, "p-1")
	sessionID := "acme/api#issues/1"

	// Seed history.
	if err := store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID}, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "a"}},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Empty Append.
	if err := store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID}, dispatcher.Result{}); err != nil {
		t.Fatalf("empty Append: %v", err)
	}
	if h := store.snapshotHistory(sessionID); len(h) != 1 {
		t.Errorf("history wiped by empty Append: got %d, want 1", len(h))
	}
}

// TestGitHubSessionStore_Load_ReturnsCopy — defensive against
// aliasing: the slice returned by Load must not share backing
// storage with the store's internal map, or a concurrent Append
// could mutate the dispatcher's view mid-request.
func TestGitHubSessionStore_Load_ReturnsCopy(t *testing.T) {
	store := newGitHubSessionStore(nil, "p-1")
	sessionID := "x"
	_ = store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID}, dispatcher.Result{
		Messages: []chat.Message{{Role: "user", Content: "original"}},
	})
	sess, _ := store.Load(context.Background(), conversation.ChannelMessage{SessionID: sessionID})
	if len(sess.History) != 1 {
		t.Fatalf("History len = %d, want 1", len(sess.History))
	}
	sess.History[0].Content = "mutated"
	// Re-read; the internal copy must still say "original".
	sess2, _ := store.Load(context.Background(), conversation.ChannelMessage{SessionID: sessionID})
	if sess2.History[0].Content != "original" {
		t.Errorf("internal history mutated: got %q", sess2.History[0].Content)
	}
}

// TestGitHubSessionStore_Concurrent_AppendLoad — two goroutines
// writing different sessions + a reader on a third must not race
// under -race. Smoke test for the mutex coverage.
func TestGitHubSessionStore_Concurrent_AppendLoad(t *testing.T) {
	store := newGitHubSessionStore(nil, "p-1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				sessionID := "session-" + string(rune('0'+id))
				_ = store.Append(context.Background(), conversation.ChannelMessage{SessionID: sessionID},
					dispatcher.Result{Messages: []chat.Message{{Role: "user", Content: "hi"}}})
				_, _ = store.Load(context.Background(), conversation.ChannelMessage{SessionID: sessionID})
			}
		}(i)
	}
	wg.Wait()
}

// TestGitHubSessionStore_Load_RegistryWiredButProjectMissing —
// non-nil registry + projectID that doesn't resolve to a project
// returns a session without prompt / project list rather than
// crashing.
func TestGitHubSessionStore_Load_RegistryWiredButProjectMissing(t *testing.T) {
	reg := registry.New()
	store := newGitHubSessionStore(reg, "no-such-project")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "x"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sess.LeadSystemPrompt != "" {
		t.Errorf("LeadSystemPrompt = %q, want empty for missing project", sess.LeadSystemPrompt)
	}
	if len(sess.AllowedProjects) != 0 {
		t.Errorf("AllowedProjects = %v, want empty for missing project", sess.AllowedProjects)
	}
}

// TestGitHubSessionStore_ImplementsInterface — compile-time + runtime
// guard against drift in the dispatcher.SessionStore contract.
func TestGitHubSessionStore_ImplementsInterface(t *testing.T) {
	var _ dispatcher.SessionStore = (*githubSessionStore)(nil)
}

// loadFullRegistry seeds a registry on disk with one project, one
// swarm carrying a lead role with a real system prompt, and one
// workflow. Used by the Load happy-path test.
func loadFullRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		full := dir + "/" + rel
		// Build parent dir under /tmp/X/...
		// Caller ensures parents via path layout; t.TempDir cleans up.
		if err := writeDirFile(full, body); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mustWrite("workflows/w-1.md", `---
workflowId: "w-1"
entrypoint: "go"
steps:
  go:
    type: "agent"
    role: "lead"
    prompt: "do the thing"
---
`)
	mustWrite("swarms/s-1.md", `---
swarmId: "s-1"
displayName: "test swarm"
leadRole: "lead"
roles:
  - name: "lead"
    description: "the leader"
    systemPrompt: "You are the lead role."
    runtime:
      image: "vornik/test-runtime:latest"
---
`)
	mustWrite("projects/p-1.yaml", `projectId: "p-1"
swarmId: "s-1"
defaultWorkflowId: "w-1"
`)
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	return reg
}

// writeDirFile writes content to path, creating parent directories
// as needed. Inlined to keep imports tidy.
func writeDirFile(path, content string) error {
	dir := path[:lastSlash(path)]
	if err := osMkdirAll(dir, 0o755); err != nil {
		return err
	}
	return osWriteFile(path, []byte(content), 0o644)
}

// lastSlash returns the index of the last `/` in s.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return 0
}

// TestGitHubSessionStore_Load_FullRegistry — registry has a real
// project + swarm + lead role, so Load assembles a full Session
// with AvailableProjects, AllowedProjects, and LeadSystemPrompt
// populated. Covers the remaining branches in Load.
func TestGitHubSessionStore_Load_FullRegistry(t *testing.T) {
	reg := loadFullRegistry(t)
	store := newGitHubSessionStore(reg, "p-1")
	sess, err := store.Load(context.Background(), conversation.ChannelMessage{SessionID: "x"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(sess.AvailableProjects) != 1 || sess.AvailableProjects[0].ID != "p-1" {
		t.Errorf("AvailableProjects = %v, want one entry [p-1]", sess.AvailableProjects)
	}
	if len(sess.AllowedProjects) != 1 || sess.AllowedProjects[0] != "p-1" {
		t.Errorf("AllowedProjects = %v, want [p-1]", sess.AllowedProjects)
	}
	if sess.LeadSystemPrompt == "" {
		t.Error("LeadSystemPrompt empty; expected BuildLeadSystemPrompt output")
	}
}
