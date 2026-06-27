-- vornik Initial PostgreSQL Schema
-- Version: 001
-- Date: 2026-04-11
-- 
-- This schema defines the core transactional tables for vornik:
-- - tasks: The queue of work items
-- - executions: Workflow execution instances
-- - artifacts: Durable outputs and intermediate products
--
-- Design Principles:
-- - State must survive process restarts
-- - Atomic leasing for safe task claiming
-- - Resumable executions via state snapshots
-- - PostgreSQL-specific for queue manager needs

-- =============================================================================
-- ENUMERATED TYPES
-- =============================================================================

-- Task status lifecycle:
-- PENDING -> QUEUED -> LEASED -> RUNNING -> (WAITING_FOR_CHILDREN | COMPLETED | FAILED | CANCELLED)
CREATE TYPE task_status AS ENUM (
    'PENDING',              -- Awaiting approval before becoming schedulable
    'QUEUED',               -- Ready to be picked up by the scheduler
    'LEASED',               -- Claimed by scheduler, reserved for execution
    'RUNNING',              -- Currently executing
    'WAITING_FOR_CHILDREN', -- Blocked on delegated child tasks
    'COMPLETED',            -- Successfully finished
    'FAILED',               -- Failed after retry exhaustion
    'CANCELLED'             -- Cancelled by operator
);

-- Execution status lifecycle:
-- PENDING -> RUNNING -> (PAUSED | COMPLETED | FAILED | CANCELLED)
CREATE TYPE execution_status AS ENUM (
    'PENDING',   -- Execution created but not yet started
    'RUNNING',   -- Execution in progress
    'PAUSED',    -- Paused awaiting operator approval
    'COMPLETED', -- Successfully finished
    'FAILED',    -- Failed with error
    'CANCELLED'  -- Cancelled by operator
);

-- Source of task creation
CREATE TYPE task_creation_source AS ENUM (
    'USER',       -- Direct user submission (API, CLI, chat)
    'DELEGATION', -- Created by another task via delegation
    'AUTONOMOUS'  -- Created by swarm lead autonomous task creation
);

-- Delegation behavior for child tasks
CREATE TYPE delegation_mode AS ENUM (
    'SEQUENTIAL', -- Parent waits for child completion before continuing
    'PARALLEL',   -- Child runs independently, parent continues
    'FAN_OUT'     -- Multiple children created, parent waits for all
);

-- Artifact classification
CREATE TYPE artifact_class AS ENUM (
    'OUTPUT',       -- Final deliverable
    'INTERMEDIATE', -- Intermediate work product
    'SNAPSHOT',     -- Checkpoint for retry/recovery
    'LOG',          -- Execution log or trace
    'METADATA',     -- Supplementary metadata file
    'INPUT'         -- User-supplied input (Telegram upload, API attachment)
);

-- =============================================================================
-- TASKS TABLE
-- =============================================================================

-- Migration tracking table.
-- The application migration runner also creates this table defensively,
-- but the bootstrap SQL used by the E2E guide needs it present up front so a
-- freshly initialized database matches the documented expected table list.
CREATE TABLE IF NOT EXISTS migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE migrations IS 'Tracks applied database schema migrations.';
COMMENT ON COLUMN migrations.version IS 'Migration version number.';
COMMENT ON COLUMN migrations.name IS 'Human-readable migration name.';
COMMENT ON COLUMN migrations.applied_at IS 'Timestamp when the migration was applied.';

-- The transactional heart of the queue system.
-- This is the single most important table for scheduling and execution.
-- Each row represents a unit of work that progresses through a defined lifecycle.

