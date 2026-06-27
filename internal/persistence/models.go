// Package persistence provides database abstractions and repository implementations
// for the vornik daemon.
package persistence

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// GenerateID creates a unique ID with the given prefix.
// Format: {prefix}_{YYYYMMDDHHMMSS}_{16 hex chars from crypto/rand}.
func GenerateID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%s_%x", prefix, time.Now().Format("20060102150405"), b)
}

// DeterministicEntityID derives a stable knowledge-graph entity ID from the
// entity's identity triple (project_id, type, canonical_name) — the same
// triple the knowledge_entities UNIQUE constraint dedups on. Using a
// content hash instead of a random ID makes extraction IDEMPOTENT: the same
// entity always resolves to the same ID, so re-running extraction (or two
// concurrent extractors) converges instead of racing two random IDs against
// the unique constraint. Format: kent_<32 hex of sha256(triple)>. The
// NUL separators keep ("a","bc","d") distinct from ("ab","c","d").
//
// Existing rows keep their historical random IDs (this only fills a blank
// ID at insert time); the UNIQUE constraint remains the authoritative dedup
// key, so old-random and new-deterministic rows coexist safely.
func DeterministicEntityID(projectID, entityType, canonicalName string) string {
	h := sha256.Sum256([]byte(projectID + "\x00" + entityType + "\x00" + canonicalName))
	return fmt.Sprintf("kent_%x", h[:16])
}

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	TaskStatusPending            TaskStatus = "PENDING"
	TaskStatusQueued             TaskStatus = "QUEUED"
	TaskStatusLeased             TaskStatus = "LEASED"
	TaskStatusRunning            TaskStatus = "RUNNING"
	TaskStatusWaitingForChildren TaskStatus = "WAITING_FOR_CHILDREN"
	TaskStatusCompleted          TaskStatus = "COMPLETED"
	TaskStatusFailed             TaskStatus = "FAILED"
	TaskStatusCancelled          TaskStatus = "CANCELLED"
	// Phase 23 — conversational task lifecycle. See
	// https://docs.vornik.io
	// COMPLETED is now non-terminal: the task is awaiting either
	// operator input or operator closure. CLOSED is the new
	// operator-confirmed terminal state.
	TaskStatusAwaitingInput    TaskStatus = "AWAITING_INPUT"
	TaskStatusAwaitingExternal TaskStatus = "AWAITING_EXTERNAL"
	TaskStatusPaused           TaskStatus = "PAUSED"
	TaskStatusClosed           TaskStatus = "CLOSED"
	// AWAITING_APPROVAL — autonomy manual-approval gate. Autonomy
	// tasks created under a project with requireApproval land here
	// (instead of PENDING, which the scheduler never leases and no UI
	// surfaced — they waited forever). The operator resolves it via
	// approve (→ QUEUED) or reject (→ CANCELLED). It is an
	// awaiting-action status (surfaces in the inbox/badge), not active,
	// not terminal. See
	// https://docs.vornik.io
	TaskStatusAwaitingApproval TaskStatus = "AWAITING_APPROVAL"
)

// ExecutionStatus represents the state of a workflow execution.
type ExecutionStatus string

const (
	ExecutionStatusPending   ExecutionStatus = "PENDING"
	ExecutionStatusRunning   ExecutionStatus = "RUNNING"
	ExecutionStatusPaused    ExecutionStatus = "PAUSED"
	ExecutionStatusCompleted ExecutionStatus = "COMPLETED"
	ExecutionStatusFailed    ExecutionStatus = "FAILED"
	ExecutionStatusCancelled ExecutionStatus = "CANCELLED"
)

// IsLive reports whether the execution is in a non-terminal state that can
// still change (PENDING/RUNNING/PAUSED). Terminal and unrecognised statuses
// return false, so a list rendered with no live rows can stop auto-refreshing.
func (s ExecutionStatus) IsLive() bool {
	switch s {
	case ExecutionStatusPending, ExecutionStatusRunning, ExecutionStatusPaused:
		return true
	default:
		return false
	}
}

// TaskCreationSource indicates how a task was created.
type TaskCreationSource string

const (
	TaskCreationSourceUser       TaskCreationSource = "USER"
	TaskCreationSourceDelegation TaskCreationSource = "DELEGATION"
	TaskCreationSourceAutonomous TaskCreationSource = "AUTONOMOUS"
	// TaskCreationSourceCheckpoint marks tasks that the executor
	// scheduled as continuations after a parent task hit the agent
	// iteration limit. Distinct from Delegation (which a step's
	// agent triggered) and Autonomous (which the lead agent
	// decided): checkpoint tasks are created by the daemon itself
	// to preserve partial progress when an agent ran out of
	// iterations mid-step.
	TaskCreationSourceCheckpoint TaskCreationSource = "CHECKPOINT"
	// TaskCreationSourceRoute marks the strict-adaptive routing
	// handoff: the parent ran the `adaptive` workflow, the lead
	// picked a real workflow, and delegateSelectedWorkflow spawned
	// this child with the parent's payload copied verbatim. Split
	// out from Delegation so monitoring can tell the two apart —
	// one Route child per parent is healthy; many Route children
	// for a single parent means the a4b24c5 routing bug regressed.
	// Free-form lead delegations (createDelegatedTasks) still use
	// TaskCreationSourceDelegation.
	TaskCreationSourceRoute TaskCreationSource = "ROUTE"
	// TaskCreationSourceFork marks tasks spawned by the failure-
	// forensics fork-from-step primitive. The task payload carries
	// a "fork_target" envelope (source_execution_id, step_id,
	// prompt_override) the executor reads to populate the new
	// execution's lineage columns + jump to the forked step on
	// first iteration. Distinct from Checkpoint (executor self-
	// scheduled continuation) and Delegation (an agent requested
	// the child) so audit + UI can tell the three apart.
	// See https://docs.vornik.io
	TaskCreationSourceFork TaskCreationSource = "FORK"
	// TaskCreationSourceA2A marks tasks submitted by an external
	// A2A protocol caller (POST /a2a/v1/agents/<p>/<wf>/tasks).
	// The audit trail wants to distinguish "another agent
	// framework drove this" from a human-typed REST POST.
	// See https://docs.vornik.io
	TaskCreationSourceA2A TaskCreationSource = "A2A"
	// TaskCreationSourceCompanion marks tasks delegated by a
	// host-LLM companion plugin (Claude Code, Codex, Gemini CLI,
	// opencode). Distinct from A2A so audit + UI can tell
	// "another agent framework drove this" from "a host LLM client
	// offloaded async work via the companion contract" — they
	// look superficially similar but have very different policy
	// surfaces (budget caps, workflow allowlists, client kind).
	// See https://docs.vornik.io
	TaskCreationSourceCompanion TaskCreationSource = "COMPANION"
)

// DelegationMode specifies how a parent task waits for child tasks.
type DelegationMode string

const (
	DelegationModeSequential DelegationMode = "SEQUENTIAL"
	DelegationModeParallel   DelegationMode = "PARALLEL"
	DelegationModeFanOut     DelegationMode = "FAN_OUT"
)

// ArtifactClass classifies the type of artifact.
type ArtifactClass string

const (
	ArtifactClassInput        ArtifactClass = "INPUT"
	ArtifactClassOutput       ArtifactClass = "OUTPUT"
	ArtifactClassIntermediate ArtifactClass = "INTERMEDIATE"
	ArtifactClassSnapshot     ArtifactClass = "SNAPSHOT"
	ArtifactClassLog          ArtifactClass = "LOG"
	ArtifactClassMetadata     ArtifactClass = "METADATA"
)

