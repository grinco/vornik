package persistence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateID(t *testing.T) {
	t.Run("contains prefix", func(t *testing.T) {
		id := GenerateID("task")
		assert.True(t, strings.HasPrefix(id, "task_"))
	})

	t.Run("has expected format", func(t *testing.T) {
		id := GenerateID("task")
		parts := strings.Split(id, "_")
		assert.GreaterOrEqual(t, len(parts), 3, "ID should have at least 3 parts: prefix, timestamp, hex")
		assert.Equal(t, "task", parts[0])
		// Timestamp part should be 14 digits (YYYYMMDDHHMMSS)
		assert.Len(t, parts[1], 14)
		// Hex part should be 16 characters (8 bytes as hex)
		assert.Len(t, parts[2], 16)
	})

	t.Run("produces unique IDs", func(t *testing.T) {
		ids := make(map[string]bool)
		for i := 0; i < 100; i++ {
			id := GenerateID("task")
			assert.False(t, ids[id], "ID should be unique, got duplicate: %s", id)
			ids[id] = true
		}
	})
}

func TestClampToolAuditDurationMs(t *testing.T) {
	t.Run("returns 0 for negative values", func(t *testing.T) {
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(-1))
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(-1000))
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(-999999))
	})

	t.Run("returns 0 for values exceeding max", func(t *testing.T) {
		maxPlusOne := MaxSaneToolDurationMs + 1
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(maxPlusOne))
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(999999999))
	})

	t.Run("passes through valid values", func(t *testing.T) {
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(0))
		assert.Equal(t, int64(100), ClampToolAuditDurationMs(100))
		assert.Equal(t, int64(1000), ClampToolAuditDurationMs(1000))
		assert.Equal(t, MaxSaneToolDurationMs, ClampToolAuditDurationMs(MaxSaneToolDurationMs))
	})

	t.Run("boundary checks", func(t *testing.T) {
		// Just below max should pass through
		assert.Equal(t, MaxSaneToolDurationMs-1, ClampToolAuditDurationMs(MaxSaneToolDurationMs-1))
		// Exactly max should pass through
		assert.Equal(t, MaxSaneToolDurationMs, ClampToolAuditDurationMs(MaxSaneToolDurationMs))
		// Just over max should be clamped to 0
		assert.Equal(t, int64(0), ClampToolAuditDurationMs(MaxSaneToolDurationMs+1))
	})
}

func TestTaskStatusConstants(t *testing.T) {
	t.Run("TaskStatus constants have expected values", func(t *testing.T) {
		assert.Equal(t, TaskStatus("PENDING"), TaskStatusPending)
		assert.Equal(t, TaskStatus("QUEUED"), TaskStatusQueued)
		assert.Equal(t, TaskStatus("LEASED"), TaskStatusLeased)
		assert.Equal(t, TaskStatus("RUNNING"), TaskStatusRunning)
		assert.Equal(t, TaskStatus("COMPLETED"), TaskStatusCompleted)
		assert.Equal(t, TaskStatus("FAILED"), TaskStatusFailed)
		assert.Equal(t, TaskStatus("CANCELLED"), TaskStatusCancelled)
	})
}

func TestValidWorkflowProposalKind(t *testing.T) {
	valid := []WorkflowProposalKind{
		WorkflowProposalKindUnspecified,
		WorkflowProposalKindAddStep,
		WorkflowProposalKindRemoveStep,
		WorkflowProposalKindChangeTransition,
		WorkflowProposalKindChangeTimeout,
		WorkflowProposalKindChangeRetryPolicy,
		WorkflowProposalKindChangeRoleAssignment,
		WorkflowProposalKindReorderSteps,
	}
	for _, k := range valid {
		assert.True(t, ValidWorkflowProposalKind(k), "kind %q should be valid", k)
	}
	// Out-of-set values are rejected so a malformed architect output
	// or API filter can't silently widen the enum.
	for _, k := range []WorkflowProposalKind{"", "change_prompt", "add_workflow", "garbage"} {
		assert.False(t, ValidWorkflowProposalKind(k), "kind %q should be invalid", k)
	}
}

