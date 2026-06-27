package persistence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests target pure, data-integrity-relevant helpers in the
// persistence model layer that were uncovered (0%) by the existing
// suite: status predicates, the task-key-name parser, the leader-lock
// holder check, and enum constant pins. They need no DB.

// --- TaskIDFromKeyName ------------------------------------------------------

func TestTaskIDFromKeyName(t *testing.T) {
	t.Run("extracts task id from a bound key name", func(t *testing.T) {
		id, ok := TaskIDFromKeyName(TaskKeyNamePrefix + "task_abc123")
		assert.True(t, ok)
		assert.Equal(t, "task_abc123", id)
	})

	t.Run("rejects a name without the reserved prefix", func(t *testing.T) {
		for _, name := range []string{
			"",
			"task_abc123",          // missing the agent: namespace
			"agent:task",           // close but not the full prefix
			"agent:other_task_123", // different namespace
			"AGENT:TASK_123",       // case sensitive
		} {
			id, ok := TaskIDFromKeyName(name)
			assert.False(t, ok, "name %q must not parse as a task binding", name)
			assert.Equal(t, "", id)
		}
	})

	t.Run("prefix with empty remainder is not a valid binding", func(t *testing.T) {
		// An empty task ID must not be treated as authoritative — this is
		// the confused-deputy guard (FIX 3): "agent:task_" alone yields false.
		id, ok := TaskIDFromKeyName(TaskKeyNamePrefix)
		assert.False(t, ok)
		assert.Equal(t, "", id)
	})

	t.Run("preserves the remainder verbatim including separators", func(t *testing.T) {
		// The remainder after the prefix is returned untouched — embedded
		// underscores or colons in a task id must survive the round trip.
		id, ok := TaskIDFromKeyName(TaskKeyNamePrefix + "weird_id:with:colons")
		assert.True(t, ok)
		assert.Equal(t, "weird_id:with:colons", id)
	})

	t.Run("prefix constant is pinned", func(t *testing.T) {
		assert.Equal(t, "agent:task_", TaskKeyNamePrefix)
	})
}

// --- CrossProjectCallStatus.IsTerminal --------------------------------------

func TestCrossProjectCallStatus_IsTerminal(t *testing.T) {
	terminal := []CrossProjectCallStatus{
		CPCStatusCompleted, CPCStatusFailed, CPCStatusTimedOut, CPCStatusRejected,
	}
	for _, s := range terminal {
		assert.True(t, s.IsTerminal(), "%q should be terminal (skipped by timeout scanner)", s)
	}
	nonTerminal := []CrossProjectCallStatus{CPCStatusPending, CPCStatusRunning}
	for _, s := range nonTerminal {
		assert.False(t, s.IsTerminal(), "%q is in-flight and must not be terminal", s)
	}
	// An unrecognised value is conservatively non-terminal so the timeout
	// scanner keeps watching a row it doesn't understand rather than
	// abandoning it.
	assert.False(t, CrossProjectCallStatus("").IsTerminal())
	assert.False(t, CrossProjectCallStatus("garbage").IsTerminal())
}

func TestCrossProjectCallStatusConstants(t *testing.T) {
	// Values are lowercase and feed a CHECK constraint (migration v52);
	// drift here silently breaks status writes.
	assert.Equal(t, CrossProjectCallStatus("pending"), CPCStatusPending)
	assert.Equal(t, CrossProjectCallStatus("running"), CPCStatusRunning)
	assert.Equal(t, CrossProjectCallStatus("completed"), CPCStatusCompleted)
	assert.Equal(t, CrossProjectCallStatus("failed"), CPCStatusFailed)
	assert.Equal(t, CrossProjectCallStatus("timed_out"), CPCStatusTimedOut)
	assert.Equal(t, CrossProjectCallStatus("rejected"), CPCStatusRejected)
}

// --- ReminderStatus.IsTerminal ----------------------------------------------

func TestReminderStatus_IsTerminal(t *testing.T) {
	terminal := []ReminderStatus{
		ReminderStatusFired, ReminderStatusCancelled, ReminderStatusExpired,
	}
	for _, s := range terminal {
		assert.True(t, s.IsTerminal(), "%q should be end-of-life", s)
	}
	live := []ReminderStatus{ReminderStatusPending, ReminderStatusFiring}
	for _, s := range live {
		assert.False(t, s.IsTerminal(), "%q is still actionable by the heartbeat", s)
	}
	// Unknown status defaults to non-terminal so the cancel surface and
	// heartbeat don't skip a row they can't classify.
	assert.False(t, ReminderStatus("").IsTerminal())
	assert.False(t, ReminderStatus("paused").IsTerminal())
}

func TestReminderStatusConstants(t *testing.T) {
	// Pins the migration-55 CHECK set.
	assert.Equal(t, ReminderStatus("pending"), ReminderStatusPending)
	assert.Equal(t, ReminderStatus("firing"), ReminderStatusFiring)
	assert.Equal(t, ReminderStatus("fired"), ReminderStatusFired)
	assert.Equal(t, ReminderStatus("cancelled"), ReminderStatusCancelled)
	assert.Equal(t, ReminderStatus("expired"), ReminderStatusExpired)
}

