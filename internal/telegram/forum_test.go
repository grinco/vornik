package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// ---------------------------------------------------------------
// In-memory stubs — kept local because the production
// persistence layer doesn't ship interface mocks for these.
// ---------------------------------------------------------------

type stubThreadRepo struct {
	mu        sync.Mutex
	byTask    map[string]*persistence.TelegramTaskThread
	byPair    map[string]*persistence.TelegramTaskThread
	insertErr error
}

func newStubThreadRepo() *stubThreadRepo {
	return &stubThreadRepo{
		byTask: map[string]*persistence.TelegramTaskThread{},
		byPair: map[string]*persistence.TelegramTaskThread{},
	}
}

func (s *stubThreadRepo) pairKey(chatID, threadID int64) string {
	return key2(chatID, threadID)
}

func key2(a, b int64) string {
	return string([]byte{
		byte(a >> 56), byte(a >> 48), byte(a >> 40), byte(a >> 32),
		byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a),
		byte(b >> 56), byte(b >> 48), byte(b >> 40), byte(b >> 32),
		byte(b >> 24), byte(b >> 16), byte(b >> 8), byte(b),
	})
}

func (s *stubThreadRepo) GetByTask(_ context.Context, taskID string) (*persistence.TelegramTaskThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byTask[taskID]; ok {
		return t, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubThreadRepo) GetByThread(_ context.Context, chatID, threadID int64) (*persistence.TelegramTaskThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byPair[s.pairKey(chatID, threadID)]; ok {
		return t, nil
	}
	return nil, persistence.ErrNotFound
}

func (s *stubThreadRepo) Insert(_ context.Context, t *persistence.TelegramTaskThread) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	pk := s.pairKey(t.ChatID, t.ThreadID)
	if _, exists := s.byPair[pk]; exists {
		return persistence.ErrDuplicateKey
	}
	if t.ID == "" {
		t.ID = "stub-" + t.TaskID
	}
	s.byTask[t.TaskID] = t
	s.byPair[pk] = t
	return nil
}

func (s *stubThreadRepo) MarkClosed(_ context.Context, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.byTask[taskID]; ok && t.ClosedAt == nil {
		now := time.Now().UTC()
		t.ClosedAt = &now
	}
	return nil
}

// ---------------------------------------------------------------
// Forum API client tests
// ---------------------------------------------------------------

