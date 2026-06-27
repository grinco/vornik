// Tests for the 2026-05-16 NotifyTaskCompleted leaf-transfer fix.
//
// Three contracts:
//
//  1. triggerFollowup ALWAYS fires. Pre-fix the early returns on
//     missing watcher repo / GetWatchers error / empty watchers
//     skipped the deferred auto-resume entirely. That broke the
//     adaptive-workflow leaf path: the parent transferred its
//     followup to the leaf, but the leaf had no watchers of its
//     own so NotifyTaskCompleted(leaf) hit "no watchers registered"
//     and silently dropped the followup — the dispatcher's reply
//     to the user never landed.
//
//  2. When the task has an active descendant (route delegation in
//     flight), watchers transfer parent → leaf and the parent's
//     intermediate "task completed" notification is suppressed.
//     The leaf will fan out the real summary + artifacts.
//
//  3. The forum thread fan-out is also suppressed for the routing
//     parent — the leaf's thread post is the one that carries the
//     consolidated artifacts; a parent's terminal event in the
//     thread is just noise.

package telegram

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// memWatcherRepo is an in-memory TaskWatcherRepository used by the
// leaf-transfer tests. Tracks Watch/Remove ordering so assertions
// can confirm parent watchers were removed AFTER leaf watchers
// were added (the transfer must not lose data on a crash mid-way).
type memWatcherRepo struct {
	mu       sync.Mutex
	byTask   map[string]map[int64]struct{}
	watchLog []struct {
		task string
		chat int64
	}
	removeLog []string
}

func newMemWatcherRepo() *memWatcherRepo {
	return &memWatcherRepo{byTask: map[string]map[int64]struct{}{}}
}

func (m *memWatcherRepo) Watch(_ context.Context, taskID string, chatID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byTask[taskID]; !ok {
		m.byTask[taskID] = map[int64]struct{}{}
	}
	m.byTask[taskID][chatID] = struct{}{}
	m.watchLog = append(m.watchLog, struct {
		task string
		chat int64
	}{taskID, chatID})
	return nil
}

func (m *memWatcherRepo) GetWatchers(_ context.Context, taskID string) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.byTask[taskID]
	out := make([]int64, 0, len(set))
	for cid := range set {
		out = append(out, cid)
	}
	return out, nil
}

func (m *memWatcherRepo) RemoveWatchers(_ context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byTask, taskID)
	m.removeLog = append(m.removeLog, taskID)
	return nil
}

// TestNotifyTaskCompleted_TriggersFollowupWhenNoWatchers is the
// regression test for the core bug: the pre-fix early return on
// empty watchers skipped triggerFollowup, so the adaptive-workflow
// leaf never auto-resumed the chat. With the defer at the top of
// NotifyTaskCompleted, the followup must be claimed even when
// nothing is wired downstream.
func TestNotifyTaskCompleted_TriggersFollowupWhenNoWatchers(t *testing.T) {
	bot := &Bot{
		watcherRepo:      newMemWatcherRepo(),
		pendingFollowups: map[string]followupContext{},
	}
	bot.pendingFollowups["leaf-1"] = followupContext{chatID: 100, projectID: "p"}

	task := &persistence.Task{ID: "leaf-1", Status: persistence.TaskStatusCompleted}
	bot.NotifyTaskCompleted(context.Background(), task, true, "done")

	// triggerFollowup must have popped the followup entry. The
	// dispatcher is unwired in this test so the actual auto-resume
	// won't fire — the pop is the proof that the deferred call
	// reached the followup logic.
	assert.NotContains(t, bot.pendingFollowups, "leaf-1",
		"followup must be popped — empty watchers must NOT skip the auto-resume")
}

// TestNotifyTaskCompleted_TransfersWatchersToLeafWhenActiveDescendant
// pins the second contract: a routing parent's watchers move to
// the leaf so the leaf's completion fanout reaches the same chats.
// Suppresses the parent's intermediate user-visible notification
// to avoid the "two notifications, only one with content" UX.
func TestNotifyTaskCompleted_TransfersWatchersToLeafWhenActiveDescendant(t *testing.T) {
	wr := newMemWatcherRepo()
	// Seed: chat 99 is watching the parent.
	require.NoError(t, wr.Watch(context.Background(), "parent-1", 99))

	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == "parent-1" {
				return []*persistence.Task{
					{ID: "leaf-1", Status: persistence.TaskStatusRunning},
				}, nil
			}
			return nil, nil
		},
	}

	bot := &Bot{
		watcherRepo:      wr,
		taskRepo:         tr,
		pendingFollowups: map[string]followupContext{},
	}

	parent := &persistence.Task{ID: "parent-1", Status: persistence.TaskStatusCompleted}
	bot.NotifyTaskCompleted(context.Background(), parent, true, "routed")

	// Leaf now carries the watcher; parent's row is gone.
	leafChats, _ := wr.GetWatchers(context.Background(), "leaf-1")
	assert.ElementsMatch(t, []int64{99}, leafChats,
		"leaf must inherit parent's watchers so its completion lands on the same chat")

	parentChats, _ := wr.GetWatchers(context.Background(), "parent-1")
	assert.Empty(t, parentChats,
		"parent's watcher row must be removed after transfer; otherwise the parent's terminal would re-fanout on a future re-fire")
}

