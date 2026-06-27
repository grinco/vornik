// Coverage rollup for the small per-file telegram helpers that
// were left at 60-80% by the bigger feature tests. Each function is
// a few lines of straightforward logic — covering them lifts the
// total without touching production code.

package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// --- WatchTask --------------------------------------------------------

type stubWatcherRepoSmall struct {
	calls []struct {
		Task string
		Chat int64
	}
}

func (s *stubWatcherRepoSmall) Watch(_ context.Context, taskID string, chatID int64) error {
	s.calls = append(s.calls, struct {
		Task string
		Chat int64
	}{taskID, chatID})
	return nil
}

func (s *stubWatcherRepoSmall) GetWatchers(_ context.Context, _ string) ([]int64, error) {
	return nil, nil
}

func (s *stubWatcherRepoSmall) RemoveWatchers(_ context.Context, _ string) error {
	return nil
}

func TestWatchTask_NoRepo(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.WatchTask("task-1", 100) // no panic when repo nil
}

func TestWatchTask_RecordsRegistration(t *testing.T) {
	repo := &stubWatcherRepoSmall{}
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.watcherRepo = repo

	b.WatchTask("task-1", 100)
	b.WatchTask("task-2", 200)
	if len(repo.calls) != 2 {
		t.Errorf("expected 2 Watch calls, got %d", len(repo.calls))
	}
}

// --- RegisterFollowup --------------------------------------------------

func TestRegisterFollowup_Idempotent(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.RegisterFollowup(100, "task-1", "p1")
	b.RegisterFollowup(101, "task-1", "p1") // overrides
	if len(b.pendingFollowups) != 1 {
		t.Errorf("expected 1 entry, got %d", len(b.pendingFollowups))
	}
	if got := b.pendingFollowups["task-1"]; got.chatID != 101 {
		t.Errorf("chatID: got %d, want 101", got.chatID)
	}
}

func TestRegisterFollowup_NilBotSilent(t *testing.T) {
	var b *Bot
	b.RegisterFollowup(100, "task-1", "p1") // must not panic
}

func TestRegisterFollowup_EmptyTaskIDIgnored(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.RegisterFollowup(100, "", "p1")
	if len(b.pendingFollowups) != 0 {
		t.Errorf("expected 0 entries, got %d", len(b.pendingFollowups))
	}
}

func TestRegisterFollowup_ZeroChatIDIgnored(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.RegisterFollowup(0, "task-1", "p1")
	if len(b.pendingFollowups) != 0 {
		t.Errorf("expected 0 entries, got %d", len(b.pendingFollowups))
	}
}

// --- recordChatUser ---------------------------------------------------

func TestRecordChatUser_StoresMapping(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.recordChatUser(100, 42)
	b.recordChatUser(200, 84)
	if got := b.userIDForChat(100); got != 42 {
		t.Errorf("chat 100: got %d, want 42", got)
	}
	if got := b.userIDForChat(200); got != 84 {
		t.Errorf("chat 200: got %d, want 84", got)
	}
}

func TestRecordChatUser_ZeroIDsIgnored(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	b.recordChatUser(0, 42)
	b.recordChatUser(100, 0)
	// userIDForChat falls back to chatID when nothing recorded.
	if got := b.userIDForChat(100); got != 100 {
		t.Errorf("chat 100 (zero user): fallback got %d, want 100", got)
	}
}

func TestRecordChatUser_NilSafe(t *testing.T) {
	var b *Bot
	b.recordChatUser(100, 42) // no panic
	if got := b.userIDForChat(100); got != 100 {
		t.Errorf("nil bot: got %d, want chatID=100", got)
	}
}

// --- effectiveDispatchTimeout ------------------------------------------

func TestEffectiveDispatchTimeout_DefaultsWhenZero(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	if got := b.effectiveDispatchTimeout(); got != defaultDispatchTimeout {
		t.Errorf("zero config: got %v, want %v", got, defaultDispatchTimeout)
	}
}

func TestEffectiveDispatchTimeout_RespectsConfig(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t", DispatchTimeout: 7 * time.Minute})
	if got := b.effectiveDispatchTimeout(); got != 7*time.Minute {
		t.Errorf("got %v, want 7m", got)
	}
}

// --- truncateTelegramLogString ----------------------------------------

func TestTruncateTelegramLogString_Short(t *testing.T) {
	if got := truncateTelegramLogString("hi", 100); got != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestTruncateTelegramLogString_Long(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncateTelegramLogString(long, 50)
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Errorf("missing truncation marker; got %q", got[len(got)-50:])
	}
}

// --- sanitizeTelegramError --------------------------------------------

func TestSanitizeTelegramError_NilReturnsEmpty(t *testing.T) {
	if got := sanitizeTelegramError(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestSanitizeTelegramError_NoURLPassesThrough(t *testing.T) {
	err := errors.New("connection refused")
	if got := sanitizeTelegramError(err); got != "connection refused" {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeTelegramError_RedactsURLToken(t *testing.T) {
	err := errors.New(`Post "https://api.telegram.org/bot12345:secret/getUpdates": EOF`)
	got := sanitizeTelegramError(err)
	if strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
}

// --- getProjectListForUser (registry-populated branch) ----------------

// fillTestRegistry-style helper here: build a tiny registry with two
// projects so getProjectListForUser has something concrete to filter.
// We can't use the existing fillTestRegistry verbatim because it
// requires fs setup; instead we craft a registry by hand.
func newProjectRegistry(t *testing.T, projectIDs ...string) *registry.Registry {
	t.Helper()
	// Use the registry.New() + Load() public path through the
	// minimal fillTestRegistry shape borrowed from notify_fill_test
	// since it already supports n projects via repeated YAML.
	return fillTestRegistry(t, projectIDs[0], 0) // re-use the first projectID
}

func TestGetProjectListForUser_WildcardSeesAll(t *testing.T) {
	reg := newProjectRegistry(t, "alpha")
	b := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"*"}},
		},
	})
	WithRegistry(reg)(b)
	got := b.getProjectListForUser(42)
	if len(got) == 0 {
		t.Errorf("wildcard: got 0 projects, want >=1")
	}
}

func TestGetProjectListForUser_ProjectScopedFilter(t *testing.T) {
	reg := newProjectRegistry(t, "alpha")
	b := newBareTestBot(t, BotConfig{
		Token: "t",
		AllowedUsers: map[int64]UserAccess{
			42: {Allowed: true, Projects: []string{"some-other-project"}},
		},
	})
	WithRegistry(reg)(b)
	got := b.getProjectListForUser(42)
	if len(got) != 0 {
		t.Errorf("project-scoped to non-existent: got %d, want 0", len(got))
	}
}

// --- handleInbox via repo-error path ----------------------------------

func TestHandleInbox_RepoErrorReturnsErrorString(t *testing.T) {
	b := newBareTestBot(t, BotConfig{Token: "t"})
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New("db unreachable")
		},
	}
	WithTaskRepository(repo)(b)
	out := handleInbox(context.Background(), b, 100, 0)
	if !strings.Contains(out, "Failed to load inbox") {
		t.Errorf("got %q", out)
	}
}
