// Package livepubsub powers the live task observation surface
// (Feature #3). Events are published by the executor + chat-stream
// tap and consumed by WebSocket subscribers on /api/v1/executions/
// {id}/live. See https://docs.vornik.io
//
// Phase A (current) covers the publisher + executor step hooks.
// The WebSocket consumer (Phase B) and UI page (Phase C) land in
// follow-up commits.
package livepubsub

import (
	"encoding/json"
	"time"
)

// LiveEvent is the wire shape every subscriber sees. The payload
// is kind-specific; subscribers downcast based on Kind. Seq is
// monotonic per execution_id so clients can detect gaps + replay
// from a known cursor on reconnect.
type LiveEvent struct {
	ExecutionID string    `json:"execution_id"`
	Seq         int64     `json:"seq"`
	Timestamp   time.Time `json:"ts"`
	Kind        string    `json:"kind"`
	Payload     any       `json:"payload,omitempty"`
}

// Event kind constants. Centralised here so the publisher, taps,
// and consumers all reference the same literals.
const (
	KindStepStarted      = "step_started"
	KindStepCompleted    = "step_completed"
	KindLLMToken         = "llm_token"
	KindToolCallStarted  = "tool_call_started"
	KindToolCallFinished = "tool_call_finished"
	KindFileEdit         = "file_edit"
	KindOutcomeRecorded  = "outcome_recorded"
	KindHintApplied      = "hint_applied"
	KindPaused           = "paused"
	KindResumed          = "resumed"
	KindForked           = "forked"
	// LLM-call events (Phase B follow-up) — coarse-grained
	// signals fired by the chat proxy when an agent makes an
	// upstream LLM call. Distinct from llm_token (per-delta
	// streaming, deferred) so a UI can show "agent is thinking"
	// for the duration of a request without per-token plumbing.
	KindLLMCallStarted  = "llm_call_started"
	KindLLMCallFinished = "llm_call_finished"
	// KindClosed is the synthetic frame the WebSocket server emits
	// on terminal status before closing the connection. Not
	// published by taps; the WS handler synthesises it.
	KindClosed = "closed"
	// Inter-project orchestration Phase C events. The first three
	// fire on the CALLER execution's live stream so an operator
	// watching the caller can see the outbound delegation; the
	// fourth fires on the CALLEE execution's stream so an
	// operator watching the callee sees where the work came
	// from. project_spawned fires on the caller's stream only —
	// the spawned project doesn't have a "first execution" until
	// the initial_task is leased.
	KindCrossProjectCallStarted  = "cross_project_call_started"
	KindCrossProjectCallResolved = "cross_project_call_resolved"
	KindCrossProjectCallReceived = "cross_project_call_received"
	KindProjectSpawned           = "project_spawned"
)

// Payload shapes per event kind. Subscribers know how to decode
// each one via the Kind label. Marshalled as the LiveEvent.Payload
// field, so all are JSON-friendly.

type StepStartedPayload struct {
	StepID    string `json:"step_id"`
	Role      string `json:"role,omitempty"`
	Model     string `json:"model,omitempty"`
	Iteration int    `json:"iteration,omitempty"`
}

