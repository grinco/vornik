package sqlite

// schemaSQL is the consolidated SQLite schema for the five
// design-doc starter tables. Idempotent via CREATE TABLE IF NOT
// EXISTS — multiple Connect/Migrate calls converge on the same
// shape without ordering concerns.
//
// Translation notes (Postgres → SQLite):
//
//   - Enum types (task_status, execution_status, etc.) → TEXT
//     columns. The CHECK constraints below validate values at write
//     time so a typo lands as an error rather than a silent garbage
//     row. The set of allowed values mirrors models.go constants;
//     keep them in sync when adding new statuses.
//   - TIMESTAMP WITH TIME ZONE → TEXT (ISO 8601). SQLite has no
//     dedicated time type. Go's `time.Time.Scan` round-trips through
//     ISO 8601 cleanly when the column is TEXT.
//   - TEXT[] (Postgres arrays) → TEXT (JSON-encoded). The repos use
//     sqliteStringArray helpers to marshal/unmarshal.
//   - JSONB → BLOB. SQLite has no JSON column type; the repos store
//     bytes verbatim and parse on read.
//   - GIN indexes → omitted. SQLite has no analog; queries that
//     touch JSON fields scan instead. Acceptable at the dev/test
//     scale this backend targets.
//   - `NOW()` defaults → omitted at the schema layer; repos pass an
//     explicit `time.Now()` value on every INSERT, matching the
//     Postgres-side pattern that already populates these defensively.
//   - Foreign keys live on tasks/executions/artifacts to catch
//     orphaned rows during integration tests. ON DELETE rules mirror
//     the Postgres bootstrap.
const schemaSQL = `

-- ============================================================
-- tasks
-- ============================================================
CREATE TABLE IF NOT EXISTS tasks (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    workflow_id         TEXT,
    idempotency_key     TEXT,
    parent_task_id      TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    creation_source     TEXT NOT NULL DEFAULT 'USER'
                        CHECK (creation_source IN ('USER', 'DELEGATION', 'AUTONOMOUS', 'ROUTE', 'A2A', 'COMPANION')),
    delegation_mode     TEXT
                        CHECK (delegation_mode IS NULL OR delegation_mode IN ('SEQUENTIAL', 'PARALLEL', 'FAN_OUT')),
    status              TEXT NOT NULL DEFAULT 'QUEUED'
                        CHECK (status IN ('QUEUED','LEASED','RUNNING','PENDING',
                                          'COMPLETED','FAILED','CANCELLED',
                                          'WAITING_FOR_CHILDREN','AWAITING_INPUT',
                                          'AWAITING_EXTERNAL','CLOSED','PAUSED',
                                          'AWAITING_APPROVAL')),
    priority            INTEGER NOT NULL DEFAULT 50,
    payload             BLOB,
    dependencies        TEXT NOT NULL DEFAULT '[]', -- JSON array
    lease_id            TEXT,
    leased_at           TEXT,
    leased_by           TEXT,
    lease_expires_at    TEXT,
    attempt             INTEGER NOT NULL DEFAULT 0,
    max_attempts        INTEGER NOT NULL DEFAULT 3,
    last_error          TEXT,
    last_error_class    TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    -- Phase 23 conversational lifecycle columns:
    brief_amended_at    TEXT,
    current_phase       TEXT,
    expected_by         TEXT,
    closed_at           TEXT,
    closed_by           TEXT,
    message_count       INTEGER NOT NULL DEFAULT 0,
    open_checkpoint_id  TEXT,
    -- Migration v46 parity: soft link to the dispatcher turn that
    -- created this task. See migrations.go for rationale.
    chat_turn_id        TEXT,
    UNIQUE (project_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_tasks_project    ON tasks(project_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status     ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_parent     ON tasks(parent_task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_lease_exp  ON tasks(lease_expires_at);
CREATE INDEX IF NOT EXISTS idx_tasks_created    ON tasks(created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_chat_turn  ON tasks(chat_turn_id) WHERE chat_turn_id IS NOT NULL;

-- ============================================================
-- executions
-- ============================================================
CREATE TABLE IF NOT EXISTS executions (
    id                     TEXT PRIMARY KEY,
    task_id                TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id             TEXT NOT NULL,
    workflow_id            TEXT NOT NULL,
    workflow_revision      TEXT NOT NULL,
    workflow_snapshot      BLOB,
    status                 TEXT NOT NULL DEFAULT 'PENDING'
                           CHECK (status IN ('PENDING','RUNNING','COMPLETED','FAILED','CANCELLED','PAUSED')),
    current_step_id        TEXT,
    completed_steps        TEXT NOT NULL DEFAULT '[]', -- JSON array
    state_snapshot         BLOB,
    result                 BLOB,
    error_message          TEXT,
    error_code             TEXT,
    started_at             TEXT,
    completed_at           TEXT,
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL,
    -- Migration 48 (failure-forensics fork lineage). All nullable;
    -- only set on executions that are forks of an earlier run.
    parent_execution_id    TEXT,
    forked_from_step_id    TEXT,
    forked_prompt_override TEXT
);
CREATE INDEX IF NOT EXISTS idx_executions_task    ON executions(task_id);
CREATE INDEX IF NOT EXISTS idx_executions_project ON executions(project_id);
CREATE INDEX IF NOT EXISTS idx_executions_status  ON executions(status);
CREATE INDEX IF NOT EXISTS idx_executions_parent  ON executions(parent_execution_id) WHERE parent_execution_id IS NOT NULL;

-- ============================================================
-- artifacts
-- ============================================================
CREATE TABLE IF NOT EXISTS artifacts (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    execution_id        TEXT REFERENCES executions(id) ON DELETE CASCADE,
    task_id             TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    name                TEXT NOT NULL,
    artifact_class      TEXT NOT NULL
                        CHECK (artifact_class IN ('INPUT','OUTPUT','INTERMEDIATE','LOG')),
    storage_path        TEXT NOT NULL,
    size_bytes          INTEGER,
    content_hash_sha256 TEXT,
    mime_type           TEXT,
    created_at          TEXT NOT NULL,
    origin              TEXT NOT NULL DEFAULT 'unknown'
);
CREATE INDEX IF NOT EXISTS idx_artifacts_project   ON artifacts(project_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_execution ON artifacts(execution_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_task      ON artifacts(task_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_hash      ON artifacts(content_hash_sha256);

-- ============================================================
-- tool_audit_log
-- ============================================================
CREATE TABLE IF NOT EXISTS tool_audit_log (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    task_id             TEXT NOT NULL,
    execution_id        TEXT NOT NULL,
    step_id             TEXT NOT NULL DEFAULT '',
    tool_name           TEXT NOT NULL,
    tool_input          TEXT NOT NULL DEFAULT '',
    tool_output         TEXT NOT NULL DEFAULT '',
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tool_audit_project   ON tool_audit_log(project_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_task      ON tool_audit_log(task_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_execution ON tool_audit_log(execution_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_tool_name ON tool_audit_log(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_audit_created   ON tool_audit_log(created_at DESC);

-- ============================================================
-- task_watchers
-- ============================================================
CREATE TABLE IF NOT EXISTS task_watchers (
    task_id    TEXT NOT NULL,
    chat_id    INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (task_id, chat_id)
);
CREATE INDEX IF NOT EXISTS idx_task_watchers_task ON task_watchers(task_id);

-- ============================================================
-- api_keys
-- ============================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    name              TEXT NOT NULL,
    key_hash          TEXT NOT NULL UNIQUE,
    key_prefix        TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    last_used_at      TEXT,
    expires_at        TEXT,
    revoked_at        TEXT,
    created_by        TEXT,
    rate_limit_rps    INTEGER,
    rate_limit_burst  INTEGER,
    -- Companion-plugin scope (LLD 21). All four are nullable;
    -- only set when a companion grant handler mints the key.
    allowed_workflows TEXT,        -- JSON array of workflow IDs; NULL = all project workflows
    budget_cap_usd    REAL,        -- lifetime USD cap for this key; NULL = uncapped
    client_kind       TEXT,        -- "claude-code", "codex", "opencode", "gemini-cli"; NULL = non-companion
    session_label     TEXT,        -- operator-friendly session marker
    default_repo_scope TEXT,       -- migration 110: repo_scope stamped on memory calls that omit it; NULL = none. Rollback (ALTER TABLE ... DROP COLUMN) needs SQLite >= 3.35.0 (2021); older builds reject DROP COLUMN and must recreate the table instead.
    -- Companion RAG capabilities (LLD 22). Default 0 so existing
    -- keys can't read or write project memory until explicitly granted.
    memory_read       INTEGER NOT NULL DEFAULT 0,
    memory_write      INTEGER NOT NULL DEFAULT 0,
    -- git-over-HTTPS push gate (LLD slice 2). Default 0 = read-only.
    allow_push        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_keys_project   ON api_keys(project_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_active    ON api_keys(key_hash) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_api_keys_client_kind ON api_keys(client_kind) WHERE client_kind IS NOT NULL;

-- ============================================================
-- webhook_events
-- ============================================================
CREATE TABLE IF NOT EXISTS webhook_events (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    source        TEXT NOT NULL,
    event_id      TEXT NOT NULL DEFAULT '',
    payload_hash  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL,
    task_id       TEXT,
    error_code    TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_webhook_events_project ON webhook_events(project_id);
CREATE INDEX IF NOT EXISTS idx_webhook_events_status  ON webhook_events(status);
CREATE INDEX IF NOT EXISTS idx_webhook_events_created ON webhook_events(created_at DESC);

-- ============================================================
-- task_messages
-- ============================================================
CREATE TABLE IF NOT EXISTS task_messages (
    id           TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL,
    execution_id TEXT,
    parent_id    TEXT,
    author_kind  TEXT NOT NULL,
    author_id    TEXT,
    message_kind TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    metadata     BLOB,
    created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_messages_task    ON task_messages(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_task_messages_parent  ON task_messages(parent_id);

-- ============================================================
-- task_scratchpads
-- ============================================================
CREATE TABLE IF NOT EXISTS task_scratchpads (
    task_id            TEXT PRIMARY KEY,
    summary            TEXT NOT NULL DEFAULT '',
    facts              BLOB,
    open_questions     BLOB,
    current_phase      TEXT,
    phase_history      BLOB,
    last_execution_id  TEXT,
    updated_at         TEXT NOT NULL
);

-- ============================================================
-- telegram_task_threads
-- ============================================================
CREATE TABLE IF NOT EXISTS telegram_task_threads (
    id         TEXT PRIMARY KEY,
    task_id    TEXT NOT NULL,
    chat_id    INTEGER NOT NULL,
    thread_id  INTEGER NOT NULL,
    topic_name TEXT NOT NULL,
    created_at TEXT NOT NULL,
    closed_at  TEXT,
    UNIQUE (chat_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_telegram_threads_task ON telegram_task_threads(task_id);

-- ============================================================
-- autonomy_evaluations
-- ============================================================
CREATE TABLE IF NOT EXISTS autonomy_evaluations (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    task_id     TEXT,
    task_type   TEXT NOT NULL DEFAULT '',
    workflow_id TEXT NOT NULL DEFAULT '',
    prompt_hash TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_autonomy_eval_project_created ON autonomy_evaluations(project_id, created_at DESC);

-- ============================================================
-- intent_verdicts
-- ============================================================
CREATE TABLE IF NOT EXISTS intent_verdicts (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    task_id          TEXT,
    execution_id     TEXT,
    chat_id          INTEGER,
    tool_name        TEXT NOT NULL,
    tool_args        TEXT NOT NULL DEFAULT '',
    heuristic_risk   TEXT NOT NULL,
    heuristic_conf   REAL NOT NULL DEFAULT 0,
    heuristic_rec    TEXT NOT NULL,
    heuristic_reason TEXT NOT NULL DEFAULT '',
    heuristic_lat_ms INTEGER NOT NULL DEFAULT 0,
    llm_risk         TEXT,
    llm_conf         REAL,
    llm_rec          TEXT,
    llm_reason       TEXT,
    llm_lat_ms       INTEGER,
    llm_model        TEXT,
    final_risk       TEXT NOT NULL,
    final_rec        TEXT NOT NULL,
    created_at       TEXT NOT NULL,
    refined_at       TEXT
);
CREATE INDEX IF NOT EXISTS idx_intent_verdicts_project_created ON intent_verdicts(project_id, created_at DESC);

-- ============================================================
-- task_judge_verdicts
-- ============================================================
CREATE TABLE IF NOT EXISTS task_judge_verdicts (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    task_id     TEXT NOT NULL UNIQUE,
    role        TEXT NOT NULL,
    model       TEXT NOT NULL,
    verdict     TEXT NOT NULL,
    confidence  REAL NOT NULL DEFAULT 0,
    signals     BLOB,
    summary     TEXT NOT NULL DEFAULT '',
    cost_usd    REAL NOT NULL DEFAULT 0,
    recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_judge_verdicts_project_recorded ON task_judge_verdicts(project_id, recorded_at DESC);

-- ============================================================
-- task_post_mortems
-- ============================================================
CREATE TABLE IF NOT EXISTS task_post_mortems (
    task_id           TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    summary           TEXT NOT NULL,
    model             TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL NOT NULL DEFAULT 0,
    recorded_at       TEXT NOT NULL
);

-- ============================================================
-- memory_retrieval_audit
-- ============================================================
CREATE TABLE IF NOT EXISTS memory_retrieval_audit (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    task_id      TEXT,
    execution_id TEXT,
    step_id      TEXT,
    role         TEXT,
    query        TEXT NOT NULL DEFAULT '',
    chunk_ids    TEXT NOT NULL DEFAULT '[]',
    retrieved_at TEXT NOT NULL,
    -- Companion RAG (LLD 22): structured agent-vs-companion split.
    -- actor_kind ∈ {"agent","companion"}; actor_id is role name for
    -- agents, key.ID for companion calls. Nullable so legacy rows
    -- read back cleanly.
    actor_kind   TEXT,
    actor_id     TEXT,
    -- Migration 75: per-deposit repo partitioning.
    repo_scope   TEXT
);
CREATE INDEX IF NOT EXISTS idx_memory_audit_project_scope ON memory_retrieval_audit(project_id, repo_scope, retrieved_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_audit_project_time ON memory_retrieval_audit(project_id, retrieved_at DESC);

-- ============================================================
-- memory_ingest_audit — per-call ingest record (migration 74 in postgres).
-- Companion-direct deposits via IngestCompanionNote bypass
-- project_ingest_queue, so the queue's per-row producer_role + state
-- don't see them. This table captures one row per attempt regardless
-- of gate decision, indexed on (project_id, ingested_at DESC) for the
-- dashboard query and on actor_id for "what did key X deposit".
-- ============================================================
CREATE TABLE IF NOT EXISTS memory_ingest_audit (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    actor_kind      TEXT,
    actor_id        TEXT,
    source_name     TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    content_bytes   INTEGER NOT NULL,
    proposed_class  TEXT,
    decision        TEXT NOT NULL CHECK (decision IN ('admitted','quarantined','rejected')),
    gate_failed     TEXT,
    chunks_admitted INTEGER NOT NULL DEFAULT 0,
    ingested_at     TEXT NOT NULL,
    -- LLD: repo_scope partitions deposits within a single project so
    -- the operator's many repos don't pollute each other's RAG.
    -- NULL = uncategorized; "*" = cross-cutting (surfaces in every
    -- scoped query); any other string = repo token. Migration 75
    -- in postgres; SQLite mirror landed in the same release.
    repo_scope      TEXT
);
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_project_time ON memory_ingest_audit(project_id, ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_actor ON memory_ingest_audit(actor_id) WHERE actor_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_project_scope ON memory_ingest_audit(project_id, repo_scope, ingested_at DESC);

-- ============================================================
-- task_llm_usage — round 2 (financial)
-- ============================================================
CREATE TABLE IF NOT EXISTS task_llm_usage (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    task_id               TEXT,
    execution_id          TEXT,
    step_id               TEXT NOT NULL DEFAULT '',
    role                  TEXT NOT NULL,
    model                 TEXT NOT NULL,
    prompt_tokens         INTEGER NOT NULL DEFAULT 0,
    completion_tokens     INTEGER NOT NULL DEFAULT 0,
    iterations            INTEGER NOT NULL DEFAULT 1,
    cost_usd              REAL NOT NULL DEFAULT 0,
    source                TEXT NOT NULL DEFAULT 'workflow_step',
    session_id            TEXT,
    recorded_at           TEXT NOT NULL,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_llm_usage_project_time ON task_llm_usage(project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_usage_task        ON task_llm_usage(task_id);

-- ============================================================
-- budget_reservations — in-flight claims on the LLM hard-cap
-- budget (trading-hardening §1). Inserted at task admission,
-- settled at task termination; the admission txn sums unsettled
-- rows so concurrent admissions can't overshoot the cap. No FK on
-- task_id — the gate runs before the task row exists (see the
-- Postgres migration 103 comment).
-- ============================================================
CREATE TABLE IF NOT EXISTS budget_reservations (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    estimated_usd REAL NOT NULL,
    reserved_at   TEXT NOT NULL,
    settled_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_budget_reservations_unsettled ON budget_reservations(project_id) WHERE settled_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_budget_reservations_task      ON budget_reservations(task_id);

-- ============================================================
-- a2a_push_configs — per-task A2A webhook push-notification
-- config (url + optional bearer token the caller wants echoed).
-- Lets the daemon POST task-state updates to an A2A caller that
-- isn't holding an open SSE stream. Keyed by task_id.
-- ============================================================
CREATE TABLE IF NOT EXISTS a2a_push_configs (
    task_id    TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    token      TEXT,
    created_at TEXT NOT NULL
);

-- ============================================================
-- recovery_events — append-only marker for graceful-recovery exits
-- (execution reached a Recovery:true terminal). See
-- https://docs.vornik.io
-- ============================================================
CREATE TABLE IF NOT EXISTS recovery_events (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    execution_id  TEXT NOT NULL,
    workflow_id   TEXT NOT NULL DEFAULT '',
    terminal_id   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recovery_events_created ON recovery_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recovery_events_project ON recovery_events(project_id);

-- ============================================================
-- trading_orders — round 2 (financial)
-- ============================================================
CREATE TABLE IF NOT EXISTS trading_orders (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    task_id             TEXT,
    execution_id        TEXT,
    broker_order_id     TEXT,
    idempotency_key     TEXT NOT NULL,
    mode                TEXT NOT NULL,
    symbol              TEXT NOT NULL,
    action              TEXT NOT NULL,
    order_type          TEXT NOT NULL,
    qty                 REAL NOT NULL,
    filled_qty          REAL NOT NULL DEFAULT 0,
    limit_price         REAL,
    stop_price          REAL,
    time_in_force       TEXT NOT NULL DEFAULT 'DAY',
    status              TEXT NOT NULL,
    last_status_reason  TEXT NOT NULL DEFAULT '',
    submitted_at        TEXT NOT NULL,
    terminal_at         TEXT,
    UNIQUE (project_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_trading_orders_project_submitted ON trading_orders(project_id, submitted_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_orders_status            ON trading_orders(status);
CREATE INDEX IF NOT EXISTS idx_trading_orders_symbol            ON trading_orders(project_id, symbol);

-- ============================================================
-- trading_fills — round 2 (financial)
-- ============================================================
CREATE TABLE IF NOT EXISTS trading_fills (
    id             TEXT PRIMARY KEY,
    order_id       TEXT NOT NULL,
    project_id     TEXT NOT NULL,
    symbol         TEXT NOT NULL,
    qty            REAL NOT NULL,
    price          REAL NOT NULL,
    commission_usd REAL,
    exec_id        TEXT,
    account_id     TEXT,
    source         TEXT NOT NULL DEFAULT 'reconcile',
    source_detail  TEXT,
    filled_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trading_fills_project_time ON trading_fills(project_id, filled_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_fills_order        ON trading_fills(order_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trading_fills_exec  ON trading_fills(account_id, exec_id) WHERE exec_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_trading_fills_filled_at    ON trading_fills(filled_at);

-- ============================================================
-- trading_fills_shadow — reconcile-side fill ledger (2026-06-26)
-- ============================================================
CREATE TABLE IF NOT EXISTS trading_fills_shadow (
    id             TEXT PRIMARY KEY,
    order_id       TEXT,
    project_id     TEXT NOT NULL,
    symbol         TEXT NOT NULL,
    qty            REAL NOT NULL,
    price          REAL NOT NULL,
    commission_usd REAL,
    exec_id        TEXT,
    account_id     TEXT,
    source         TEXT NOT NULL DEFAULT 'reconcile',
    source_detail  TEXT,
    filled_at      TEXT NOT NULL,
    recorded_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trading_fills_shadow_project_time ON trading_fills_shadow(project_id, filled_at DESC);

-- ============================================================
-- trading_safety_events — round 2 (financial)
-- ============================================================
CREATE TABLE IF NOT EXISTS trading_safety_events (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    kind        TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'warn',
    symbol      TEXT,
    detail      BLOB
);
CREATE INDEX IF NOT EXISTS idx_trading_safety_project_time ON trading_safety_events(project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_safety_kind         ON trading_safety_events(kind);

-- ============================================================
-- trading_positions_snapshots — round 2 (financial)
-- ============================================================
CREATE TABLE IF NOT EXISTS trading_positions_snapshots (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    recorded_at         TEXT NOT NULL,
    cash_usd            REAL NOT NULL DEFAULT 0,
    equity_usd          REAL NOT NULL DEFAULT 0,
    unrealised_pl_usd   REAL NOT NULL DEFAULT 0,
    realised_pl_day_usd REAL NOT NULL DEFAULT 0,
    positions_json      BLOB
);
CREATE INDEX IF NOT EXISTS idx_trading_snapshots_project_time ON trading_positions_snapshots(project_id, recorded_at DESC);

-- ============================================================
-- execution_step_outcomes — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS execution_step_outcomes (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    task_id               TEXT NOT NULL,
    execution_id          TEXT NOT NULL,
    step_id               TEXT NOT NULL,
    role                  TEXT NOT NULL DEFAULT '',
    model                 TEXT NOT NULL DEFAULT '',
    outcome               TEXT NOT NULL,
    attributed_to_step_id TEXT,
    error_class           TEXT NOT NULL DEFAULT '',
    error_detail          TEXT NOT NULL DEFAULT '',
    duration_ms           INTEGER,
    finalized_at          TEXT,
    recorded_at           TEXT NOT NULL,
    hallucination_signals BLOB,
    -- migration 106 parity: budget-stamp columns for the instinct ↔ tool-budget seam
    complexity_tier       TEXT,
    effective_tool_budget INTEGER,
    tool_calls_used       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_execution ON execution_step_outcomes(execution_id);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_project_time ON execution_step_outcomes(project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_pending
  ON execution_step_outcomes(execution_id, step_id) WHERE outcome = 'pending_validation';

-- ============================================================
-- knowledge_entities — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS knowledge_entities (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    type              TEXT NOT NULL,
    canonical_name    TEXT NOT NULL,
    aliases           BLOB,
    description       TEXT NOT NULL DEFAULT '',
    properties        BLOB,
    embedding         BLOB,
    extracted_by      TEXT NOT NULL DEFAULT '',
    resolved_by       TEXT NOT NULL DEFAULT '',
    confidence        REAL NOT NULL DEFAULT 0,
    lifecycle_state   TEXT NOT NULL DEFAULT 'pending',
    validation_status TEXT NOT NULL DEFAULT 'pending',
    epoch_id          TEXT,
    expires_at        TEXT,
    supersedes_id     TEXT,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL,
    UNIQUE (project_id, type, canonical_name)
);
CREATE INDEX IF NOT EXISTS idx_kg_entities_project_type ON knowledge_entities(project_id, type);

-- ============================================================
-- knowledge_edges — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS knowledge_edges (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    from_entity     TEXT NOT NULL,
    to_entity       TEXT NOT NULL,
    predicate       TEXT NOT NULL,
    properties      BLOB,
    source_chunks   TEXT NOT NULL DEFAULT '[]',
    extracted_by    TEXT NOT NULL DEFAULT '',
    confidence      REAL NOT NULL DEFAULT 0,
    faithfulness    REAL,
    lifecycle_state TEXT NOT NULL DEFAULT 'pending',
    epoch_id        TEXT,
    created_at      TEXT NOT NULL,
    UNIQUE (project_id, from_entity, predicate, to_entity),
    -- No self-loops: a relationship from an entity to itself is
    -- meaningless and pollutes graph walks (mirrors postgres migration
    -- 98's knowledge_edges_no_self_loop CHECK).
    CHECK (from_entity <> to_entity)
);
CREATE INDEX IF NOT EXISTS idx_kg_edges_from ON knowledge_edges(from_entity);
CREATE INDEX IF NOT EXISTS idx_kg_edges_to   ON knowledge_edges(to_entity);
-- from/to entities must belong to the edge's project (write-time
-- enforcement; mirrors postgres migration 98's same-project trigger).
CREATE TRIGGER IF NOT EXISTS knowledge_edges_same_project_ins
BEFORE INSERT ON knowledge_edges
WHEN EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.from_entity AND project_id <> NEW.project_id)
   OR EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.to_entity AND project_id <> NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'knowledge_edges: from/to entity must belong to edge project');
END;
CREATE TRIGGER IF NOT EXISTS knowledge_edges_same_project_upd
BEFORE UPDATE ON knowledge_edges
WHEN EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.from_entity AND project_id <> NEW.project_id)
   OR EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.to_entity AND project_id <> NEW.project_id)
BEGIN
    SELECT RAISE(ABORT, 'knowledge_edges: from/to entity must belong to edge project');
END;

-- ============================================================
-- entity_mentions — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS entity_mentions (
    chunk_id   TEXT NOT NULL,
    entity_id  TEXT NOT NULL,
    char_start INTEGER NOT NULL,
    char_end   INTEGER,
    surface    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (chunk_id, entity_id, char_start)
);
CREATE INDEX IF NOT EXISTS idx_entity_mentions_entity ON entity_mentions(entity_id);

-- ============================================================
-- project_memory_chunks — needed by ChunkGraphExtractionRepository
-- + the round-1 MemoryRetrievalAudit gap. Slim schema (real
-- daemon uses pgvector + full-text indexes; for tests we just
-- need rows the worker can drain).
-- ============================================================
CREATE TABLE IF NOT EXISTS project_memory_chunks (
    id                                 TEXT PRIMARY KEY,
    project_id                         TEXT NOT NULL,
    content                            TEXT NOT NULL DEFAULT '',
    content_hash                       TEXT,
    needs_graph_extraction             INTEGER NOT NULL DEFAULT 0,
    derived_from_extracted_document_id TEXT,
    derived_from_section_id            TEXT,
    created_at                         TEXT NOT NULL,
    -- repo_scope partitions deposits within a single project so the
    -- operator's many repos don't pollute each other's RAG. NULL =
    -- uncategorized; "*" = cross-cutting; any other string = repo
    -- token. See migration 75.
    repo_scope                         TEXT,
    -- Rollback x supersession provenance (postgres migration 89; see
    -- memory-rollback-supersession-design.md). Carried here so the
    -- sqlite CorpusEpochRepository's rollback restore pass runs
    -- against the same column shape.
    validation_status                  TEXT NOT NULL DEFAULT 'unverified',
    superseded_in_epoch                TEXT,
    pre_supersede_status               TEXT,
    -- A recorded supersession epoch only makes sense on a superseded
    -- chunk (the restore pass keys on both). The reverse is NOT an
    -- invariant: a chunk superseded with no tracked epoch (empty epochID,
    -- or pre-migration history) is legitimately status='superseded' with
    -- a NULL epoch. Mirrors postgres migration 100.
    CHECK (superseded_in_epoch IS NULL OR validation_status = 'superseded')
);
CREATE INDEX IF NOT EXISTS idx_chunks_project_needs ON project_memory_chunks(project_id, needs_graph_extraction);
CREATE INDEX IF NOT EXISTS idx_chunks_project_time ON project_memory_chunks(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_chunks_project_scope ON project_memory_chunks(project_id, repo_scope, created_at DESC);

-- ============================================================
-- extracted_documents — Phase 0 of document-extraction pipeline.
-- See https://docs.vornik.io §5.
-- Test-fidelity slim schema (production runs on PostgreSQL with
-- richer JSONB + index types).
-- ============================================================
CREATE TABLE IF NOT EXISTS extracted_documents (
    id                     TEXT PRIMARY KEY,
    project_id             TEXT NOT NULL,
    source_artifact_id     TEXT NOT NULL,
    extractor_name         TEXT NOT NULL,
    extractor_version      TEXT NOT NULL,
    mime_type              TEXT NOT NULL,
    storage_path           TEXT NOT NULL,
    metadata_blob          TEXT NOT NULL DEFAULT '{}',
    outline_blob           TEXT NOT NULL DEFAULT '[]',
    section_count          INTEGER NOT NULL DEFAULT 0,
    total_text_bytes       INTEGER NOT NULL DEFAULT 0,
    extraction_duration_ms INTEGER,
    status                 TEXT NOT NULL DEFAULT 'OK',
    extracted_at           TEXT NOT NULL,
    UNIQUE (source_artifact_id, extractor_name, extractor_version)
);
CREATE INDEX IF NOT EXISTS idx_extdoc_project ON extracted_documents(project_id, extracted_at);
CREATE INDEX IF NOT EXISTS idx_extdoc_source ON extracted_documents(source_artifact_id);

-- ============================================================
-- corpus_epochs — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS corpus_epochs (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    ingest_execution_id TEXT,
    created_at          TEXT NOT NULL,
    closed_at           TEXT,
    chunks_admitted     INTEGER NOT NULL DEFAULT 0,
    chunks_quarantined  INTEGER NOT NULL DEFAULT 0,
    chunks_verified     INTEGER NOT NULL DEFAULT 0,
    chunks_refuted      INTEGER NOT NULL DEFAULT 0,
    chunks_superseded   INTEGER NOT NULL DEFAULT 0,
    notes               TEXT,
    -- Explicit-deactivation tombstone (migration 89): rollback's
    -- re-activation pass skips tombstoned epochs; Activate clears.
    deactivated_at      TEXT,
    deactivated_by      TEXT
);
CREATE INDEX IF NOT EXISTS idx_epochs_project_created ON corpus_epochs(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS corpus_epochs_active (
    project_id    TEXT NOT NULL,
    epoch_id      TEXT NOT NULL,
    activated_at  TEXT NOT NULL,
    activated_by  TEXT NOT NULL DEFAULT 'system',
    reason        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, epoch_id)
);

CREATE TABLE IF NOT EXISTS corpus_rollbacks (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    from_epoch_id TEXT,
    to_epoch_id   TEXT,
    triggered_by TEXT NOT NULL DEFAULT '',
    reason       TEXT,
    applied_at   TEXT NOT NULL,
    -- Restore-pass audit counter (migration 89).
    chunks_restored INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_rollbacks_project_time ON corpus_rollbacks(project_id, applied_at DESC);

-- ============================================================
-- memory_quarantine — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS memory_quarantine (
    id                   TEXT PRIMARY KEY,
    project_id           TEXT NOT NULL,
    source_artifact_id   TEXT NOT NULL,
    producer_role        TEXT,
    ingest_execution_id  TEXT,
    content              TEXT NOT NULL DEFAULT '',
    content_hash         TEXT NOT NULL DEFAULT '',
    proposed_class       TEXT,
    failed_gate          TEXT NOT NULL,
    failure_detail       TEXT,
    quarantined_at       TEXT NOT NULL,
    released_at          TEXT,
    released_chunk_id    TEXT,
    dropped_at           TEXT,
    repo_scope           TEXT
);
CREATE INDEX IF NOT EXISTS idx_quarantine_project_time ON memory_quarantine(project_id, quarantined_at DESC);
CREATE INDEX IF NOT EXISTS idx_quarantine_project_gate ON memory_quarantine(project_id, failed_gate);
CREATE INDEX IF NOT EXISTS idx_quarantine_project_scope ON memory_quarantine(project_id, repo_scope, quarantined_at DESC);

-- ============================================================
-- project_ingest_queue — round 3 (memory/KG)
-- ============================================================
CREATE TABLE IF NOT EXISTS project_ingest_queue (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    source_artifact_id  TEXT NOT NULL,
    producer_role       TEXT NOT NULL DEFAULT '',
    ingest_execution_id TEXT,
    priority            INTEGER NOT NULL DEFAULT 0,
    proposed_class      TEXT,
    proposed_confidence REAL NOT NULL DEFAULT 0,
    state               TEXT NOT NULL DEFAULT 'queued',
    attempts            INTEGER NOT NULL DEFAULT 0,
    enqueued_at         TEXT NOT NULL,
    started_at          TEXT,
    finished_at         TEXT,
    last_error          TEXT,
    repo_scope          TEXT
);
CREATE INDEX IF NOT EXISTS idx_ingest_project_state ON project_ingest_queue(project_id, state);
CREATE INDEX IF NOT EXISTS idx_ingest_state_started ON project_ingest_queue(state, started_at);
-- At most one ACTIVE (queued|processing) row per (project, artifact) so a
-- replayed / re-processed enqueue can't double-ingest the same content.
-- Mirrors postgres migration 97; Enqueue uses ON CONFLICT DO NOTHING.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ingest_queue_active
    ON project_ingest_queue(project_id, source_artifact_id)
    WHERE state IN ('queued','processing');

-- ============================================================
-- admin_audit — admin-ui-design.md §3.3
--
-- Operator-action audit log. SQLite parity for the migration
-- v37 postgres table. ip is TEXT here (no INET type in SQLite);
-- before_state / after_state are TEXT carrying serialised JSON.
-- ============================================================
CREATE TABLE IF NOT EXISTS admin_audit (
    id           TEXT PRIMARY KEY,
    ts           TEXT NOT NULL,
    principal    TEXT NOT NULL,
    source       TEXT NOT NULL,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL DEFAULT '',
    before_state TEXT,
    after_state  TEXT,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_ts          ON admin_audit(ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_action_ts   ON admin_audit(action, ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_principal_ts ON admin_audit(principal, ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_target      ON admin_audit(target) WHERE target <> '';

-- ============================================================
-- chat_audit_log + chat_system_prompts — migration v39 parity
--
-- Per-turn dispatcher activity. One row per inbound user message
-- processed through the LLM tool loop. system_prompt_hash points
-- at the content-addressed prompts table so the body is stored
-- once per distinct hash.
-- ============================================================
CREATE TABLE IF NOT EXISTS chat_system_prompts (
    hash TEXT PRIMARY KEY,
    body TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat_audit_log (
    id                  TEXT PRIMARY KEY,
    ts                  TEXT NOT NULL,
    chat_id             TEXT NOT NULL DEFAULT '',
    user_id             TEXT NOT NULL DEFAULT '',
    project_id          TEXT NOT NULL DEFAULT '',
    role_used           TEXT NOT NULL DEFAULT '',
    model               TEXT NOT NULL DEFAULT '',
    system_prompt_hash  TEXT NOT NULL DEFAULT '',
    user_message        TEXT NOT NULL DEFAULT '',
    tool_calls_json     TEXT NOT NULL DEFAULT '[]',
    response            TEXT NOT NULL DEFAULT '',
    iterations          INTEGER NOT NULL DEFAULT 0,
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0,
    hallucination_signals_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_chat_audit_ts         ON chat_audit_log(ts DESC);
CREATE INDEX IF NOT EXISTS idx_chat_audit_chat_ts    ON chat_audit_log(chat_id, ts DESC) WHERE chat_id <> '';
CREATE INDEX IF NOT EXISTS idx_chat_audit_project_ts ON chat_audit_log(project_id, ts DESC) WHERE project_id <> '';

-- ============================================================
-- project_wizard_sessions (migration 49 — Feature #2)
-- ============================================================
CREATE TABLE IF NOT EXISTS project_wizard_sessions (
    id                   TEXT PRIMARY KEY,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    operator_id          TEXT NOT NULL,
    transcript           TEXT NOT NULL DEFAULT '[]',
    current_proposal     TEXT,
    suggested_template   TEXT,
    ready_to_commit      INTEGER NOT NULL DEFAULT 0,
    committed_project_id TEXT,
    committed_at         TEXT,
    cancelled_at         TEXT
);
CREATE INDEX IF NOT EXISTS idx_pw_sessions_operator    ON project_wizard_sessions(operator_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_pw_sessions_uncommitted ON project_wizard_sessions(updated_at DESC) WHERE committed_project_id IS NULL;

-- ============================================================
-- execution_hints (migration 50 — Feature #3; task-scope ext 66)
-- ============================================================
-- 2026-05-26: task_id nullable allows scoping a hint to the whole
-- task (carries across retries). execution_id is also nullable
-- now; the CHECK enforces at least one scope is set.
CREATE TABLE IF NOT EXISTS execution_hints (
    id           TEXT PRIMARY KEY,
    task_id      TEXT,
    execution_id TEXT,
    step_id      TEXT,
    content      TEXT NOT NULL,
    applied_at   TEXT,
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL,
    CHECK (task_id IS NOT NULL OR execution_id IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_execution_hints_pending
    ON execution_hints(execution_id, applied_at) WHERE applied_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_execution_hints_task_pending
    ON execution_hints(task_id, applied_at)
    WHERE applied_at IS NULL AND task_id IS NOT NULL;

-- ============================================================
-- instincts + instinct_evidence + instinct_applications
-- (migrations 85/86 — continuous-learning instinct layer).
--
-- Parity notes vs the Postgres side:
--   - trigger_json JSONB → BLOB (raw JSON bytes, parsed on read).
--   - confidence DOUBLE PRECISION → REAL.
--   - CHECK constraints mirror the models.go domain/status/source/
--     polarity constants — keep them in sync when adding values
--     (the CHECK-parity lesson).
--   - idx_instincts_dedup UNIQUE (scope, project_id, trigger_key)
--     is the upsert atomicity key.
--   - instinct_evidence PK (instinct_id, outcome_id) makes the
--     extraction worker idempotent across overlapping windows.
-- ============================================================
CREATE TABLE IF NOT EXISTS instincts (
    id               TEXT PRIMARY KEY,
    scope            TEXT NOT NULL DEFAULT 'project'
                     CHECK (scope IN ('project','global')),
    project_id       TEXT NOT NULL DEFAULT '',
    domain           TEXT NOT NULL
                     CHECK (domain IN ('recovery','cost','quality','retrieval','workflow')),
    trigger_key      TEXT NOT NULL,
    trigger_json     BLOB,
    action           TEXT NOT NULL,
    confidence       REAL NOT NULL DEFAULT 0,
    support_count    INTEGER NOT NULL DEFAULT 0,
    contradict_count INTEGER NOT NULL DEFAULT 0,
    source           TEXT NOT NULL DEFAULT 'observer'
                     CHECK (source IN ('observer','operator','architect-reject')),
    status           TEXT NOT NULL DEFAULT 'candidate'
                     CHECK (status IN ('candidate','active','promoted','retired')),
    distill_model    TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_instincts_dedup ON instincts(scope, project_id, trigger_key);
CREATE INDEX IF NOT EXISTS idx_instincts_project_domain ON instincts(project_id, domain);
CREATE INDEX IF NOT EXISTS idx_instincts_status_confidence ON instincts(status, confidence DESC);

CREATE TABLE IF NOT EXISTS instinct_evidence (
    instinct_id  TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    outcome_id   TEXT NOT NULL,
    polarity     TEXT NOT NULL DEFAULT 'support'
                 CHECK (polarity IN ('support','contradict')),
    -- W6 per-action evidence partitioning (migration 101 parity): the
    -- instinct action this outcome corroborated at record time.
    -- RecomputeConfidence counts only evidence matching the instinct's
    -- current action, so a replaced action doesn't inherit foreign evidence.
    action       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (instinct_id, outcome_id)
);
CREATE INDEX IF NOT EXISTS idx_instinct_evidence_instinct ON instinct_evidence(instinct_id);
CREATE INDEX IF NOT EXISTS idx_instinct_evidence_instinct_action ON instinct_evidence(instinct_id, action);

-- W6 action-version history (migration 101 parity): append-only ledger
-- snapshotting a displaced action's final state before a new action takes
-- over. reason ∈ {action_change, w6_replace}.
CREATE TABLE IF NOT EXISTS instinct_action_history (
    id               TEXT PRIMARY KEY,
    instinct_id      TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    action           TEXT NOT NULL,
    confidence       REAL NOT NULL DEFAULT 0,
    support_count    INTEGER NOT NULL DEFAULT 0,
    contradict_count INTEGER NOT NULL DEFAULT 0,
    reason           TEXT NOT NULL,
    recorded_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_instinct_action_history_instinct ON instinct_action_history(instinct_id, recorded_at DESC);

-- ============================================================
-- cluster_nodes — fleet heartbeat registry (migration 95).
--
-- Each DB-having node (ui/worker/all profiles) upserts its own
-- row on startup and every ~15s. Deleted on graceful drain;
-- stale rows (last_seen older than 45s) are flagged by the
-- /api/v1/cluster endpoint.
--
-- Parity note: capabilities is JSONB on Postgres → BLOB here
-- (raw JSON bytes, parsed on read). last_seen is TEXT (RFC3339Nano).
-- ============================================================
CREATE TABLE IF NOT EXISTS cluster_nodes (
    instance_id  TEXT PRIMARY KEY,
    profile      TEXT NOT NULL,
    version      TEXT NOT NULL,
    address      TEXT NOT NULL DEFAULT '',
    capabilities BLOB NOT NULL DEFAULT '{}',
    last_seen    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cluster_nodes_last_seen ON cluster_nodes (last_seen);

CREATE TABLE IF NOT EXISTS instinct_applications (
    id           TEXT PRIMARY KEY,
    instinct_id  TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    task_id      TEXT NOT NULL DEFAULT '',
    surface      TEXT NOT NULL,
    result       TEXT NOT NULL,
    applied_at   TEXT NOT NULL,
    execution_id TEXT NOT NULL DEFAULT '',
    step_id      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_instinct_applications_instinct ON instinct_applications(instinct_id, applied_at DESC);
CREATE INDEX IF NOT EXISTS idx_instinct_applications_pending ON instinct_applications(execution_id, step_id) WHERE surface = 'lead_recovery' AND result IN ('ignored', 'auto_applied');

-- ============================================================
-- daemon_leader_locks — per-worker leader-election lease rows
-- (migration 96 parity).
--
-- SQLite is single-process so Acquire always succeeds, but we
-- persist the row so diagnostics (Get/List) and the epoch fence
-- token work the same as on Postgres. epoch is a monotonic
-- BIGINT that increments on takeover and is preserved on renew.
-- ============================================================
CREATE TABLE IF NOT EXISTS daemon_leader_locks (
    worker_id   TEXT PRIMARY KEY,
    holder_id   TEXT NOT NULL,
    acquired_at TEXT NOT NULL,
    renewed_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    epoch       INTEGER NOT NULL DEFAULT 0
);
`