func TestForumEnabled(t *testing.T) {
	chatClient := chat.NewClient("https://api.example.com", "k", "m")

	cases := []struct {
		name string
		opts []BotOption
		want bool
	}{
		{"both unset", nil, false},
		{"only chat", []BotOption{WithForumChatID(-100, 0)}, false},
		{"only repo", []BotOption{WithTelegramThreadRepository(newStubThreadRepo())}, false},
		{"both set", []BotOption{WithForumChatID(-100, 0), WithTelegramThreadRepository(newStubThreadRepo())}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bot, err := NewBot(BotConfig{Token: "t"}, chatClient, c.opts...)
			if err != nil {
				t.Fatalf("NewBot: %v", err)
			}
			if got := bot.forumEnabled(); got != c.want {
				t.Errorf("forumEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCreateForumTopic_HappyPath(t *testing.T) {
	var seen struct {
		chatID    int64
		name      string
		iconColor int
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "createForumTopic") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen.chatID = int64(body["chat_id"].(float64))
		seen.name = body["name"].(string)
		if v, ok := body["icon_color"].(float64); ok {
			seen.iconColor = int(v)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":42,"name":"x"}}`))
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1009876543210, 7322096), // blue
		WithTelegramThreadRepository(newStubThreadRepo()),
	)

	tid, err := bot.createForumTopic(context.Background(), "demo")
	if err != nil {
		t.Fatalf("createForumTopic: %v", err)
	}
	if tid != 42 {
		t.Errorf("thread_id: got %d want 42", tid)
	}
	if seen.chatID != -1009876543210 {
		t.Errorf("chat_id: got %d", seen.chatID)
	}
	if seen.name != "demo" {
		t.Errorf("name: got %q", seen.name)
	}
	if seen.iconColor != 7322096 {
		t.Errorf("icon_color should be passed through for valid palette value; got %d", seen.iconColor)
	}
}

func TestCreateForumTopic_OmitsInvalidIconColor(t *testing.T) {
	var sawIcon atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["icon_color"]; ok {
			sawIcon.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":1}}`))
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 12345), // not a valid palette value
		WithTelegramThreadRepository(newStubThreadRepo()),
	)

	if _, err := bot.createForumTopic(context.Background(), "x"); err != nil {
		t.Fatalf("createForumTopic: %v", err)
	}
	if sawIcon.Load() {
		t.Error("invalid icon_color must be omitted from the request payload")
	}
}

func TestCreateForumTopic_TelegramErrorSurfacesAsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"not enough rights"}`))
	}))
	defer server.Close()

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(newStubThreadRepo()),
	)

	_, err := bot.createForumTopic(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "not enough rights") {
		t.Errorf("expected Telegram error to surface, got %v", err)
	}
}

func TestBuildForumTopicName(t *testing.T) {
	// The chip is intentionally short — the full task prompt is
	// posted in the thread body via formatTaskBrief, not jammed
	// into the topic name. Format: "<project-id> • <8-char-suffix>".
	cases := []struct {
		name      string
		taskID    string
		projectID string
		payload   string
		want      string
	}{
		{
			name:      "project plus 8-char suffix",
			taskID:    "task_abcdef12345678",
			projectID: "ibkr-trader",
			payload:   `{"context":{"prompt":"Research Prague Castle opening hours"}}`,
			want:      "ibkr-trader • 12345678",
		},
		{
			name:      "no project falls back to task id",
			taskID:    "task_xyz",
			projectID: "",
			payload:   `{}`,
			want:      "task_xyz",
		},
		{
			name:      "prompt length no longer affects chip — only project + suffix do",
			taskID:    "task_aaaaaaaaaaaaaaaa",
			projectID: "vornik-autocoder",
			payload:   `{"context":{"prompt":"` + strings.Repeat("z", 500) + `"}}`,
			want:      "vornik-autocoder • aaaaaaaa",
		},
		{
			name:      "long project id is defensively truncated to 128 chars",
			taskID:    "task_short",
			projectID: strings.Repeat("p", 200),
			payload:   `{}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildForumTopicName(&persistence.Task{
				ID:        c.taskID,
				ProjectID: c.projectID,
				Payload:   []byte(c.payload),
			})
			if c.want != "" && got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
			if len(got) > maxTopicNameLen {
				t.Errorf("topic name must be ≤%d chars; got %d", maxTopicNameLen, len(got))
			}
			if got == "" {
				t.Error("topic name must never be empty")
			}
		})
	}
}

// TestFormatTaskBrief asserts the first-message brief that
// ensureTaskThread posts when a topic is created. The chip carries
// scanning context; the brief carries the actual task intent.
func TestFormatTaskBrief(t *testing.T) {
	t.Run("includes id, project, type, priority, and full prompt", func(t *testing.T) {
		task := &persistence.Task{
			ID:        "task_20260511180002_e008225fbece811a",
			ProjectID: "ibkr-trader",
			Priority:  50,
			Payload:   []byte(`{"taskType":"trading","context":{"prompt":"Run one trading tick on the ibkr-trader watchlist per the strategy."}}`),
		}
		got := formatTaskBrief(task)
		mustContain := []string{
			"task_20260511180002_e008225fbece811a",
			"ibkr-trader",
			"trading",
			"priority: 50",
			"Run one trading tick on the ibkr-trader watchlist per the strategy.",
		}
		for _, s := range mustContain {
			if !strings.Contains(got, s) {
				t.Errorf("brief missing %q\nfull: %s", s, got)
			}
		}
	})

	t.Run("falls back to id+metadata when payload has no prompt", func(t *testing.T) {
		task := &persistence.Task{
			ID:        "task_no_prompt",
			ProjectID: "p",
			Priority:  5,
			Payload:   []byte(`{}`),
		}
		got := formatTaskBrief(task)
		if !strings.Contains(got, "task_no_prompt") {
			t.Errorf("brief must include task id even without prompt; got %q", got)
		}
	})

	t.Run("very long prompts are truncated below Telegram's sendMessage cap", func(t *testing.T) {
		task := &persistence.Task{
			ID:        "task_xl",
			ProjectID: "p",
			Priority:  5,
			Payload: []byte(`{"context":{"prompt":"` +
				strings.Repeat("a", 10000) + `"}}`),
		}
		got := formatTaskBrief(task)
		if len(got) >= 4096 {
			t.Errorf("brief must stay under Telegram's 4096-char sendMessage cap; got %d", len(got))
		}
		if !strings.Contains(got, "truncated") {
			t.Errorf("expected truncation marker in long-prompt brief; got %q", got[len(got)-200:])
		}
	})
}