// Task represents a unit of work in the queue.
type Task struct {
	ID             string             `json:"id"`
	ProjectID      string             `json:"project_id"`
	WorkflowID     *string            `json:"workflow_id,omitempty"`
	IdempotencyKey *string            `json:"idempotency_key,omitempty"`
	ParentTaskID   *string            `json:"parent_task_id,omitempty"`
	CreationSource TaskCreationSource `json:"creation_source"`
	DelegationMode *DelegationMode    `json:"delegation_mode,omitempty"`
	Status         TaskStatus         `json:"status"`
	Priority       int                `json:"priority"`
	Payload        []byte             `json:"payload,omitempty"`
	Dependencies   []string           `json:"dependencies,omitempty"`
	LeaseID        *string            `json:"lease_id,omitempty"`
	LeasedAt       *time.Time         `json:"leased_at,omitempty"`
	LeasedBy       *string            `json:"leased_by,omitempty"`
	LeaseExpiresAt *time.Time         `json:"lease_expires_at,omitempty"`
	Attempt        int                `json:"attempt"`
	MaxAttempts    int                `json:"max_attempts"`
	LastError      *string            `json:"last_error,omitempty"`
	// LastErrorClass is the typed classification of the most recent
	// failure. See TaskFailureClass* constants below. Nil means the task
	// never failed (or pre-dates this column). Drives retry policy and
	// lets operators group "why is my project wedged" queries by class
	// rather than string-matching LastError.
	LastErrorClass *string   `json:"last_error_class,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// Phase 23 — conversational lifecycle. All nullable / default-zero
	// so existing tasks read back unchanged. See
	// https://docs.vornik.io §4.3.
	BriefAmendedAt   *time.Time `json:"brief_amended_at,omitempty"`
	CurrentPhase     *string    `json:"current_phase,omitempty"`
	ExpectedBy       *time.Time `json:"expected_by,omitempty"` // populated for AWAITING_EXTERNAL
	ClosedAt         *time.Time `json:"closed_at,omitempty"`
	ClosedBy         *string    `json:"closed_by,omitempty"`
	MessageCount     int        `json:"message_count"`
	OpenCheckpointID *string    `json:"open_checkpoint_id,omitempty"` // task_messages.id of the open checkpoint, if any
	// ChatTurnID is the chat_audit_log.id of the dispatcher turn that
	// spawned this task. Populated by chat.ExecuteAction when the
	// originating context carries a turn id. Nil for API-initiated and
	// autonomous-scheduled tasks. Used by in-conversation dedup,
	// follow-up coalescing, and the "which tasks did this turn spawn?"
	// audit query — see migration v46.
	ChatTurnID *string `json:"chat_turn_id,omitempty"`

	// CrossProjectCallID is the id of the cross_project_calls
	// row that spawned this task — only set on callee tasks
	// created by a `call_project` step in another project.
	// When the executor terminates the task, the resolve hook
	// uses this id to write the result envelope back to the CPC
	// row + wake the caller. Nil for ordinary tasks.
	// See https://docs.vornik.io
	CrossProjectCallID *string `json:"cross_project_call_id,omitempty"`

	// ResultEnvelope is the validated outcome JSON the task
	// terminates with. Populated by the executor on callee
	// tasks (sourced from result.json + the expected schema in
	// the CPC row); also useful for caller tasks that want to
	// expose a typed outcome to downstream steps. Stored as
	// raw bytes so callers JSON-unmarshal into their own
	// schemas. Nil for tasks that don't produce envelopes.
	ResultEnvelope []byte `json:"result_envelope,omitempty"`
}

// TaskFailureClass enumerates the concrete failure modes a task can
// land in. Kept as plain strings rather than a DB enum so operators
// can extend it from the classifier without a schema migration.
//
// Classifier lives in internal/executor/failure_classifier.go; the
// class rides on ReleaseOptions.ErrorClass into ReleaseLease. A task
// that fails for a reason we don't classify gets TaskFailureClassUnknown.
const (
	TaskFailureClassLLMError      = "LLM_ERROR"
	TaskFailureClassTimeout       = "TIMEOUT"
	TaskFailureClassToolError     = "TOOL_ERROR"
	TaskFailureClassInvalidOutput = "INVALID_OUTPUT"
	TaskFailureClassMergeFailed   = "MERGE_FAILED"
	TaskFailureClassGateFailed    = "GATE_FAILED"
	TaskFailureClassBudgetBlocked = "BUDGET_BLOCKED"
	TaskFailureClassRateLimited   = "RATE_LIMITED"
	TaskFailureClassWorkflowRole  = "WORKFLOW_ROLE_MISSING"
	TaskFailureClassWorkflowCfg   = "WORKFLOW_CONFIG_ERROR"
	TaskFailureClassOrphaned      = "ORPHANED"
	TaskFailureClassCancelled     = "CANCELLED"
	TaskFailureClassRuntimeError  = "RUNTIME_ERROR"
	TaskFailureClassUnknown       = "UNKNOWN"
	TaskFailureClassLeaseExpired  = "LEASE_EXPIRED"
	// TaskFailureClassWorkflowDrift fires when an execution's stored
	// workflow_revision no longer matches the live workflow YAML — the
	// operator edited the file between start and resume, so the state
	// machine can't safely be replayed.
	TaskFailureClassWorkflowDrift = "WORKFLOW_DRIFT"
	// TaskFailureClassStuckExecution is set by the watchdog when an
	// execution has not advanced its state checkpoint within the
	// configured stuck threshold. Distinct from TIMEOUT (which fires
	// from a context deadline inside the executor) and LEASE_EXPIRED
	// (which fires from the scheduler's recovery loop): STUCK_EXECUTION
	// is what the watchdog assigns when the executor is still nominally
	// running but hasn't made forward progress.
	TaskFailureClassStuckExecution = "STUCK_EXECUTION"
	// TaskFailureClassToolIterationLimit fires when an agent's
	// tool-use loop hit the configured VORNIK_MAX_TOOL_ITERATIONS
	// cap before producing a final answer. Distinct from
	// TaskFailureClassToolError (a single tool call broke); this
	// class means "the model wanted to keep going but ran out of
	// iterations." The executor reacts by committing any partial
	// work, merging the worktree, and scheduling a CHECKPOINT-
	// source continuation task — so a TOOL_ITERATION_LIMIT row
	// usually carries a follow-up child task.
	TaskFailureClassToolIterationLimit = "TOOL_ITERATION_LIMIT"
	// TaskFailureClassSecretLeak fires when a Phase 1/2 secret-leak
	// checkpoint is configured to Block and the agent's output
	// contained a credential-shaped value (API keys, PEM private
	// keys, JWTs, connection strings with embedded passwords).
	// Distinct from INVALID_OUTPUT — the output is structurally
	// fine, but contains data the project's secrets policy refuses
	// to persist or display. The internal/secrets package details
	// the detection corpus + per-checkpoint policy.
	TaskFailureClassSecretLeak = "SECRET_LEAK"
	// TaskFailureClassChildFailed fires on a parent task when one or
	// more of its child tasks finished in FAILED state. The parent's
	// own execution didn't fail — the bubble-up came from
	// checkParentUnblock seeing a failed child. Distinct from
	// LLM_ERROR / TOOL_ERROR (the parent ran fine) and from ORPHANED
	// (the parent's own infrastructure is intact).
	TaskFailureClassChildFailed = "CHILD_FAILED"
	// TaskFailureClassInvalidOutputLoop fires when a single role keeps
	// emitting result.json that fails schema validation across the
	// shape-retry + model-fallback budget. Distinct from INVALID_OUTPUT
	// (one bad attempt) — this class is the watchdog escalation that
	// stops the loop instead of letting the role burn budget producing
	// the same wrong shape across daemon restarts. Operators reading
	// this class on a task should investigate the role's model choice
	// or the systemPrompt's required-output-key contract — the model
	// is structurally unable to follow the schema with the current
	// configuration.
	TaskFailureClassInvalidOutputLoop = "INVALID_OUTPUT_LOOP"
	// TaskFailureClassHallucinatedPlacement fires when the
	// placements_match_audit verifier observed that the executor's
	// declared placed[] entries outnumber the actual
	// mcp__broker__place_order audit calls. Distinct from TOOL_ERROR
	// (a tool call actually broke) — the tool was never invoked; the
	// agent invented broker_order_ids and idempotency_keys to fill the
	// response shape. Operators reading this class should investigate
	// the executor role's model choice (the failure mode is
	// reproducible across minimax.minimax-m2.5 ticks observed
	// 2026-05-13: sequential fake broker_order_ids "86287740/41/42/44"
	// and hexalphabetical idempotency_keys).
	TaskFailureClassHallucinatedPlacement = "HALLUCINATED_PLACEMENT"
	// TaskFailureClassDelegationGuard fires on a parent task when its
	// delegation request was rejected by one of the N4 delegation
	// guards: depth limit exceeded, fan-out (per-batch child count)
	// limit exceeded, or a circular dependency (the requested children
	// would re-enter an ancestor already in the lineage chain). Distinct
	// from CHILD_FAILED — no child was ever created; the guard refused
	// the batch before insertion. Operators reading this class should
	// inspect the delegation chain depth and the requesting role's
	// fan-out behavior.
	// See https://docs.vornik.io §3 (Delegation Limits).
	TaskFailureClassDelegationGuard = "DELEGATION_GUARD"
)

// Execution represents a workflow execution instance.
type Execution struct {
	ID               string `json:"id"`
	TaskID           string `json:"task_id"`
	ProjectID        string `json:"project_id"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRevision string `json:"workflow_revision"`
	// WorkflowSnapshot is the JSON-marshaled workflow at execution
	// start. Replay paths prefer this over the live workflow when
	// non-nil so a YAML edit while a long-running task is in flight
	// can't change the step graph mid-execution. Empty means the
	// execution predates this column or the snapshot wasn't captured;
	// callers fall back to the live workflow + hash-based drift guard.
	WorkflowSnapshot []byte          `json:"workflow_snapshot,omitempty"`
	Status           ExecutionStatus `json:"status"`
	CurrentStepID    *string         `json:"current_step_id,omitempty"`
	CompletedSteps   []string        `json:"completed_steps,omitempty"`
	StateSnapshot    []byte          `json:"state_snapshot,omitempty"`
	Result           []byte          `json:"result,omitempty"`
	ErrorMessage     *string         `json:"error_message,omitempty"`
	ErrorCode        *string         `json:"error_code,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`

	// Failure-forensics Feature #1 Phase B — fork lineage. All three
	// fields are nil for non-fork executions. Migration 48 added the
	// columns; older deployments read NULL and these stay nil-safe.
	// See https://docs.vornik.io

	// ParentExecutionID is the source execution this row was forked
	// from. Soft FK — a deleted parent doesn't break the fork's
	// lineage walk (the walker stops when the parent is missing).
	ParentExecutionID *string `json:"parent_execution_id,omitempty"`
	// ForkedFromStepID is the workflow step the fork started at.
	// Read by the executor's pre-step hook on the first iteration
	// of this step to apply ForkedPromptOverride.
	ForkedFromStepID *string `json:"forked_from_step_id,omitempty"`
	// ForkedPromptOverride is the operator-supplied prompt prefix
	// prepended to the forked step's prompt on iteration 1 only.
	// Empty/nil means "no override" (fork still happens — just with
	// the workflow's original prompt).
	ForkedPromptOverride *string `json:"forked_prompt_override,omitempty"`
}

