// Tests for the 2026-05-16 followup-descendant fix.
//
// When the dispatcher schedules a task with await_completion=true
// and that task spawns route / delegation children, the parent
// completes BEFORE the children produce the final artifacts.
// triggerFollowup now walks the parent's descendants and, when
// any are still active, transfers the followup to the most
// recent active descendant rather than firing the resume on the
// parent's premature completion.
//
// These tests pin three contracts:
//   - active child → transfer + skip resume
//   - all children terminated → fire resume on parent
//   - no children at all (legacy single-task case) → fire as before

package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestFindActiveDescendant_ReturnsRunningChild — canonical
// case: parent is COMPLETED, has one RUNNING child. The walk
// surfaces the child's ID so triggerFollowup can transfer.
func TestFindActiveDescendant_ReturnsRunningChild(t *testing.T) {
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			if parentID != "parent-1" {
				return nil, nil
			}
			return []*persistence.Task{
				{ID: "child-1", Status: persistence.TaskStatusRunning},
			}, nil
		},
	}
	b := &Bot{taskRepo: tr}
	got := b.findActiveDescendant("parent-1")
	assert.Equal(t, "child-1", got,
		"a running child must surface as the active descendant so the followup can transfer")
}

// TestFindActiveDescendant_AllChildrenTerminatedReturnsEmpty
// — when every descendant has already reached a terminal
// status, the walk returns empty and triggerFollowup falls
// through to the parent-resume path.
func TestFindActiveDescendant_AllChildrenTerminatedReturnsEmpty(t *testing.T) {
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			switch parentID {
			case "parent-1":
				return []*persistence.Task{
					{ID: "child-1", Status: persistence.TaskStatusCompleted},
					{ID: "child-2", Status: persistence.TaskStatusFailed},
				}, nil
			default:
				return nil, nil
			}
		},
	}
	b := &Bot{taskRepo: tr}
	assert.Equal(t, "", b.findActiveDescendant("parent-1"),
		"all-terminal descendants must return empty so the parent's resume fires")
}

// TestFindActiveDescendant_NoChildrenReturnsEmpty — legacy
// single-task case: no children at all. The walk terminates
// immediately and the parent resume path proceeds unchanged.
func TestFindActiveDescendant_NoChildrenReturnsEmpty(t *testing.T) {
	tr := &mocks.MockTaskRepository{}
	b := &Bot{taskRepo: tr}
	assert.Equal(t, "", b.findActiveDescendant("parent-1"))
}

// TestFindActiveDescendant_GrandchildWalkedTransitively —
// parent → completed child → running grandchild. The BFS
// must descend through the terminal child to find the
// active grandchild.
func TestFindActiveDescendant_GrandchildWalkedTransitively(t *testing.T) {
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, parentID string) ([]*persistence.Task, error) {
			switch parentID {
			case "parent-1":
				return []*persistence.Task{
					{ID: "child-1", Status: persistence.TaskStatusCompleted},
				}, nil
			case "child-1":
				return []*persistence.Task{
					{ID: "grandchild-1", Status: persistence.TaskStatusRunning},
				}, nil
			}
			return nil, nil
		},
	}
	b := &Bot{taskRepo: tr}
	got := b.findActiveDescendant("parent-1")
	assert.Equal(t, "grandchild-1", got,
		"BFS must descend through terminal children to reach an active grandchild")
}

// TestFindActiveDescendant_RepoErrorReturnsEmpty —
// defensive: a transient DB error during the walk should
// not crash the notification path. Returning empty falls
// through to the parent-resume which is the safer default
// (losing a notification is worse than over-firing once).
func TestFindActiveDescendant_RepoErrorReturnsEmpty(t *testing.T) {
	tr := &mocks.MockTaskRepository{
		GetChildrenFunc: func(_ context.Context, _ string) ([]*persistence.Task, error) {
			return nil, assert.AnError
		},
	}
	b := &Bot{taskRepo: tr}
	assert.Equal(t, "", b.findActiveDescendant("parent-1"))
}

// TestFindActiveDescendant_NilSafe — defensive shape.
func TestFindActiveDescendant_NilSafe(t *testing.T) {
	var nilBot *Bot
	assert.Equal(t, "", nilBot.findActiveDescendant("x"))

	emptyBot := &Bot{}
	assert.Equal(t, "", emptyBot.findActiveDescendant("x"),
		"bot without taskRepo wired must return empty")

	bot := &Bot{taskRepo: &mocks.MockTaskRepository{}}
	assert.Equal(t, "", bot.findActiveDescendant(""),
		"empty parentID must return empty")
}

// TestIsTerminalTaskStatus pins the predicate against every
// known status — a future addition needs an explicit decision
// in this test rather than slipping through.
func TestIsTerminalTaskStatus(t *testing.T) {
	terminal := []persistence.TaskStatus{
		persistence.TaskStatusCompleted,
		persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled,
	}
	for _, s := range terminal {
		if !isTerminalTaskStatus(s) {
			t.Errorf("%q must be terminal", s)
		}
	}
	nonTerminal := []persistence.TaskStatus{
		persistence.TaskStatusPending,
		persistence.TaskStatusQueued,
		persistence.TaskStatusLeased,
		persistence.TaskStatusRunning,
		persistence.TaskStatusWaitingForChildren,
	}
	for _, s := range nonTerminal {
		if isTerminalTaskStatus(s) {
			t.Errorf("%q must NOT be terminal", s)
		}
	}
}