type StepCompletedPayload struct {
	StepID     string  `json:"step_id"`
	Outcome    string  `json:"outcome,omitempty"`
	DurationMs int64   `json:"duration_ms,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
}

type LLMTokenPayload struct {
	StepID       string `json:"step_id"`
	MessageID    string `json:"message_id,omitempty"`
	Role         string `json:"role,omitempty"` // "assistant" usually
	Delta        string `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type ToolCallStartedPayload struct {
	StepID    string          `json:"step_id"`
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	InputJSON json.RawMessage `json:"input_json,omitempty"`
}

type ToolCallFinishedPayload struct {
	CallID     string          `json:"call_id"`
	OutputJSON json.RawMessage `json:"output_json,omitempty"`
	DurationMs int64           `json:"duration_ms,omitempty"`
	Err        string          `json:"err,omitempty"`
}

type FileEditPayload struct {
	StepID    string `json:"step_id,omitempty"`
	Path      string `json:"path"`
	Op        string `json:"op"`             // create / modify / delete
	Hash      string `json:"hash,omitempty"` // content sha256 when known
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

type OutcomeRecordedPayload struct {
	StepID string `json:"step_id"`
	Class  string `json:"class"` // ok / hallucination / schema_violation / ...
	Notes  string `json:"notes,omitempty"`
}

type HintAppliedPayload struct {
	HintID    string `json:"hint_id"`
	StepID    string `json:"step_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

type PausedPayload struct {
	PauseKind string `json:"pause_kind"` // operator / shutdown / awaiting_children
	By        string `json:"by,omitempty"`
}

type ResumedPayload struct {
	By string `json:"by,omitempty"`
}

type ForkedPayload struct {
	NewExecutionID string `json:"new_execution_id"`
	FromStepID     string `json:"from_step_id"`
	By             string `json:"by,omitempty"`
}

// LLMCallStartedPayload announces the chat proxy received an
// agent's chat request and is about to forward to the upstream
// LLM. Model is whatever the agent requested (post-router
// resolution).
type LLMCallStartedPayload struct {
	Model string `json:"model,omitempty"`
}

// LLMCallFinishedPayload fires when the chat proxy returns. On
// success Tokens + DurationMs + CostUSD are set; on failure Err
// carries the upstream error string and the token + cost fields
// stay zero. The UI uses this to flip "agent thinking" → idle and
// to update the running cost counter live (2026-05-26 fix — the
// step_completed event historically carried CostUSD=0 with the
// reconciliation deferred to the spend dashboard, leaving the
// live header at $0.00 for the operator-visible session).
type LLMCallFinishedPayload struct {
	Model            string  `json:"model,omitempty"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	DurationMs       int64   `json:"duration_ms,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Err              string  `json:"err,omitempty"`
}

// CrossProjectCallStartedPayload fires on the CALLER execution's
// stream when a call_project step issues its outbound CPC. Lets
// the operator watching the caller see "delegated to architect
// → produce-spec" without drilling into the lineage tree.
type CrossProjectCallStartedPayload struct {
	CPCId          string `json:"cpc_id"`
	CalleeProject  string `json:"callee_project"`
	CalleeWorkflow string `json:"callee_workflow"`
	CalleeTaskID   string `json:"callee_task_id,omitempty"`
	ExpectedSchema string `json:"expected_schema,omitempty"`
	StepID         string `json:"step_id,omitempty"`
}

// CrossProjectCallResolvedPayload fires on the CALLER's stream
// when the CPC ledger row flips to a terminal status. Status
// mirrors the persistence.CrossProjectCallStatus string
// (completed / failed / rejected / timed_out).
type CrossProjectCallResolvedPayload struct {
	CPCId        string `json:"cpc_id"`
	Status       string `json:"status"`
	Summary      string `json:"summary,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	DurationMs   int64  `json:"duration_ms,omitempty"`
}

// CrossProjectCallReceivedPayload fires on the CALLEE
// execution's stream when its task is leased and an execution
// starts. Helps an operator watching the callee project
// understand "this work came from marketing/handoff-step".
type CrossProjectCallReceivedPayload struct {
	CPCId          string `json:"cpc_id"`
	CallerProject  string `json:"caller_project"`
	CallerTaskID   string `json:"caller_task_id"`
	CallerStepID   string `json:"caller_step_id,omitempty"`
	ExpectedSchema string `json:"expected_schema,omitempty"`
}

// ProjectSpawnedPayload fires on the CALLER's stream when a
// spawn_project step materialises a new project. InitialTaskID
// is set when the step declared an initial_task and the seed
// task creation succeeded.
type ProjectSpawnedPayload struct {
	SpawnID        string `json:"spawn_id"`
	SpawnedProject string `json:"spawned_project"`
	Template       string `json:"template"`
	InitialTaskID  string `json:"initial_task_id,omitempty"`
	StepID         string `json:"step_id,omitempty"`
}