CREATE TABLE tasks (
    -- Unique identifier (UUID v7 recommended for time-ordering)
    id TEXT PRIMARY KEY,
    
    -- Project association: the main tenancy and execution boundary
    project_id TEXT NOT NULL,
    
    -- Workflow reference: which workflow definition governs this task
    -- NULL means use the project's default workflow
    workflow_id TEXT,

    -- Optional deduplication key supplied by clients for retry-safe submission
    idempotency_key TEXT,
    
    -- Delegation lineage: parent task if this was created via delegation
    -- Enables traversal of the delegation tree for result propagation
    parent_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    
    -- Creation metadata
    creation_source task_creation_source NOT NULL DEFAULT 'USER',
    delegation_mode delegation_mode,
    
    -- Task state
    status task_status NOT NULL DEFAULT 'QUEUED',
    priority INTEGER NOT NULL DEFAULT 50,
    
    -- Task payload: JSON blob containing the initial prompt and input artifact references
    -- Structure defined by workflow, but typically includes:
    -- { "prompt": "...", "inputs": [...], "context": {...} }
    payload JSONB,
    
    -- Dependency tracking: array of task IDs that must complete before this task can run
    -- Used by scheduler for dependency-aware scheduling
    dependencies TEXT[] DEFAULT '{}',
    
    -- Lease fields for queue manager:
    -- The lease mechanism provides safe task claiming with automatic recovery
    lease_id TEXT,              -- Unique lease identifier (UUID)
    leased_at TIMESTAMP WITH TIME ZONE,  -- When the lease was acquired
    leased_by TEXT,             -- Identifier of the lease holder (scheduler/worker ID)
    lease_expires_at TIMESTAMP WITH TIME ZONE,  -- Lease deadline for recovery
    
    -- Retry tracking
    attempt INTEGER NOT NULL DEFAULT 1,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    last_error TEXT,            -- Error message from most recent failure
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    
    -- Constraints
    CONSTRAINT valid_priority CHECK (priority >= 0 AND priority <= 100),
    CONSTRAINT valid_attempt CHECK (attempt >= 1 AND attempt <= max_attempts)
);

-- Comment on table
COMMENT ON TABLE tasks IS 'Queue of work items. Each task progresses through a defined lifecycle from submission to completion.';

-- Column comments
COMMENT ON COLUMN tasks.id IS 'Unique identifier (UUID v7 recommended for time-ordering)';
COMMENT ON COLUMN tasks.project_id IS 'Project association: main tenancy and execution boundary';
COMMENT ON COLUMN tasks.workflow_id IS 'Workflow definition governing this task (NULL = project default)';
COMMENT ON COLUMN tasks.idempotency_key IS 'Optional project-scoped key used to deduplicate retried task submissions';
COMMENT ON COLUMN tasks.parent_task_id IS 'Parent task if created via delegation; enables lineage traversal';
COMMENT ON COLUMN tasks.creation_source IS 'Origin of the task: USER, DELEGATION, or AUTONOMOUS';
COMMENT ON COLUMN tasks.delegation_mode IS 'How parent should wait for this task: SEQUENTIAL, PARALLEL, FAN_OUT';
COMMENT ON COLUMN tasks.status IS 'Current lifecycle state of the task';
COMMENT ON COLUMN tasks.priority IS 'Scheduling priority (0-100, lower = more urgent)';
COMMENT ON COLUMN tasks.payload IS 'JSON blob with prompt, inputs, and context';
COMMENT ON COLUMN tasks.dependencies IS 'Array of task IDs that must complete before execution';
COMMENT ON COLUMN tasks.lease_id IS 'Unique lease identifier when task is claimed';
COMMENT ON COLUMN tasks.leased_at IS 'Timestamp when lease was acquired';
COMMENT ON COLUMN tasks.leased_by IS 'Identifier of the lease holder';
COMMENT ON COLUMN tasks.lease_expires_at IS 'Deadline for lease; task re-queued if expired';
COMMENT ON COLUMN tasks.attempt IS 'Current execution attempt number';
COMMENT ON COLUMN tasks.max_attempts IS 'Maximum retry attempts allowed';
COMMENT ON COLUMN tasks.last_error IS 'Error message from most recent failure';

-- =============================================================================
-- EXECUTIONS TABLE
-- =============================================================================

-- Tracks runtime state of a workflow execution.
-- The state_snapshot is the key to durability - enabling resume from checkpoint.