func TestExecutionStatusConstants(t *testing.T) {
	t.Run("ExecutionStatus constants have expected values", func(t *testing.T) {
		assert.Equal(t, ExecutionStatus("PENDING"), ExecutionStatusPending)
		assert.Equal(t, ExecutionStatus("RUNNING"), ExecutionStatusRunning)
		assert.Equal(t, ExecutionStatus("PAUSED"), ExecutionStatusPaused)
		assert.Equal(t, ExecutionStatus("COMPLETED"), ExecutionStatusCompleted)
		assert.Equal(t, ExecutionStatus("FAILED"), ExecutionStatusFailed)
		assert.Equal(t, ExecutionStatus("CANCELLED"), ExecutionStatusCancelled)
	})
}

func TestExecutionStatusIsLive(t *testing.T) {
	live := []ExecutionStatus{ExecutionStatusPending, ExecutionStatusRunning, ExecutionStatusPaused}
	for _, st := range live {
		assert.True(t, st.IsLive(), "%s should be live (non-terminal)", st)
	}
	terminal := []ExecutionStatus{ExecutionStatusCompleted, ExecutionStatusFailed, ExecutionStatusCancelled}
	for _, st := range terminal {
		assert.False(t, st.IsLive(), "%s should not be live (terminal)", st)
	}
	// An unknown status is conservatively treated as not-live so an
	// unrecognised value can't keep the executions list polling forever.
	assert.False(t, ExecutionStatus("WAT").IsLive(), "unknown status should not be live")
}