// --- Reminder.IsRecurring ---------------------------------------------------

func TestReminder_IsRecurring(t *testing.T) {
	t.Run("non-empty cron expr means recurring", func(t *testing.T) {
		r := &Reminder{CronExpr: "0 9 * * *"}
		assert.True(t, r.IsRecurring())
	})

	t.Run("empty cron expr is one-shot", func(t *testing.T) {
		r := &Reminder{CronExpr: ""}
		assert.False(t, r.IsRecurring())
	})

	t.Run("recurrence-until alone does not make it recurring", func(t *testing.T) {
		// CronExpr is the single source of truth: a bounded window without
		// a cron expression is still a one-shot reminder.
		until := time.Now().Add(24 * time.Hour)
		r := &Reminder{CronExpr: "", RecurrenceUntil: &until}
		assert.False(t, r.IsRecurring())
	})

	t.Run("nil receiver is safe and non-recurring", func(t *testing.T) {
		var r *Reminder
		assert.False(t, r.IsRecurring())
	})
}

// --- HealingTriggerStatus.IsTerminal ----------------------------------------

func TestHealingTriggerStatus_IsTerminal(t *testing.T) {
	assert.True(t, HealingTriggerStatusDismissed.IsTerminal())
	assert.True(t, HealingTriggerStatusGeneratedCandidate.IsTerminal())
	// The detector only dedupes against non-terminal rows, so a fresh
	// regression after a dismissal must open a new trigger — i.e. these
	// must read non-terminal.
	assert.False(t, HealingTriggerStatus("open").IsTerminal())
	assert.False(t, HealingTriggerStatus("").IsTerminal())
	assert.False(t, HealingTriggerStatus("acknowledged").IsTerminal())
}

// --- DaemonLeaderLock.IsHeldBy ----------------------------------------------

func TestDaemonLeaderLock_IsHeldBy(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	t.Run("held by matching holder with unexpired lease", func(t *testing.T) {
		l := DaemonLeaderLock{HolderID: "daemon-a", ExpiresAt: now.Add(time.Minute)}
		assert.True(t, l.IsHeldBy("daemon-a", now))
	})

	t.Run("not held by a different holder", func(t *testing.T) {
		l := DaemonLeaderLock{HolderID: "daemon-a", ExpiresAt: now.Add(time.Minute)}
		assert.False(t, l.IsHeldBy("daemon-b", now))
	})

	t.Run("not held once the lease has expired", func(t *testing.T) {
		l := DaemonLeaderLock{HolderID: "daemon-a", ExpiresAt: now.Add(-time.Second)}
		assert.False(t, l.IsHeldBy("daemon-a", now))
	})

	t.Run("expiry exactly at now is still held", func(t *testing.T) {
		// IsHeldBy uses !ExpiresAt.Before(now): an expiry equal to now is
		// not yet expired (boundary inclusive on the held side).
		l := DaemonLeaderLock{HolderID: "daemon-a", ExpiresAt: now}
		assert.True(t, l.IsHeldBy("daemon-a", now))
	})

	t.Run("empty holder does not match an empty query by accident", func(t *testing.T) {
		// A zero-value row (no holder, zero expiry) must not report as held
		// by anyone, including the empty holder id.
		var l DaemonLeaderLock
		assert.False(t, l.IsHeldBy("", now))
	})
}

// --- ClusterNode.StaleAfter (boundary hardening) ----------------------------

func TestClusterNode_StaleAfter_Boundary(t *testing.T) {
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ttl := 30 * time.Second
	node := ClusterNode{LastSeen: base}

	// Just inside the TTL window: not stale.
	assert.False(t, node.StaleAfter(base.Add(ttl-time.Millisecond), ttl))
	// Exactly at the TTL boundary: heartbeat age == ttl is not yet stale.
	assert.False(t, node.StaleAfter(base.Add(ttl), ttl))
	// One step past the boundary: stale.
	assert.True(t, node.StaleAfter(base.Add(ttl+time.Millisecond), ttl))
	// A heartbeat in the future is never stale.
	assert.False(t, node.StaleAfter(base.Add(-time.Hour), ttl))
}

// --- GenerateID prefix isolation + collision resistance ---------------------

