// Package telegram: tests for the per-bot taskNotifTracker (in-memory
// chat-message → task map) and the pure JSON helpers
// (matchChoiceFromText, taskTitleFromPayload, renderInbox,
// handleInbox). All exercised in isolation — no network, no DB.
package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// --- taskNotifTracker -------------------------------------------------

func TestTaskNotifTracker_RememberLookupCycle(t *testing.T) {
	tr := newTaskNotifTracker()
	tr.remember(1001, 42, "task-1", "proj-1")
	taskID, projID, ok := tr.lookup(1001, 42)
	if !ok || taskID != "task-1" || projID != "proj-1" {
		t.Errorf("lookup: got (%q, %q, %v), want (task-1, proj-1, true)", taskID, projID, ok)
	}
}

func TestTaskNotifTracker_LookupMissing(t *testing.T) {
	tr := newTaskNotifTracker()
	if _, _, ok := tr.lookup(999, 1); ok {
		t.Errorf("expected lookup of unknown key to return ok=false")
	}
}

func TestTaskNotifTracker_OverwritesSameKey(t *testing.T) {
	tr := newTaskNotifTracker()
	tr.remember(1, 1, "task-a", "p1")
	tr.remember(1, 1, "task-b", "p1")
	taskID, _, _ := tr.lookup(1, 1)
	if taskID != "task-b" {
		t.Errorf("overwrite: got %q, want task-b", taskID)
	}
}

func TestTaskNotifTracker_PruneOldEntries(t *testing.T) {
	tr := newTaskNotifTracker()
	// Inject a stale entry directly.
	tr.entries[taskNotifKey{ChatID: 99, MessageID: 99}] = taskNotifEntry{
		TaskID:    "old-task",
		ProjectID: "p1",
		SentAt:    time.Now().Add(-8 * 24 * time.Hour),
	}
	// remember triggers prune.
	tr.remember(1, 1, "fresh-task", "p1")
	if _, _, ok := tr.lookup(99, 99); ok {
		t.Errorf("stale entry should have been pruned")
	}
}

// --- matchChoiceFromText ---------------------------------------------