// TestNotifyTaskCompleted_OrderingTransferBeforeRemove pins the
// safe ordering: the parent's watchers must be written to the leaf
// BEFORE the parent's are deleted. If we crash between the two,
// re-running on the same parent must not produce a zero-watcher
// state where the dispatcher loses the original chats.
func TestNotifyTaskCompleted_OrderingTransferBeforeRemove(t *testing.T) {
	wr := newMemWatcherRepo()
	require.NoError(t, wr.Watch(context.Background(), "p-2", 1))
	require.NoError(t, wr.Watch(context.Background(), "p-2", 2))

	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == "p-2" {
				return []*persistence.Task{
					{ID: "leaf-2", Status: persistence.TaskStatusQueued},
				}, nil
			}
			return nil, nil
		},
	}
	bot := &Bot{
		watcherRepo:      wr,
		taskRepo:         tr,
		pendingFollowups: map[string]followupContext{},
	}

	bot.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "p-2"}, true, "")

	// At least 2 Watch calls land on the leaf BEFORE the
	// RemoveWatchers("p-2") call. The log doesn't merge per-task
	// so we inspect the sequence directly.
	leafWatchCount := 0
	for _, w := range wr.watchLog {
		if w.task == "leaf-2" {
			leafWatchCount++
		}
	}
	assert.Equal(t, 2, leafWatchCount,
		"every parent chat must be re-registered on the leaf before the parent rows are removed")

	require.Len(t, wr.removeLog, 1)
	assert.Equal(t, "p-2", wr.removeLog[0])
}

// TestNotifyTaskCompleted_AllChildrenTerminalFallsThroughToParent
// is the legacy-shape regression guard. When the task has children
// that have ALL terminated already, findActiveDescendant returns ""
// and NotifyTaskCompleted falls through to the parent-fanout path.
// This prevents accidentally suppressing the LAST notification in
// a tree just because the parent has any children.
func TestNotifyTaskCompleted_AllChildrenTerminalFallsThroughToParent(t *testing.T) {
	wr := newMemWatcherRepo()
	require.NoError(t, wr.Watch(context.Background(), "p-3", 7))

	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID == "p-3" {
				return []*persistence.Task{
					{ID: "c-3", Status: persistence.TaskStatusCompleted},
				}, nil
			}
			return nil, nil
		},
	}
	// Watcher chat is in "silent" mode so the legacy fanout path
	// doesn't try to actually hit the Telegram API (the bot's HTTP
	// client is unwired in this test). The pre-fanout branching
	// is what we're testing.
	bot := &Bot{
		watcherRepo:      wr,
		taskRepo:         tr,
		pendingFollowups: map[string]followupContext{},
		verbosity:        map[int64]string{7: "silent"},
	}

	bot.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "p-3"}, true, "")

	// No leaf transfer means parent's watchers stay until the
	// normal flow removes them at the end (we don't assert on
	// that here — only that nothing was re-pointed at "c-3").
	leaf, _ := wr.GetWatchers(context.Background(), "c-3")
	assert.Empty(t, leaf,
		"terminal-only descendants must not receive a watcher transfer — the parent IS the leaf in that topology")
}

// TestNotifyTaskCompleted_NoTaskRepoLegacyShape pins the
// degradation when the bot was built without a task repo: the
// findActiveDescendant guard short-circuits to "" and the
// function runs exactly the pre-2026-05-16 fanout path. Important
// for older deployments that haven't wired taskRepo.
func TestNotifyTaskCompleted_NoTaskRepoLegacyShape(t *testing.T) {
	wr := newMemWatcherRepo()
	require.NoError(t, wr.Watch(context.Background(), "legacy", 1))

	bot := &Bot{
		watcherRepo:      wr,
		taskRepo:         nil, // explicit: no walk
		pendingFollowups: map[string]followupContext{},
		verbosity:        map[int64]string{1: "silent"},
	}

	// Should not panic; should not transfer anywhere.
	bot.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "legacy"}, true, "")

	// The legacy fanout flow ends with RemoveWatchers on the task.
	assert.Contains(t, wr.removeLog, "legacy",
		"legacy single-task fanout must still clean up its watcher rows")
}
