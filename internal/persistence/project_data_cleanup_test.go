package persistence

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProjectDataTables_IncludesDispatcherReminders pins B-12.
// Without this entry, a deleted project's pending reminders fire
// after the project is gone — operator sees a phantom
// Telegram/email reminder from a project they can't navigate to
// anymore. The fix is purely declarative (one row in the
// ProjectDataTables slice) but easy to lose in a future refactor,
// so a name-pinning test guards it.
func TestProjectDataTables_IncludesDispatcherReminders(t *testing.T) {
	want := "dispatcher_reminders"
	for _, table := range ProjectDataTables {
		if table == want {
			return
		}
	}
	t.Fatalf("ProjectDataTables must include %q so project-wide cleanup wipes reminders tied to deleted projects (B-12)", want)
}

// TestProjectDataTables_IncludesWorkflowHealing pins the blackbox
// self-healing cleanup. Both tables carry project_id NOT NULL but
// neither FK-cascades, so without an explicit entry a deleted
// project orphans its open healing triggers (they keep rendering in
// /ui/admin/blackbox/triggers/ for a vanished project) and its
// detection overrides. Declarative, easy to lose in a refactor —
// guard it by name.
func TestProjectDataTables_IncludesWorkflowHealing(t *testing.T) {
	for _, want := range []string{"workflow_healing_triggers", "workflow_healing_overrides"} {
		found := false
		for _, table := range ProjectDataTables {
			if table == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ProjectDataTables must include %q so project-wide cleanup wipes orphaned blackbox healing rows", want)
		}
	}
}

// TestProjectDataTables_OrderingPutsParentsLast pins the tasks/
// executions ordering invariant the cleanup transaction relies on.
// Child tables (task_messages, task_scratchpad, …) cascade off
// tasks via ON DELETE CASCADE; deleting tasks too early would
// strand the explicit children-with-project_id passes that come
// after. tasks + executions therefore must appear AFTER the
// project-scoped audit tables.
func TestProjectDataTables_OrderingPutsParentsLast(t *testing.T) {
	idx := func(name string) int {
		for i, t := range ProjectDataTables {
			if t == name {
				return i
			}
		}
		return -1
	}
	tasksIdx := idx("tasks")
	execsIdx := idx("executions")
	auditsIdx := idx("tool_audit_log")
	assert.Greater(t, tasksIdx, auditsIdx,
		"tasks must come AFTER tool_audit_log so audit deletes complete before their parent cascades")
	assert.Greater(t, tasksIdx, execsIdx,
		"tasks must come AFTER executions so executions delete cleanly before tasks cascades fire")
}