func TestGenerateID_PrefixAndUniqueness(t *testing.T) {
	t.Run("distinct prefixes never collide and are recoverable", func(t *testing.T) {
		task := GenerateID("task")
		exec := GenerateID("exec")
		require.NotEqual(t, task, exec)
		assert.True(t, strings.HasPrefix(task, "task_"))
		assert.True(t, strings.HasPrefix(exec, "exec_"))
	})

	t.Run("hex suffix is valid lowercase hex of fixed width", func(t *testing.T) {
		id := GenerateID("kg")
		parts := strings.Split(id, "_")
		require.GreaterOrEqual(t, len(parts), 3)
		hex := parts[len(parts)-1]
		assert.Len(t, hex, 16)
		for _, c := range hex {
			assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
				"suffix char %q must be lowercase hex", string(c))
		}
	})

	t.Run("high-volume generation stays unique", func(t *testing.T) {
		// 16 hex chars = 64 bits of crypto entropy; 5000 draws must not
		// collide. Guards against an accidental switch to a weak/seeded RNG.
		const n = 5000
		seen := make(map[string]struct{}, n)
		for i := 0; i < n; i++ {
			id := GenerateID("task")
			_, dup := seen[id]
			require.False(t, dup, "duplicate id generated: %s", id)
			seen[id] = struct{}{}
		}
		assert.Len(t, seen, n)
	})
}

// --- JSON round-trip of a model with omitempty + pointer fields -------------

func TestCrossProjectCall_JSONRoundTrip(t *testing.T) {
	t.Run("omitempty pointer/byte fields are dropped when zero", func(t *testing.T) {
		c := CrossProjectCall{
			ID:            "cpc_1",
			CallerTaskID:  "task_caller",
			CallerProject: "proj-a",
			CalleeProject: "proj-b",
			Status:        CPCStatusPending,
			// CalleeTaskID, ResultEnvelope, ErrorMessage, TimeoutAt,
			// ResolvedAt left nil/zero; CancelOnTimeout false.
		}
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		s := string(raw)
		assert.NotContains(t, s, "callee_task_id")
		assert.NotContains(t, s, "result_envelope")
		assert.NotContains(t, s, "error_message")
		assert.NotContains(t, s, "timeout_at")
		assert.NotContains(t, s, "resolved_at")
		assert.NotContains(t, s, "cancel_on_timeout")
		// Required fields survive.
		assert.Contains(t, s, `"id":"cpc_1"`)
		assert.Contains(t, s, `"status":"pending"`)
	})

	t.Run("populated fields survive a marshal/unmarshal cycle", func(t *testing.T) {
		callee := "task_callee"
		errMsg := "schema validation failed"
		ts := time.Date(2026, 6, 18, 9, 30, 0, 0, time.UTC)
		orig := CrossProjectCall{
			ID:              "cpc_2",
			CallerTaskID:    "task_caller",
			CallerStepID:    "step_1",
			CallerProject:   "proj-a",
			CalleeProject:   "proj-b",
			CalleeWorkflow:  "wf-x",
			CalleeTaskID:    &callee,
			Payload:         []byte(`{"k":"v"}`),
			ExpectedSchema:  "schema://x",
			Status:          CPCStatusRejected,
			ResultEnvelope:  []byte(`{"r":1}`),
			ErrorMessage:    &errMsg,
			TimeoutAt:       &ts,
			CreatedAt:       ts,
			ResolvedAt:      &ts,
			CancelOnTimeout: true,
		}
		raw, err := json.Marshal(orig)
		require.NoError(t, err)

		var got CrossProjectCall
		require.NoError(t, json.Unmarshal(raw, &got))

		assert.Equal(t, orig.ID, got.ID)
		assert.Equal(t, orig.Status, got.Status)
		require.NotNil(t, got.CalleeTaskID)
		assert.Equal(t, callee, *got.CalleeTaskID)
		require.NotNil(t, got.ErrorMessage)
		assert.Equal(t, errMsg, *got.ErrorMessage)
		assert.Equal(t, orig.Payload, got.Payload)
		assert.Equal(t, orig.ResultEnvelope, got.ResultEnvelope)
		assert.True(t, got.CancelOnTimeout)
		require.NotNil(t, got.TimeoutAt)
		assert.True(t, orig.TimeoutAt.Equal(*got.TimeoutAt))
	})
}

// --- TaskStatus / ExecutionStatus terminal-vs-live consistency pins ---------

func TestTaskStatusConstantValues(t *testing.T) {
	// Wire values are uppercase and feed the scheduler + UI badge logic;
	// pin the full conversational-lifecycle set so a rename can't slip in.
	cases := map[TaskStatus]string{
		TaskStatusPending:            "PENDING",
		TaskStatusQueued:             "QUEUED",
		TaskStatusLeased:             "LEASED",
		TaskStatusRunning:            "RUNNING",
		TaskStatusWaitingForChildren: "WAITING_FOR_CHILDREN",
		TaskStatusCompleted:          "COMPLETED",
		TaskStatusFailed:             "FAILED",
		TaskStatusCancelled:          "CANCELLED",
		TaskStatusAwaitingInput:      "AWAITING_INPUT",
		TaskStatusAwaitingExternal:   "AWAITING_EXTERNAL",
		TaskStatusPaused:             "PAUSED",
		TaskStatusClosed:             "CLOSED",
		TaskStatusAwaitingApproval:   "AWAITING_APPROVAL",
	}
	for got, want := range cases {
		assert.Equal(t, want, string(got))
	}
}
