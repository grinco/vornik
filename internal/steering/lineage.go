package steering

import (
	"context"

	"vornik.io/vornik/internal/persistence"
)

// TaskGetter is the narrow read the notifiers need to walk a task's ancestry.
// Satisfied by persistence.TaskRepository (c.repos.Tasks). Optional — a nil
// getter disables the ancestry walk (only the immediate task is inspected),
// preserving the pre-2026-07-08 behaviour.
type TaskGetter interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

// lineageWalkHardLimit bounds the ParentTaskID walk so a corrupt cycle can't
// spin forever. Mirrors executor.lineageWalkHardLimit.
const lineageWalkHardLimit = 256

// chatOriginTurnID returns the ChatTurnID that a steering notification for
// `task` should route to: the task's own ChatTurnID when set, otherwise the
// nearest ancestor's (walking ParentTaskID). Returns "" when neither the task
// nor any ancestor is chat-originated.
//
// This is the fix for the 2026-07-08 report: a task scheduled from a Telegram
// chat carries a ChatTurnID, but the checkpoint / route / delegation children
// it spawns do NOT — they only carry ParentTaskID. Without the walk, a paused
// descendant of a chat-scheduled task was mis-routed to the generic operator
// alert instead of back to the originating chat.
func chatOriginTurnID(ctx context.Context, task *persistence.Task, getter TaskGetter) string {
	if task == nil {
		return ""
	}
	if task.ChatTurnID != nil && *task.ChatTurnID != "" {
		return *task.ChatTurnID
	}
	if getter == nil {
		return ""
	}
	seen := map[string]bool{task.ID: true}
	parentID := task.ParentTaskID
	for i := 0; i < lineageWalkHardLimit; i++ {
		if parentID == nil || *parentID == "" || seen[*parentID] {
			return ""
		}
		seen[*parentID] = true
		parent, err := getter.Get(ctx, *parentID)
		if err != nil || parent == nil {
			// A missing ancestor (pruned/archived) terminates the chain — best
			// effort, same as the executor's lineage walkers.
			return ""
		}
		if parent.ChatTurnID != nil && *parent.ChatTurnID != "" {
			return *parent.ChatTurnID
		}
		parentID = parent.ParentTaskID
	}
	return ""
}
