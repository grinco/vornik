package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// TestW3Sess_MultimodalBlocksRoundtrip: a message whose payload is a
// multimodal block slice (text + image) must survive a Save→Load cycle
// with Blocks intact and Content empty. Channels carrying screenshots
// (webchat paste, slack file_share) rely on this — losing the image on
// restart would silently drop context the model already saw.
func TestW3Sess_MultimodalBlocksRoundtrip(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	in := []chat.Message{{
		Role: "user",
		Blocks: []chat.ContentBlock{
			chat.TextBlock("describe this"),
			chat.ImageBlock("data:image/png;base64,AAAA"),
		},
	}}
	if err := p.Save(context.Background(), "sess-mm", "proj", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, found, err := p.Load(context.Background(), "sess-mm")
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if len(got) != 1 {
		t.Fatalf("history len = %d, want 1", len(got))
	}
	m := got[0]
	if m.Content != "" {
		t.Errorf("Content should be empty when Blocks set; got %q", m.Content)
	}
	if len(m.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2", len(m.Blocks))
	}
	if m.Blocks[0].Type != "text" || m.Blocks[0].Text != "describe this" {
		t.Errorf("block[0] = %+v", m.Blocks[0])
	}
	if m.Blocks[1].Type != "image_url" || m.Blocks[1].ImageURL == nil ||
		m.Blocks[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("block[1] = %+v", m.Blocks[1])
	}
}

// TestW3Sess_ToolCallRoundtrip: assistant turns that requested a tool
// carry ToolCalls (id, type, function name + JSON args). The OpenAI
// continuation contract requires replaying these verbatim on the next
// request, so they must persist across a restart unaltered.
func TestW3Sess_ToolCallRoundtrip(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	in := []chat.Message{
		{Role: "user", Content: "weather in Prague?"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{
			ID:   "call_abc",
			Type: "function",
			Function: chat.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"Prague"}`,
			},
		}}},
		{Role: "tool", ToolCallID: "call_abc", Name: "get_weather", Content: "12C"},
	}
	if err := p.Save(context.Background(), "sess-tool", "", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, _, err := p.Load(context.Background(), "sess-tool")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("history len = %d, want 3", len(got))
	}
	tc := got[1].ToolCalls
	if len(tc) != 1 || tc[0].ID != "call_abc" || tc[0].Function.Name != "get_weather" ||
		tc[0].Function.Arguments != `{"city":"Prague"}` {
		t.Errorf("tool call not preserved: %+v", tc)
	}
	if got[2].ToolCallID != "call_abc" || got[2].Name != "get_weather" || got[2].Content != "12C" {
		t.Errorf("tool result not preserved: %+v", got[2])
	}
}

// TestW3Sess_ToolCallExtraContentRoundtrip: ExtraContent carries
// provider-specific replay metadata (Gemini's thought_signature).
// Dropping it makes Gemini 3 reject the continuation, so it must
// survive the persist boundary byte-for-byte.
func TestW3Sess_ToolCallExtraContentRoundtrip(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	in := []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{{
		ID:           "c1",
		Type:         "function",
		Function:     chat.FunctionCall{Name: "f", Arguments: "{}"},
		ExtraContent: json.RawMessage(`{"google":{"thought_signature":"sig123"}}`),
	}}}}
	if err := p.Save(context.Background(), "sess-extra", "", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, _, err := p.Load(context.Background(), "sess-extra")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ec := got[0].ToolCalls[0].ExtraContent
	var probe struct {
		Google struct {
			Sig string `json:"thought_signature"`
		} `json:"google"`
	}
	if err := json.Unmarshal(ec, &probe); err != nil {
		t.Fatalf("ExtraContent not valid JSON after roundtrip: %v (%q)", err, ec)
	}
	if probe.Google.Sig != "sig123" {
		t.Errorf("thought_signature lost: %q", ec)
	}
}

// TestW3Sess_OverwriteReplacesHistory: a second Save for the same
// (kind, session_id) fully replaces the prior history — channels store
// the authoritative post-turn slice, never append-merge. A stale longer
// history must not bleed through after a shorter rewrite.
func TestW3Sess_OverwriteReplacesHistory(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "email", zerolog.Nop())
	first := []chat.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	if err := p.Save(context.Background(), "s", "p1", first); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second := []chat.Message{{Role: "user", Content: "reset"}}
	if err := p.Save(context.Background(), "s", "p2", second); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, ap, _, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "reset" {
		t.Errorf("overwrite leaked old history: %+v", got)
	}
	if ap != "p2" {
		t.Errorf("active_project = %q, want p2", ap)
	}
}

// TestW3Sess_ActiveProjectClearedOnRewrite: pinning a project then
// rewriting with an empty active project clears it. The "unpin project"
// affordance depends on Save honoring an empty string rather than
// preserving the prior value.
func TestW3Sess_ActiveProjectClearedOnRewrite(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	msgs := []chat.Message{{Role: "user", Content: "x"}}
	if err := p.Save(context.Background(), "s", "pinned", msgs); err != nil {
		t.Fatalf("Save pinned: %v", err)
	}
	if err := p.Save(context.Background(), "s", "", msgs); err != nil {
		t.Fatalf("Save unpinned: %v", err)
	}
	_, ap, _, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ap != "" {
		t.Errorf("active_project should be cleared; got %q", ap)
	}
}

// TestW3Sess_LongHistoryFidelity: a long conversation persists with
// ordering and per-message content fully intact. Guards against silent
// truncation or reordering at the marshal boundary for sessions that
// accumulate many turns before a restart.
func TestW3Sess_LongHistoryFidelity(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "github", zerolog.Nop())
	const n = 200
	in := make([]chat.Message, n)
	for i := range in {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		in[i] = chat.Message{Role: role, Content: fmt.Sprintf("msg-%d", i)}
	}
	if err := p.Save(context.Background(), "long", "p", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, _, err := p.Load(context.Background(), "long")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != n {
		t.Fatalf("history len = %d, want %d", len(got), n)
	}
	for i := range got {
		want := fmt.Sprintf("msg-%d", i)
		if got[i].Content != want {
			t.Fatalf("history[%d].Content = %q, want %q", i, got[i].Content, want)
		}
	}
}

// TestW3Sess_UnicodeAndControlCharsRoundtrip: content with emoji,
// CJK, newlines and embedded quotes/braces must survive JSON
// marshalling unchanged. A naive concatenation-based serializer would
// corrupt these; this proves the json boundary is used.
func TestW3Sess_UnicodeAndControlCharsRoundtrip(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "telegram", zerolog.Nop())
	tricky := "emoji 🚀 cjk 日本語 \"quoted\" {\"json\":true}\nline2\ttab"
	in := []chat.Message{{Role: "user", Content: tricky}}
	if err := p.Save(context.Background(), "s", "p", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, _, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Content != tricky {
		t.Errorf("content corrupted: got %q want %q", got[0].Content, tricky)
	}
}

// TestW3Sess_EmptyHistoryRoundtripsAsEmptySlice: saving nil history
// then loading yields a found row with no messages (not an error, not
// a phantom message). Channels distinguish "row exists, empty" from
// "no row" via the found flag.
func TestW3Sess_EmptyHistoryRoundtripsAsEmptySlice(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	if err := p.Save(context.Background(), "s", "p", nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ap, found, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Errorf("found=false; an explicitly saved empty session should report found=true")
	}
	if len(got) != 0 {
		t.Errorf("history len = %d, want 0", len(got))
	}
	if ap != "p" {
		t.Errorf("active_project = %q, want p", ap)
	}
}

// TestW3Sess_DeleteIdempotent: Delete on a never-saved session must
// not error. The stale-session sweeper and "clear chat" both fire
// Delete without first checking existence, so it has to be idempotent.
func TestW3Sess_DeleteIdempotent(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	if err := p.Delete(context.Background(), "ghost"); err != nil {
		t.Errorf("Delete of missing session erred: %v", err)
	}
	// Second delete after a real save+delete also no-ops.
	_ = p.Save(context.Background(), "real", "p", []chat.Message{{Role: "user", Content: "x"}})
	if err := p.Delete(context.Background(), "real"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := p.Delete(context.Background(), "real"); err != nil {
		t.Errorf("repeat Delete erred: %v", err)
	}
}

// TestW3Sess_DeleteErrorPropagates: a non-idempotent repo failure
// (e.g. DB connection lost mid-sweep) surfaces to the caller so the
// sweeper can retry rather than mark the row reclaimed.
func TestW3Sess_DeleteErrorPropagates(t *testing.T) {
	repo := newFakeRepo()
	repo.deleteErr = errors.New("connection reset")
	p := New(repo, "webchat", zerolog.Nop())
	if err := p.Delete(context.Background(), "s"); err == nil {
		t.Errorf("expected Delete error to propagate")
	}
}

// TestW3Sess_DeleteThenSaveResurrects: deleting a session and saving
// again under the same key produces a fresh row (clear-chat then a new
// message). Proves Delete doesn't tombstone the (kind, session_id) key.
func TestW3Sess_DeleteThenSaveResurrects(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	_ = p.Save(context.Background(), "s", "old", []chat.Message{{Role: "user", Content: "old"}})
	if err := p.Delete(context.Background(), "s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := p.Save(context.Background(), "s", "new", []chat.Message{{Role: "user", Content: "new"}}); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	got, ap, found, err := p.Load(context.Background(), "s")
	if err != nil || !found {
		t.Fatalf("Load: found=%v err=%v", found, err)
	}
	if len(got) != 1 || got[0].Content != "new" || ap != "new" {
		t.Errorf("resurrected session wrong: hist=%+v ap=%q", got, ap)
	}
}

// TestW3Sess_CorruptArrayElementServesEmpty: history bytes that parse
// as JSON but aren't a []chat.Message (an object instead of an array)
// must fail-soft to empty, not crash the channel. Distinct from the
// existing "not json at all" case.
func TestW3Sess_CorruptArrayElementServesEmpty(t *testing.T) {
	repo := newFakeRepo()
	repo.rows["webchat"] = map[string]*persistence.ChannelSession{
		"s": {
			Kind:      "webchat",
			SessionID: "s",
			History:   []byte(`{"not":"an array"}`),
		},
	}
	p := New(repo, "webchat", zerolog.Nop())
	hist, _, found, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Errorf("corrupt Load erred: %v", err)
	}
	if !found {
		t.Errorf("present-but-corrupt row should report found=true")
	}
	if hist != nil {
		t.Errorf("corrupt history should yield nil; got %+v", hist)
	}
}

// TestW3Sess_EmptyBytesHistoryNoUnmarshal: a row with zero-length
// History bytes (DB default / NULL coerced to empty) returns nil
// history without attempting an unmarshal. Load guards len(History)>0
// to avoid logging a spurious corruption warning on legitimately empty
// rows.
func TestW3Sess_EmptyBytesHistoryNoUnmarshal(t *testing.T) {
	repo := newFakeRepo()
	repo.rows["email"] = map[string]*persistence.ChannelSession{
		"s": {Kind: "email", SessionID: "s", ActiveProject: "p", History: []byte{}},
	}
	p := New(repo, "email", zerolog.Nop())
	hist, ap, found, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found || ap != "p" || hist != nil {
		t.Errorf("empty-bytes row: found=%v ap=%q hist=%+v", found, ap, hist)
	}
}

// TestW3Sess_NilRepoDeleteAndLoadConsistent: with a nil repo every op
// is a no-op, so a Save (silently dropped) followed by a Load must
// still report not-found — the in-memory map is the caller's, not this
// wrapper's. Confirms the wrapper holds no hidden state.
func TestW3Sess_NilRepoHoldsNoState(t *testing.T) {
	p := New(nil, "webchat", zerolog.Nop())
	if err := p.Save(context.Background(), "s", "p", []chat.Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("nil-repo Save: %v", err)
	}
	_, _, found, err := p.Load(context.Background(), "s")
	if err != nil {
		t.Fatalf("nil-repo Load: %v", err)
	}
	if found {
		t.Errorf("nil-repo wrapper must not retain state across Save/Load")
	}
}

// TestW3Sess_NilPointerReceiverSafe: a nil *Persister is a valid
// no-op on every method. Channel constructors may leave the field nil
// when persistence is disabled; method calls must not panic.
func TestW3Sess_NilPointerReceiverSafe(t *testing.T) {
	var p *Persister
	if _, _, found, err := p.Load(context.Background(), "s"); found || err != nil {
		t.Errorf("nil receiver Load: found=%v err=%v", found, err)
	}
	if err := p.Save(context.Background(), "s", "p", []chat.Message{{Role: "user", Content: "x"}}); err != nil {
		t.Errorf("nil receiver Save: %v", err)
	}
	if err := p.Delete(context.Background(), "s"); err != nil {
		t.Errorf("nil receiver Delete: %v", err)
	}
}

// TestW3Sess_ConcurrentSaveLoadDistinctSessions: many goroutines
// saving and loading distinct sessions through one Persister must not
// race or lose data. Each replica handles concurrent channel turns;
// the wrapper has to be safe under -race. Run with: go test -race.
func TestW3Sess_ConcurrentSaveLoadDistinctSessions(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "webchat", zerolog.Nop())
	const workers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sess := fmt.Sprintf("sess-%d", id)
			want := fmt.Sprintf("payload-%d", id)
			if err := p.Save(context.Background(), sess, "p", []chat.Message{{Role: "user", Content: want}}); err != nil {
				errCh <- err
				return
			}
			got, _, found, err := p.Load(context.Background(), sess)
			if err != nil {
				errCh <- err
				return
			}
			if !found || len(got) != 1 || got[0].Content != want {
				errCh <- fmt.Errorf("session %d readback mismatch: found=%v hist=%+v", id, found, got)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestW3Sess_ConcurrentReadWriteSameSession: hammering one session key
// with concurrent Save/Load/Delete must not panic or race. The final
// Load is only asserted to be internally consistent (found⇒parseable),
// since interleaving makes the exact contents nondeterministic.
func TestW3Sess_ConcurrentReadWriteSameSession(t *testing.T) {
	repo := newFakeRepo()
	p := New(repo, "slack", zerolog.Nop())
	const iters = 100
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = p.Save(context.Background(), "hot", "p", []chat.Message{{Role: "user", Content: fmt.Sprintf("v%d", i)}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, _, found, err := p.Load(context.Background(), "hot"); err != nil {
				t.Errorf("Load erred: %v", err)
				_ = found
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = p.Delete(context.Background(), "hot")
		}
	}()
	wg.Wait()
	// Final state is whatever the last write/delete left; just ensure
	// a clean Load doesn't error or corrupt.
	if _, _, _, err := p.Load(context.Background(), "hot"); err != nil {
		t.Errorf("final Load erred: %v", err)
	}
}
