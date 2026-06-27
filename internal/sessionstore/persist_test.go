package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// fakeRepo is an in-memory ChannelSessionRepository for tests.
// Mirrors the Postgres semantics: (kind, session_id) composite
// key, Save upserts, Load returns ErrNotFound for missing.
type fakeRepo struct {
	mu        sync.Mutex
	rows      map[string]map[string]*persistence.ChannelSession
	saveErr   error
	loadErr   error
	deleteErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]map[string]*persistence.ChannelSession{}}
}

func (f *fakeRepo) Load(_ context.Context, kind, sessionID string) (*persistence.ChannelSession, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[kind][sessionID]; ok {
		cp := *row
		cp.History = append([]byte(nil), row.History...)
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (f *fakeRepo) Save(_ context.Context, kind, sessionID, activeProject string, history []byte) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[kind] == nil {
		f.rows[kind] = map[string]*persistence.ChannelSession{}
	}
	f.rows[kind][sessionID] = &persistence.ChannelSession{
		Kind:          kind,
		SessionID:     sessionID,
		ActiveProject: activeProject,
		History:       append([]byte(nil), history...),
		UpdatedAt:     time.Now(),
	}
	return nil
}

func (f *fakeRepo) Delete(_ context.Context, kind, sessionID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows[kind], sessionID)
	return nil
}

// TestPersister_NilRepoNoOp: a Persister built with a nil repo
// makes every call a safe no-op. Lets the channel stores construct
// unconditionally without branching on backend wiring.
func TestPersister_NilRepoNoOp(t *testing.T) {
	p := New(nil, "webchat", zerolog.Nop())
	hist, ap, found, err := p.Load(context.Background(), "sess-1")
	if err != nil || found || hist != nil || ap != "" {
		t.Errorf("nil-repo Load: got hist=%v ap=%q found=%v err=%v; want all zero", hist, ap, found, err)
	}
	if err := p.Save(context.Background(), "sess-1", "proj-x", []chat.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Errorf("nil-repo Save errored: %v", err)
	}
	if err := p.Delete(context.Background(), "sess-1"); err != nil {
		t.Errorf("nil-repo Delete errored: %v", err)
	}
}

// TestPersister_SaveLoadRoundtrip: a Save followed by a Load
// returns the same history + active project, byte-for-byte. This
// is the core "restart-recovery / replica-failover" invariant.
func TestPersister_SaveLoadRoundtrip(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	msgs := []chat.Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	if err := p.Save(context.Background(), "sess-roundtrip", "proj-42", msgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ap, found, err := p.Load(context.Background(), "sess-roundtrip")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatalf("Load: found=false; want true")
	}
	if ap != "proj-42" {
		t.Errorf("active_project = %q, want %q", ap, "proj-42")
	}
	if len(got) != len(msgs) {
		t.Fatalf("history length = %d, want %d", len(got), len(msgs))
	}
	for i, m := range msgs {
		if got[i].Role != m.Role || got[i].Content != m.Content {
			t.Errorf("history[%d] = %+v, want %+v", i, got[i], m)
		}
	}
}

// TestPersister_LoadMissingReturnsNotFound: an unknown session
// reports (nil, "", false, nil) rather than an error — channels
// rely on this to distinguish "fresh session" from "DB failure".
func TestPersister_LoadMissingReturnsNotFound(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "email", zerolog.Nop())
	hist, ap, found, err := p.Load(context.Background(), "never-saved")
	if err != nil {
		t.Errorf("missing-session Load erred: %v", err)
	}
	if found || hist != nil || ap != "" {
		t.Errorf("missing-session Load: found=%v hist=%v ap=%q; want all zero", found, hist, ap)
	}
}

// TestPersister_LoadErrorPropagates: a non-ErrNotFound failure
// returns the error so the channel can log + fall back to its
// in-memory cache rather than silently treating "DB down" as
// "fresh session".
func TestPersister_LoadErrorPropagates(t *testing.T) {
	repo := newFakeRepo()
	repo.loadErr = errors.New("connection refused")
	p := New(repo, "github", zerolog.Nop())
	_, _, _, err := p.Load(context.Background(), "anything")
	if err == nil || !errors.Is(err, repo.loadErr) {
		t.Errorf("expected wrapped repo error, got %v", err)
	}
}