func TestTradingFillExecFields(t *testing.T) {
	exec := "0001f4e8.5f2a"
	acct := "DUH691769"
	f := TradingFill{ID: "exec-" + exec, ExecID: &exec, AccountID: &acct, Source: "reconcile"}
	b, err := json.Marshal(f)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"exec_id":"0001f4e8.5f2a"`)
	assert.Contains(t, string(b), `"source":"reconcile"`)
}

func TestAdditionalModelConstants(t *testing.T) {
	t.Run("TaskStatus includes conversational lifecycle values", func(t *testing.T) {
		assert.Equal(t, TaskStatus("WAITING_FOR_CHILDREN"), TaskStatusWaitingForChildren)
		assert.Equal(t, TaskStatus("AWAITING_INPUT"), TaskStatusAwaitingInput)
		assert.Equal(t, TaskStatus("AWAITING_EXTERNAL"), TaskStatusAwaitingExternal)
		assert.Equal(t, TaskStatus("PAUSED"), TaskStatusPaused)
		assert.Equal(t, TaskStatus("CLOSED"), TaskStatusClosed)
	})

	t.Run("TaskCreationSource constants have expected values", func(t *testing.T) {
		assert.Equal(t, TaskCreationSource("USER"), TaskCreationSourceUser)
		assert.Equal(t, TaskCreationSource("DELEGATION"), TaskCreationSourceDelegation)
		assert.Equal(t, TaskCreationSource("AUTONOMOUS"), TaskCreationSourceAutonomous)
		assert.Equal(t, TaskCreationSource("CHECKPOINT"), TaskCreationSourceCheckpoint)
		assert.Equal(t, TaskCreationSource("ROUTE"), TaskCreationSourceRoute)
	})

	t.Run("DelegationMode constants have expected values", func(t *testing.T) {
		assert.Equal(t, DelegationMode("SEQUENTIAL"), DelegationModeSequential)
		assert.Equal(t, DelegationMode("PARALLEL"), DelegationModeParallel)
		assert.Equal(t, DelegationMode("FAN_OUT"), DelegationModeFanOut)
	})

	t.Run("ArtifactClass constants have expected values", func(t *testing.T) {
		assert.Equal(t, ArtifactClass("INPUT"), ArtifactClassInput)
		assert.Equal(t, ArtifactClass("OUTPUT"), ArtifactClassOutput)
		assert.Equal(t, ArtifactClass("INTERMEDIATE"), ArtifactClassIntermediate)
		assert.Equal(t, ArtifactClass("SNAPSHOT"), ArtifactClassSnapshot)
		assert.Equal(t, ArtifactClass("LOG"), ArtifactClassLog)
		assert.Equal(t, ArtifactClass("METADATA"), ArtifactClassMetadata)
	})

	t.Run("TaskFailureClass constants include key classifier outputs", func(t *testing.T) {
		assert.Equal(t, "LLM_ERROR", TaskFailureClassLLMError)
		assert.Equal(t, "TIMEOUT", TaskFailureClassTimeout)
		assert.Equal(t, "TOOL_ERROR", TaskFailureClassToolError)
		assert.Equal(t, "INVALID_OUTPUT", TaskFailureClassInvalidOutput)
		assert.Equal(t, "MERGE_FAILED", TaskFailureClassMergeFailed)
		assert.Equal(t, "GATE_FAILED", TaskFailureClassGateFailed)
		assert.Equal(t, "BUDGET_BLOCKED", TaskFailureClassBudgetBlocked)
		assert.Equal(t, "RATE_LIMITED", TaskFailureClassRateLimited)
		assert.Equal(t, "WORKFLOW_ROLE_MISSING", TaskFailureClassWorkflowRole)
		assert.Equal(t, "WORKFLOW_CONFIG_ERROR", TaskFailureClassWorkflowCfg)
		assert.Equal(t, "ORPHANED", TaskFailureClassOrphaned)
		assert.Equal(t, "CANCELLED", TaskFailureClassCancelled)
		assert.Equal(t, "RUNTIME_ERROR", TaskFailureClassRuntimeError)
		assert.Equal(t, "UNKNOWN", TaskFailureClassUnknown)
		assert.Equal(t, "LEASE_EXPIRED", TaskFailureClassLeaseExpired)
		assert.Equal(t, "WORKFLOW_DRIFT", TaskFailureClassWorkflowDrift)
		assert.Equal(t, "STUCK_EXECUTION", TaskFailureClassStuckExecution)
		assert.Equal(t, "TOOL_ITERATION_LIMIT", TaskFailureClassToolIterationLimit)
		assert.Equal(t, "SECRET_LEAK", TaskFailureClassSecretLeak)
		assert.Equal(t, "CHILD_FAILED", TaskFailureClassChildFailed)
		assert.Equal(t, "INVALID_OUTPUT_LOOP", TaskFailureClassInvalidOutputLoop)
	})

	t.Run("TaskMessage constants have expected values", func(t *testing.T) {
		assert.Equal(t, "message", TaskMessageKindMessage)
		assert.Equal(t, "directive", TaskMessageKindDirective)
		assert.Equal(t, "checkpoint", TaskMessageKindCheckpoint)
		assert.Equal(t, "answer", TaskMessageKindAnswer)
		assert.Equal(t, "plan", TaskMessageKindPlan)
		assert.Equal(t, "phase_marker", TaskMessageKindPhaseMarker)
		assert.Equal(t, "note", TaskMessageKindNote)
		assert.Equal(t, "closure_request", TaskMessageKindClosureRequest)
		assert.Equal(t, "system", TaskMessageKindSystem)
		assert.Equal(t, "operator", TaskMessageAuthorOperator)
		assert.Equal(t, "lead", TaskMessageAuthorLead)
		assert.Equal(t, "system", TaskMessageAuthorSystem)
		assert.Equal(t, "role:", TaskMessageAuthorRolePrefix)
	})

	t.Run("WebhookEvent status constants have expected values", func(t *testing.T) {
		assert.Equal(t, "accepted", WebhookEventStatusAccepted)
		assert.Equal(t, "rejected", WebhookEventStatusRejected)
		assert.Equal(t, "duplicate", WebhookEventStatusDuplicate)
	})

	t.Run("AutonomyOutcome constants have expected values", func(t *testing.T) {
		assert.Equal(t, "CREATED", AutonomyOutcomeCreated)
		assert.Equal(t, "NO_ACTION", AutonomyOutcomeNoAction)
		assert.Equal(t, "RATE_LIMITED", AutonomyOutcomeRateLimited)
		assert.Equal(t, "BUDGET_BLOCKED", AutonomyOutcomeBudgetBlocked)
		assert.Equal(t, "ACTIVE_TASKS", AutonomyOutcomeActiveTasks)
		assert.Equal(t, "LLM_ERROR", AutonomyOutcomeLLMError)
		assert.Equal(t, "PARSE_ERROR", AutonomyOutcomeParseError)
		assert.Equal(t, "WORKFLOW_INVALID", AutonomyOutcomeWorkflowInvalid)
		assert.Equal(t, "TYPE_REJECTED", AutonomyOutcomeTypeRejected)
		assert.Equal(t, "CIRCUIT_OPEN", AutonomyOutcomeCircuitOpen)
		assert.Equal(t, "DUPLICATE", AutonomyOutcomeDuplicate)
		assert.Equal(t, "COOLDOWN", AutonomyOutcomeCooldown)
		assert.Equal(t, "IDEMPOTENCY_HIT", AutonomyOutcomeIdempotencyHit)
		assert.Equal(t, "DB_ERROR", AutonomyOutcomeDBError)
		assert.Equal(t, "PRECHECK_SKIPPED", AutonomyOutcomePreCheckSkipped)
		assert.Equal(t, "ABORTED", AutonomyOutcomeAborted)
	})

	t.Run("TaskLLMUsageSource constants have expected values", func(t *testing.T) {
		assert.Equal(t, "workflow_step", TaskLLMUsageSourceWorkflowStep)
		assert.Equal(t, "dispatcher", TaskLLMUsageSourceDispatcher)
		assert.Equal(t, "judge", TaskLLMUsageSourceJudge)
		assert.Equal(t, "post_mortem", TaskLLMUsageSourcePostMortem)
	})
}

// TestAPIKey_RotatedCopy_PreservesScope guards the centralised rotation
// carry-over: every scope/limit/capability column must survive, the
// identity fields must be fresh, and pointer fields must be deep-copied
// (no aliasing with the prior row). Regression anchor for the
// 2026-06-27 UI-rotate companion-scope-loss incident.
func TestAPIKey_RotatedCopy_PreservesScope(t *testing.T) {
	rps, burst := 7, 14
	budget := 9.5
	exp := time.Now().Add(48 * time.Hour).UTC()
	prior := &APIKey{
		ID: "akey-old", ProjectID: "companion-example", Name: "vadim/laptop",
		KeyHash: "oldhash", KeyPrefix: "sk-vornik-a7",
		CreatedAt: time.Now().Add(-time.Hour).UTC(), ExpiresAt: &exp,
		CreatedBy: "operator", LastUsedAt: &exp, // LastUsedAt must NOT carry over
		RateLimitRPS: &rps, RateLimitBurst: &burst, BudgetCapUSD: &budget,
		AllowedWorkflows: []string{"companion-architectural-review"},
		ClientKind:       "claude-code", SessionLabel: "vadim/laptop",
		DefaultRepoScope: "github.com/grinco/vornik",
		MemoryRead:       true, MemoryWrite: true, AllowPush: true,
	}
	now := time.Now().UTC()
	fresh := prior.RotatedCopy("akey-new", "newhash", "sk-vornik-b9", "ui", now)

	// Fresh identity.
	assert.Equal(t, "akey-new", fresh.ID)
	assert.Equal(t, "newhash", fresh.KeyHash)
	assert.Equal(t, "sk-vornik-b9", fresh.KeyPrefix)
	assert.Equal(t, "ui", fresh.CreatedBy)
	assert.Equal(t, now, fresh.CreatedAt)
	assert.Nil(t, fresh.LastUsedAt, "rotated key must start unused")
	assert.Nil(t, fresh.RevokedAt, "rotated key must start active")

	// Carried-over scope/limits/capabilities.
	assert.Equal(t, prior.ProjectID, fresh.ProjectID)
	assert.Equal(t, prior.Name, fresh.Name)
	assert.Equal(t, prior.ExpiresAt, fresh.ExpiresAt)
	assert.Equal(t, "claude-code", fresh.ClientKind)
	assert.Equal(t, "vadim/laptop", fresh.SessionLabel)
	assert.Equal(t, "github.com/grinco/vornik", fresh.DefaultRepoScope)
	assert.True(t, fresh.MemoryRead)
	assert.True(t, fresh.MemoryWrite)
	assert.True(t, fresh.AllowPush)
	assert.Equal(t, []string{"companion-architectural-review"}, fresh.AllowedWorkflows)
	require.NotNil(t, fresh.RateLimitRPS)
	assert.Equal(t, 7, *fresh.RateLimitRPS)
	require.NotNil(t, fresh.RateLimitBurst)
	assert.Equal(t, 14, *fresh.RateLimitBurst)
	require.NotNil(t, fresh.BudgetCapUSD)
	assert.Equal(t, 9.5, *fresh.BudgetCapUSD)

	// Deep copy: mutating the fresh pointers/slice must not touch prior.
	*fresh.RateLimitRPS = 99
	*fresh.BudgetCapUSD = 99
	fresh.AllowedWorkflows[0] = "mutated"
	assert.Equal(t, 7, *prior.RateLimitRPS, "RPS pointer must not alias prior")
	assert.Equal(t, 9.5, *prior.BudgetCapUSD, "budget pointer must not alias prior")
	assert.Equal(t, "companion-architectural-review", prior.AllowedWorkflows[0], "workflow slice must not alias prior")
}