// Artifact represents a durable output or intermediate product.
type Artifact struct {
	ID                string         `json:"id"`
	ProjectID         string         `json:"project_id"`
	ExecutionID       *string        `json:"execution_id,omitempty"`
	TaskID            *string        `json:"task_id,omitempty"`
	Name              string         `json:"name"`
	ArtifactClass     ArtifactClass  `json:"artifact_class"`
	StoragePath       string         `json:"storage_path"`
	SizeBytes         *int64         `json:"size_bytes,omitempty"`
	ContentHashSHA256 *string        `json:"content_hash_sha256,omitempty"`
	MimeType          *string        `json:"mime_type,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	Origin            ArtifactOrigin `json:"origin"`
}

// ExtractedDocumentStatus enumerates the lifecycle states of an
// extracted document row. OK is the only status the read path treats
// as fully usable; PARTIAL means the extractor produced some sections
// before timing out/failing (consumers may still index what landed);
// FAILED rows are kept as a tombstone for diagnostics but no sections
// exist on disk.
const (
	ExtractedDocumentStatusOK      = "OK"
	ExtractedDocumentStatusPartial = "PARTIAL"
	ExtractedDocumentStatusFailed  = "FAILED"
)

// ExtractedDocument is the cached result of running a MIME-keyed
// extractor over an INPUT artifact. See
// https://docs.vornik.io §5.
//
// One row per (source artifact, extractor name, extractor version).
// Re-running the same extractor returns the existing row; upgrading
// the extractor produces a new row alongside the old.
type ExtractedDocument struct {
	ID                   string    `json:"id"`
	ProjectID            string    `json:"project_id"`
	SourceArtifactID     string    `json:"source_artifact_id"`
	ExtractorName        string    `json:"extractor_name"`
	ExtractorVersion     string    `json:"extractor_version"`
	MimeType             string    `json:"mime_type"`
	StoragePath          string    `json:"storage_path"`
	MetadataBlob         []byte    `json:"metadata_blob"` // JSON
	OutlineBlob          []byte    `json:"outline_blob"`  // JSON
	SectionCount         int       `json:"section_count"`
	TotalTextBytes       int64     `json:"total_text_bytes"`
	ExtractionDurationMS *int64    `json:"extraction_duration_ms,omitempty"`
	Status               string    `json:"status"`
	ExtractedAt          time.Time `json:"extracted_at"`
}

// TaskWatcher records a request to be notified when a task completes.
type TaskWatcher struct {
	TaskID    string    `json:"task_id"`
	ChatID    int64     `json:"chat_id"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskMessageKind enumerates the message_kind values written to
// task_messages. See task-lifecycle-conversational-design.md §4.1.1.
// Each kind has a different UI affordance and a different effect
// on the task state machine — split rather than munged into a
// single freeform shape.
const (
	TaskMessageKindMessage        = "message"         // operator/lead chat
	TaskMessageKindDirective      = "directive"       // operator course-correction
	TaskMessageKindCheckpoint     = "checkpoint"      // lead blocks the task
	TaskMessageKindAnswer         = "answer"          // operator replies to a checkpoint
	TaskMessageKindPlan           = "plan"            // lead emits per-execution plan
	TaskMessageKindPhaseMarker    = "phase_marker"    // lead enter/exit/skip a phase
	TaskMessageKindNote           = "note"            // lead exec summary or "added X to memory"
	TaskMessageKindClosureRequest = "closure_request" // lead recommends task closure
	TaskMessageKindSystem         = "system"          // daemon-authored (resume reasons, etc.)
	TaskMessageKindHint           = "hint"            // synthetic — operator steering hint surfaced in the thread (2026-05-26 unified-timeline refactor; not persisted to task_messages, materialised from execution_hints at render time)
)

// TaskMessageAuthorKind tags who wrote a message.
const (
	TaskMessageAuthorOperator = "operator"
	TaskMessageAuthorLead     = "lead"
	TaskMessageAuthorSystem   = "system"
	// Other roles use a "role:<name>" prefix (e.g. "role:researcher").
	TaskMessageAuthorRolePrefix = "role:"
)

// TaskMessage is one entry in a task's persistent conversation
// thread. The thread accumulates over the task's lifetime —
// sometimes weeks of calendar time across many executions. See
// https://docs.vornik.io
type TaskMessage struct {
	ID          string  `json:"id"`
	TaskID      string  `json:"task_id"`
	ExecutionID *string `json:"execution_id,omitempty"` // nil when the message lives between executions
	ParentID    *string `json:"parent_id,omitempty"`    // threaded reply (answer → checkpoint)
	AuthorKind  string  `json:"author_kind"`            // see TaskMessageAuthor* consts
	AuthorID    *string `json:"author_id,omitempty"`    // telegram user id, ui session, etc.
	MessageKind string  `json:"message_kind"`           // see TaskMessageKind* consts
	Content     string  `json:"content"`
	// Metadata is a JSON blob holding kind-specific payload:
	// checkpoint shape, attachment references, phase transition
	// details. Stored as raw bytes so callers can unmarshal into
	// whichever struct matches the kind.
	Metadata  []byte    `json:"metadata,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TelegramTaskThread maps a Telegram Forum Topic to a task. One row
// per task; topics are created lazily on the first lifecycle
// notification. See https://docs.vornik.io
type TelegramTaskThread struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"task_id"`
	ChatID    int64      `json:"chat_id"`    // forum supergroup chat_id
	ThreadID  int64      `json:"thread_id"`  // message_thread_id assigned by Telegram
	TopicName string     `json:"topic_name"` // name passed to createForumTopic
	CreatedAt time.Time  `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"` // set when the topic is locked on terminal state
}

// TaskMessageFilter narrows a list query.
type TaskMessageFilter struct {
	TaskID         string
	After          *string  // pagination cursor (return rows with id > After)
	MessageKinds   []string // optional; restrict to these kinds
	IncludeDeleted bool     // reserved; v1 doesn't soft-delete
	Limit          int
}

// TransitionOpts carries optional companion-column mutations for
// TransitionConditional. Each field is a pointer so "leave alone"
// is distinguishable from "set to zero". The repo applies any
// non-nil value in the same UPDATE so the row stays consistent
// with the new status (e.g. CLOSED rows always carry closed_at +
// closed_by; AWAITING_EXTERNAL rows always carry expected_by).
type TransitionOpts struct {
	ClosedBy       *string
	ExpectedBy     *time.Time
	CurrentPhase   *string
	BriefAmendedAt *time.Time
	LastError      *string
	LastErrorClass *string
	// SetClosedAtNow stamps closed_at = NOW() on the row. Only
	// honoured when the destination status is CLOSED.
	SetClosedAtNow bool
	// ClearLease wipes the lease columns on transition. Used when
	// re-queueing from AWAITING_*/PAUSED so a stale lease pointer
	// doesn't survive into the next round.
	ClearLease bool
	// Attempt + MaxAttempts persist the retry counters on the same
	// UPDATE so the row's attempt budget stays in sync with the new
	// status. Used by the executor's self-release path for recovered
	// (retry-from-step) executions which have no lease and so can't
	// use ReleaseLease but still need to bump attempt + persist
	// max_attempts when re-queueing. Zero means "leave the column
	// alone" — the SQL gate honours that so existing callers don't
	// accidentally clobber attempt counters when they only meant to
	// change the status.
	Attempt     int
	MaxAttempts int
}

// Knowledge-graph entity types — closed top-level taxonomy.
// Subtype refinements live in entity.Properties.subtype.
// See https://docs.vornik.io §3.4.
const (
	EntityTypePerson     = "PERSON"
	EntityTypeVendor     = "VENDOR" // also covers ORG
	EntityTypeProduct    = "PRODUCT"
	EntityTypeDecision   = "DECISION"
	EntityTypeEvent      = "EVENT"
	EntityTypeDate       = "DATE"
	EntityTypePrice      = "PRICE"
	EntityTypeLocation   = "LOCATION"
	EntityTypeTechnology = "TECHNOLOGY"
	EntityTypeFact       = "FACT"
	EntityTypeOther      = "OTHER"
)

// Closed predicate vocabulary. Free-form predicates also accepted;
// the closed list is what the UI renders specially. See §3.5.
const (
	PredicateMentionedIn  = "MENTIONED_IN"
	PredicateRelatesTo    = "RELATES_TO"
	PredicateQuotedPrice  = "QUOTED_PRICE"
	PredicateChosenOver   = "CHOSEN_OVER"
	PredicateMeasuredAs   = "MEASURED_AS"
	PredicateDependsOn    = "DEPENDS_ON"
	PredicateSupersededBy = "SUPERSEDED_BY"
	PredicateLocatedAt    = "LOCATED_AT"
	PredicateOwnedBy      = "OWNED_BY"
	PredicateHasDeadline  = "HAS_DEADLINE"
)

// KnowledgeEntity is a typed noun extracted from chunks.
// Phase 43+ of the knowledge-graph memory roadmap.
type KnowledgeEntity struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	Type          string `json:"type"`
	CanonicalName string `json:"canonical_name"`
	Aliases       []byte `json:"aliases,omitempty"` // JSONB array of strings
	Description   string `json:"description"`
	Properties    []byte `json:"properties,omitempty"` // JSONB object
	// Embedding bytes are written via pq raw; readers convert via pgvector.
	// Stored as []float32 to match the chunks pipeline's vector(1024).
	Embedding []float32 `json:"-"`

	ExtractedBy string  `json:"extracted_by,omitempty"`
	ResolvedBy  string  `json:"resolved_by,omitempty"`
	Confidence  float32 `json:"confidence"`

	LifecycleState   string     `json:"lifecycle_state"`
	ValidationStatus string     `json:"validation_status"`
	EpochID          *string    `json:"epoch_id,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	SupersedesID     *string    `json:"supersedes_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// KnowledgeEntityFilter narrows entity list queries.
type KnowledgeEntityFilter struct {
	ProjectID string
	Types     []string // empty = all
	Lifecycle []string // default = ["published"]
	NameLike  string   // ILIKE % wrap
	Limit     int
	Offset    int
}

// KnowledgeEdge is a typed relationship between two entities.
type KnowledgeEdge struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	FromEntity string `json:"from_entity"`
	ToEntity   string `json:"to_entity"`
	Predicate  string `json:"predicate"`
	Properties []byte `json:"properties,omitempty"` // JSONB

	SourceChunks []string `json:"source_chunks"`
	ExtractedBy  string   `json:"extracted_by,omitempty"`
	Confidence   float32  `json:"confidence"`
	Faithfulness *float32 `json:"faithfulness,omitempty"`

	LifecycleState string  `json:"lifecycle_state"`
	EpochID        *string `json:"epoch_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// KnowledgeEdgeFilter narrows edge list queries.
type KnowledgeEdgeFilter struct {
	ProjectID  string
	FromEntity string
	ToEntity   string
	Predicate  string
	Lifecycle  []string // default = ["published"]
	Limit      int
}

// EntityMention records that a chunk references an entity at
// (char_start, char_end). char_start defaults to 0 when the
// extractor doesn't return offsets (some models won't).
type EntityMention struct {
	ChunkID   string `json:"chunk_id"`
	EntityID  string `json:"entity_id"`
	CharStart int    `json:"char_start"`
	CharEnd   *int   `json:"char_end,omitempty"`
	Surface   string `json:"surface,omitempty"`
}

// TaskScratchpad is the lead's running summary for one task.
// One row per task; updated at the end of every execution. Read
// at the start of every subsequent execution. See LLD §4.2.
type TaskScratchpad struct {
	TaskID  string `json:"task_id"`
	Summary string `json:"summary"`
	// Facts is a JSON object holding structured key/value pairs
	// the lead has captured (window measurements, vendor names,
	// quote prices, deadlines). Stored opaque so the lead schema
	// can evolve without a migration.
	Facts []byte `json:"facts,omitempty"`
	// OpenQuestions is a JSON array of strings — what's blocking
	// progress, in the lead's own words.
	OpenQuestions []byte  `json:"open_questions,omitempty"`
	CurrentPhase  *string `json:"current_phase,omitempty"`
	// PhaseHistory is a JSON array of {name, entered_at,
	// exited_at, status} objects — the time-ordered phase log.
	PhaseHistory    []byte    `json:"phase_history,omitempty"`
	LastExecutionID *string   `json:"last_execution_id,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MaxSaneToolDurationMs is the upper bound for an agent-reported
// tool duration before we treat the value as drift and clamp it to 0.
// One hour is a generous ceiling — every legitimate tool call (LLM
// proxy, MCP bridge, file_read) finishes well under this, while sign-
// flipped deltas (start - now instead of now - start) and absolute
// Unix-ms timestamps dressed as deltas both blow past it. Caller logs
// the original value before clamping so the underlying agent bug stays
// visible. See ClampToolAuditDurationMs and migration 22.
const MaxSaneToolDurationMs int64 = 3600 * 1000

// ClampToolAuditDurationMs normalises an agent-reported duration before
// persistence. Returns 0 for negative or absurd values; passthrough
// otherwise. Cross-package because three writers feed the audit log:
// the realtime stream handler in api/, the post-step batch in
// executor/, and the dispatcher tool wrapper. All three need the same
// bounds check; duplicating it created the gap that let -1.6e12
// values land via the realtime path while the batch path clamped.
// CrossProjectCallStatus enumerates the states a cross-project
// call moves through. See https://docs.vornik.io
// orchestration-design.md §4.1 for the state machine.
type CrossProjectCallStatus string

const (
	// CPCStatusPending — row created, callee task created, no
	// scheduler has leased the callee task yet.
	CPCStatusPending CrossProjectCallStatus = "pending"
	// CPCStatusRunning — callee task has been leased and is
	// running (or paused mid-flight). Set when the callee task
	// transitions RUNNING for the first time.
	CPCStatusRunning CrossProjectCallStatus = "running"
	// CPCStatusCompleted — callee task terminated COMPLETED
	// and its result envelope validated against expected_schema.
	CPCStatusCompleted CrossProjectCallStatus = "completed"
	// CPCStatusFailed — callee task terminated FAILED/CANCELLED.
	CPCStatusFailed CrossProjectCallStatus = "failed"
	// CPCStatusTimedOut — timeout_at elapsed before the callee
	// reached a terminal state. The callee task is NOT auto-
	// cancelled (v1 design choice); operator can clean up.
	CPCStatusTimedOut CrossProjectCallStatus = "timed_out"
	// CPCStatusRejected — caller violated acceptCallsFrom, the
	// callee project doesn't exist, or the result envelope
	// failed schema validation.
	CPCStatusRejected CrossProjectCallStatus = "rejected"
)

// IsTerminal reports whether the call has finished moving and
// is safe to skip in the timeout scanner.
func (s CrossProjectCallStatus) IsTerminal() bool {
	switch s {
	case CPCStatusCompleted, CPCStatusFailed, CPCStatusTimedOut, CPCStatusRejected:
		return true
	}
	return false
}

// CrossProjectCall is the durable ledger row for one
// inter-project delegation. See migration v52 + the LLD above.
type CrossProjectCall struct {
	ID             string `json:"id"`
	CallerTaskID   string `json:"caller_task_id"`
	CallerStepID   string `json:"caller_step_id"`
	CallerProject  string `json:"caller_project"`
	CalleeProject  string `json:"callee_project"`
	CalleeWorkflow string `json:"callee_workflow"`
	// CalleeTaskID is set after the callee task is created. nil
	// transiently between row insert and task create (within the
	// same transaction in the v1 design — so nil rows shouldn't
	// be observable in practice).
	CalleeTaskID   *string                `json:"callee_task_id,omitempty"`
	Payload        []byte                 `json:"payload"`
	ExpectedSchema string                 `json:"expected_schema"`
	Status         CrossProjectCallStatus `json:"status"`
	ResultEnvelope []byte                 `json:"result_envelope,omitempty"`
	ErrorMessage   *string                `json:"error_message,omitempty"`
	TimeoutAt      *time.Time             `json:"timeout_at,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	ResolvedAt     *time.Time             `json:"resolved_at,omitempty"`
	// CancelOnTimeout drives the timeout scanner's cascade
	// behaviour for this call. When true the scanner cancels
	// the callee task on timeout in addition to resolving the
	// CPC; when false (default) the callee keeps running and
	// the operator decides via vornikctl. LLD §8.1.
	CancelOnTimeout bool `json:"cancel_on_timeout,omitempty"`
}

// ProjectSpawn is the lineage row for one spawn_project step
// execution. Persisted in project_spawns (migration v53); see
// https://docs.vornik.io §5.2.
//
// Distinct from CrossProjectCall: a CPC is a synchronous wait
// for an existing project's workflow; a spawn creates a NEW
// project at runtime. Both can fire from the same workflow
// (the marketing → sales-launch chain in the LLD uses CPC for
// architect + implementation, spawn for sales-q3-launch-2026).
type ProjectSpawn struct {
	ID             string    `json:"id"`
	ParentTaskID   string    `json:"parent_task_id"`
	ParentProject  string    `json:"parent_project"`
	ParentStepID   string    `json:"parent_step_id"`
	SpawnedProject string    `json:"spawned_project"`
	TemplateSlug   string    `json:"template_slug"`
	Params         []byte    `json:"params"`
	CreatedAt      time.Time `json:"created_at"`
}

func ClampToolAuditDurationMs(ms int64) int64 {
	if ms < 0 || ms > MaxSaneToolDurationMs {
		return 0
	}
	return ms
}

// ToolAuditEntry records a single tool invocation for audit purposes.
//
// DurationMs is int64 to match the column's BIGINT type (migration 22).
// Pre-22 it was `int` and the column was INTEGER; agent-emitted values
// >2.1e9 ms (caused by ms_now() drift on the agent side) silently
// overflowed the column and the audit row dropped. The widening
// closes that loss class; the daemon also clamps the value to a sane
// range before INSERT (ClampToolAuditDurationMs above) so a buggy
// agent can't pollute the audit with absolute timestamps.
type ToolAuditEntry struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      string    `json:"task_id"`
	ExecutionID string    `json:"execution_id"`
	StepID      string    `json:"step_id,omitempty"`
	ToolName    string    `json:"tool_name"`
	ToolInput   string    `json:"tool_input,omitempty"`
	ToolOutput  string    `json:"tool_output,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ToolAuditFilter defines filtering options for tool audit queries.
//
// StepID scopes the result to one workflow step within an execution.
// Used by the Phase 2 verifier engine to keep per-step verifiers from
// inheriting an earlier step's audit (e.g. the recover step seeing the
// research step's 4 blocked fetches and re-failing the same verifier).
type ToolAuditFilter struct {
	ProjectID   *string
	TaskID      *string
	ExecutionID *string
	StepID      *string
	ToolName    *string
	PageSize    int
	Offset      int
}

// RecoveryEvent is an append-only marker written when an execution reaches a
// terminal flagged Recovery:true (an intentional on_fail→recovery exit). Recovery
// terminals are COMPLETED-status, so this is the only durable signal that a run
// recovered rather than completed straight through. See
// https://docs.vornik.io
type RecoveryEvent struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      string    `json:"task_id"`
	ExecutionID string    `json:"execution_id"`
	WorkflowID  string    `json:"workflow_id"`
	TerminalID  string    `json:"terminal_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// AdminAuditEntry is one operator-action row in the admin_audit
// table — see admin-ui-design.md §3.3. Distinct from the per-call
// ToolAuditEntry (which logs agent tool invocations); admin_audit
// captures operator actions against the daemon itself (admin UI
// posts, /admin CLI commands, danger-zone confirmations).
//
// Before / After are stored as raw JSON strings so a config-edit
// row can carry the pre/post values for diffing in the UI without
// the admin code having to know each value's typed shape.
type AdminAuditEntry struct {
	// ID is the row PK. Format: admaud_<utc-ts>_<hex>. Generated
	// by callers via persistence.GenerateID("admaud").
	ID string `json:"id"`
	// Timestamp the action happened. UTC. Defaults to NOW() when
	// the caller leaves it zero — Insert handles the substitution.
	Timestamp time.Time `json:"ts"`
	// Principal identifies who triggered the action. For admin-UI
	// POSTs this is the matched API key's id; for /admin CLI this
	// is the Telegram user id (prefixed with "tg:"). Free-form to
	// accommodate future auth backends.
	Principal string `json:"principal"`
	// Source identifies the entry point: "ui", "cli", or "api".
	Source string `json:"source"`
	// Action is a short verb-noun identifier — e.g. "mcp.refresh",
	// "config.reload", "key.revoke". Used as both the audit row's
	// human label and the /ui/admin/audit filter axis.
	Action string `json:"action"`
	// Target is the affected object's identifier — typically a
	// project ID, key ID, or MCP server name. Empty for global
	// actions (e.g. "config.reload").
	Target string `json:"target"`
	// Before / After carry pre/post snapshots for diff-able mutations.
	// Empty (zero-length) on actions where the concept doesn't apply
	// (e.g. mcp.refresh — no before/after state).
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// IP is the source IP (X-Forwarded-For honoured upstream). May be
	// empty for CLI-originated rows.
	IP string `json:"ip,omitempty"`
	// UserAgent is the HTTP User-Agent header as observed. May be
	// empty for CLI-originated rows.
	UserAgent string `json:"user_agent,omitempty"`
}

// AdminAuditFilter narrows a List call. PageSize is required —
// implementations reject filter.PageSize <= 0 to prevent unbounded
// scans on a hot operator surface.
type AdminAuditFilter struct {
	// Action filters to a single verb-noun (exact match).
	Action string
	// Principal filters to a single actor (exact match).
	Principal string
	// TargetPrefix filters rows whose Target starts with this
	// string. Allows narrow-to-project filters via "<projectID>".
	TargetPrefix string
	// Since / Until bound the timestamp range. Zero values disable
	// the corresponding bound.
	Since time.Time
	Until time.Time
	// PageSize caps the result count. Required to be > 0.
	PageSize int
	// Offset for pagination. 0-based.
	Offset int
}

// ChatAuditEntry is one dispatcher turn — one inbound user message
// processed through the LLM tool loop until the final assistant
// reply is sent. Captures everything an operator needs to answer
// "why did the bot do X (or not do X)?" without grepping journald
// for log lines. Distinct from:
//
//   - ToolAuditEntry: agent-side tool invocations inside task
//     containers (not chat).
//   - AdminAuditEntry: operator config-change actions.
//
// SystemPromptHash + the parallel chat_system_prompts table form a
// content-addressed prompt cache: the full prompt (typically
// 5-10 KB) is stored once and referenced from every turn that used
// it, keeping the chat_audit_log table compact while still being
// fully reproducible. Hash is sha256 hex of the prompt bytes.
type ChatAuditEntry struct {
	// ID is the row PK. Format: chat_<utc-ts>_<hex>. Generated by
	// callers via persistence.GenerateID("chat").
	ID string `json:"id"`
	// Timestamp the turn completed (UTC). Defaults to NOW() when
	// callers leave it zero.
	Timestamp time.Time `json:"ts"`
	// ChatID identifies the conversation. For Telegram this is the
	// chat_id int64; for other channels it's the channel-native
	// session identifier stringified.
	ChatID string `json:"chat_id"`
	// UserID identifies the speaker. Free-form so future channels
	// (Slack, email) plug in without schema churn. Empty for
	// server-internal synthetic turns (e.g. task auto-resume).
	UserID string `json:"user_id"`
	// ProjectID is the user's active project at turn start. Empty
	// means no project pin / dispatcher generic mode.
	ProjectID string `json:"project_id"`
	// RoleUsed is the dispatcher role label ("dispatcher" for the
	// generic surface, "lead" when the user has pinned a project
	// and the lead's system prompt is active). Future channels add
	// channel-specific labels.
	RoleUsed string `json:"role_used"`
	// Model is the LLM model identifier the turn was completed with
	// (e.g. "nvidia.nemotron-super-3-120b"). May reflect a fallback
	// when the primary failed mid-turn.
	Model string `json:"model"`
	// SystemPromptHash is the sha256 hex of the rendered system
	// prompt. Points at the chat_system_prompts table for the
	// full body. Empty when the prompt couldn't be captured.
	SystemPromptHash string `json:"system_prompt_hash"`
	// UserMessage is the operator's inbound text excerpt (first
	// ~500 chars). Full text isn't persisted to bound row size.
	UserMessage string `json:"user_message"`
	// ToolCallsJSON is the JSON-encoded list of tool calls the
	// dispatcher made during this turn, with names + truncated
	// arguments + truncated results. Empty array `[]` for turns
	// the model answered directly without any tool calls.
	ToolCallsJSON string `json:"tool_calls_json"`
	// Response is the final assistant reply excerpt (first ~500
	// chars). For streaming turns this is the post-stream
	// accumulated text.
	Response string `json:"response"`
	// Iterations is the count of LLM round-trips (tool loop
	// iterations + final reply). 1 means a one-shot answer with no
	// tool calls.
	Iterations int `json:"iterations"`
	// DurationMs is the end-to-end turn wall-clock (message
	// received → final reply sent).
	DurationMs int64 `json:"duration_ms"`
	// CostUSD is the aggregated dollar cost across every LLM call
	// in this turn (input + output tokens × pricing table).
	CostUSD float64 `json:"cost_usd"`
	// HallucinationSignalsJSON is the JSON-encoded array of
	// hallucination.Signal entries the dispatcher's detector fired
	// on this turn's reply. Empty string when no detector fired
	// (the common case). Populated only when the agent has a
	// hallucination detector wired — older deployments without it
	// always see empty. Migration 44 (chat_audit_hallucination_
	// signals) adds the underlying column.
	HallucinationSignalsJSON string `json:"hallucination_signals_json,omitempty"`
}

// ChatAuditFilter narrows a List call. PageSize is required.
type ChatAuditFilter struct {
	// ChatID filters to a single conversation.
	ChatID string
	// ProjectID filters to a single project.
	ProjectID string
	// Since / Until bound the timestamp range. Zero values disable
	// the corresponding bound.
	Since time.Time
	Until time.Time
	// PageSize caps the result count. Required to be > 0.
	PageSize int
	// Offset for pagination. 0-based.
	Offset int
}

// WebhookEvent records the durable ingress audit trail for signed webhooks.
type WebhookEvent struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Source       string    `json:"source"`
	EventID      string    `json:"event_id"`
	PayloadHash  string    `json:"payload_hash"`
	Status       string    `json:"status"`
	TaskID       *string   `json:"task_id,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// WebhookEvent status values.
const (
	WebhookEventStatusAccepted  = "accepted"
	WebhookEventStatusRejected  = "rejected"
	WebhookEventStatusDuplicate = "duplicate"
	// WebhookEventStatusFiltered marks a verified delivery that a source's
	// `filter` predicate excluded — acknowledged 200 with no task created.
	WebhookEventStatusFiltered = "filtered"
)

// IntentVerdict captures the two-tier intent judge's decision for
// one tool call. The heuristic tier (sync, sub-ms) fills the
// heuristic_* fields immediately; the LLM tier (async, seconds)
// refines them later and stamps refined_at when it lands.
// final_risk / final_rec are what the dispatcher actually acted
// on at the time of the call — usually the heuristic verdict
// (LLM rarely beats it back), updated to the LLM tier only when
// the async refinement returned before the dispatcher consulted
// the verdict for a follow-up decision.
type IntentVerdict struct {
	ID          string
	ProjectID   string
	TaskID      *string
	ExecutionID *string
	ChatID      *int64
	ToolName    string
	ToolArgs    string

	HeuristicRisk           string
	HeuristicConfidence     float64
	HeuristicRecommendation string
	HeuristicReasoning      string
	HeuristicLatencyMs      int64

	// LLM fields are nullable — populated by the async refiner
	// after the row is initially written.
	LLMRisk           *string
	LLMConfidence     *float64
	LLMRecommendation *string
	LLMReasoning      *string
	LLMLatencyMs      *int64
	LLMModel          *string

	FinalRisk           string
	FinalRecommendation string

	CreatedAt time.Time
	RefinedAt *time.Time
}

// APIKey is one DB-backed bearer token bound to a single project.
// Replaces the static YAML `api.api_keys` map for new deployments
// while leaving that path intact for legacy single-tenant installs.
// The raw key never lives in the DB — only the sha256 hex digest
// (KeyHash) — so a DB dump can't be used to authenticate.
//
// `KeyPrefix` stores the first 12 chars (e.g. "sk-vornik-Ab")
// purely for UI display so operators can recognise which row
// they're about to revoke. `Name` is operator-supplied.
//
// Lifecycle: rows are soft-deleted (RevokedAt set) rather than
// removed so the audit trail survives revocation. ExpiresAt is
// optional — empty means "no expiry".
type APIKey struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"` // never serialised over the API
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	// RateLimitRPS / RateLimitBurst configure per-key request
	// throttling. Both nil = no limit (the legacy default).
	// AuthMiddleware reads these after a successful DB-key match
	// and feeds them to internal/ratelimit.APIKeyLimiter; a
	// blocked call returns HTTP 429 with a Retry-After header
	// derived from the bucket's refill time.
	RateLimitRPS   *int `json:"rate_limit_rps,omitempty"`
	RateLimitBurst *int `json:"rate_limit_burst,omitempty"`

	// AllowedWorkflows narrows the workflow IDs this key can
	// invoke via the companion MCP server's delegate() tool.
	// Nil means "every workflow the project permits" — the same
	// posture as a non-companion key. An empty slice ([] after
	// JSON decode) means "explicitly none" and is a footgun we
	// reject at grant time. See LLD 21.
	AllowedWorkflows []string `json:"allowed_workflows,omitempty"`

	// BudgetCapUSD is the lifetime USD ceiling for delegated work
	// invoked with this key. Nil = uncapped. The companion MCP
	// server checks this before accepting a delegate() call; the
	// running cost (sum of cost_usd over the key's tasks) is
	// computed on demand from cost_attribution events.
	BudgetCapUSD *float64 `json:"budget_cap_usd,omitempty"`

	// ClientKind labels the host LLM client that owns this key:
	// "claude-code", "codex", "opencode", "gemini-cli". Empty on
	// non-companion keys. Used to filter list/audit views and to
	// stamp client identity into task metadata.
	ClientKind string `json:"client_kind,omitempty"`

	// SessionLabel is an operator-friendly marker like
	// "vadim/laptop". Pure UX — never authoritative.
	SessionLabel string `json:"session_label,omitempty"`

	// MemoryRead / MemoryWrite grant access to the companion RAG
	// MCP tools (LLD 22). Both default false; set per-key by
	// `vornikctl companion grant --memory-read` / `--memory-write`.
	// `recall` requires MemoryRead; `remember` requires both
	// (write implies read — the caller must be able to verify a
	// deposit landed). Stored as BOOLEAN on postgres and INTEGER
	// 0/1 on the sqlite mirror.
	MemoryRead  bool `json:"memory_read,omitempty"`
	MemoryWrite bool `json:"memory_write,omitempty"`

	// AllowPush grants git-push access over HTTPS (git-over-HTTPS design,
	// LLD slice 2). Default false — keys are read-only by default; push
	// is opt-in per key. Stored as BOOLEAN on postgres and INTEGER 0/1
	// on the sqlite mirror.
	AllowPush bool `json:"allow_push,omitempty"`
}

// RotatedCopy returns the replacement key for a rotation: a brand-new
// identity (caller supplies id / keyHash / keyPrefix / createdBy and the
// timestamp) that inherits EVERY scope, limit, and capability attribute
// of the prior key. LastUsedAt and RevokedAt are intentionally left zero
// — the replacement is fresh and active.
//
// Both the REST rotate handler and the web-UI rotate action MUST build
// the replacement through this method. They previously hand-constructed
// the struct independently and drifted: the UI path omitted the whole
// companion scope block (ClientKind, MemoryRead/Write, AllowedWorkflows,
// SessionLabel, BudgetCapUSD) plus the rate-limit fields, and the REST
// path omitted AllowPush. The UI omission silently demoted a UI-rotated
// companion key to a plain, unscoped key — the companion MCP endpoint
// then rejected it with "not a companion-scoped key" (incident
// 2026-06-27). Centralising the carry-over here removes the divergence.
func (k *APIKey) RotatedCopy(id, keyHash, keyPrefix, createdBy string, now time.Time) *APIKey {
	fresh := &APIKey{
		ID:               id,
		ProjectID:        k.ProjectID,
		Name:             k.Name,
		KeyHash:          keyHash,
		KeyPrefix:        keyPrefix,
		CreatedAt:        now,
		ExpiresAt:        k.ExpiresAt,
		CreatedBy:        createdBy,
		AllowedWorkflows: append([]string(nil), k.AllowedWorkflows...),
		ClientKind:       k.ClientKind,
		SessionLabel:     k.SessionLabel,
		MemoryRead:       k.MemoryRead,
		MemoryWrite:      k.MemoryWrite,
		AllowPush:        k.AllowPush,
	}
	// Pointer-valued fields: copy the pointee so the rotated row never
	// aliases the prior row's storage.
	if k.RateLimitRPS != nil {
		v := *k.RateLimitRPS
		fresh.RateLimitRPS = &v
	}
	if k.RateLimitBurst != nil {
		v := *k.RateLimitBurst
		fresh.RateLimitBurst = &v
	}
	if k.BudgetCapUSD != nil {
		v := *k.BudgetCapUSD
		fresh.BudgetCapUSD = &v
	}
	return fresh
}

// TaskKeyNamePrefix is the reserved APIKey.Name prefix for per-task
// scoped agent credentials minted by the container scheduler. The
// task ID is appended verbatim: "agent:task_<taskID>". Two parties
// share this contract — the minter (service.taskKeyMinter) writes
// names with this prefix; the MCP consumer (api.CallMCPTool) reads
// the bound task ID back out of an authenticated key's name. It is
// extracted here (next to the APIKey model) so the literal is not
// open-coded at both ends, and so operator-facing key creation can
// reject the prefix to close the confused-deputy hole (FIX 3).
const TaskKeyNamePrefix = "agent:task_"

// TaskIDFromKeyName returns the task ID bound to a task-scoped API
// key name, and true when name carries the reserved
// TaskKeyNamePrefix. Returns ("", false) for any other name. A
// prefix with an empty remainder ("agent:task_") is not a valid
// binding and returns false so callers don't treat an empty task ID
// as authoritative.
func TaskIDFromKeyName(name string) (string, bool) {
	if !strings.HasPrefix(name, TaskKeyNamePrefix) {
		return "", false
	}
	id := strings.TrimPrefix(name, TaskKeyNamePrefix)
	if id == "" {
		return "", false
	}
	return id, true
}

// AutonomyEvaluation records the outcome of one autonomy tick. One row is
// written per tick, regardless of whether a task was created — operators
// need to see rejections, rate-limit blocks, and no-action decisions so
// silent autonomy failure stops being invisible.
type AutonomyEvaluation struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Outcome    string    `json:"outcome"`               // see AutonomyOutcome* constants
	Reason     string    `json:"reason,omitempty"`      // short human-readable detail
	TaskID     *string   `json:"task_id,omitempty"`     // set only on CREATED
	TaskType   string    `json:"task_type,omitempty"`   // echoes args.type from the LLM
	WorkflowID string    `json:"workflow_id,omitempty"` // effective workflow (explicit OR project default)
	PromptHash string    `json:"prompt_hash,omitempty"` // SHA-256 of the normalised prompt for correlation
	DurationMs int64     `json:"duration_ms,omitempty"` // wall-clock duration of the tick
	CreatedAt  time.Time `json:"created_at"`
}