func TestEnsureTaskThread_CreatesOnFirstCallIdempotentOnSecond(t *testing.T) {
	var createCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "createForumTopic") {
			createCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":777}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	repo := newStubThreadRepo()
	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
	)

	task := &persistence.Task{ID: "task_xyz", Payload: []byte(`{"context":{"prompt":"hi"}}`)}

	tid, _, err := bot.ensureTaskThread(context.Background(), task)
	if err != nil {
		t.Fatalf("ensureTaskThread: %v", err)
	}
	if tid != 777 {
		t.Errorf("first call thread_id: got %d want 777", tid)
	}

	// Second call: must not hit createForumTopic again.
	tid2, _, err := bot.ensureTaskThread(context.Background(), task)
	if err != nil {
		t.Fatalf("ensureTaskThread (2nd): %v", err)
	}
	if tid2 != 777 {
		t.Errorf("second call thread_id: got %d want 777", tid2)
	}
	if got := createCalls.Load(); got != 1 {
		t.Errorf("createForumTopic should be called exactly once; got %d", got)
	}

	// Repo state matches the returned thread_id.
	stored, err := repo.GetByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("repo.GetByTask: %v", err)
	}
	if stored.ThreadID != 777 {
		t.Errorf("stored thread_id: got %d want 777", stored.ThreadID)
	}
}

func TestEnsureTaskThread_DuplicateKeyFallback(t *testing.T) {
	// Simulates a cross-process race: Telegram createForumTopic
	// succeeds, but the repo Insert fails because a concurrent
	// peer already wrote the row. ensureTaskThread must recover
	// by re-resolving via GetByTask.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":111}}`))
	}))
	defer server.Close()

	repo := newStubThreadRepo()
	// Pre-seed a row representing the cross-process winner.
	winner := &persistence.TelegramTaskThread{
		TaskID: "task_race", ChatID: -1, ThreadID: 999, TopicName: "winner",
	}
	if err := repo.Insert(context.Background(), winner); err != nil {
		t.Fatal(err)
	}
	// But hide it from this caller's first GetByTask by clearing
	// byTask only — the second GetByTask after the conflict will
	// find it again.
	delete(repo.byTask, winner.TaskID)

	bot := newTestBotWithForum(t, server,
		WithForumChatID(-1, 0),
		WithTelegramThreadRepository(repo),
	)
	// Re-add the byTask entry so the post-conflict GetByTask sees
	// the winner. The model is: between our first GetByTask
	// (miss) and our Insert (conflict on byPair key), another
	// process inserted a different thread row for the same task.
	repo.byTask[winner.TaskID] = winner

	tid, _, err := bot.ensureTaskThread(context.Background(), &persistence.Task{ID: "task_race"})
	if err != nil {
		t.Fatalf("ensureTaskThread: %v", err)
	}
	if tid != winner.ThreadID {
		t.Errorf("race fallback must return winner's thread_id (%d), got %d", winner.ThreadID, tid)
	}
}