// TestPersister_SaveErrorPropagates: same shape — channels need
// to see the error so they can log it (the in-memory cache still
// has the post-turn state, so the user's session is unaffected).
func TestPersister_SaveErrorPropagates(t *testing.T) {
	repo := newFakeRepo()
	repo.saveErr = errors.New("disk full")
	p := New(repo, "webchat", zerolog.Nop())
	err := p.Save(context.Background(), "sess-1", "p", []chat.Message{{Role: "user", Content: "x"}})
	if err == nil {
		t.Errorf("expected error from failing Save")
	}
}

// TestPersister_CorruptJSONServesEmpty: a row whose history bytes
// can't be unmarshalled (schema drift, hand-edit) returns empty
// history instead of failing. The next successful Save rewrites.
func TestPersister_CorruptJSONServesEmpty(t *testing.T) {
	repo := newFakeRepo()
	repo.rows["telegram"] = map[string]*persistence.ChannelSession{
		"sess-corrupt": {
			Kind:      "telegram",
			SessionID: "sess-corrupt",
			History:   []byte("this is not json"),
		},
	}
	p := New(repo, "telegram", zerolog.Nop())
	hist, _, found, err := p.Load(context.Background(), "sess-corrupt")
	if err != nil {
		t.Errorf("corrupt Load erred: %v", err)
	}
	if !found {
		t.Errorf("corrupt-but-present row should still report found=true")
	}
	if hist != nil {
		t.Errorf("corrupt history should yield nil slice; got %v", hist)
	}
}

// TestPersister_DeleteRemovesRow: the webchat "clear chat" path
// + the future stale-session sweeper both depend on Delete
// actually removing the row, not just clearing fields.
func TestPersister_DeleteRemovesRow(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	_ = p.Save(context.Background(), "sess-del", "proj-x", []chat.Message{{Role: "user", Content: "x"}})
	if err := p.Delete(context.Background(), "sess-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, _, found, _ := p.Load(context.Background(), "sess-del")
	if found {
		t.Errorf("session should be gone after Delete")
	}
}

// TestPersister_KindIsolation: two channels with the same
// session_id but different kinds don't collide. The (kind,
// session_id) composite PK is the contract every store relies on.
func TestPersister_KindIsolation(t *testing.T) {
	repo := newFakeRepo()
	pWeb := New(repo, "webchat", zerolog.Nop())
	pEmail := New(repo, "email", zerolog.Nop())
	const sessID = "42"
	if err := pWeb.Save(context.Background(), sessID, "proj-web", []chat.Message{{Role: "user", Content: "web-side"}}); err != nil {
		t.Fatalf("webchat Save: %v", err)
	}
	if err := pEmail.Save(context.Background(), sessID, "proj-email", []chat.Message{{Role: "user", Content: "email-side"}}); err != nil {
		t.Fatalf("email Save: %v", err)
	}
	wh, _, _, _ := pWeb.Load(context.Background(), sessID)
	eh, _, _, _ := pEmail.Load(context.Background(), sessID)
	if len(wh) == 0 || wh[0].Content != "web-side" {
		t.Errorf("webchat side leaked: %v", wh)
	}
	if len(eh) == 0 || eh[0].Content != "email-side" {
		t.Errorf("email side leaked: %v", eh)
	}
}

// TestPersister_SaveEmptyHistoryWritesEmptyJSON: callers
// typically guard against empty Result.Messages, but Save itself
// should still produce a valid JSON payload (an empty array) so
// Postgres's NOT NULL constraint is satisfied.
func TestPersister_SaveEmptyHistoryWritesEmptyJSON(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	if err := p.Save(context.Background(), "sess-empty", "proj", nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	row, _ := repo.Load(context.Background(), "webchat", "sess-empty")
	if row == nil {
		t.Fatalf("row should exist after Save")
	}
	// Should round-trip as a valid JSON array (`[]` or `null`).
	var probe []chat.Message
	if err := json.Unmarshal(row.History, &probe); err != nil {
		t.Errorf("Save produced invalid JSON: %v (bytes=%q)", err, row.History)
	}
}