// AutonomyOutcome values recorded in autonomy_evaluations.outcome.
// Kept as plain strings so the enum can grow without a column migration.
const (
	AutonomyOutcomeCreated         = "CREATED"
	AutonomyOutcomeNoAction        = "NO_ACTION"
	AutonomyOutcomeRateLimited     = "RATE_LIMITED"
	AutonomyOutcomeBudgetBlocked   = "BUDGET_BLOCKED"
	AutonomyOutcomeActiveTasks     = "ACTIVE_TASKS"
	AutonomyOutcomeLLMError        = "LLM_ERROR"
	AutonomyOutcomeParseError      = "PARSE_ERROR"
	AutonomyOutcomeWorkflowInvalid = "WORKFLOW_INVALID"
	AutonomyOutcomeTypeRejected    = "TYPE_REJECTED"
	AutonomyOutcomeCircuitOpen     = "CIRCUIT_OPEN"
	AutonomyOutcomeDuplicate       = "DUPLICATE"
	AutonomyOutcomeCooldown        = "COOLDOWN"
	AutonomyOutcomeIdempotencyHit  = "IDEMPOTENCY_HIT"
	AutonomyOutcomeDBError         = "DB_ERROR"
	// AutonomyOutcomePreCheckSkipped means the project's
	// pre-LLM gate (autonomy.preCheck) refused this tick.
	// e.g. "trading-rth" rejected the tick because market is
	// closed or insufficient time remains for a workflow.
	AutonomyOutcomePreCheckSkipped = "PRECHECK_SKIPPED"
	// AutonomyOutcomeAborted is benign teardown — the eval's LLM call
	// was cancelled because the autonomy loop was torn down mid-eval
	// (config reload / loop restart / daemon shutdown). NOT an error:
	// the loop re-evaluates on the next start/tick.
	AutonomyOutcomeAborted = "ABORTED"
)