func TestFormatTaskEvent_Completion(t *testing.T) {
	bot := &Bot{}
	task := &persistence.Task{
		ID:     "t1",
		Status: persistence.TaskStatusCompleted,
	}
	got := bot.formatTaskEvent(context.Background(), task, true, "All checks passed.", false)
	if !strings.Contains(got, "✅ Task completed") {
		t.Errorf("expected completion marker, got: %s", got)
	}
	if !strings.Contains(got, "All checks passed") {
		t.Errorf("expected humanized message body, got: %s", got)
	}
}

func TestFormatTaskEvent_Failure(t *testing.T) {
	errMsg := "tests failed"
	errClass := "test_failure"
	bot := &Bot{}
	task := &persistence.Task{
		ID:             "t2",
		Status:         persistence.TaskStatusFailed,
		LastError:      &errMsg,
		LastErrorClass: &errClass,
	}
	got := bot.formatTaskEvent(context.Background(), task, false, "headline", false)
	if !strings.Contains(got, "❌ Task failed") {
		t.Errorf("expected failure marker, got: %s", got)
	}
	if !strings.Contains(got, "Error: tests failed") {
		t.Errorf("expected error body, got: %s", got)
	}
	if !strings.Contains(got, "Failure class: test_failure") {
		t.Errorf("expected failure class line, got: %s", got)
	}
}

func TestFormatTaskEvent_AwaitingInputWithCheckpoint(t *testing.T) {
	cpID := "tmsg_cp1"
	mrepo := &stubTaskMessageRepo{
		openByTask: map[string]*persistence.TaskMessage{
			"t3": {
				ID:          cpID,
				TaskID:      "t3",
				MessageKind: persistence.TaskMessageKindCheckpoint,
				Content:     "Which vendor?",
				Metadata:    []byte(`{"options":[{"id":"acme","label":"ACME Co"},{"id":"globex","label":"Globex"}]}`),
			},
		},
	}
	bot := &Bot{taskMessageRepo: mrepo}
	task := &persistence.Task{
		ID:               "t3",
		Status:           persistence.TaskStatusAwaitingInput,
		OpenCheckpointID: &cpID,
	}
	got := bot.formatTaskEvent(context.Background(), task, true, "Need decision", false)
	for _, want := range []string{"⏸ Task awaiting input", "Which vendor?", "acme — ACME Co", "globex — Globex", "Reply in this thread"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------

// newTestBotWithForum builds a Bot wired to a stub Telegram API
// server. The default opts include WithHTTPClient(server.Client())
// so the bot's outbound calls land on the stub.
func newTestBotWithForum(t *testing.T, server *httptest.Server, opts ...BotOption) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://api.example.com", "test-key", "gpt-4")
	all := append([]BotOption{WithHTTPClient(server.Client())}, opts...)
	bot, err := NewBot(BotConfig{Token: "t"}, chatClient, all...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = server.URL
	return bot
}

// stubTaskMessageRepo is a minimal TaskMessageRepository stub.
// formatTaskEvent only uses GetOpenCheckpoint, but Insert and
// List exist to satisfy the interface so callers can mount it
// from routing tests too.
type stubTaskMessageRepo struct {
	openByTask map[string]*persistence.TaskMessage
	inserts    []*persistence.TaskMessage
}

func (s *stubTaskMessageRepo) Insert(_ context.Context, m *persistence.TaskMessage) error {
	s.inserts = append(s.inserts, m)
	return nil
}

func (s *stubTaskMessageRepo) List(_ context.Context, _ persistence.TaskMessageFilter) ([]*persistence.TaskMessage, error) {
	return nil, nil
}

func (s *stubTaskMessageRepo) GetOpenCheckpoint(_ context.Context, taskID string) (*persistence.TaskMessage, error) {
	if m, ok := s.openByTask[taskID]; ok {
		return m, nil
	}
	return nil, nil
}

func (s *stubTaskMessageRepo) MarkCheckpointResolved(_ context.Context, _, _ string) error {
	return nil
}