CREATE TABLE executions (
    -- Unique identifier
    id TEXT PRIMARY KEY,
    
    -- Task and project association
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    
    -- Workflow reference with version pinning
    -- Executions are tied to a specific workflow version for reproducibility
    workflow_id TEXT NOT NULL,
    workflow_revision TEXT NOT NULL,
    
    -- Execution state
    status execution_status NOT NULL DEFAULT 'PENDING',
    
    -- Current position in the workflow
    -- References a step/node ID in the workflow definition
    current_step_id TEXT,
    
    -- Completed steps for progress tracking and resumption
    -- Array of step IDs that have been completed
    completed_steps TEXT[] DEFAULT '{}',
    
    -- State snapshot: JSON blob containing full, resumable state
    -- Includes current step, step outputs, artifact references, and workflow variables
    -- Structure: { "currentStep": "...", "stepOutputs": {...}, "artifacts": [...], "variables": {...} }
    state_snapshot JSONB,
    
    -- Result and error tracking
    result JSONB,               -- Final result on completion
    error_message TEXT,         -- Error details on failure
    error_code TEXT,            -- Categorized error code for programmatic handling
    
    -- Timing
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Comments
COMMENT ON TABLE executions IS 'Runtime state of workflow executions. State snapshots enable durable checkpointing and resumption.';
COMMENT ON COLUMN executions.id IS 'Unique identifier for the execution';
COMMENT ON COLUMN executions.task_id IS 'Associated task that triggered this execution';
COMMENT ON COLUMN executions.project_id IS 'Project association for scoping';
COMMENT ON COLUMN executions.workflow_id IS 'Workflow definition being executed';
COMMENT ON COLUMN executions.workflow_revision IS 'Version/revisions of workflow for reproducibility';
COMMENT ON COLUMN executions.status IS 'Current execution state';
COMMENT ON COLUMN executions.current_step_id IS 'Current workflow step/node being executed';
COMMENT ON COLUMN executions.completed_steps IS 'Array of completed step IDs for progress tracking';
COMMENT ON COLUMN executions.state_snapshot IS 'Full resumable state including step outputs and artifact refs';
COMMENT ON COLUMN executions.result IS 'Final execution result on completion';
COMMENT ON COLUMN executions.error_message IS 'Detailed error message on failure';
COMMENT ON COLUMN executions.error_code IS 'Categorized error code for programmatic handling';

-- =============================================================================
-- ARTIFACTS TABLE
-- =============================================================================

-- Index of all durable outputs and intermediate products.
-- Content is stored on the filesystem; this table tracks metadata.

CREATE TABLE artifacts (
    -- Unique identifier
    id TEXT PRIMARY KEY,
    
    -- Project and execution association
    project_id TEXT NOT NULL,
    execution_id TEXT REFERENCES executions(id) ON DELETE CASCADE,
    task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    
    -- Artifact identity
    name TEXT NOT NULL,         -- User-provided filename
    artifact_class artifact_class NOT NULL,
    
    -- Storage location
    -- Relative path within the project's artifact directory
    storage_path TEXT NOT NULL,
    
    -- Content metadata
    size_bytes BIGINT,
    content_hash_sha256 TEXT,
    mime_type TEXT,
    
    -- Timestamps
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    
    -- Constraints
    CONSTRAINT valid_size CHECK (size_bytes IS NULL OR size_bytes >= 0)
);

-- Comments
COMMENT ON TABLE artifacts IS 'Index of durable outputs and intermediate products. Content stored on filesystem.';
COMMENT ON COLUMN artifacts.id IS 'Unique identifier for the artifact';
COMMENT ON COLUMN artifacts.project_id IS 'Project association for scoping';
COMMENT ON COLUMN artifacts.execution_id IS 'Execution that produced this artifact';
COMMENT ON COLUMN artifacts.task_id IS 'Task that produced this artifact (may differ from execution task)';
COMMENT ON COLUMN artifacts.name IS 'User-provided filename';
COMMENT ON COLUMN artifacts.artifact_class IS 'Classification: OUTPUT, INTERMEDIATE, SNAPSHOT, LOG, METADATA, INPUT';
COMMENT ON COLUMN artifacts.storage_path IS 'Relative path within project artifact directory';
COMMENT ON COLUMN artifacts.size_bytes IS 'File size in bytes';
COMMENT ON COLUMN artifacts.content_hash_sha256 IS 'SHA-256 hash for integrity verification';
COMMENT ON COLUMN artifacts.mime_type IS 'MIME type for content identification';

-- =============================================================================
-- INDEXES
-- =============================================================================

-- Tasks indexes for common query patterns

-- Queue lookup: find available tasks ordered by priority
-- Critical for scheduler performance
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority ASC, created_at ASC)
    WHERE status = 'QUEUED';

-- Project-scoped task queries
-- Used for project dashboards and project-specific operations
CREATE INDEX idx_tasks_project ON tasks (project_id, created_at DESC);