// MemoryRetrievalAudit is one Searcher.Search call's record. Captures
// the chunks returned plus best-effort attribution to the
// (task, execution, step) that triggered the search. Persisted for
// chunk-utility analytics: which chunks are dead weight, which
// pulled their weight by feeding successful steps.
type MemoryRetrievalAudit struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TaskID      *string   `json:"task_id,omitempty"`
	ExecutionID *string   `json:"execution_id,omitempty"`
	StepID      *string   `json:"step_id,omitempty"`
	Role        *string   `json:"role,omitempty"`
	Query       string    `json:"query"`
	ChunkIDs    []string  `json:"chunk_ids"`
	RetrievedAt time.Time `json:"retrieved_at"`
	// ActorKind / ActorID split agent vs companion recalls on the
	// audit row (LLD 22 migration 72). Existing Role column stays
	// populated for backwards-compat — these provide the indexable
	// "show every companion recall this week" query without LIKE-
	// scanning role. nil-safe: pre-LLD-22 rows return nil here.
	ActorKind *string `json:"actor_kind,omitempty"`
	ActorID   *string `json:"actor_id,omitempty"`
	// RepoScope records which repo partition the caller queried.
	// Migration 75. Nil = no scope filter applied (project-wide
	// query). Surfacing on the audit row lets dashboards report
	// "what scope is being searched most" + makes the
	// retrieval-audit feedback loop scope-aware.
	RepoScope *string `json:"repo_scope,omitempty"`
}

// MemoryRetrievalAuditFilter narrows a MemoryRetrievalAuditRepository.
// List call. All fields optional; empty values disable that axis.
// PageSize must be > 0 — implementations reject zero to keep the
// /ui/admin/memory-audit page bounded.
type MemoryRetrievalAuditFilter struct {
	ProjectID string
	ActorKind string // exact match — "companion:claude-code", "rest_api", "ui", "agent"
	RepoScope string // exact match; pass "" to disable
	Since     time.Time
	PageSize  int
	Offset    int
}

// MemoryIngestAuditFilter narrows a MemoryIngestAuditRepository.List
// call. Symmetric to MemoryRetrievalAuditFilter; PageSize required.
type MemoryIngestAuditFilter struct {
	ProjectID string
	ActorKind string
	RepoScope string
	Decision  string // "admitted" / "quarantined" / "rejected"
	Since     time.Time
	PageSize  int
	Offset    int
}

// MemoryIngestAuditDecision enumerates the gate-stack outcome
// recorded on a memory_ingest_audit row. Mirrors the GateOutcome
// constants in internal/memory but kept stringly-typed at the
// persistence boundary to avoid an import cycle.
const (
	MemoryIngestAuditAdmitted    = "admitted"
	MemoryIngestAuditQuarantined = "quarantined"
	MemoryIngestAuditRejected    = "rejected"
)

// MemoryIngestAudit is one IngestCompanionNote / equivalent direct-
// deposit call's record. Captures the actor that initiated the
// deposit, a content hash + byte count, and the final gate-stack
// decision (admit / quarantine / reject). Append-only; powers the
// per-call ingest audit trail that LLD-22 left to the queue +
// quarantine tables (which work fine for queued agent deposits but
// don't see companion-direct deposits that bypass the queue).
//
// One row per deposit attempt. Migration 74.
type MemoryIngestAudit struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	ActorKind      *string   `json:"actor_kind,omitempty"` // "companion:<client_kind>", "agent", nil for legacy
	ActorID        *string   `json:"actor_id,omitempty"`   // api_keys.id for companion; role name for agent
	SourceName     string    `json:"source_name"`
	ContentHash    string    `json:"content_hash"`
	ContentBytes   int64     `json:"content_bytes"`
	ProposedClass  *string   `json:"proposed_class,omitempty"`
	Decision       string    `json:"decision"` // one of the MemoryIngestAudit* constants
	GateFailed     *string   `json:"gate_failed,omitempty"`
	ChunksAdmitted int       `json:"chunks_admitted"`
	IngestedAt     time.Time `json:"ingested_at"`
	// RepoScope partitions deposits within one project. Migration 75.
	// Nil = uncategorized; "*" = cross-cutting; any other string =
	// repo token (typically git remote URL <host>/<path> or repo
	// basename). See LLD-22 follow-on for full semantics.
	RepoScope *string `json:"repo_scope,omitempty"`
}

// CorpusEpoch is one row of corpus_epochs — the manifest for one
// ingest pipeline run. Iceberg-style snapshot. IsActive is filled
// by joins through corpus_epochs_active when listing.
type CorpusEpoch struct {
	ID                string
	ProjectID         string
	IngestExecutionID *string
	CreatedAt         time.Time
	ClosedAt          *time.Time
	ChunksAdmitted    int
	ChunksQuarantined int
	ChunksVerified    int
	ChunksRefuted     int
	ChunksSuperseded  int
	Notes             *string
	IsActive          bool
}

// CorpusEpochCounts is the per-class summary the pipeline rolls up
// at snapshot time.
type CorpusEpochCounts struct {
	Admitted    int
	Quarantined int
	Verified    int
	Refuted     int
	Superseded  int
}

// CorpusRollback is one row of corpus_rollbacks — the audit trail
// for corpus_epochs_active mutations.
type CorpusRollback struct {
	ID          string
	ProjectID   string
	FromEpochID *string
	ToEpochID   *string
	TriggeredBy string
	Reason      *string
	AppliedAt   time.Time
	// ChunksRestored is the supersession-revert pass count (migration
	// 89): superseded chunks whose causing epoch this rollback
	// deactivated and whose prior validation_status was restored.
	ChunksRestored int
}

// MemoryQuarantineItem is one row of project_memory_quarantine: a
// chunk that an ingest gate refused. Operators can inspect via
// `vornikctl memory quarantine list`, release with `release <id>`
// (which re-runs the gates with overrides), or drop with `drop <id>`.
type MemoryQuarantineItem struct {
	ID                string
	ProjectID         string
	SourceArtifactID  string
	ProducerRole      *string
	IngestExecutionID *string
	Content           string
	ContentHash       string
	ProposedClass     *string
	FailedGate        string
	FailureDetail     *string
	QuarantinedAt     time.Time
	ReleasedAt        *time.Time
	ReleasedChunkID   *string
	DroppedAt         *time.Time
	// RepoScope partitions quarantine rows by repo within one
	// project. Migration 75. See MemoryIngestAudit.RepoScope for
	// the full convention.
	RepoScope *string
}

