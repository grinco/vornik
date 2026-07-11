package steering

import (
	"context"

	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/persistence"
)

// TaskGetter is the narrow read the notifiers need to walk a task's
// ancestry. Alias of chatorigin.TaskGetter — this chain is now SHARED with
// internal/narrator's chat push (task 2.3) via internal/chatorigin, so a
// future channel-resolution change updates one place, not two (companion
// review finding 5/8 on the narrated-execution design). Satisfied by
// persistence.TaskRepository (c.repos.Tasks). Optional — a nil getter
// disables the ancestry walk (only the immediate task is inspected),
// preserving the pre-2026-07-08 behaviour.
type TaskGetter = chatorigin.TaskGetter

// chatOriginTurnID delegates to the shared chatorigin.TurnID —
// see that function's doc comment for the resolution rule (own ChatTurnID,
// else nearest chat-originated ancestor's). Kept as a package-local
// function (rather than inlining the chatorigin call at both call sites in
// notifier.go / operator_alert.go) purely to keep those diffs small.
func chatOriginTurnID(ctx context.Context, task *persistence.Task, getter TaskGetter) string {
	return chatorigin.TurnID(ctx, task, getter)
}