func TestMatchChoiceFromText(t *testing.T) {
	cases := []struct {
		name string
		meta string
		text string
		want string
	}{
		{"nil-meta", "", "anything", ""},
		{"invalid-json", "{not-json", "anything", ""},
		{"empty-options", `{"options":[]}`, "yes", ""},
		{"id-match", `{"options":[{"id":"yes","label":"Yes, ship it"},{"id":"no","label":"Block"}]}`, "yes", "yes"},
		{"label-match-case-insensitive", `{"options":[{"id":"yes","label":"Yes, ship it"}]}`, "yes, ship it", "yes"},
		{"contains-id", `{"options":[{"id":"a","label":"Alpha"},{"id":"b","label":"Beta"}]}`, "let's go with b", "b"},
		{"empty-text", `{"options":[{"id":"x","label":"X"}]}`, "", ""},
		{"whitespace-only-text", `{"options":[{"id":"x","label":"X"}]}`, "   ", ""},
		{"no-match", `{"options":[{"id":"a","label":"Alpha"}]}`, "completely different", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := matchChoiceFromText([]byte(tc.meta), tc.text)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- taskTitleFromPayload --------------------------------------------

func TestTaskTitleFromPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		max     int
		want    string
	}{
		{"empty", "", 80, ""},
		{"invalid-json", "{not-json", 80, ""},
		{"no-prompt", `{"context":{}}`, 80, ""},
		{"whitespace-prompt", `{"context":{"prompt":"   "}}`, 80, ""},
		{"short-fits", `{"context":{"prompt":"do the thing"}}`, 80, "do the thing"},
		{"long-gets-truncated", `{"context":{"prompt":"` + strings.Repeat("x", 100) + `"}}`, 20, strings.Repeat("x", 17) + "..."},
		{"max-too-small-no-truncate", `{"context":{"prompt":"long prompt text"}}`, 3, "long prompt text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := taskTitleFromPayload([]byte(tc.payload), tc.max)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- additional tests imported from coverage-follow-up Z --------------

func TestTaskNotifTracker_RememberAndLookup(t *testing.T) {
	tr := newTaskNotifTracker()
	tr.remember(42, 1001, "task-1", "proj-1")
	gotTask, gotProj, ok := tr.lookup(42, 1001)
	if !ok {
		t.Fatal("lookup after remember: ok=false")
	}
	if gotTask != "task-1" || gotProj != "proj-1" {
		t.Errorf("lookup: got %q/%q, want task-1/proj-1", gotTask, gotProj)
	}
}

func TestTaskNotifTracker_LookupMisses(t *testing.T) {
	tr := newTaskNotifTracker()
	tr.remember(42, 1001, "task-1", "proj-1")
	if _, _, ok := tr.lookup(42, 9999); ok {
		t.Error("lookup of unknown msg-id: ok=true, want false")
	}
	if _, _, ok := tr.lookup(99, 1001); ok {
		t.Error("lookup with wrong chat: ok=true, want false")
	}
}

// Prune is normally driven through remember (called at the end). We
// can drive it directly by lying about the SentAt of an entry that's
// older than 7 days.
func TestTaskNotifTracker_PruneDropsAgedEntries(t *testing.T) {
	tr := newTaskNotifTracker()
	tr.entries[taskNotifKey{ChatID: 1, MessageID: 1}] = taskNotifEntry{
		TaskID: "old", SentAt: time.Now().Add(-30 * 24 * time.Hour),
	}
	tr.entries[taskNotifKey{ChatID: 1, MessageID: 2}] = taskNotifEntry{
		TaskID: "fresh", SentAt: time.Now(),
	}
	// remember fires prune. The fresh entry survives.
	tr.remember(1, 3, "another-fresh", "p")
	if _, _, ok := tr.lookup(1, 1); ok {
		t.Error("aged entry survived prune")
	}
	if _, _, ok := tr.lookup(1, 2); !ok {
		t.Error("fresh entry pruned by mistake")
	}
}

// --- matchChoiceFromText -------------------------------------------------

func TestMatchChoiceFromText_MatchesByID(t *testing.T) {
	meta := []byte(`{"options":[{"id":"yes","label":"Yes go"},{"id":"no","label":"Cancel"}]}`)
	if got := matchChoiceFromText(meta, "yes"); got != "yes" {
		t.Errorf("got %q, want yes", got)
	}
}

func TestMatchChoiceFromText_MatchesByLabel(t *testing.T) {
	meta := []byte(`{"options":[{"id":"approve","label":"Yes go"},{"id":"reject","label":"Cancel"}]}`)
	if got := matchChoiceFromText(meta, "Cancel"); got != "reject" {
		t.Errorf("got %q, want reject", got)
	}
}

func TestMatchChoiceFromText_LooseSubstring(t *testing.T) {
	meta := []byte(`{"options":[{"id":"yes","label":"Yes go"}]}`)
	if got := matchChoiceFromText(meta, "go with yes please"); got != "yes" {
		t.Errorf("got %q, want yes (loose match)", got)
	}
}

func TestMatchChoiceFromText_NoMatch(t *testing.T) {
	meta := []byte(`{"options":[{"id":"yes","label":"Yes"}]}`)
	if got := matchChoiceFromText(meta, "totally unrelated"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestMatchChoiceFromText_EmptyMeta(t *testing.T) {
	if got := matchChoiceFromText(nil, "yes"); got != "" {
		t.Errorf("nil meta: got %q, want empty", got)
	}
	if got := matchChoiceFromText([]byte(`{}`), "yes"); got != "" {
		t.Errorf("no-options meta: got %q, want empty", got)
	}
}

func TestMatchChoiceFromText_EmptyText(t *testing.T) {
	meta := []byte(`{"options":[{"id":"yes"}]}`)
	if got := matchChoiceFromText(meta, ""); got != "" {
		t.Errorf("empty text: got %q, want empty", got)
	}
	if got := matchChoiceFromText(meta, "   "); got != "" {
		t.Errorf("whitespace text: got %q, want empty", got)
	}
}

func TestMatchChoiceFromText_MalformedMeta(t *testing.T) {
	if got := matchChoiceFromText([]byte("not json"), "yes"); got != "" {
		t.Errorf("malformed meta: got %q, want empty", got)
	}
}

// --- taskTitleFromPayload ------------------------------------------------

func TestTaskTitleFromPayload_HappyPath(t *testing.T) {
	payload := []byte(`{"context":{"prompt":"Migrate auth to JWT"}}`)
	if got := taskTitleFromPayload(payload, 80); got != "Migrate auth to JWT" {
		t.Errorf("got %q", got)
	}
}

func TestTaskTitleFromPayload_TruncatesLong(t *testing.T) {
	long := strings.Repeat("a", 100)
	payload, _ := json.Marshal(map[string]any{"context": map[string]string{"prompt": long}})
	got := taskTitleFromPayload(payload, 30)
	if len(got) != 30 {
		t.Errorf("len = %d, want 30", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated title should end with ...; got %q", got)
	}
}

func TestTaskTitleFromPayload_EmptyOrMissing(t *testing.T) {
	if got := taskTitleFromPayload(nil, 80); got != "" {
		t.Errorf("nil payload: got %q", got)
	}
	if got := taskTitleFromPayload([]byte("not json"), 80); got != "" {
		t.Errorf("invalid: got %q", got)
	}
	if got := taskTitleFromPayload([]byte(`{}`), 80); got != "" {
		t.Errorf("empty payload: got %q", got)
	}
	if got := taskTitleFromPayload([]byte(`{"context":{"prompt":"   "}}`), 80); got != "" {
		t.Errorf("whitespace prompt: got %q", got)
	}
}

// --- renderInbox / handleInbox -------------------------------------------

func TestRenderInbox_NoTaskRepo(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	out, err := b.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("renderInbox: %v", err)
	}
	if !strings.Contains(out, "Inbox unavailable") {
		t.Errorf("expected 'Inbox unavailable' fallback; got %q", out)
	}
}

func TestRenderInbox_EmptyList(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, nil
		},
	}
	b := newBareTestBot(t, BotConfig{Token: "t"})
	WithTaskRepository(repo)(b)

	out, err := b.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("renderInbox: %v", err)
	}
	if !strings.Contains(out, "📭") {
		t.Errorf("expected empty-state emoji; got %q", out)
	}
}

func TestRenderInbox_PopulatedList(t *testing.T) {
	phase := "review"
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			// We expect the filter to scope to AWAITING_INPUT.
			if filter.Status == nil || *filter.Status != persistence.TaskStatusAwaitingInput {
				t.Errorf("filter.Status: got %v, want AWAITING_INPUT", filter.Status)
			}
			payload, _ := json.Marshal(map[string]any{"context": map[string]string{"prompt": "Approve deploy"}})
			return []*persistence.Task{
				{ID: "t-1", Payload: payload, CurrentPhase: &phase},
				{ID: "t-2"},
			}, nil
		},
	}
	b := newBareTestBot(t, BotConfig{Token: "t"})
	WithTaskRepository(repo)(b)

	out, err := b.renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatalf("renderInbox: %v", err)
	}
	if !strings.Contains(out, "Approve deploy") {
		t.Errorf("expected payload prompt in summary; got %q", out)
	}
	if !strings.Contains(out, "t-2") {
		t.Errorf("expected task ID for prompt-less task; got %q", out)
	}
	if !strings.Contains(out, "review") {
		t.Errorf("expected phase tag; got %q", out)
	}
}

func TestHandleInbox_NilBot_ExactWording(t *testing.T) {
	got := handleInbox(context.Background(), nil, 100, 0)
	if got != "Bot unavailable." {
		t.Errorf("nil-bot fallback: got %q", got)
	}
}

func TestHandleInbox_DelegatesToRenderInbox(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	got := handleInbox(context.Background(), b, 100, 0)
	if !strings.Contains(got, "Inbox unavailable") {
		t.Errorf("expected delegation to renderInbox; got %q", got)
	}
}