// IngestQueueItem is one row of project_ingest_queue: a producer's
// request to ingest the content of one OUTPUT artifact into project
// memory. Drained by the IngestWorker which dispatches the rag-ingest
// pipeline (Phase 2+) — Phase 1 calls IngestText directly.
type IngestQueueItem struct {
	ID                 string
	ProjectID          string
	SourceArtifactID   string
	ProducerRole       string
	IngestExecutionID  *string
	Priority           int16
	ProposedClass      *string
	ProposedConfidence float32
	State              string // queued | processing | done | failed
	Attempts           int16
	EnqueuedAt         time.Time
	StartedAt          *time.Time
	FinishedAt         *time.Time
	LastError          *string
	// RepoScope is the deposit-time repo_scope (migration 76).
	// Carried across the enqueue → drain boundary so the worker
	// can stamp it on the resulting chunks via PatchScopeByArtifact
	// after IngestText returns. NULL = uncategorized.
	RepoScope *string
}

// MemoryFeedbackStats aggregates retrieval activity for one project
// over a window. Powers `vornikctl memory feedback` and the future
// auto-prune flow. Kept narrow so the SQL stays cheap; richer
// breakdowns live as separate queries.
type MemoryFeedbackStats struct {
	// TotalChunks is the count of chunks currently indexed for the
	// project (project_memory_chunks).
	TotalChunks int
	// RetrievedChunks is the count of distinct chunk IDs that
	// appeared in any retrieval row in the window.
	RetrievedChunks int
	// UnretrievedChunks is TotalChunks - RetrievedChunks. Auto-prune
	// candidates: chunks that have been in the index for the full
	// window without ever being recalled.
	UnretrievedChunks int
	// TotalSearches is the total number of Search calls in the window.
	TotalSearches int
}

// AutonomyEvaluationFilter defines filtering options for evaluation queries.
type AutonomyEvaluationFilter struct {
	ProjectID *string
	Outcome   *string
	PageSize  int
	Offset    int
}

// WebhookEventFilter defines filtering options for webhook ingress audit rows.
type WebhookEventFilter struct {
	ProjectID *string
	Source    *string
	Status    *string
	PageSize  int
	Offset    int
}

// TaskLLMUsage records LLM token consumption and derived cost. Two kinds
// of rows are stored here, distinguished by Source:
//
//   - Source="workflow_step": one row per (execution, step_id). The agent
//     container accumulates usage across its tool-calling loop before
//     returning, so there's a single row per step. TaskID/ExecutionID are
//     set; SessionID is nil.
//   - Source="dispatcher": one row per dispatcher LLM call. The dispatcher
//     runs a per-turn tool-calling loop that isn't tied to any task, so
//     TaskID/ExecutionID are nil and SessionID carries the chat/CLI
//     session identifier for rollup.
type TaskLLMUsage struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"project_id"`
	TaskID           *string   `json:"task_id,omitempty"`
	ExecutionID      *string   `json:"execution_id,omitempty"`
	StepID           string    `json:"step_id"`
	Role             string    `json:"role"`
	Model            string    `json:"model"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	Iterations       int       `json:"iterations"`
	CostUSD          float64   `json:"cost_usd"`
	Source           string    `json:"source"`
	SessionID        *string   `json:"session_id,omitempty"`
	RecordedAt       time.Time `json:"recorded_at"`
	// CacheCreationTokens / CacheReadTokens carry the LLM-caching
	// phase-A observability fields. Populated by the chat router
	// from Bedrock + Anthropic response metadata (zero on
	// providers without prompt caching). The spend dashboard uses
	// these to render a cache-hit-ratio tile + dollar-savings
	// counter once pricing.yaml gains cache rates.
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
}

// TaskLLMUsage.Source values.
const (
	TaskLLMUsageSourceWorkflowStep = "workflow_step"
	TaskLLMUsageSourceDispatcher   = "dispatcher"
	// TaskLLMUsageSourceExternalAPI — calls made by third-party
	// clients to the OpenAI- and Ollama-compatible proxy
	// surfaces (/v1/chat/completions, /api/chat, /api/generate).
	// task_id and execution_id are NULL — these calls don't ride
	// a workflow. project_id comes from the X-Vornik-Project-ID
	// header when set, falling back to the daemon's configured
	// ExternalAPIBillingProjectID (or "_external" when neither
	// is set) so the spend dashboard can attribute cost to a
	// real bucket instead of leaving it unbilled. role is always
	// "external_api"; session_id carries the client-supplied
	// User-Agent (or a stable fingerprint) so audit queries can
	// group by client.
	TaskLLMUsageSourceExternalAPI = "external_api"
	// TaskLLMUsageSourceJudge — one row per Phase 3 LLM-as-judge
	// call. Fires once per task that opted in. Distinct source so
	// the spend dashboard can split judge cost from worker cost
	// when computing $/success per role-model pair (a worker
	// model with a judge attached should see its own
	// effective-cost figure unaffected by the judge's spend).
	TaskLLMUsageSourceJudge = "judge"
	// TaskLLMUsageSourcePostMortem — one row per failed-task
	// explainer call. Triggered by the operator from the failed-
	// task UI; idempotent per task (cached result returns
	// without re-billing).
	TaskLLMUsageSourcePostMortem = "post_mortem"
	// TaskLLMUsageSourceKGExtraction — KG extraction pipeline
	// stages (extractor / resolver / relationship / validator).
	// One row per stage per chunk. task_id is NULL because the
	// KG worker is a background drain loop, not a per-task
	// surface; project_id + step_id (= chunk_id) are the
	// load-bearing labels for cost attribution. role carries
	// the stage name ("kg_extractor", "kg_resolver", etc.) so
	// the spend dashboard can split per-stage.
	TaskLLMUsageSourceKGExtraction = "kg_extraction"
	// TaskLLMUsageSourceMemoryTitler — one row per topic-label
	// generation for a memory chunk. Drives the node names in
	// the operator vector-cloud UI. task_id is NULL (background
	// consumer); project_id + step_id (= chunk_id) attribute
	// cost. role is always "memory_titler".
	TaskLLMUsageSourceMemoryTitler = "memory_titler"
	// TaskLLMUsageSourceMemoryClassifier — one row per LLM
	// classification call (vornikctl memory reclassify --use-llm
	// + future on-ingest classifier hooks). Same attribution
	// shape as the titler: task_id NULL, project_id + step_id
	// (= chunk_id), role = "memory_classifier".
	TaskLLMUsageSourceMemoryClassifier = "memory_classifier"
	// TaskLLMUsageSourceMemoryNarrative — one row per LLM-tier
	// project narrative produced by LLMConsolidateWorker. Layers
	// on top of the LLM-free term-frequency gist; runs on a
	// slower cadence (default 1h). task_id NULL (background
	// consumer); project_id is the only attribution label since
	// the narrative covers the project as a whole. role =
	// "memory_narrative".
	TaskLLMUsageSourceMemoryNarrative = "memory_narrative"
)

// TaskLLMUsageFilter defines filtering options for LLM usage queries.
// Combined with Since/Until it powers the UI spend panel ("last 24h",
// "last 7d") and the per-project budget checks.
type TaskLLMUsageFilter struct {
	ProjectID   *string
	TaskID      *string
	ExecutionID *string
	Role        *string
	Model       *string
	Source      *string
	SessionID   *string
	Since       *time.Time
	Until       *time.Time
	PageSize    int
	Offset      int
}

// ExecutionStepOutcome records the per-step *output usability* signal.
// One row per (execution, step_id); rows start in Outcome="pending_validation"
// and are finalized by the consumer (the next step) when it tries to use
// the output. Unlike TaskLLMUsage (which measures cost/tokens), this
// table measures whether the producer's output was actually usable.
//
// Attribution rule: when a downstream consumer can't use an upstream
// producer's output, the row for the *producer* gets flipped from
// pending_validation to parse_error / schema_violation / refused, with
// AttributedToStepID pointing at the step whose failure triggered the
// finalization. This keeps the blame on the model that produced the
// garbage, not on the model that tried to read it.
type ExecutionStepOutcome struct {
	ID                 string     `json:"id"`
	ProjectID          string     `json:"project_id"`
	TaskID             string     `json:"task_id"`
	ExecutionID        string     `json:"execution_id"`
	StepID             string     `json:"step_id"`
	Role               string     `json:"role"`
	Model              string     `json:"model"`
	Outcome            string     `json:"outcome"`
	AttributedToStepID *string    `json:"attributed_to_step_id,omitempty"`
	ErrorClass         string     `json:"error_class,omitempty"`
	ErrorDetail        string     `json:"error_detail,omitempty"`
	DurationMS         *int64     `json:"duration_ms,omitempty"`
	FinalizedAt        *time.Time `json:"finalized_at,omitempty"`
	RecordedAt         time.Time  `json:"recorded_at"`

	// HallucinationSignals carries the JSONB-encoded slice of
	// detector findings written by the executor's hallucination
	// pass. The persistence layer round-trips it as a raw JSON
	// blob to keep the persistence package free of a dependency
	// on internal/hallucination — callers marshal/unmarshal at
	// their boundary. Empty / nil means "no signals or detector
	// not run". One row in execution_step_outcomes can carry
	// multiple findings (a step can hallucinate several URLs).
	HallucinationSignals []byte `json:"hallucination_signals,omitempty"`

	// ContextSource records which canonical-context layout
	// resolved at workspace prep ("dot_autonomy" /
	// "plain_autonomy" / "mixed" / ""). Empty when the project
	// doesn't use the .autonomy/ convention. Context-discovery
	// Phase B (migration 56). Operators query this column to
	// see who's still on the legacy plain-autonomy layout.
	ContextSource string `json:"context_source,omitempty"`

	// ComplexityTier is the resolved toolbudget tier that was
	// injected into the agent container at spawn time (e.g.
	// "standard", "complex", "open_ended"). Empty/NULL on
	// non-agent steps and pre-migration rows. Migration 106.
	ComplexityTier string `json:"complexity_tier,omitempty"`

	// EffectiveToolBudget is the integer tool-iteration budget
	// that toolbudget.Resolve returned for this step. NULL on
	// non-agent steps and pre-migration rows. Migration 106.
	EffectiveToolBudget *int `json:"effective_tool_budget,omitempty"`

	// ToolCallsUsed is the count of tool calls reported in the
	// agent's result.json toolAudit list. NULL on non-agent
	// steps and pre-migration rows. Migration 106.
	ToolCallsUsed *int `json:"tool_calls_used,omitempty"`
}

// TaskJudgeVerdict records one LLM-as-judge evaluation of a
// completed task. Phase 3 of hallucination detection: every
// task that opts in (per-project flag) gets one verdict row
// after it terminates. Verdict is "pass" / "fail" / "abstain";
// signals are structured findings echoing the Phase 1 detector
// shape but emitted by the judge LLM rather than rule-based
// extractors.
type TaskJudgeVerdict struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	TaskID     string    `json:"task_id"`
	Role       string    `json:"role"`
	Model      string    `json:"model"`
	Verdict    string    `json:"verdict"`
	Confidence float64   `json:"confidence"`
	Signals    []byte    `json:"signals,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	CostUSD    float64   `json:"cost_usd"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Verdict literal values. Stored as plain strings so ad-hoc
// SQL queries stay readable.
const (
	JudgeVerdictPass    = "pass"
	JudgeVerdictFail    = "fail"
	JudgeVerdictAbstain = "abstain"
)

// TradingOrder is one row in the broker-side audit trail: every
// place_order / place_bracket_order call (including refused
// ones) shows up here. Written via the broker→daemon audit
// channel (POST /api/v1/internal/trading-orders); the broker
// container has no direct postgres access by design — see
// https://docs.vornik.io → "Audit
// Channel" for the rationale.
//
// IdempotencyKey is unique per (project_id, idempotency_key) —
// retried POSTs from the broker's audit writer (after a
// transient daemon outage) collide on this and silently no-op.
//
// status values: submitted | filled | partial | cancelled |
// rejected | refused. "refused" is broker-side policy refusal
// (kill switch, cap, missing stop) — captured for the audit
// trail even though no order ever reached IBKR.
type TradingOrder struct {
	ID               string     `json:"id"`
	ProjectID        string     `json:"project_id"`
	TaskID           *string    `json:"task_id,omitempty"`
	ExecutionID      *string    `json:"execution_id,omitempty"`
	BrokerOrderID    *string    `json:"broker_order_id,omitempty"`
	IdempotencyKey   string     `json:"idempotency_key"`
	Mode             string     `json:"mode"`
	Symbol           string     `json:"symbol"`
	Action           string     `json:"action"`
	OrderType        string     `json:"order_type"`
	Qty              float64    `json:"qty"`
	FilledQty        float64    `json:"filled_qty"`
	LimitPrice       *float64   `json:"limit_price,omitempty"`
	StopPrice        *float64   `json:"stop_price,omitempty"`
	TimeInForce      string     `json:"time_in_force"`
	Status           string     `json:"status"`
	LastStatusReason string     `json:"last_status_reason,omitempty"`
	SubmittedAt      time.Time  `json:"submitted_at"`
	TerminalAt       *time.Time `json:"terminal_at,omitempty"`
}