-- Idempotent submission lookup for retry-safe API clients
CREATE UNIQUE INDEX idx_tasks_project_idempotency_key ON tasks (project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Status filtering for queue depth metrics and filtering
CREATE INDEX idx_tasks_status ON tasks (status);

-- Lease expiration recovery: find tasks with expired leases
-- Used by background recovery process
CREATE INDEX idx_tasks_lease_expired ON tasks (lease_expires_at) 
    WHERE status IN ('LEASED', 'RUNNING') AND lease_expires_at IS NOT NULL;

-- Parent task lookup for delegation tree traversal
-- Used to find children of a task and propagate results
CREATE INDEX idx_tasks_parent ON tasks (parent_task_id) 
    WHERE parent_task_id IS NOT NULL;

-- Workflow association for workflow-specific operations
CREATE INDEX idx_tasks_workflow ON tasks (workflow_id) 
    WHERE workflow_id IS NOT NULL;

-- Dependency check: find tasks waiting on a specific task
-- Used to unblock dependent tasks when a task completes
CREATE INDEX idx_tasks_dependencies ON tasks USING GIN (dependencies) 
    WHERE array_length(dependencies, 1) > 0;

-- Executions indexes

-- Task to execution lookup
CREATE INDEX idx_executions_task ON executions (task_id);

-- Project-scoped execution queries
CREATE INDEX idx_executions_project ON executions (project_id, created_at DESC);

-- Status filtering
CREATE INDEX idx_executions_status ON executions (status);

-- Workflow association with version
CREATE INDEX idx_executions_workflow ON executions (workflow_id, workflow_revision);

-- Running executions: for scheduler capacity tracking
CREATE INDEX idx_executions_running ON executions (project_id) 
    WHERE status = 'RUNNING';

-- Artifacts indexes

-- Execution to artifacts lookup
CREATE INDEX idx_artifacts_execution ON artifacts (execution_id);

-- Project-scoped artifact queries
CREATE INDEX idx_artifacts_project ON artifacts (project_id, created_at DESC);

-- Task to artifacts lookup
CREATE INDEX idx_artifacts_task ON artifacts (task_id) 
    WHERE task_id IS NOT NULL;

-- Content hash lookup for deduplication
CREATE INDEX idx_artifacts_hash ON artifacts (content_hash_sha256) 
    WHERE content_hash_sha256 IS NOT NULL;

-- =============================================================================
-- TRIGGERS
-- =============================================================================

-- Automatic updated_at timestamp

CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER update_tasks_updated_at
    BEFORE UPDATE ON tasks
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();

CREATE TRIGGER update_executions_updated_at
    BEFORE UPDATE ON executions
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();

-- =============================================================================
-- VIEWS
-- =============================================================================

-- Active queue view: all tasks currently being processed or queued
CREATE VIEW active_tasks AS
SELECT 
    id, project_id, workflow_id, status, priority,
    created_at, updated_at, attempt, max_attempts
FROM tasks
WHERE status IN ('PENDING', 'QUEUED', 'LEASED', 'RUNNING', 'WAITING_FOR_CHILDREN');

COMMENT ON VIEW active_tasks IS 'Currently active tasks (not terminal state)';

-- Failed tasks view: dead-letter queue
CREATE VIEW failed_tasks AS
SELECT 
    t.id, t.project_id, t.status, t.attempt, t.max_attempts,
    t.last_error, t.created_at, t.updated_at,
    e.id AS execution_id, e.error_message, e.error_code
FROM tasks t
LEFT JOIN executions e ON e.task_id = t.id
WHERE t.status = 'FAILED';

COMMENT ON VIEW failed_tasks IS 'Failed tasks requiring operator attention (dead-letter queue)';

-- =============================================================================
-- TOOL AUDIT LOG
-- =============================================================================

-- Records every tool invocation for audit and debugging.
-- Populated by the executor from agent container result.json toolAudit entries,
-- and by the dispatcher for Telegram/chat tool calls.

CREATE TABLE IF NOT EXISTS tool_audit_log (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    execution_id TEXT NOT NULL,
    step_id TEXT DEFAULT '',
    tool_name TEXT NOT NULL,
    tool_input TEXT DEFAULT '',
    tool_output TEXT DEFAULT '',
    duration_ms INTEGER DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE tool_audit_log IS 'Audit trail of all tool invocations across agents and dispatchers.';
COMMENT ON COLUMN tool_audit_log.tool_name IS 'Tool name: file_read, file_write, run_shell, create_task, etc.';
COMMENT ON COLUMN tool_audit_log.tool_input IS 'Serialized tool input arguments (truncated to 4KB).';
COMMENT ON COLUMN tool_audit_log.tool_output IS 'Serialized tool output/result (truncated to 4KB).';
COMMENT ON COLUMN tool_audit_log.duration_ms IS 'Tool execution time in milliseconds.';

CREATE INDEX IF NOT EXISTS idx_tool_audit_log_project ON tool_audit_log(project_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_task ON tool_audit_log(task_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_execution ON tool_audit_log(execution_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_tool_name ON tool_audit_log(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_created_at ON tool_audit_log(created_at DESC);

-- =============================================================================
-- TASK WATCHERS (completion notifications)
-- =============================================================================

CREATE TABLE IF NOT EXISTS task_watchers (
    task_id TEXT NOT NULL,
    chat_id BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, chat_id)
);

COMMENT ON TABLE task_watchers IS 'Tracks Telegram chats that want notification when a task completes.';

CREATE INDEX IF NOT EXISTS idx_task_watchers_task ON task_watchers(task_id);

-- =============================================================================
-- PROJECT MEMORY (RAG)
-- =============================================================================

-- Enable pgvector for semantic search. Best-effort: no error if not installed.
-- When unavailable the system falls back to full-text search only.
DO $$ BEGIN
    CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION WHEN OTHERS THEN NULL; END $$;

-- Chunked text from task OUTPUT artifacts, indexed for hybrid retrieval.
-- Populated asynchronously after each successful task completion.
CREATE TABLE IF NOT EXISTS project_memory_chunks (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    task_id      TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    artifact_id  TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    source_name  TEXT NOT NULL,
    chunk_index  INT  NOT NULL DEFAULT 0,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    tsv          TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE project_memory_chunks IS 'Chunked text from task outputs, indexed for hybrid semantic+keyword retrieval.';
COMMENT ON COLUMN project_memory_chunks.content_hash IS 'SHA-256 of content; used for deduplication within a project.';
COMMENT ON COLUMN project_memory_chunks.tsv IS 'Full-text search vector, auto-generated from content.';

CREATE INDEX IF NOT EXISTS idx_memory_chunks_project ON project_memory_chunks (project_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_chunks_hash ON project_memory_chunks (project_id, content_hash);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_tsv ON project_memory_chunks USING GIN (tsv);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_task ON project_memory_chunks (task_id) WHERE task_id IS NOT NULL;

-- URL liveness flags (2026-05-17, migration 37).
-- NULL on both means "URL never checked" — search treats those as
-- unknown and surfaces them anyway. Populated by
-- internal/memory.URLLivenessChecker (CLI: `vornikctl memory
-- recheck-urls --project <id>`); dead URLs stay indexed but flagged
-- so downstream agents can prefer alive ones.
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'project_memory_chunks' AND column_name = 'last_checked_at'
    ) THEN
        ALTER TABLE project_memory_chunks
            ADD COLUMN last_checked_at TIMESTAMPTZ,
            ADD COLUMN is_alive        BOOLEAN;
        CREATE INDEX IF NOT EXISTS idx_project_memory_chunks_url_recheck
            ON project_memory_chunks (project_id, last_checked_at NULLS FIRST);
    END IF;
END $$;

-- Add vector embedding column + HNSW index when pgvector is available.
--
-- Dimension is 1024 to match the default local embedder (bge-m3 via
-- Ollama). If you use a different embedding model whose native dim
-- doesn't match, you MUST drop + re-add this column at the correct
-- dim BEFORE any vectors are inserted — pgvector is strict about
-- dimension on INSERT. See https://docs.vornik.io
-- and https://docs.vornik.io for the supported embedders + their dims.
--
-- Common alternatives:
--   bge-m3                    -> vector(1024)  (default; 8K context, multilingual)
--   snowflake-arctic-embed2   -> vector(1024)
--   nomic-embed-text:v1.5     -> vector(768)
--   OpenAI text-embedding-3-* -> vector(1536)  (cloud only)
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_name = 'project_memory_chunks' AND column_name = 'embedding'
        ) THEN
            ALTER TABLE project_memory_chunks ADD COLUMN embedding vector(1024);
            CREATE INDEX IF NOT EXISTS idx_memory_chunks_embedding ON project_memory_chunks
                USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
        END IF;
    END IF;
END $$;

-- Background queue for async embedding of newly-inserted chunks.
-- Worker processes this table and writes vectors back to project_memory_chunks.
CREATE TABLE IF NOT EXISTS memory_embed_queue (
    chunk_id    TEXT PRIMARY KEY REFERENCES project_memory_chunks(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL,
    enqueued_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE memory_embed_queue IS 'Pending embedding jobs; drained by the background embed worker.';

CREATE INDEX IF NOT EXISTS idx_embed_queue_project ON memory_embed_queue (project_id, enqueued_at);

-- =============================================================================
-- WORKFLOW SNAPSHOT (2026.4.30) — idempotent column add
-- =============================================================================
-- Stores the resolved workflow YAML body at execution start so the
-- replay path uses the snapshot rather than the live (possibly edited)
-- file. Eliminates the WORKFLOW_DRIFT failure class entirely.
-- workflow_snapshot is JSON-serialized rather than YAML so the loader
-- doesn't need to re-run the YAML parser; for an in-progress execution
-- the snapshot bytes are the authoritative source of truth for the
-- step graph.
--
-- NULL / empty means the execution started before this column existed
-- or the snapshot wasn't captured for any reason. The replay path
-- falls back to the live workflow + the existing hash-based drift
-- guard in those cases.
DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'executions' AND column_name = 'workflow_snapshot'
    ) THEN
        ALTER TABLE executions ADD COLUMN workflow_snapshot BYTEA;
        COMMENT ON COLUMN executions.workflow_snapshot IS
            'JSON-serialized snapshot of the workflow at execution start. NULL = legacy execution without a snapshot; replay falls back to the live workflow.';
    END IF;
END $$;

-- =============================================================================
-- MEMORY RETRIEVAL AUDIT (2026-04-30)
-- =============================================================================
-- One row per Searcher.Search call. Records which chunks were
-- returned and (best-effort) the (task, execution, step) the call
-- was made from. Powers two analytics:
--   - Chunks never retrieved: candidates for auto-prune. Chunks
--     in project_memory_chunks whose ID isn't in any audit row's
--     chunk_ids over the last N days.
--   - Retrieval-success correlation: join chunk_ids with
--     execution_step_outcomes(outcome='ok') to find chunks that
--     pulled their weight. Future ranking heuristic.
--
-- chunk_ids stored as TEXT[] so the unnest-and-aggregate queries
-- the analytics rely on stay simple.
CREATE TABLE IF NOT EXISTS memory_retrieval_audit (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    task_id       TEXT,
    execution_id  TEXT,
    step_id       TEXT,
    role          TEXT,
    query         TEXT NOT NULL,
    chunk_ids     TEXT[] NOT NULL DEFAULT '{}',
    retrieved_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE memory_retrieval_audit IS 'Per-search audit of which memory chunks were returned. Powers auto-prune (chunks never retrieved) and retrieval-success correlation (chunks fed into successful steps).';

CREATE INDEX IF NOT EXISTS idx_memory_retrieval_project_time
    ON memory_retrieval_audit (project_id, retrieved_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_chunks
    ON memory_retrieval_audit USING gin (chunk_ids);
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_execution
    ON memory_retrieval_audit (execution_id) WHERE execution_id IS NOT NULL;

-- =============================================================================
-- LLM USAGE (2026.4.11)
-- =============================================================================

-- Per-step token and cost rollup for the UI spend panel and budget enforcement.
-- Two kinds of rows live here, distinguished by `source`:
--   - 'workflow_step': one row per agent step (the agent container accumulates
--     tokens across its tool-calling loop before returning). task_id and
--     execution_id are set.
--   - 'dispatcher': one row per dispatcher LLM call (tool-calling loop runs
--     per-turn, not per-task). task_id and execution_id are NULL; session_id
--     carries the chat/session identifier for rollup.
CREATE TABLE IF NOT EXISTS task_llm_usage (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    task_id           TEXT,
    execution_id      TEXT,
    step_id           TEXT NOT NULL DEFAULT '',
    role              TEXT NOT NULL,
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    iterations        INT    NOT NULL DEFAULT 0,
    cost_usd          DOUBLE PRECISION NOT NULL DEFAULT 0,
    source            TEXT NOT NULL DEFAULT 'workflow_step',
    session_id        TEXT,
    recorded_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE task_llm_usage IS 'Per-call LLM token usage and derived cost. source=workflow_step rows are per agent step (aggregated by container); source=dispatcher rows are per LLM call from the chat dispatcher.';
COMMENT ON COLUMN task_llm_usage.source IS 'Origin of the row: workflow_step | dispatcher. Determines whether task_id/execution_id or session_id is populated.';
COMMENT ON COLUMN task_llm_usage.session_id IS 'Dispatcher session identifier (chat/CLI session). NULL for workflow_step rows.';

CREATE INDEX IF NOT EXISTS idx_task_llm_usage_project_time
    ON task_llm_usage (project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_task
    ON task_llm_usage (task_id);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_execution
    ON task_llm_usage (execution_id);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_role_model
    ON task_llm_usage (role, model);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_source_time
    ON task_llm_usage (source, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_session
    ON task_llm_usage (session_id) WHERE session_id IS NOT NULL;

-- =============================================================================
-- STEP OUTCOMES (2026.4.20)
-- =============================================================================

-- Per-step outcome classification. Distinct from task_llm_usage (which is
-- about dollars and tokens): this table is about whether a step's output
-- was *usable* by the next step. A step writes 'pending_validation' on
-- completion; the consumer finalizes it to 'ok' if it parsed/consumed
-- cleanly, or to 'parse_error' / 'schema_violation' / 'refused' /
-- 'downstream_rejected' when the output wasn't usable. Attribution via
-- attributed_to_step_id lets model-effectiveness metrics reflect real
-- output quality per (role, model), not just LLM round-trip success.
CREATE TABLE IF NOT EXISTS execution_step_outcomes (
    id                     TEXT PRIMARY KEY,
    project_id             TEXT NOT NULL,
    task_id                TEXT NOT NULL,
    execution_id           TEXT NOT NULL,
    step_id                TEXT NOT NULL,
    role                   TEXT NOT NULL,
    model                  TEXT NOT NULL DEFAULT '',
    outcome                TEXT NOT NULL,
    attributed_to_step_id  TEXT,
    error_class            TEXT NOT NULL DEFAULT '',
    error_detail           TEXT NOT NULL DEFAULT '',
    duration_ms            BIGINT,
    finalized_at           TIMESTAMP WITH TIME ZONE,
    recorded_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    hallucination_signals  JSONB
);

CREATE TABLE IF NOT EXISTS task_judge_verdicts (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'judge',
    model        TEXT NOT NULL,
    verdict      TEXT NOT NULL,
    confidence   DOUBLE PRECISION NOT NULL DEFAULT 0,
    signals      JSONB,
    summary      TEXT NOT NULL DEFAULT '',
    cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_judge_verdicts_task ON task_judge_verdicts(task_id);
CREATE INDEX IF NOT EXISTS idx_task_judge_verdicts_project_time ON task_judge_verdicts(project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_judge_verdicts_role_model ON task_judge_verdicts(role, model);

COMMENT ON TABLE execution_step_outcomes IS 'Per-step outcome taxonomy: ok / pending_validation / parse_error / schema_violation / refused / iteration_exhausted / degenerate_loop / downstream_rejected / gate_failed / timeout / cancelled / failed / budget_tripwire.';
COMMENT ON COLUMN execution_step_outcomes.attributed_to_step_id IS 'For downstream_rejected etc., the step whose output caused this failure — NOT this step. Lets the UI point the blame at the producer.';
COMMENT ON COLUMN execution_step_outcomes.finalized_at IS 'NULL while outcome = pending_validation; set when the consumer has decided. A sweep at execution terminate finalizes anything still pending.';

CREATE INDEX IF NOT EXISTS idx_step_outcomes_execution
    ON execution_step_outcomes (execution_id, recorded_at);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_project_time
    ON execution_step_outcomes (project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_role_model
    ON execution_step_outcomes (role, model);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_pending
    ON execution_step_outcomes (execution_id, step_id) WHERE outcome = 'pending_validation';

-- =============================================================================
-- WEBHOOK EVENTS (2026.4.15)
-- =============================================================================

-- Durable audit trail for signed webhook ingress attempts. Raw payloads are not
-- stored; payload_hash is enough to correlate retries without retaining secrets
-- or large third-party event bodies.
CREATE TABLE IF NOT EXISTS webhook_events (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    source        TEXT NOT NULL,
    event_id      TEXT NOT NULL DEFAULT '',
    payload_hash  TEXT NOT NULL,
    status        TEXT NOT NULL,
    task_id       TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    error_code    TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE webhook_events IS 'Durable audit trail for signed webhook ingress attempts.';
COMMENT ON COLUMN webhook_events.status IS 'accepted | rejected | duplicate';
COMMENT ON COLUMN webhook_events.payload_hash IS 'SHA-256 hash of the raw request body; raw payload is intentionally not persisted.';

CREATE INDEX IF NOT EXISTS idx_webhook_events_project_time
    ON webhook_events (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_events_source_time
    ON webhook_events (project_id, source, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_events_status_time
    ON webhook_events (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_events_event_id
    ON webhook_events (project_id, source, event_id);

-- =============================================================================
-- TRADING (2026.5.1+)
-- =============================================================================

CREATE TABLE IF NOT EXISTS trading_orders (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    task_id             TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    execution_id        TEXT,
    broker_order_id     TEXT,
    idempotency_key     TEXT NOT NULL,
    mode                TEXT NOT NULL,
    symbol              TEXT NOT NULL,
    action              TEXT NOT NULL,
    order_type          TEXT NOT NULL,
    qty                 NUMERIC(18,6) NOT NULL,
    limit_price         NUMERIC(18,6),
    stop_price          NUMERIC(18,6),
    time_in_force       TEXT NOT NULL DEFAULT 'DAY',
    status              TEXT NOT NULL,
    last_status_reason  TEXT NOT NULL DEFAULT '',
    submitted_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    terminal_at         TIMESTAMP WITH TIME ZONE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trading_orders_idempotency
    ON trading_orders(project_id, idempotency_key);
CREATE INDEX IF NOT EXISTS idx_trading_orders_project_time
    ON trading_orders(project_id, submitted_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_orders_status
    ON trading_orders(project_id, status, submitted_at DESC);

CREATE TABLE IF NOT EXISTS trading_fills (
    id                  TEXT PRIMARY KEY,
    order_id            TEXT NOT NULL REFERENCES trading_orders(id) ON DELETE CASCADE,
    project_id          TEXT NOT NULL,
    symbol              TEXT NOT NULL,
    qty                 NUMERIC(18,6) NOT NULL,
    price               NUMERIC(18,6) NOT NULL,
    commission_usd      NUMERIC(18,6),
    filled_at           TIMESTAMP WITH TIME ZONE NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trading_fills_project_time
    ON trading_fills(project_id, filled_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_fills_order
    ON trading_fills(order_id, filled_at);

CREATE TABLE IF NOT EXISTS trading_positions_snapshots (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    recorded_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    cash_usd            NUMERIC(18,2) NOT NULL,
    equity_usd          NUMERIC(18,2) NOT NULL,
    unrealised_pl_usd   NUMERIC(18,2) NOT NULL,
    realised_pl_day_usd NUMERIC(18,2) NOT NULL,
    positions_json      JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trading_positions_project_time
    ON trading_positions_snapshots(project_id, recorded_at DESC);

-- Execution-keyed fill reconciliation (2026-06-26, migration 38).
-- Broker-side fills are booked by IBKR execId (stable, per-account) so a
-- fill survives a broker restart that empties the in-memory placedOrders
-- map. filled_qty drives share-based order-status transitions; source/
-- source_detail tag broker-reconcile fills with no local order link.
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
        WHERE table_name = 'trading_orders' AND column_name = 'filled_qty') THEN
        ALTER TABLE trading_orders ADD COLUMN filled_qty NUMERIC(18,6) NOT NULL DEFAULT 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
        WHERE table_name = 'trading_fills' AND column_name = 'exec_id') THEN
        ALTER TABLE trading_fills
            ADD COLUMN exec_id       TEXT,
            ADD COLUMN account_id    TEXT,
            ADD COLUMN source        TEXT NOT NULL DEFAULT 'reconcile',
            ADD COLUMN source_detail TEXT;
        CREATE UNIQUE INDEX IF NOT EXISTS idx_trading_fills_exec
            ON trading_fills(account_id, exec_id) WHERE exec_id IS NOT NULL;
        CREATE INDEX IF NOT EXISTS idx_trading_fills_filled_at
            ON trading_fills(filled_at);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS trading_fills_shadow (
    id              TEXT PRIMARY KEY,
    order_id        TEXT,
    project_id      TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    qty             NUMERIC(18,6) NOT NULL,
    price           NUMERIC(18,6) NOT NULL,
    commission_usd  NUMERIC(18,6),
    exec_id         TEXT,
    account_id      TEXT,
    source          TEXT NOT NULL DEFAULT 'reconcile',
    source_detail   TEXT,
    filled_at       TIMESTAMP WITH TIME ZONE NOT NULL,
    recorded_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_trading_fills_shadow_project_time
    ON trading_fills_shadow(project_id, filled_at DESC);

-- =============================================================================
-- INITIAL DATA / SEED
-- =============================================================================

-- No seed data required. Projects, swarms, and workflows are defined in YAML config.
-- This schema only tracks transactional state.