// TradingOrderFilter scopes trading_orders queries for the
// soak-panel UI tiles + the per-symbol breakdown (Phase 3).
type TradingOrderFilter struct {
	ProjectID *string
	Status    *string
	Symbol    *string
	Since     *time.Time
	Until     *time.Time
	PageSize  int
	Offset    int
}

// TradingSafetyEvent is one broker-side decision worth recording
// independently — kill-switch toggles, drawdown breaker trips,
// every cap refusal, idempotency replay hits. Foundation for
// cross-component audit (compare with the agent's view in
// tool_audit_log) and future quorum-based guardrails.
//
// Kind is a free-form string rather than an enum so new event
// categories don't require a schema change. The curated set
// today: kill_switch_on, kill_switch_off, breaker_trip,
// cap_refused, replay_hit. New kinds get added by code.
//
// Detail is JSONB — kind-specific payload (the cap that
// fired, the replayed order's id, the equity at breaker
// trip, etc).
type TradingSafetyEvent struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	RecordedAt time.Time `json:"recorded_at"`
	Kind       string    `json:"kind"`
	Severity   string    `json:"severity"`
	Symbol     *string   `json:"symbol,omitempty"`
	Detail     []byte    `json:"detail,omitempty"`
}

// TradingSafetyEventFilter scopes safety-event queries for the
// project-page timeline + future quorum-check logic.
type TradingSafetyEventFilter struct {
	ProjectID *string
	Kind      *string
	Symbol    *string
	Since     *time.Time
	Until     *time.Time
	PageSize  int
	Offset    int
}

// Trading safety event kinds (curated; the column accepts
// open vocabulary so new event types don't need a migration).
const (
	TradingSafetyKindKillSwitchOn  = "kill_switch_on"
	TradingSafetyKindKillSwitchOff = "kill_switch_off"
	TradingSafetyKindBreakerTrip   = "breaker_trip"
	TradingSafetyKindCapRefused    = "cap_refused"
	TradingSafetyKindReplayHit     = "replay_hit"
)

// ExecutionHint is one operator-injected nudge for a live
// execution. The executor reads pending rows (applied_at IS NULL)
// at the start of each agent step and prepends them to the user
// message; consuming a hint flips applied_at to NOW() so retries
// don't re-apply.
// See https://docs.vornik.io
//
// TaskID (added 2026-05-26) lets the operator scope a hint to the
// whole TASK rather than one execution. When a task is requeued (a
// new execution is created for the same task), execution-scoped
// hints from the prior execution are orphaned, so steering a task
// across retries previously required re-submitting after every
// failure. A task-scoped hint (ExecutionID="") is consumed by the
// first step of any subsequent execution for that task.
//
// One of TaskID or ExecutionID must be set. ExecutionID-scoped hints
// are the legacy default; TaskID-scoped hints carry across retries.
type ExecutionHint struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	// StepID NULL ("") = apply to whichever step runs next.
	// StepID set = only the named step picks it up.
	StepID    string     `json:"step_id,omitempty"`
	Content   string     `json:"content"`
	AppliedAt *time.Time `json:"applied_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy string     `json:"created_by"`
}

// ProjectWizardSession is one operator's conversational project-
// setup transcript. Created on the first /wizard/converse call,
// updated on every subsequent turn, terminal-flipped on commit.
// See https://docs.vornik.io
//
// Transcript carries the JSON-marshalled []ProjectWizardTurn
// slice. CurrentProposal carries the latest LLM-emitted
// ProjectYAML proposal (nil until the LLM is confident enough
// to propose a draft). ReadyToCommit flips true when the LLM
// signals the draft is committable and the validator agrees.
//
// CommittedProjectID + CommittedAt are non-nil only after a
// successful commit. Subsequent /converse calls on a committed
// session return 410 Gone.
type ProjectWizardSession struct {
	ID                 string     `json:"id"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	OperatorID         string     `json:"operator_id"`
	Transcript         []byte     `json:"transcript"`                 // JSON-encoded []ProjectWizardTurn
	CurrentProposal    []byte     `json:"current_proposal,omitempty"` // JSON-encoded ProjectYAML or null
	SuggestedTemplate  string     `json:"suggested_template,omitempty"`
	ReadyToCommit      bool       `json:"ready_to_commit"`
	CommittedProjectID *string    `json:"committed_project_id,omitempty"`
	CommittedAt        *time.Time `json:"committed_at,omitempty"`
	CancelledAt        *time.Time `json:"cancelled_at,omitempty"`
}

// ProjectWizardTurn is one conversation message inside the
// transcript. Roles mirror the chat conversation: "user" /
// "assistant". On assistant turns, EnvelopeJSON carries the
// LLM's full envelope (message + optional proposal + flags) as
// raw JSON so a future schema change doesn't invalidate older
// rows — the renderer parses opportunistically.
type ProjectWizardTurn struct {
	Role         string    `json:"role"`               // "user" | "assistant"
	Content      string    `json:"content"`            // operator-visible message text
	EnvelopeJSON []byte    `json:"envelope,omitempty"` // assistant turns: full envelope JSON
	CreatedAt    time.Time `json:"created_at"`
}

// TaskPostMortem is one LLM-generated explainer for why a
// failed task failed. Joins step outcomes + tool audit + last
// container-log lines into a one-paragraph operator-friendly
// summary. One row per task — the post-mortem is idempotent
// per task; re-triggering returns the cached row instead of
// burning another LLM call.
type TaskPostMortem struct {
	TaskID           string    `json:"task_id"`
	ProjectID        string    `json:"project_id"`
	Summary          string    `json:"summary"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	RecordedAt       time.Time `json:"recorded_at"`
}

// TradingPositionsSnapshot is one sampled point in the
// equity / cash / unrealised-PL time series. Powers the
// project-page Sharpe + drawdown calc and the in-memory
// drawdown circuit breaker. Sampler runs at a fixed cadence
// (5 min default) per project that has a broker MCP wired,
// reading from the broker's GetAccountSummary.
//
// PositionsJSON is the raw positions array from the broker.
// JSON-as-blob so a future per-symbol P&L panel can reconstruct
// holdings without a separate table; the soak metrics
// themselves don't decode it.
// TradingFill is one fill event observed against a previously
// submitted order. Phase-3 ingestion path: the broker's poller
// detects submit → filled transitions on its placedOrders state
// map, posts one fill row per fill (a partial fill produces a
// row with qty < order.qty; subsequent fills append more rows
// referencing the same order_id). Filled order rows on
// trading_orders track terminal status; trading_fills tracks
// the granular volume + commission detail per execution.
//
// IDs are broker-generated and idempotent on the (order_id,
// filled_at) pair so the poller can re-post under transient
// daemon outages without double-counting.
type TradingFill struct {
	ID            string    `json:"id"`
	OrderID       string    `json:"order_id"`
	ProjectID     string    `json:"project_id"`
	Symbol        string    `json:"symbol"`
	Qty           float64   `json:"qty"`
	Price         float64   `json:"price"`
	CommissionUSD *float64  `json:"commission_usd,omitempty"`
	FilledAt      time.Time `json:"filled_at"`
	ExecID        *string   `json:"exec_id,omitempty"`
	AccountID     *string   `json:"account_id,omitempty"`
	Source        string    `json:"source,omitempty"`
	SourceDetail  *string   `json:"source_detail,omitempty"`
}

// TradingFillFilter scopes fill queries — soak panel summing,
// per-symbol P&L rollups, and the Phase-3 stopped-out detector
// when reconciling on a daemon restart.
type TradingFillFilter struct {
	ProjectID *string
	OrderID   *string
	Symbol    *string
	Since     *time.Time
	Until     *time.Time
	PageSize  int
	Offset    int
}

type TradingPositionsSnapshot struct {
	ID               string    `json:"id"`
	ProjectID        string    `json:"project_id"`
	RecordedAt       time.Time `json:"recorded_at"`
	CashUSD          float64   `json:"cash_usd"`
	EquityUSD        float64   `json:"equity_usd"`
	UnrealisedPLUSD  float64   `json:"unrealised_pl_usd"`
	RealisedPLDayUSD float64   `json:"realised_pl_day_usd"`
	PositionsJSON    []byte    `json:"positions_json,omitempty"`
}

// ExecutionStepOutcomeFilter scopes outcome queries for the dashboard
// and investigation tools.
type ExecutionStepOutcomeFilter struct {
	ProjectID   *string
	TaskID      *string
	ExecutionID *string
	StepID      *string
	Role        *string
	Model       *string
	Outcome     *string
	Since       *time.Time
	Until       *time.Time
	PageSize    int
	Offset      int
}

// TaskFilter defines filtering options for task queries.
type TaskFilter struct {
	ProjectID *string
	Status    *TaskStatus
	// UpdatedBefore is the upper bound on tasks.updated_at. Used by
	// the closure-grace scan (2026-05-29) to avoid fetching the full
	// COMPLETED slice every tick when only stale-enough tasks
	// qualify. Nil = no bound (legacy behaviour). The audit-agent
	// flagged the unbounded scan as a per-tick latency hazard for
	// the external_wait monitor's adjacent re-queue path.
	UpdatedBefore *time.Time
	PageSize      int
	Offset        int
}

// ExecutionFilter defines filtering options for execution queries.
type ExecutionFilter struct {
	ProjectID *string
	TaskID    *string
	Status    *ExecutionStatus
	PageSize  int
	Offset    int
}

// ArtifactFilter defines filtering options for artifact queries.
type ArtifactFilter struct {
	ProjectID   *string
	ExecutionID *string
	TaskID      *string
	PageSize    int
	Offset      int
}

// RoleQuality is a per-role output quality summary aggregated from
// execution_step_outcomes over a recent time window. Used by the UI to
// surface model performance in the swarm section of the project detail page.
type RoleQuality struct {
	// RoleName is the parsed role identifier (e.g. "coder", "analyst",
	// "scout").
	RoleName string

	// Executions is the number of finalized, non-cancelled step outcomes
	// this role produced within the query window. The field name is kept
	// for API compatibility with older UI code.
	Executions int64

	// Completed is the number of those outcomes marked ok.
	Completed int64

	// Failed is the number of non-ok, non-cancelled terminal outcomes.
	Failed int64

	// SuccessRatePct is Completed / Executions * 100, rounded to one
	// decimal.
	SuccessRatePct float64

	// AvgDurationSec is the mean execution duration (completed_at -
	// started_at) in seconds, across executions where both timestamps
	// are set. Zero if no completed executions are in the window.
	AvgDurationSec float64
}

// WorkflowProposalStatus is the typed enum for
// WorkflowProposal.Status. State machine lifecycle pinned by the
// repository methods (not a DB CHECK constraint — future status
// additions don't need a migration).
type WorkflowProposalStatus string

const (
	WorkflowProposalStatusPending    WorkflowProposalStatus = "pending"
	WorkflowProposalStatusApproved   WorkflowProposalStatus = "approved"
	WorkflowProposalStatusRejected   WorkflowProposalStatus = "rejected"
	WorkflowProposalStatusApplied    WorkflowProposalStatus = "applied"
	WorkflowProposalStatusRolledBack WorkflowProposalStatus = "rolled_back"
	WorkflowProposalStatusRegressed  WorkflowProposalStatus = "regressed"
)

// WorkflowProposalKind is the closed set of structural-edit classes
// the architect can propose. Stored on the proposal row (migration
// 83) for analytics + per-class filtering. The set mirrors
// https://docs.vornik.io § "What the
// architect can propose". WorkflowProposalKindUnspecified is the
// sentinel for rows predating the column AND for proposals the
// architect hasn't yet tagged (the LLM-output `kind` field is a
// tracked follow-on — see mitigation plan §8.5).
type WorkflowProposalKind string

const (
	WorkflowProposalKindUnspecified          WorkflowProposalKind = "unspecified"
	WorkflowProposalKindAddStep              WorkflowProposalKind = "add_step"
	WorkflowProposalKindRemoveStep           WorkflowProposalKind = "remove_step"
	WorkflowProposalKindChangeTransition     WorkflowProposalKind = "change_transition"
	WorkflowProposalKindChangeTimeout        WorkflowProposalKind = "change_timeout"
	WorkflowProposalKindChangeRetryPolicy    WorkflowProposalKind = "change_retry_policy"
	WorkflowProposalKindChangeRoleAssignment WorkflowProposalKind = "change_role_assignment"
	WorkflowProposalKindReorderSteps         WorkflowProposalKind = "reorder_steps"
)

// ValidWorkflowProposalKind reports whether k is in the closed set
// (the sentinel counts as valid). Used at the wire boundary so a
// malformed architect output / API filter is rejected rather than
// silently widening the enum.
func ValidWorkflowProposalKind(k WorkflowProposalKind) bool {
	switch k {
	case WorkflowProposalKindUnspecified,
		WorkflowProposalKindAddStep,
		WorkflowProposalKindRemoveStep,
		WorkflowProposalKindChangeTransition,
		WorkflowProposalKindChangeTimeout,
		WorkflowProposalKindChangeRetryPolicy,
		WorkflowProposalKindChangeRoleAssignment,
		WorkflowProposalKindReorderSteps:
		return true
	default:
		return false
	}
}

// WorkflowProposal is one architect-emitted proposal awaiting (or
// past) operator decision. The architect emits these as `pending`;
// the operator approval flow transitions to approved/rejected; the
// apply path (Slice 4) transitions approved → applied; the Slice 5
// safety net can later flip to rolled_back or regressed.
//
// See https://docs.vornik.io
type WorkflowProposal struct {
	ID         string                 `json:"id"`
	WorkflowID string                 `json:"workflow_id"`
	Status     WorkflowProposalStatus `json:"status"`
	// Kind is the structural-edit class (migration 83). Defaults to
	// WorkflowProposalKindUnspecified for untagged / pre-column rows.
	Kind           WorkflowProposalKind `json:"kind"`
	ProposalYAML   string               `json:"proposal_yaml"`
	Motivation     string               `json:"motivation"`
	EvidenceRunIDs []string             `json:"evidence_run_ids"`
	// InstinctIDs lists the instinct-layer priors that supported this
	// proposal (migration 92). Before it existed only the priors'
	// action TEXT survived (folded into Motivation) — the 2026-06-07
	// architecture review flagged that proposals couldn't be traced
	// back to the instincts that shaped them. Positive priors only;
	// negative (architect-reject) priors are never folded in.
	InstinctIDs    []string   `json:"instinct_ids,omitempty"`
	Confidence     float32    `json:"confidence"`
	ArchitectModel string     `json:"architect_model"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	AppliedAt      *time.Time `json:"applied_at,omitempty"`
	AppliedCommit  string     `json:"applied_commit,omitempty"`
	RollbackCommit string     `json:"rollback_commit,omitempty"`
	Notes          string     `json:"notes,omitempty"`
}

// ---------------------------------------------------------------------------
// Continuous-learning instinct layer (migrations 85/86).
//
// An Instinct is an atomic, confidence-scored learned pattern — "in
// situation T, action/observation A held" — mined by the leader-elected
// extraction worker (internal/instinct) from the audit spine. Instincts
// are ADVISORY: they are surfaced and used as evidence/priors but never
// silently mutate behaviour. See
// https://docs.vornik.io
// ---------------------------------------------------------------------------

// Instinct domain values. Stored as plain TEXT so ad-hoc SQL stays
// readable and new domains land with a code change, not a migration.
const (
	InstinctDomainRecovery  = "recovery"
	InstinctDomainCost      = "cost"
	InstinctDomainQuality   = "quality"
	InstinctDomainRetrieval = "retrieval"
	InstinctDomainWorkflow  = "workflow"
	// InstinctDomainBudget is the tool-budget provisioning domain mined by
	// ExtractBudgetProvisioning. It records over/under-provisioning signals
	// per (project, role) from the stamped (tier, budget, used) triple on
	// execution_step_outcomes. See https://docs.vornik.io
	InstinctDomainBudget = "budget"
)

// Instinct scope values.
const (
	InstinctScopeProject = "project"
	InstinctScopeGlobal  = "global"
)

// Instinct source values — how the instinct came to exist.
const (
	InstinctSourceObserver        = "observer"         // mined by the extraction worker
	InstinctSourceOperator        = "operator"         // hand-authored by an operator
	InstinctSourceArchitectReject = "architect-reject" // a rejected architect proposal (negative example)
)

// Instinct status values. Transitions are driven by the confidence
// model (candidate→active→promoted) and the retire floor (→retired).
const (
	InstinctStatusCandidate = "candidate"
	InstinctStatusActive    = "active"
	InstinctStatusPromoted  = "promoted"
	InstinctStatusRetired   = "retired"
)

// Instinct evidence polarity values.
const (
	InstinctPolaritySupport    = "support"
	InstinctPolarityContradict = "contradict"
)

// Instinct application surfaces — where an instinct was shown/used.
const (
	InstinctSurfaceFailedTaskUI      = "failed_task_ui"
	InstinctSurfaceLeadRecovery      = "lead_recovery"
	InstinctSurfaceArchitectEvidence = "architect_evidence"
	// InstinctSurfaceToolBudget is the active budget-consumer surface
	// (Slice 4, LLD §7): a learned tier was supplied to toolbudget.Resolve
	// on the absent-verdict path. Result is always "ignored" at apply time;
	// the feedback loop grades it later when enabled.
	InstinctSurfaceToolBudget = "tool_budget"
)

// Instinct application results — what happened after surfacing.
const (
	InstinctResultAccepted  = "accepted"
	InstinctResultRejected  = "rejected"
	InstinctResultSucceeded = "succeeded"
	InstinctResultFailed    = "failed"
	InstinctResultIgnored   = "ignored"
	// InstinctResultAutoApplied (v2): the remediation was surfaced as a
	// prompt-level directive (auto-apply consumer), not merely shown. It is
	// a PENDING state like "ignored" — the RecoveryResolver later flips it to
	// succeeded/failed from the step's outcome — but distinguished so the
	// feedback loop can tell auto-applied from advisory surfacings.
	InstinctResultAutoApplied = "auto_applied"
)

// Instinct is one row of the instincts table. SupportCount /
// ContradictCount / Confidence are derived columns the extraction
// worker recomputes from InstinctEvidence on every upsert; callers
// should treat them as read-only snapshots.
//
// Trigger carries the structured trigger as raw JSON bytes (the
// trigger_json JSONB / BLOB column). It is stored opaquely so the
// persistence layer needs no knowledge of the trigger shape; the
// extraction worker marshals/unmarshals at its boundary. TriggerKey
// is the canonical hash of the trigger and the dedup key.
type Instinct struct {
	ID              string          `json:"id"`
	Scope           string          `json:"scope"`
	ProjectID       string          `json:"project_id"`
	Domain          string          `json:"domain"`
	TriggerKey      string          `json:"trigger_key"`
	Trigger         json.RawMessage `json:"trigger,omitempty"`
	Action          string          `json:"action"`
	Confidence      float64         `json:"confidence"`
	SupportCount    int             `json:"support_count"`
	ContradictCount int             `json:"contradict_count"`
	Source          string          `json:"source"`
	Status          string          `json:"status"`
	DistillModel    string          `json:"distill_model,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	LastSeenAt      time.Time       `json:"last_seen_at"`
}

// InstinctEvidence links an instinct to one corroborating or
// contradicting execution_step_outcomes row. The (InstinctID,
// OutcomeID) pair is unique, which is what makes the extraction
// worker idempotent across overlapping windows.
type InstinctEvidence struct {
	InstinctID string `json:"instinct_id"`
	OutcomeID  string `json:"outcome_id"`
	Polarity   string `json:"polarity"`
	// Action is the instinct action this outcome corroborated/contradicted
	// AT RECORD TIME (W6 per-action evidence partitioning, migration 101).
	// RecomputeConfidence counts only evidence whose action matches the
	// instinct's CURRENT action, so when a cross-project conflict replaces
	// the global action the new action no longer inherits the displaced
	// action's evidence — without deleting it (audit preserved). Empty on
	// rows that pre-date the column; AddEvidence resolves it from the
	// instinct's current action when the caller leaves it unset.
	Action    string    `json:"action,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// InstinctActionVersion is one row of an instinct's action-transition
// history (W6 versioning, migration 101). A row is appended whenever an
// instinct's action changes — the displaced action's final confidence /
// support / contradict snapshot is captured before the new action takes
// over, giving an audit + rollback substrate for cross-project
// (W6 "replaced") and project-scoped action churn.
type InstinctActionVersion struct {
	ID              string  `json:"id"`
	InstinctID      string  `json:"instinct_id"`
	Action          string  `json:"action"`
	Confidence      float64 `json:"confidence"`
	SupportCount    int     `json:"support_count"`
	ContradictCount int     `json:"contradict_count"`
	// Reason tags why the action changed: "action_change" (a project
	// instinct's trigger now maps to a different action) or "w6_replace"
	// (a higher-confidence cross-project challenger took the global slot).
	Reason     string    `json:"reason"`
	RecordedAt time.Time `json:"recorded_at"`
}

// InstinctApplication records when an instinct was surfaced/used and
// what happened next — the feedback loop the consumers close. No
// consumer writes it in slice 1; the schema lands now.
type InstinctApplication struct {
	ID         string    `json:"id"`
	InstinctID string    `json:"instinct_id"`
	TaskID     string    `json:"task_id,omitempty"`
	Surface    string    `json:"surface"`
	Result     string    `json:"result"`
	AppliedAt  time.Time `json:"applied_at"`
	// ExecutionID / StepID link a surfaced application back to the
	// execution + step it was attached to (slice 7). They are set at the
	// lead_recovery surface site so the RecoveryResolver can later match
	// a pending (surfaced-but-unresolved) row against the step's outcome
	// and flip it to succeeded/failed in place. Empty for surfaces that
	// have no execution/step context.
	ExecutionID string `json:"execution_id,omitempty"`
	StepID      string `json:"step_id,omitempty"`
}

// InstinctDomainStatusCount is one (domain, status) bucket of the live
// instinct population. Returned by CountByDomainStatus to back the
// vornik_instinct_total{domain,status} gauge.
type InstinctDomainStatusCount struct {
	Domain string
	Status string
	Count  int
}

// InstinctApplicationCounts is the per-instinct application-feedback
// tally used by the UI lift column (slice 7). It buckets
// instinct_applications rows by result: Succeeded counts 'succeeded'
// rows; Failed collapses 'failed' + 'rejected'; Ignored counts
// surfaced-but-unresolved 'ignored' rows. The 'accepted' result is
// excluded (it is a surfacing event with no efficacy signal yet).
// Returned by ListApplicationCounts as a map keyed by InstinctID.
type InstinctApplicationCounts struct {
	InstinctID string
	Succeeded  int
	Failed     int
	Ignored    int
}

// InstinctFilter defines filtering options for instinct queries.
// All pointer fields are optional; nil means "no constraint".
type InstinctFilter struct {
	ProjectID     *string
	Scope         *string
	Domain        *string
	Status        *string
	TriggerKey    *string
	MinConfidence *float64
	PageSize      int
	Offset        int
}

// User is a human principal in the identity core
// (oidc-identity-permissions-design.md §3). Machine credentials
// stay on api_keys; users exist only for channel-bound humans.
type User struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	CreatedAt   time.Time  `json:"created_at"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
}

// Group carries a role ("admin" | "user") and, for user-role
// groups, a project scope held in group_projects ('*' = all).
// Admin-role groups are instance-wide and ignore group_projects.
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Role        string    `json:"role"`
	Description string    `json:"description,omitempty"` // optional (NULL in DB when empty)
	CreatedAt   time.Time `json:"created_at"`
}

// UserIdentity binds one channel identity (google email, telegram
// id, ...) to a user. UNIQUE(channel, external_id) is enforced in
// the DB and spans revoked rows — rebinding repoints the row.
type UserIdentity struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Channel    string     `json:"channel"`
	ExternalID string     `json:"external_id"`
	Display    string     `json:"display,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// UISession is one browser login session (design §4.3). TokenHash
// is the sha256 of the cookie value; the raw token is never stored.
type UISession struct {
	ID         string     `json:"id"`
	TokenHash  string     `json:"-"`
	UserID     string     `json:"user_id"`
	Provider   string     `json:"provider"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	IP         string     `json:"ip,omitempty"`
	UserAgent  string     `json:"user_agent,omitempty"`
}
