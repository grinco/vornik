// Package persistence provides database abstractions and repository implementations
// for the vornik daemon.
package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
)

// Migration represents a single database schema migration.
type Migration struct {
	// Version is the migration version number (e.g., 1, 2, 3).
	Version int

	// Name is a human-readable description of the migration.
	Name string

	// Up contains the SQL to apply the migration.
	Up string

	// Down contains the SQL to rollback the migration (optional).
	Down string
}

// MigrationRunner executes database migrations.
type MigrationRunner struct {
	db         *sql.DB
	migrations []Migration
	mu         sync.Mutex
}

// NewMigrationRunner creates a new migration runner.
func NewMigrationRunner(db *sql.DB) *MigrationRunner {
	return &MigrationRunner{
		db:         db,
		migrations: DefaultMigrations,
	}
}

// migrationLockKey is the bigint passed to pg_advisory_lock to
// serialise migration runs across daemon processes. Picked once
// (0x73776D64_6D696772 == "swmdmigr" in ASCII) and frozen —
// changing it would let an old process and a new process both
// run migrations against the same DB.
const migrationLockKey int64 = 0x73776D646D696772

// Run executes all pending migrations.
//
// Concurrency: an in-process mutex serialises callers within a
// single daemon, AND a Postgres advisory lock serialises across
// processes. Both are necessary — a rolling deploy that starts a
// new pod before the old one exits would otherwise double-apply
// migrations (both pods read version N, both try to apply N+1).
// The advisory lock is session-scoped so it auto-releases if the
// daemon crashes mid-migration.
func (r *MigrationRunner) Run(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Cross-process serialisation. pg_advisory_lock blocks until
	// the lock is available — when a rolling deploy lands two
	// pods simultaneously the second waits for the first to
	// finish before reading currentVersion. Released explicitly
	// on success path; on panic / process death the session
	// teardown releases it.
	if _, err := r.db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Best-effort release. If this fails the lock auto-releases
		// at session end so it's not a leak risk, just noise.
		_, _ = r.db.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	// Ensure migrations table exists
	if err := r.ensureMigrationsTable(ctx); err != nil {
		return fmt.Errorf("failed to ensure migrations table: %w", err)
	}

	if err := r.syncBootstrapSchema(ctx); err != nil {
		return fmt.Errorf("failed to sync bootstrap schema: %w", err)
	}

	// Determine which migrations are already recorded as applied. We use
	// the full applied SET, not MAX(version), so a migration that was
	// skipped earlier — a gap in the migrations table left by an inversion
	// in the DefaultMigrations slice, or by a migration added after the DB
	// had already migrated past its version — is re-applied idempotently
	// rather than silently left missing. The migrations' DDL is
	// IF NOT EXISTS / IF NOT EXISTS-guarded, so re-applying a partially- or
	// never-applied migration is safe.
	applied, err := r.getAppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("failed to get applied migration versions: %w", err)
	}

	// Apply in VERSION order (not slice order). DefaultMigrations is
	// maintained in roughly version order but has inversions (e.g. v26
	// before v25, v74 before v73); applying in slice order with a
	// "version <= currentMax" skip silently dropped the inverted
	// migrations. Sorting by version respects inter-migration dependencies
	// (a migration may FK or index a table created by an earlier one) and
	// ensures none are skipped on ordering grounds.
	for _, m := range pendingMigrations(applied, r.migrations) {
		if err := r.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("failed to apply migration %d (%s): %w", m.Version, m.Name, err)
		}
		applied[m.Version] = true
	}

	return nil
}

// syncBootstrapSchema records the initial migration when the schema was
// applied out-of-band via deployments/postgres/schema/001_initial.sql.
func (r *MigrationRunner) syncBootstrapSchema(ctx context.Context) error {
	if len(r.migrations) == 0 {
		return nil
	}

	const initialVersion = 1

	var alreadyApplied bool
	if err := r.db.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM migrations WHERE version = $1)",
		initialVersion,
	).Scan(&alreadyApplied); err != nil {
		return err
	}

	if alreadyApplied {
		return nil
	}

	const bootstrapSchemaQuery = `
		SELECT
			EXISTS (SELECT 1 FROM pg_type WHERE typname = 'task_status') AND
			EXISTS (SELECT 1 FROM pg_type WHERE typname = 'execution_status') AND
			EXISTS (SELECT 1 FROM pg_type WHERE typname = 'task_creation_source') AND
			EXISTS (SELECT 1 FROM pg_type WHERE typname = 'delegation_mode') AND
			EXISTS (SELECT 1 FROM pg_type WHERE typname = 'artifact_class') AND
			EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'tasks') AND
			EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'executions') AND
			EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'artifacts')
	`

	var bootstrapSchemaPresent bool
	if err := r.db.QueryRowContext(ctx, bootstrapSchemaQuery).Scan(&bootstrapSchemaPresent); err != nil {
		return err
	}

	if !bootstrapSchemaPresent {
		return nil
	}

	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO migrations (version, name)
		 VALUES ($1, $2)
		 ON CONFLICT (version) DO NOTHING`,
		initialVersion,
		r.migrations[0].Name,
	)
	return err
}

// ensureMigrationsTable creates the migrations tracking table if it doesn't exist.
func (r *MigrationRunner) ensureMigrationsTable(ctx context.Context) error {
	query := `
		CREATE TABLE IF NOT EXISTS migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
		)
	`
	_, err := r.db.ExecContext(ctx, query)
	return err
}

// getCurrentVersion returns the highest applied migration version.
func (r *MigrationRunner) getCurrentVersion(ctx context.Context) (int, error) {
	var version int
	err := r.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM migrations").Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// getAppliedVersions returns the set of migration versions recorded in the
// migrations table. Run uses this (rather than getCurrentVersion's MAX) to
// decide what to apply: a migration is applied iff its version is NOT in this
// set. The old "version > MAX(version)" check silently skipped any migration
// whose version was below the max but that had never actually been applied —
// a gap left by slice inversions (v26 before v25, v74 before v73) or by a
// migration added after the DB had already migrated past its version (the
// memory-hardening v23 tables going missing on a long-lived DB).
func (r *MigrationRunner) getAppliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT version FROM migrations")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// pendingMigrations returns the migrations to apply, in VERSION order, that
// are not already in the applied set. Sorting by version (rather than
// preserving slice order) respects inter-migration dependencies and defeats
// the inversions in DefaultMigrations; filtering by the applied set (rather
// than "version > MAX") re-applies any migration that was skipped earlier.
// Pure (no DB) so it can be unit-tested directly.
func pendingMigrations(applied map[int]bool, all []Migration) []Migration {
	sorted := make([]Migration, len(all))
	copy(sorted, all)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	out := make([]Migration, 0, len(sorted))
	for _, m := range sorted {
		if !applied[m.Version] {
			out = append(out, m)
		}
	}
	return out
}

// applyMigration executes a single migration.
func (r *MigrationRunner) applyMigration(ctx context.Context, m Migration) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Execute migration SQL
	if _, err := tx.ExecContext(ctx, m.Up); err != nil {
		return fmt.Errorf("migration SQL failed: %w", err)
	}

	// Record migration
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO migrations (version, name) VALUES ($1, $2)",
		m.Version, m.Name); err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	return tx.Commit()
}

// Rollback reverses the most recent migration.
func (r *MigrationRunner) Rollback(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	currentVersion, err := r.getCurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current version: %w", err)
	}

	if currentVersion == 0 {
		return fmt.Errorf("no migrations to rollback")
	}

	// Find the migration to rollback
	var targetMigration *Migration
	for i := range r.migrations {
		if r.migrations[i].Version == currentVersion {
			targetMigration = &r.migrations[i]
			break
		}
	}

	if targetMigration == nil {
		return fmt.Errorf("migration version %d not found", currentVersion)
	}

	if targetMigration.Down == "" {
		return fmt.Errorf("migration %d has no down migration", currentVersion)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Execute down migration
	if _, err := tx.ExecContext(ctx, targetMigration.Down); err != nil {
		return fmt.Errorf("down migration failed: %w", err)
	}

	// Remove migration record
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM migrations WHERE version = $1",
		currentVersion); err != nil {
		return fmt.Errorf("failed to remove migration record: %w", err)
	}

	return tx.Commit()
}

// Version returns the current schema version.
func (r *MigrationRunner) Version(ctx context.Context) (int, error) {
	return r.getCurrentVersion(ctx)
}

// Status returns information about applied and pending migrations.
type MigrationStatus struct {
	CurrentVersion int
	Applied        []MigrationInfo
	Pending        []MigrationInfo
}

// MigrationInfo contains details about a migration.
type MigrationInfo struct {
	Version   int
	Name      string
	AppliedAt *string // nil if not applied
}

// Status returns the current migration status.
func (r *MigrationRunner) Status(ctx context.Context) (*MigrationStatus, error) {
	if err := r.ensureMigrationsTable(ctx); err != nil {
		return nil, err
	}
	if err := r.syncBootstrapSchema(ctx); err != nil {
		return nil, err
	}

	appliedVersions := make(map[int]string)
	rows, err := r.db.QueryContext(ctx,
		"SELECT version, name FROM migrations ORDER BY version")
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return nil, err
		}
		appliedVersions[version] = name
	}

	status := &MigrationStatus{
		CurrentVersion: 0,
	}

	for _, m := range r.migrations {
		info := MigrationInfo{
			Version: m.Version,
			Name:    m.Name,
		}

		if name, ok := appliedVersions[m.Version]; ok {
			info.AppliedAt = &name
			status.Applied = append(status.Applied, info)
			if m.Version > status.CurrentVersion {
				status.CurrentVersion = m.Version
			}
		} else {
			status.Pending = append(status.Pending, info)
		}
	}

	return status, nil
}

// DefaultMigrations contains the standard migration set for vornik.
// These are PostgreSQL-focused migrations that align with the schema in
// deployments/postgres/schema/001_initial.sql.
var DefaultMigrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		Up: `
-- Enumerated types
CREATE TYPE task_status AS ENUM (
    'PENDING', 'QUEUED', 'LEASED', 'RUNNING',
    'WAITING_FOR_CHILDREN', 'COMPLETED', 'FAILED', 'CANCELLED'
);

CREATE TYPE execution_status AS ENUM (
    'PENDING', 'RUNNING', 'COMPLETED', 'FAILED', 'CANCELLED'
);

CREATE TYPE task_creation_source AS ENUM (
    'USER', 'DELEGATION', 'AUTONOMOUS'
);

CREATE TYPE delegation_mode AS ENUM (
    'SEQUENTIAL', 'PARALLEL', 'FAN_OUT'
);

CREATE TYPE artifact_class AS ENUM (
    'OUTPUT', 'INTERMEDIATE', 'SNAPSHOT', 'LOG', 'METADATA'
);

-- Tasks table
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    workflow_id TEXT,
    idempotency_key TEXT,
    parent_task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    creation_source task_creation_source NOT NULL DEFAULT 'USER',
    delegation_mode delegation_mode,
    status task_status NOT NULL DEFAULT 'QUEUED',
    priority INTEGER NOT NULL DEFAULT 50,
    payload JSONB,
    dependencies TEXT[] DEFAULT '{}',
    lease_id TEXT,
    leased_at TIMESTAMP WITH TIME ZONE,
    leased_by TEXT,
    lease_expires_at TIMESTAMP WITH TIME ZONE,
    attempt INTEGER NOT NULL DEFAULT 1,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    last_error TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_priority CHECK (priority >= 0 AND priority <= 100),
    CONSTRAINT valid_attempt CHECK (attempt >= 1 AND attempt <= max_attempts)
);

-- Executions table
CREATE TABLE executions (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL,
    workflow_revision TEXT NOT NULL,
    status execution_status NOT NULL DEFAULT 'PENDING',
    current_step_id TEXT,
    completed_steps TEXT[] DEFAULT '{}',
    state_snapshot JSONB,
    result JSONB,
    error_message TEXT,
    error_code TEXT,
    started_at TIMESTAMP WITH TIME ZONE,
    completed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Artifacts table
CREATE TABLE artifacts (
    id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    execution_id TEXT REFERENCES executions(id) ON DELETE CASCADE,
    task_id TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    name TEXT NOT NULL,
    artifact_class artifact_class NOT NULL,
    storage_path TEXT NOT NULL,
    size_bytes BIGINT,
    content_hash_sha256 TEXT,
    mime_type TEXT,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    CONSTRAINT valid_size CHECK (size_bytes IS NULL OR size_bytes >= 0)
);

-- Indexes for tasks
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority ASC, created_at ASC)
    WHERE status = 'QUEUED';
CREATE INDEX idx_tasks_project ON tasks (project_id, created_at DESC);
CREATE UNIQUE INDEX idx_tasks_project_idempotency_key ON tasks (project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_tasks_status ON tasks (status);
CREATE INDEX idx_tasks_lease_expired ON tasks (lease_expires_at)
    WHERE status IN ('LEASED', 'RUNNING') AND lease_expires_at IS NOT NULL;
CREATE INDEX idx_tasks_parent ON tasks (parent_task_id)
    WHERE parent_task_id IS NOT NULL;
CREATE INDEX idx_tasks_workflow ON tasks (workflow_id)
    WHERE workflow_id IS NOT NULL;
CREATE INDEX idx_tasks_dependencies ON tasks USING GIN (dependencies)
    WHERE array_length(dependencies, 1) > 0;

-- Indexes for executions
CREATE INDEX idx_executions_task ON executions (task_id);
CREATE INDEX idx_executions_project ON executions (project_id, created_at DESC);
CREATE INDEX idx_executions_status ON executions (status);
CREATE INDEX idx_executions_workflow ON executions (workflow_id, workflow_revision);
CREATE INDEX idx_executions_running ON executions (project_id)
    WHERE status = 'RUNNING';

-- Indexes for artifacts
CREATE INDEX idx_artifacts_execution ON artifacts (execution_id);
CREATE INDEX idx_artifacts_project ON artifacts (project_id, created_at DESC);
CREATE INDEX idx_artifacts_task ON artifacts (task_id)
    WHERE task_id IS NOT NULL;
CREATE INDEX idx_artifacts_hash ON artifacts (content_hash_sha256)
    WHERE content_hash_sha256 IS NOT NULL;

-- Updated_at trigger function
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

-- Views
CREATE VIEW active_tasks AS
SELECT
    id, project_id, workflow_id, status, priority,
    created_at, updated_at, attempt, max_attempts
FROM tasks
WHERE status IN ('PENDING', 'QUEUED', 'LEASED', 'RUNNING', 'WAITING_FOR_CHILDREN');

CREATE VIEW failed_tasks AS
SELECT
    t.id, t.project_id, t.status, t.attempt, t.max_attempts,
    t.last_error, t.created_at, t.updated_at,
    e.id AS execution_id, e.error_message, e.error_code
FROM tasks t
LEFT JOIN executions e ON e.task_id = t.id
WHERE t.status = 'FAILED';
`,
		Down: `
DROP VIEW IF EXISTS failed_tasks;
DROP VIEW IF EXISTS active_tasks;
DROP TRIGGER IF EXISTS update_executions_updated_at ON executions;
DROP TRIGGER IF EXISTS update_tasks_updated_at ON tasks;
DROP FUNCTION IF EXISTS update_updated_at_column();
DROP INDEX IF EXISTS idx_artifacts_hash;
DROP INDEX IF EXISTS idx_artifacts_task;
DROP INDEX IF EXISTS idx_artifacts_project;
DROP INDEX IF EXISTS idx_artifacts_execution;
DROP INDEX IF EXISTS idx_executions_running;
DROP INDEX IF EXISTS idx_executions_workflow;
DROP INDEX IF EXISTS idx_executions_status;
DROP INDEX IF EXISTS idx_executions_project;
DROP INDEX IF EXISTS idx_executions_task;
DROP INDEX IF EXISTS idx_tasks_dependencies;
DROP INDEX IF EXISTS idx_tasks_workflow;
DROP INDEX IF EXISTS idx_tasks_parent;
DROP INDEX IF EXISTS idx_tasks_lease_expired;
DROP INDEX IF EXISTS idx_tasks_status;
DROP INDEX IF EXISTS idx_tasks_project;
DROP INDEX IF EXISTS idx_tasks_queue_lookup;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS executions;
DROP TABLE IF EXISTS tasks;
DROP TYPE IF EXISTS artifact_class;
DROP TYPE IF EXISTS delegation_mode;
DROP TYPE IF EXISTS task_creation_source;
DROP TYPE IF EXISTS execution_status;
DROP TYPE IF EXISTS task_status;
`,
	},
	{
		Version: 2,
		Name:    "add_paused_execution_status",
		Up: `
-- Add PAUSED to the execution_status enum so that approval workflow
-- steps can persist their paused state in the database.
ALTER TYPE execution_status ADD VALUE IF NOT EXISTS 'PAUSED' AFTER 'RUNNING';
`,
		Down: `
-- PostgreSQL does not support removing enum values.
-- PAUSED will remain in the enum after rollback; it is harmless.
`,
	},
	{
		Version: 3,
		Name:    "create_tool_audit_log",
		Up: `
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

CREATE INDEX IF NOT EXISTS idx_tool_audit_log_project ON tool_audit_log(project_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_task ON tool_audit_log(task_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_execution ON tool_audit_log(execution_id);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_tool_name ON tool_audit_log(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_audit_log_created_at ON tool_audit_log(created_at DESC);
`,
		Down: `
DROP TABLE IF EXISTS tool_audit_log;
`,
	},
	{
		Version: 4,
		Name:    "create_task_watchers",
		Up: `
CREATE TABLE IF NOT EXISTS task_watchers (
    task_id TEXT NOT NULL,
    chat_id BIGINT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (task_id, chat_id)
);

CREATE INDEX IF NOT EXISTS idx_task_watchers_task ON task_watchers(task_id);
`,
		Down: `
DROP TABLE IF EXISTS task_watchers;
`,
	},
	{
		Version: 5,
		Name:    "add_task_idempotency_keys",
		Up: `
ALTER TABLE tasks
ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_project_idempotency_key
ON tasks(project_id, idempotency_key)
WHERE idempotency_key IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_project_idempotency_key;
ALTER TABLE tasks DROP COLUMN IF EXISTS idempotency_key;
`,
	},
	{
		Version: 6,
		Name:    "reindex_queue_lookup_by_updated_at",
		Up: `
-- Replace the queue lookup index so re-queued tasks (with bumped updated_at)
-- sort behind untried tasks at the same priority, improving throughput by
-- round-robining across tasks instead of exhausting retries on one task first.
DROP INDEX IF EXISTS idx_tasks_queue_lookup;
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority ASC, updated_at ASC)
    WHERE status = 'QUEUED';
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_queue_lookup;
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority ASC, created_at ASC)
    WHERE status = 'QUEUED';
`,
	},
	{
		Version: 7,
		Name:    "create_project_memory",
		Up: `
-- Enable pgvector if available (best-effort; no error if extension missing).
DO $$ BEGIN
    CREATE EXTENSION IF NOT EXISTS vector;
EXCEPTION WHEN OTHERS THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS project_memory_chunks (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    task_id      TEXT,
    artifact_id  TEXT,
    source_name  TEXT NOT NULL,
    chunk_index  INT  NOT NULL DEFAULT 0,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    tsv          TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_memory_project     ON project_memory_chunks (project_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_hash ON project_memory_chunks (project_id, content_hash);
CREATE INDEX IF NOT EXISTS idx_memory_tsv         ON project_memory_chunks USING GIN (tsv);

-- Add vector column + HNSW index only when pgvector is available.
-- Dim is 1024 to match the default local embedder (bge-m3 via Ollama).
-- Operators using a different-dim model must drop+re-add this column
-- at the native dim BEFORE any embeddings are inserted.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        IF NOT EXISTS (
            SELECT 1 FROM information_schema.columns
            WHERE table_name = 'project_memory_chunks' AND column_name = 'embedding'
        ) THEN
            ALTER TABLE project_memory_chunks ADD COLUMN embedding vector(1024);
            CREATE INDEX IF NOT EXISTS idx_memory_embedding ON project_memory_chunks
                USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
        END IF;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS memory_embed_queue (
    chunk_id    TEXT PRIMARY KEY REFERENCES project_memory_chunks(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL,
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_embed_queue_project ON memory_embed_queue (project_id, enqueued_at);
`,
		Down: `
DROP TABLE IF EXISTS memory_embed_queue;
DROP TABLE IF EXISTS project_memory_chunks;
`,
	},
	{
		Version: 8,
		Name:    "create_task_llm_usage",
		Up: `
CREATE TABLE IF NOT EXISTS task_llm_usage (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    task_id           TEXT NOT NULL,
    execution_id      TEXT NOT NULL,
    step_id           TEXT NOT NULL DEFAULT '',
    role              TEXT NOT NULL,
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    iterations        INT    NOT NULL DEFAULT 0,
    cost_usd          DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

-- Spend panel queries: "cost by project over last 24h / 7d / 30d" hits
-- (project_id, recorded_at). Per-task drill-down hits (task_id).
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_project_time
    ON task_llm_usage (project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_task
    ON task_llm_usage (task_id);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_execution
    ON task_llm_usage (execution_id);
-- Model-effectiveness aggregates read by (role, model) across projects.
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_role_model
    ON task_llm_usage (role, model);
`,
		Down: `
DROP TABLE IF EXISTS task_llm_usage;
`,
	},
	{
		Version: 9,
		Name:    "extend_task_llm_usage_for_dispatcher",
		// Dispatcher LLM calls aren't tied to a task/execution — they're
		// per-turn tool loops driven by chat/CLI sessions. Relax the
		// task_id/execution_id NOT NULL constraints and add source +
		// session_id so dispatcher rows can live in the same table and
		// aggregate cleanly for the dashboard and budget paths.
		Up: `
ALTER TABLE task_llm_usage ALTER COLUMN task_id DROP NOT NULL;
ALTER TABLE task_llm_usage ALTER COLUMN execution_id DROP NOT NULL;
ALTER TABLE task_llm_usage ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'workflow_step';
ALTER TABLE task_llm_usage ADD COLUMN IF NOT EXISTS session_id TEXT;

CREATE INDEX IF NOT EXISTS idx_task_llm_usage_source_time
    ON task_llm_usage (source, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_llm_usage_session
    ON task_llm_usage (session_id) WHERE session_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_task_llm_usage_session;
DROP INDEX IF EXISTS idx_task_llm_usage_source_time;
ALTER TABLE task_llm_usage DROP COLUMN IF EXISTS session_id;
ALTER TABLE task_llm_usage DROP COLUMN IF EXISTS source;
-- Note: we do not re-enable NOT NULL on task_id/execution_id here; any
-- dispatcher rows written since the upgrade would prevent that.
`,
	},
	{
		Version: 10,
		Name:    "create_execution_step_outcomes",
		// Richer per-step outcome taxonomy. Distinct from task_llm_usage:
		// that table is about cost and tokens; this one is about whether
		// the output a step produced was actually *usable* by the next
		// step. A step writes 'pending_validation' on completion; the
		// consumer finalizes it to 'ok' on successful consumption, or to
		// 'parse_error'/'schema_violation'/'refused' when the output
		// can't be used. This lets the dashboard compute
		// cost-per-usable-output per (role, model) rather than just
		// cost-per-LLM-roundtrip — the latter counts unparseable output
		// as "success", which is the issue this table exists to fix.
		Up: `
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
    recorded_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE execution_step_outcomes IS 'Per-step outcome taxonomy: ok / pending_validation / parse_error / schema_violation / refused / iteration_exhausted / degenerate_loop / downstream_rejected / gate_failed / timeout / cancelled / failed. Attribution to the producing step lets model-effectiveness metrics reflect output usability, not just LLM round-trip success.';

CREATE INDEX IF NOT EXISTS idx_step_outcomes_execution
    ON execution_step_outcomes (execution_id, recorded_at);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_project_time
    ON execution_step_outcomes (project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_role_model
    ON execution_step_outcomes (role, model);
CREATE INDEX IF NOT EXISTS idx_step_outcomes_pending
    ON execution_step_outcomes (execution_id, step_id) WHERE outcome = 'pending_validation';
`,
		Down: `
DROP TABLE IF EXISTS execution_step_outcomes;
`,
	},
	{
		Version: 11,
		Name:    "create_webhook_events",
		Up: `
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
`,
		Down: `
DROP TABLE IF EXISTS webhook_events;
`,
	},
	{
		Version: 12,
		Name:    "queue_priority_low_number_first",
		Up: `
-- Task priority semantics match project defaultPriority and chat backend
-- scheduling: lower numeric values are more urgent.
DROP INDEX IF EXISTS idx_tasks_queue_lookup;
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority ASC, updated_at ASC)
    WHERE status = 'QUEUED';

COMMENT ON COLUMN tasks.priority IS 'Scheduling priority (0-100, lower = more urgent)';
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_queue_lookup;
CREATE INDEX idx_tasks_queue_lookup ON tasks (priority DESC, updated_at ASC)
    WHERE status = 'QUEUED';

COMMENT ON COLUMN tasks.priority IS 'Scheduling priority (0-100, higher = more urgent)';
`,
	},
	{
		Version: 13,
		Name:    "create_autonomy_evaluations",
		// Durable audit trail for autonomy evaluations. One row per tick —
		// CREATED, NO_ACTION, RATE_LIMITED, PARSE_ERROR, WORKFLOW_INVALID,
		// TYPE_REJECTED, CIRCUIT_OPEN, DUPLICATE, COOLDOWN, LLM_ERROR,
		// BUDGET_BLOCKED, ACTIVE_TASKS, IDEMPOTENCY_HIT, DB_ERROR.
		//
		// Why this matters: before this table, autonomy rejections were
		// only visible as warn-level log lines; tool_audit_log only
		// recorded successful task creations. An operator debugging
		// "why isn't autonomy scheduling anything" had to SSH and grep.
		// Now the reason is one SELECT away.
		Up: `
CREATE TABLE IF NOT EXISTS autonomy_evaluations (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    outcome      TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    task_id      TEXT REFERENCES tasks(id) ON DELETE SET NULL,
    task_type    TEXT NOT NULL DEFAULT '',
    workflow_id  TEXT NOT NULL DEFAULT '',
    prompt_hash  TEXT NOT NULL DEFAULT '',
    duration_ms  BIGINT NOT NULL DEFAULT 0,
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE autonomy_evaluations IS 'Durable audit trail: one row per autonomy tick, outcome + reason.';

CREATE INDEX IF NOT EXISTS idx_autonomy_eval_project_time
    ON autonomy_evaluations (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_autonomy_eval_outcome_time
    ON autonomy_evaluations (project_id, outcome, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_autonomy_eval_task
    ON autonomy_evaluations (task_id) WHERE task_id IS NOT NULL;
`,
		Down: `
DROP TABLE IF EXISTS autonomy_evaluations;
`,
	},
	{
		Version: 14,
		Name:    "add_task_last_error_class",
		// Typed failure classification for tasks. Pairs with the
		// freeform last_error column — that remains the human-readable
		// detail, and last_error_class is the enum-like tag operators
		// filter/group by. Plain TEXT rather than a DB enum so new
		// classes can land with just a classifier change.
		Up: `
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_error_class TEXT;

COMMENT ON COLUMN tasks.last_error_class IS
  'Typed failure class (LLM_ERROR, TIMEOUT, TOOL_ERROR, INVALID_OUTPUT, MERGE_FAILED, GATE_FAILED, BUDGET_BLOCKED, RATE_LIMITED, WORKFLOW_ROLE_MISSING, WORKFLOW_CONFIG_ERROR, ORPHANED, CANCELLED, RUNTIME_ERROR, LEASE_EXPIRED, UNKNOWN). NULL for tasks that never failed.';

CREATE INDEX IF NOT EXISTS idx_tasks_last_error_class
    ON tasks (project_id, last_error_class)
    WHERE last_error_class IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_last_error_class;
ALTER TABLE tasks DROP COLUMN IF EXISTS last_error_class;
`,
	},
	{
		Version: 15,
		Name:    "create_memory_embed_dlq",
		// Dead-letter queue for memory chunks that failed to embed.
		// Previously the worker logged + dropped failures on the floor:
		// a 10-minute embed endpoint outage silently turned into a
		// permanent RAG index gap. Now failing chunks move here with a
		// retry_after backoff so an operator can replay or inspect.
		//
		// retry_count doubles the backoff on each failure
		// (10min, 20min, 40min…) capped at 24h by the worker. A row
		// with retry_count = -1 is "parked" — never auto-retried,
		// operator must explicitly replay. Used for permanent classes
		// like dimension mismatch where the model/config needs to be
		// fixed first.
		Up: `
CREATE TABLE IF NOT EXISTS memory_embed_dlq (
    chunk_id        TEXT PRIMARY KEY REFERENCES project_memory_chunks(id) ON DELETE CASCADE,
    project_id      TEXT NOT NULL,
    reason          TEXT NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    retry_count     INT  NOT NULL DEFAULT 0,
    retry_after     TIMESTAMPTZ NOT NULL DEFAULT now(),
    first_failed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_failed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE memory_embed_dlq IS 'Chunks the embed worker could not store, with retry_after backoff. Operators list via vornikctl memory dlq; replay via vornikctl memory dlq replay.';

CREATE INDEX IF NOT EXISTS idx_memory_dlq_project      ON memory_embed_dlq (project_id, last_failed_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_dlq_retry_after  ON memory_embed_dlq (retry_after) WHERE retry_count >= 0;
`,
		Down: `
DROP TABLE IF EXISTS memory_embed_dlq;
`,
	},
	{
		Version: 16,
		Name:    "artifact_class_input",
		// Add INPUT to the artifact_class enum so user-supplied
		// inputs (Telegram uploads, API task attachments) can be
		// persisted with the right classification. Previously the
		// enum only covered agent-produced artifacts (OUTPUT,
		// INTERMEDIATE, SNAPSHOT, LOG, METADATA), which left
		// uploads sitting in the project workspace with no DB
		// record — retries lost the file when /tmp got reaped or
		// the workspace dir was nuked.
		//
		// PG 12+ allows ALTER TYPE ... ADD VALUE inside a
		// transaction; we run on PG 16 so this is safe inside the
		// migration runner's tx wrapper. IF NOT EXISTS makes the
		// migration idempotent for re-runs after partial failure.
		Up: `
ALTER TYPE artifact_class ADD VALUE IF NOT EXISTS 'INPUT';
`,
		// No-op down: removing an enum value would orphan rows
		// referring to it. Operators rolling back must first
		// purge or re-classify the affected artifacts manually.
		Down: ``,
	},
	{
		Version: 17,
		Name:    "execution_step_outcomes_hallucination_signals",
		// Phase 1 of the hallucination-detection feature: a JSONB
		// column on the per-step outcome row carries the
		// detector's findings. Stored as JSONB (not a side table)
		// because the signals are always read alongside the
		// outcome row — joining adds latency on hot paths
		// (failed-task UI render, retry decisions) for no
		// queryability win, since signals are inspected per-row
		// not aggregated.
		//
		// IF NOT EXISTS makes the migration idempotent for
		// re-runs after partial failure.
		Up: `
ALTER TABLE execution_step_outcomes
    ADD COLUMN IF NOT EXISTS hallucination_signals JSONB;
COMMENT ON COLUMN execution_step_outcomes.hallucination_signals IS 'Phase 1 claim-grounding detector findings; nil/empty when no signals or detector not run.';
`,
		Down: `
ALTER TABLE execution_step_outcomes
    DROP COLUMN IF EXISTS hallucination_signals;
`,
	},
	{
		Version: 18,
		Name:    "create_task_judge_verdicts",
		// Phase 3: LLM-as-judge verdict per completed task. One
		// row per task. Stored as a side table (not on tasks)
		// because the verdict is async — it lands AFTER the task
		// row reaches its terminal status, and we don't want to
		// bloat the hot tasks table with a JSONB blob that's only
		// read on detail pages.
		//
		// Model + prompt are denormalised so an audit can answer
		// "which model judged this task?" without joining against
		// a separate config table that may have changed since.
		Up: `
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
COMMENT ON TABLE task_judge_verdicts IS 'Phase 3 LLM-as-judge verdicts: one row per task evaluated.';
COMMENT ON COLUMN task_judge_verdicts.verdict IS 'pass | fail | abstain';
COMMENT ON COLUMN task_judge_verdicts.confidence IS '0.0–1.0 model self-reported confidence';
`,
		Down: `
DROP TABLE IF EXISTS task_judge_verdicts;
`,
	},
	{
		Version: 19,
		Name:    "create_trading_tables",
		// Trading state. Three tables:
		//   trading_orders — every order request/response. The
		//     UNIQUE(project_id, idempotency_key) is the core
		//     safety invariant: a retried place_order with the
		//     same key resolves to the existing row, never a
		//     duplicate IBKR submit.
		//   trading_fills — fill events. Multiple fills per order
		//     possible (partial fills); commission per-fill.
		//   trading_positions_snapshots — sampled state for the
		//     drawdown circuit breaker + UI history. Snapshot
		//     rather than current-state because positions belong
		//     to the broker; this table is for vornik's internal
		//     drawdown tracking + audit.
		//
		// NUMERIC(18,6) for prices/qty: 12 integer digits is enough
		// for any equity price (stocks rarely cross $10k); 6
		// decimals covers fractional-share buys IBKR offers. NUMERIC
		// rather than DOUBLE PRECISION because monetary math
		// shouldn't lose pennies to FP drift.
		Up: `
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
COMMENT ON TABLE trading_orders IS 'Per-order audit + idempotency record. UNIQUE(project_id, idempotency_key) prevents duplicate submits on agent retry.';
COMMENT ON COLUMN trading_orders.mode IS 'paper | live';
COMMENT ON COLUMN trading_orders.status IS 'submitted | filled | partial | cancelled | rejected';

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
COMMENT ON TABLE trading_positions_snapshots IS 'Sampled account state for drawdown breaker + UI history.';
`,
		Down: `
DROP TABLE IF EXISTS trading_positions_snapshots;
DROP TABLE IF EXISTS trading_fills;
DROP TABLE IF EXISTS trading_orders;
`,
	},
	{
		Version: 20,
		Name:    "create_task_post_mortems",
		// One LLM-summarised explainer per task. Triggered on
		// the failed-task UI when the operator clicks "Explain
		// this failure"; the LLM joins step outcomes + tool
		// audit + last lines of container logs into a one-
		// paragraph operator-friendly summary. PRIMARY KEY on
		// task_id so a re-trigger on the same task short-
		// circuits to the cached row instead of burning another
		// LLM call.
		//
		// cost_usd / model / token columns mirror task_llm_usage
		// so the post-mortem cost lands on the same spend
		// dashboard as the rest of the daemon's LLM traffic
		// (fed via task_llm_usage with source='post_mortem').
		Up: `
CREATE TABLE IF NOT EXISTS task_post_mortems (
    task_id           TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    project_id        TEXT NOT NULL,
    summary           TEXT NOT NULL,
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(18,6) NOT NULL DEFAULT 0,
    recorded_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_post_mortems_project_time
    ON task_post_mortems(project_id, recorded_at DESC);
COMMENT ON TABLE task_post_mortems IS 'LLM-generated failure explainers, one per task. Cached so the UI button is idempotent.';
`,
		Down: `
DROP TABLE IF EXISTS task_post_mortems;
`,
	},
	{
		Version: 21,
		Name:    "create_trading_safety_events",
		// Broker-side decisions worth recording independently —
		// kill-switch toggles, drawdown breaker trips, every cap
		// refusal, idempotency replay hits. Two operator-facing
		// goals:
		//   1. Cross-component audit trail. When an order didn't
		//      land as expected, the operator compares the
		//      agent's view (tool_audit_log), the broker's view
		//      (trading_orders + this table), and the actual fills
		//      (trading_fills, future).
		//   2. Foundation for quorum-based guardrails. Each
		//      component records its decision independently;
		//      downstream logic can require agreement before
		//      treating a position as "real".
		//
		// Schema lands in Phase 1 of the broker→daemon audit
		// channel work, even though writes don't start until
		// Phase 2 — avoids a second migration.
		//
		// `kind` is a free-form string rather than an enum so new
		// event categories don't require a schema change. The
		// curated set today: kill_switch_on, kill_switch_off,
		// breaker_trip, cap_refused, replay_hit. New kinds get
		// added by code; the index handles arbitrary new strings.
		Up: `
CREATE TABLE IF NOT EXISTS trading_safety_events (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL,
    recorded_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    kind        TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'info',
    symbol      TEXT,
    detail      JSONB
);
CREATE INDEX IF NOT EXISTS idx_trading_safety_events_project_time
    ON trading_safety_events(project_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS idx_trading_safety_events_kind
    ON trading_safety_events(project_id, kind, recorded_at DESC);
COMMENT ON TABLE trading_safety_events IS 'Broker-side safety decisions: kill-switch toggles, breaker trips, cap refusals. Foundation for cross-component audit + quorum guardrails.';
COMMENT ON COLUMN trading_safety_events.kind IS 'kill_switch_on | kill_switch_off | breaker_trip | cap_refused | replay_hit (open vocabulary)';
COMMENT ON COLUMN trading_safety_events.severity IS 'info | warn | critical';
`,
		Down: `
DROP TABLE IF EXISTS trading_safety_events;
`,
	},
	{
		Version: 22,
		Name:    "tool_audit_duration_ms_to_bigint",
		// 2026-05-07: ibkr-trader exec_20260507180631 surfaced
		// repeated `pq: value "1600352997448" is out of range for
		// type integer` errors on tool_audit_log inserts. The
		// agent harness's ms_now() correctly returns 13-digit
		// millisecond timestamps, but coreutils-rust + a stale
		// tc_start_ms can collide and produce values that exceed
		// INT max (2.1e9). The duration_ms field has always been
		// conceptually a millisecond count (int64-domain in the
		// Go struct; persistence/models.go ToolAuditEntry uses
		// `int`), so widening the column to BIGINT is the only
		// correct cross-platform fix — Postgres INTEGER is i32
		// regardless of host word size, but Go's `int` is i64 on
		// 64-bit which means the model's value space already
		// exceeded the column's. Daemon-side clamp lands in the
		// same release to keep meaningless values from polluting
		// the audit (see executor/artifacts.go).
		Up: `
ALTER TABLE tool_audit_log ALTER COLUMN duration_ms TYPE BIGINT;
`,
		Down: `
ALTER TABLE tool_audit_log ALTER COLUMN duration_ms TYPE INTEGER;
`,
	},
	{
		Version: 23,
		Name:    "memory_hardening_phase0_provenance_epochs_quarantine",
		// Phase 0 of the memory-hardening roadmap (see
		// https://docs.vornik.io §11
		// + rag-retrieval-and-graph-design.md §8). Additive only —
		// this migration MUST NOT change any existing query result.
		// Behaviour change is gated by later phases that begin
		// reading the new columns / tables.
		//
		// What lands here:
		//   1. Provenance + classification columns on
		//      project_memory_chunks (lifecycle_state, epoch_id,
		//      producer_role, validator_role, ingest_execution_id,
		//      content_class, validation_status, confidence,
		//      supersedes_id, expires_at, embedding_version_id,
		//      needs_graph_extraction).
		//   2. Snapshot tables: corpus_epochs (manifest),
		//      corpus_epochs_active (visibility pointer),
		//      corpus_rollbacks (audit), corpus_embedding_versions_active
		//      (per-project active embedder pointer for the future
		//      model-swap path).
		//   3. Ingest queue: project_ingest_queue.
		//   4. Quarantine: project_memory_quarantine.
		//   5. Backfill: every existing chunk gets
		//      lifecycle_state='published', validation_status='legacy',
		//      content_class='unclassified'. epoch_id stays NULL —
		//      no synthetic epoch is fabricated for legacy data;
		//      the first real ingest creates the project's first
		//      epoch row.
		//
		// Defaults are picked so today's read paths return identical
		// results: the memory_search query in
		// internal/memory/repository.go doesn't reference any new
		// column, and the new tables are populated by phases ≥1.
		Up: `
-- 1. Additive columns on project_memory_chunks. Each defaults to a
--    value that preserves today's behaviour (every existing chunk
--    is treated as a published, legacy, unclassified row from an
--    unknown embedding version).
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS lifecycle_state         TEXT NOT NULL DEFAULT 'published',
    ADD COLUMN IF NOT EXISTS epoch_id                TEXT,
    ADD COLUMN IF NOT EXISTS producer_role           TEXT,
    ADD COLUMN IF NOT EXISTS validator_role          TEXT,
    ADD COLUMN IF NOT EXISTS ingest_execution_id     TEXT,
    ADD COLUMN IF NOT EXISTS content_class           TEXT NOT NULL DEFAULT 'unclassified',
    ADD COLUMN IF NOT EXISTS validation_status       TEXT NOT NULL DEFAULT 'unverified',
    ADD COLUMN IF NOT EXISTS confidence              REAL NOT NULL DEFAULT 0.5,
    ADD COLUMN IF NOT EXISTS supersedes_id           TEXT,
    ADD COLUMN IF NOT EXISTS expires_at              TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS embedding_version_id    TEXT,
    ADD COLUMN IF NOT EXISTS needs_graph_extraction  BOOLEAN NOT NULL DEFAULT FALSE;

-- CHECK constraints added separately so ALTER TABLE … ADD COLUMN …
-- DEFAULT … picks up the default before the constraint applies. We
-- guard with NOT VALID + VALIDATE so a backfill error doesn't abort
-- the migration on a long-lived corpus.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'memory_chunks_lifecycle_state_check'
    ) THEN
        ALTER TABLE project_memory_chunks
            ADD CONSTRAINT memory_chunks_lifecycle_state_check
            CHECK (lifecycle_state IN ('raw','staged','validated','published','quarantined','superseded'))
            NOT VALID;
        ALTER TABLE project_memory_chunks
            VALIDATE CONSTRAINT memory_chunks_lifecycle_state_check;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'memory_chunks_validation_status_check'
    ) THEN
        ALTER TABLE project_memory_chunks
            ADD CONSTRAINT memory_chunks_validation_status_check
            CHECK (validation_status IN ('unverified','verified','superseded','refuted','legacy'))
            NOT VALID;
        ALTER TABLE project_memory_chunks
            VALIDATE CONSTRAINT memory_chunks_validation_status_check;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'memory_chunks_confidence_range_check'
    ) THEN
        ALTER TABLE project_memory_chunks
            ADD CONSTRAINT memory_chunks_confidence_range_check
            CHECK (confidence >= 0 AND confidence <= 1)
            NOT VALID;
        ALTER TABLE project_memory_chunks
            VALIDATE CONSTRAINT memory_chunks_confidence_range_check;
    END IF;
END$$;

-- Indexes for the new columns. Created with IF NOT EXISTS so a
-- partially-applied migration can be re-run.
CREATE INDEX IF NOT EXISTS idx_memory_chunks_lifecycle
    ON project_memory_chunks (project_id, lifecycle_state);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_epoch
    ON project_memory_chunks (epoch_id) WHERE epoch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_memory_chunks_class
    ON project_memory_chunks (project_id, content_class);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_validation
    ON project_memory_chunks (project_id, validation_status);
CREATE INDEX IF NOT EXISTS idx_memory_chunks_needs_graph
    ON project_memory_chunks (project_id) WHERE needs_graph_extraction = TRUE;

-- 2. Snapshot tables.
--
-- corpus_epochs is the manifest: one row per ingest run that
-- admitted ≥1 chunk. The chunk's epoch_id FK lands here.
CREATE TABLE IF NOT EXISTS corpus_epochs (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    ingest_execution_id   TEXT REFERENCES executions(id) ON DELETE SET NULL,
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    closed_at             TIMESTAMP WITH TIME ZONE,
    chunks_admitted       INT NOT NULL DEFAULT 0,
    chunks_quarantined    INT NOT NULL DEFAULT 0,
    chunks_verified       INT NOT NULL DEFAULT 0,
    chunks_refuted        INT NOT NULL DEFAULT 0,
    chunks_superseded     INT NOT NULL DEFAULT 0,
    notes                 TEXT
);
CREATE INDEX IF NOT EXISTS idx_corpus_epochs_project_created
    ON corpus_epochs (project_id, created_at DESC);

COMMENT ON TABLE corpus_epochs IS 'Manifest of chunks admitted by one ingest run (Iceberg-style snapshot). Phase 0: schema only; first row written by Phase 2 ingest pipeline.';

-- corpus_epochs_active is the per-project visibility pointer.
-- Multi-row state means a rollback is in progress or operator has
-- pinned a window. Default search WHERE epoch_id IN (SELECT … FROM
-- corpus_epochs_active WHERE project_id = $1).
CREATE TABLE IF NOT EXISTS corpus_epochs_active (
    project_id      TEXT NOT NULL,
    epoch_id        TEXT NOT NULL REFERENCES corpus_epochs(id) ON DELETE CASCADE,
    activated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    activated_by    TEXT NOT NULL DEFAULT 'system',
    reason          TEXT,
    PRIMARY KEY (project_id, epoch_id)
);
CREATE INDEX IF NOT EXISTS idx_corpus_epochs_active_project
    ON corpus_epochs_active (project_id);

COMMENT ON TABLE corpus_epochs_active IS 'Per-project visibility pointer over corpus_epochs. Atomic UPDATE = atomic rollback.';

-- corpus_rollbacks is the operator-facing audit of rollback events
-- (manual, automatic, scheduled).
CREATE TABLE IF NOT EXISTS corpus_rollbacks (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    from_epoch_id   TEXT REFERENCES corpus_epochs(id) ON DELETE SET NULL,
    to_epoch_id     TEXT REFERENCES corpus_epochs(id) ON DELETE SET NULL,
    triggered_by    TEXT NOT NULL,         -- 'operator:<name>' | 'circuit_breaker' | 'staged_rollout'
    reason          TEXT,
    applied_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_corpus_rollbacks_project_time
    ON corpus_rollbacks (project_id, applied_at DESC);

COMMENT ON TABLE corpus_rollbacks IS 'Audit trail for corpus_epochs_active mutations. Operator-visible via vornikctl memory rollback list.';

-- corpus_embedding_versions_active pins which embedding model
-- search consults per project. Schema lands now so Phase 0 can
-- record the current model id; the active-version filter activates
-- when a swap happens.
CREATE TABLE IF NOT EXISTS corpus_embedding_versions_active (
    project_id            TEXT NOT NULL,
    embedding_version_id  TEXT NOT NULL,
    activated_at          TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    activated_by          TEXT NOT NULL DEFAULT 'system',
    notes                 TEXT,
    PRIMARY KEY (project_id, embedding_version_id)
);

COMMENT ON TABLE corpus_embedding_versions_active IS 'Per-project active embedding version(s). Multi-row state during a model swap allows search to consult both old and new while re-embedding completes.';

-- 3. Ingest queue. Producers enqueue here; the future ingest worker
--    drains and dispatches a rag-ingest task per batch. UUIDv7 for
--    time-ordered scan; we store the ID as TEXT and let the
--    application generate it.
CREATE TABLE IF NOT EXISTS project_ingest_queue (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    source_artifact_id    TEXT NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    producer_role         TEXT NOT NULL,
    ingest_execution_id   TEXT REFERENCES executions(id) ON DELETE SET NULL,
    priority              SMALLINT NOT NULL DEFAULT 50,
    proposed_class        TEXT,
    proposed_confidence   REAL NOT NULL DEFAULT 0.5,
    state                 TEXT NOT NULL DEFAULT 'queued',
    attempts              SMALLINT NOT NULL DEFAULT 0,
    enqueued_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    started_at            TIMESTAMP WITH TIME ZONE,
    finished_at           TIMESTAMP WITH TIME ZONE,
    last_error            TEXT,
    CONSTRAINT ingest_queue_priority_range CHECK (priority >= 0 AND priority <= 100),
    CONSTRAINT ingest_queue_state_check
        CHECK (state IN ('queued','processing','done','failed'))
);

-- Drain-path index: pick next N candidates per project ordered by
-- priority then age.
CREATE INDEX IF NOT EXISTS idx_ingest_queue_drain
    ON project_ingest_queue (project_id, priority, enqueued_at)
    WHERE state = 'queued';
CREATE INDEX IF NOT EXISTS idx_ingest_queue_state_age
    ON project_ingest_queue (state, enqueued_at);

COMMENT ON TABLE project_ingest_queue IS 'Producer→pipeline contract. Phase 0: schema only; Phase 1 wires the producer hook + worker.';

-- 4. Quarantine. Chunks the gates refused; operator-visible via
--    vornikctl memory quarantine.
CREATE TABLE IF NOT EXISTS project_memory_quarantine (
    id                    TEXT PRIMARY KEY,
    project_id            TEXT NOT NULL,
    source_artifact_id    TEXT REFERENCES artifacts(id) ON DELETE SET NULL,
    producer_role         TEXT,
    ingest_execution_id   TEXT REFERENCES executions(id) ON DELETE SET NULL,
    content               TEXT NOT NULL,
    content_hash          TEXT NOT NULL,
    proposed_class        TEXT,
    failed_gate           TEXT NOT NULL,
    failure_detail        TEXT,
    quarantined_at        TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    released_at           TIMESTAMP WITH TIME ZONE,
    released_chunk_id     TEXT REFERENCES project_memory_chunks(id) ON DELETE SET NULL,
    dropped_at            TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_quarantine_project_pending
    ON project_memory_quarantine (project_id, quarantined_at DESC)
    WHERE released_at IS NULL AND dropped_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_quarantine_failed_gate
    ON project_memory_quarantine (failed_gate, quarantined_at DESC);

COMMENT ON TABLE project_memory_quarantine IS 'Chunks rejected by an ingest gate (DMARC-quarantine pattern). Operator inspects + releases or drops.';

-- 5. Backfill: existing chunks get explicit lifecycle + validation
--    status so the eventual published-only filter (Phase 3+) keeps
--    them visible. We mark them 'legacy' (not 'unverified') so the
--    Phase D rerank weights demote them below freshly-validated
--    peers — preserving today's hits while leaving room for the new
--    pipeline to elevate above them.
--
-- We do NOT fabricate epoch rows for legacy chunks. Their epoch_id
-- stays NULL; the published-only filter will be expressed as
-- "epoch_id IS NULL OR epoch_id IN (active set)" so legacy chunks
-- remain searchable until operators decide to retire them.
UPDATE project_memory_chunks
SET lifecycle_state    = 'published',
    validation_status  = 'legacy',
    content_class      = COALESCE(NULLIF(content_class, ''), 'unclassified')
WHERE lifecycle_state IS NULL
   OR lifecycle_state = 'published';   -- the column default; targets every row
`,
		Down: `
-- Drop dependent objects in reverse order. Drop the new columns on
-- project_memory_chunks last; the FK from corpus_epochs would
-- otherwise block.
DROP TABLE IF EXISTS project_memory_quarantine;
DROP TABLE IF EXISTS project_ingest_queue;
DROP TABLE IF EXISTS corpus_embedding_versions_active;
DROP TABLE IF EXISTS corpus_rollbacks;
DROP TABLE IF EXISTS corpus_epochs_active;
DROP TABLE IF EXISTS corpus_epochs;

ALTER TABLE project_memory_chunks
    DROP CONSTRAINT IF EXISTS memory_chunks_confidence_range_check,
    DROP CONSTRAINT IF EXISTS memory_chunks_validation_status_check,
    DROP CONSTRAINT IF EXISTS memory_chunks_lifecycle_state_check;

DROP INDEX IF EXISTS idx_memory_chunks_needs_graph;
DROP INDEX IF EXISTS idx_memory_chunks_validation;
DROP INDEX IF EXISTS idx_memory_chunks_class;
DROP INDEX IF EXISTS idx_memory_chunks_epoch;
DROP INDEX IF EXISTS idx_memory_chunks_lifecycle;

ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS needs_graph_extraction,
    DROP COLUMN IF EXISTS embedding_version_id,
    DROP COLUMN IF EXISTS expires_at,
    DROP COLUMN IF EXISTS supersedes_id,
    DROP COLUMN IF EXISTS confidence,
    DROP COLUMN IF EXISTS validation_status,
    DROP COLUMN IF EXISTS content_class,
    DROP COLUMN IF EXISTS ingest_execution_id,
    DROP COLUMN IF EXISTS validator_role,
    DROP COLUMN IF EXISTS producer_role,
    DROP COLUMN IF EXISTS epoch_id,
    DROP COLUMN IF EXISTS lifecycle_state;
`,
	},
	{
		Version: 24,
		Name:    "task_lifecycle_conversational",
		// Phase 23 of the conversational task lifecycle (see
		// https://docs.vornik.io).
		// Additive only — every existing task reads back unchanged
		// because:
		//   - new columns default to NULL or 0
		//   - new statuses are only reachable by callers using the
		//     new state-machine validator
		//   - the two new tables are populated only by Phase 24+ writers
		//
		// What lands:
		//   1. New columns on tasks: brief_amended_at, current_phase,
		//      expected_by, closed_at, closed_by, message_count,
		//      open_checkpoint_id.
		//   2. CHECK constraint on tasks.status widened to include the
		//      four new statuses (AWAITING_INPUT, AWAITING_EXTERNAL,
		//      PAUSED, CLOSED). NOT VALID + VALIDATE pattern matches
		//      the v23 hardening migrations.
		//   3. task_messages table — the conversation thread.
		//   4. task_scratchpad table — the lead's running summary,
		//      one row per task.
		//   5. Inbox indexes (idx_tasks_awaiting_input,
		//      idx_tasks_awaiting_external) — the most-hit query
		//      under the new lifecycle is "what's waiting on me".
		Up: `
-- 1. Additive columns on tasks. All nullable / default-zero so
--    existing rows read back unchanged.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS brief_amended_at    TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS current_phase       TEXT,
    ADD COLUMN IF NOT EXISTS expected_by         TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS closed_at           TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS closed_by           TEXT,
    ADD COLUMN IF NOT EXISTS message_count       INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS open_checkpoint_id  TEXT;

-- 2. tasks.status is a Postgres enum (task_status). Extend it with
--    the four new values. ALTER TYPE ... ADD VALUE IF NOT EXISTS
--    is idempotent and fast (no table rewrite). The IF NOT EXISTS
--    clause means re-running the migration on a partially-applied
--    DB stays a no-op for already-added values.
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'AWAITING_INPUT';
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'AWAITING_EXTERNAL';
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'PAUSED';
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'CLOSED';

-- 3. task_messages — the conversation thread. One row per chat
--    entry. message_kind drives UI affordance and state-machine
--    effects (see TaskMessageKind* in models.go).
CREATE TABLE IF NOT EXISTS task_messages (
    id            TEXT PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    execution_id  TEXT REFERENCES executions(id),
    parent_id     TEXT REFERENCES task_messages(id),
    author_kind   TEXT NOT NULL,
    author_id     TEXT,
    message_kind  TEXT NOT NULL,
    content       TEXT NOT NULL,
    metadata      JSONB,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

-- author_kind constraint kept loose to admit "role:<name>" entries
-- the workflow may add later. message_kind constrained because the
-- UI dispatches on it and unknown values would render as gaps.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'task_messages_kind_check') THEN
        ALTER TABLE task_messages
            ADD CONSTRAINT task_messages_kind_check
            CHECK (message_kind IN (
                'message','directive','checkpoint','answer',
                'plan','phase_marker','note','closure_request','system'
            ))
            NOT VALID;
        ALTER TABLE task_messages VALIDATE CONSTRAINT task_messages_kind_check;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_task_messages_task
    ON task_messages(task_id, created_at);
-- Partial index over open checkpoints (resolved is missing or false).
CREATE INDEX IF NOT EXISTS idx_task_messages_open_checkpoints
    ON task_messages(task_id)
    WHERE message_kind = 'checkpoint'
      AND COALESCE((metadata->>'resolved')::boolean, FALSE) = FALSE;

-- 4. task_scratchpad — one row per task.
CREATE TABLE IF NOT EXISTS task_scratchpad (
    task_id            TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    summary            TEXT NOT NULL DEFAULT '',
    facts              JSONB NOT NULL DEFAULT '{}',
    open_questions     JSONB NOT NULL DEFAULT '[]',
    current_phase      TEXT,
    phase_history      JSONB NOT NULL DEFAULT '[]',
    last_execution_id  TEXT REFERENCES executions(id),
    updated_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
);

-- 5. Inbox indexes deferred to migration v25. The partial
--    predicate "WHERE status = 'AWAITING_INPUT'" can't be parsed
--    in the same transaction that ADDs the value to the
--    task_status enum (Postgres rule: unsafe use of new enum
--    value). v25 runs in its own tx and creates the indexes.

-- FK: tasks.open_checkpoint_id → task_messages.id. Added after the
-- task_messages table exists. ON DELETE SET NULL so deleting a
-- message doesn't cascade-orphan the task pointer; in practice
-- messages aren't deleted, but defensive default.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'tasks_open_checkpoint_id_fkey'
    ) THEN
        ALTER TABLE tasks
            ADD CONSTRAINT tasks_open_checkpoint_id_fkey
            FOREIGN KEY (open_checkpoint_id)
            REFERENCES task_messages(id)
            ON DELETE SET NULL;
    END IF;
END $$;
`,
		Down: `
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_open_checkpoint_id_fkey;

-- v25 owns the inbox indexes; this migration's Down doesn't drop
-- them, but the FK and tables removal below is still correct.
DROP TABLE IF EXISTS task_scratchpad;
DROP INDEX IF EXISTS idx_task_messages_open_checkpoints;
DROP INDEX IF EXISTS idx_task_messages_task;
DROP TABLE IF EXISTS task_messages;

-- Note: Postgres has no native DROP VALUE for enum types. The four
-- new task_status values (AWAITING_INPUT, AWAITING_EXTERNAL, PAUSED,
-- CLOSED) are left in the type. A clean removal would require
-- recreating the enum type and rewriting every dependent column —
-- not a safe rollback path for production data. Operators rolling
-- back this migration get an enum that's a strict superset of the
-- pre-v24 shape; nothing breaks.

ALTER TABLE tasks
    DROP COLUMN IF EXISTS open_checkpoint_id,
    DROP COLUMN IF EXISTS message_count,
    DROP COLUMN IF EXISTS closed_by,
    DROP COLUMN IF EXISTS closed_at,
    DROP COLUMN IF EXISTS expected_by,
    DROP COLUMN IF EXISTS current_phase,
    DROP COLUMN IF EXISTS brief_amended_at;
`,
	},
	{
		Version: 26,
		Name:    "memory_knowledge_graph",
		// Phase 43 of the knowledge-graph memory roadmap (LLD:
		// https://docs.vornik.io).
		//
		// Three new tables on top of the existing chunk store:
		//   knowledge_entities — typed nouns extracted from chunks
		//                        (VENDOR, PRODUCT, DECISION, …)
		//   knowledge_edges    — typed relationships between entities
		//                        (QUOTED_PRICE, CHOSEN_OVER, …)
		//   entity_mentions    — chunk ↔ entity link table
		//
		// Plus a backfill pass that flips needs_graph_extraction to
		// TRUE on every existing published chunk so the post-Phase-49
		// pipeline picks them up. No chunks are deleted; the column
		// already exists from migration v23.
		//
		// pgvector is required (already enabled in v15). entity
		// embeddings carry the same 1024-dim shape as chunks so the
		// existing embedder pipeline reuses without per-stage
		// configuration. ivfflat index over entity embeddings keeps
		// the resolver's "top-K similar entities" lookup index-served
		// even at 100k entities/project.
		Up: `
-- 0. Best-effort enable pg_trgm BEFORE any index that uses
-- gin_trgm_ops below. If the role can't CREATE EXTENSION (or the
-- extension isn't packaged), the dependent index creation falls
-- through gracefully — the resolver still works via name + embedding
-- match, just without trigram fuzzy lookup.
DO $$
BEGIN
    EXECUTE 'CREATE EXTENSION IF NOT EXISTS pg_trgm';
EXCEPTION WHEN OTHERS THEN
    NULL;
END $$;

-- 1. knowledge_entities — the noun layer.
CREATE TABLE IF NOT EXISTS knowledge_entities (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    type            TEXT NOT NULL,
    canonical_name  TEXT NOT NULL,
    aliases         JSONB NOT NULL DEFAULT '[]',
    description     TEXT NOT NULL DEFAULT '',
    properties      JSONB NOT NULL DEFAULT '{}',
    embedding       vector(1024),

    -- provenance
    extracted_by    TEXT,
    resolved_by     TEXT,
    confidence      REAL NOT NULL DEFAULT 1.0,

    -- lifecycle (mirrors project_memory_chunks)
    lifecycle_state TEXT NOT NULL DEFAULT 'published',
    validation_status TEXT NOT NULL DEFAULT 'unverified',
    epoch_id        TEXT,
    expires_at      TIMESTAMP WITH TIME ZONE,
    supersedes_id   TEXT,

    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),

    UNIQUE (project_id, type, canonical_name)
);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_entities_confidence_range') THEN
        ALTER TABLE knowledge_entities
            ADD CONSTRAINT knowledge_entities_confidence_range
            CHECK (confidence >= 0 AND confidence <= 1) NOT VALID;
        ALTER TABLE knowledge_entities VALIDATE CONSTRAINT knowledge_entities_confidence_range;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_entities_lifecycle_check') THEN
        ALTER TABLE knowledge_entities
            ADD CONSTRAINT knowledge_entities_lifecycle_check
            CHECK (lifecycle_state IN ('raw','staged','validated','published','quarantined','superseded','shadow')) NOT VALID;
        ALTER TABLE knowledge_entities VALIDATE CONSTRAINT knowledge_entities_lifecycle_check;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_entities_project_type
    ON knowledge_entities(project_id, type)
    WHERE lifecycle_state = 'published';
CREATE INDEX IF NOT EXISTS idx_entities_aliases_gin
    ON knowledge_entities USING gin (aliases);
-- Trigram index — gated on pg_trgm being actually present
-- (CREATE EXTENSION at the top is best-effort and may have been
-- skipped if the operator's role can't install extensions).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_entities_canonical_name_trgm ' ||
                'ON knowledge_entities USING gin (canonical_name gin_trgm_ops)';
    END IF;
EXCEPTION WHEN OTHERS THEN
    NULL;
END $$;

-- ivfflat over the embedding column. Skipped silently when
-- pgvector isn't installed; the resolver falls back to
-- name-substring matching only.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') THEN
        EXECUTE 'CREATE INDEX IF NOT EXISTS idx_entities_embedding_ivfflat ' ||
                'ON knowledge_entities USING ivfflat (embedding vector_cosine_ops) WITH (lists = 64)';
    END IF;
EXCEPTION WHEN OTHERS THEN
    -- pg_trgm or vector ext missing or insufficient privilege; ignore.
    NULL;
END $$;

-- 2. knowledge_edges — the relationship layer.
CREATE TABLE IF NOT EXISTS knowledge_edges (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    from_entity     TEXT NOT NULL REFERENCES knowledge_entities(id) ON DELETE CASCADE,
    to_entity       TEXT NOT NULL REFERENCES knowledge_entities(id) ON DELETE CASCADE,
    predicate       TEXT NOT NULL,
    properties      JSONB NOT NULL DEFAULT '{}',

    -- provenance
    source_chunks   TEXT[] NOT NULL,
    extracted_by    TEXT,
    confidence      REAL NOT NULL DEFAULT 1.0,
    faithfulness    REAL,

    -- lifecycle
    lifecycle_state TEXT NOT NULL DEFAULT 'published',
    epoch_id        TEXT,

    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now(),

    UNIQUE (project_id, from_entity, predicate, to_entity)
);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_edges_confidence_range') THEN
        ALTER TABLE knowledge_edges
            ADD CONSTRAINT knowledge_edges_confidence_range
            CHECK (confidence >= 0 AND confidence <= 1) NOT VALID;
        ALTER TABLE knowledge_edges VALIDATE CONSTRAINT knowledge_edges_confidence_range;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_edges_faithfulness_range') THEN
        ALTER TABLE knowledge_edges
            ADD CONSTRAINT knowledge_edges_faithfulness_range
            CHECK (faithfulness IS NULL OR (faithfulness >= 0 AND faithfulness <= 1)) NOT VALID;
        ALTER TABLE knowledge_edges VALIDATE CONSTRAINT knowledge_edges_faithfulness_range;
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_edges_lifecycle_check') THEN
        ALTER TABLE knowledge_edges
            ADD CONSTRAINT knowledge_edges_lifecycle_check
            CHECK (lifecycle_state IN ('raw','staged','validated','published','quarantined','superseded','shadow')) NOT VALID;
        ALTER TABLE knowledge_edges VALIDATE CONSTRAINT knowledge_edges_lifecycle_check;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_edges_project_from
    ON knowledge_edges(project_id, from_entity)
    WHERE lifecycle_state = 'published';
CREATE INDEX IF NOT EXISTS idx_edges_project_to
    ON knowledge_edges(project_id, to_entity)
    WHERE lifecycle_state = 'published';
CREATE INDEX IF NOT EXISTS idx_edges_predicate
    ON knowledge_edges(project_id, predicate);

-- 3. entity_mentions — chunk ↔ entity link.
CREATE TABLE IF NOT EXISTS entity_mentions (
    chunk_id   TEXT NOT NULL REFERENCES project_memory_chunks(id) ON DELETE CASCADE,
    entity_id  TEXT NOT NULL REFERENCES knowledge_entities(id) ON DELETE CASCADE,
    char_start INT NOT NULL DEFAULT 0,
    char_end   INT,
    surface    TEXT,
    PRIMARY KEY (chunk_id, entity_id, char_start)
);

CREATE INDEX IF NOT EXISTS idx_mentions_entity ON entity_mentions(entity_id);

-- 4. Backfill: flip needs_graph_extraction on every existing
-- published chunk so the Phase-49 pipeline (when it lands) picks
-- them up. Idempotent; safe to re-run.
UPDATE project_memory_chunks
SET    needs_graph_extraction = TRUE
WHERE  needs_graph_extraction IS NULL
   OR  needs_graph_extraction = FALSE;
`,
		Down: `
DROP TABLE IF EXISTS entity_mentions;
DROP TABLE IF EXISTS knowledge_edges;
DROP TABLE IF EXISTS knowledge_entities;
-- Leave needs_graph_extraction values alone — the column itself
-- belongs to v23 and survives this rollback.
`,
	},
	{
		Version: 25,
		Name:    "task_lifecycle_inbox_indexes",
		// v24 added the AWAITING_INPUT / AWAITING_EXTERNAL enum
		// values; this migration creates the partial indexes that
		// filter on them. Split into a separate migration because
		// Postgres rejects "unsafe use of new value" when an index
		// predicate references an enum value added in the same
		// transaction.
		//
		// Indexes are partial (only rows in the awaiting status are
		// in the index) so writes against the broader task pool pay
		// nothing; the inbox query is exactly index-served.
		Up: `
CREATE INDEX IF NOT EXISTS idx_tasks_awaiting_input
    ON tasks(updated_at DESC)
    WHERE status = 'AWAITING_INPUT';
CREATE INDEX IF NOT EXISTS idx_tasks_awaiting_external
    ON tasks(expected_by)
    WHERE status = 'AWAITING_EXTERNAL';
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_awaiting_external;
DROP INDEX IF EXISTS idx_tasks_awaiting_input;
`,
	},
	{
		Version: 27,
		Name:    "tasks_lease_audit",
		// Forensic instrumentation: capture every UPDATE on tasks
		// that mutates lease_id or status, into a dedicated audit
		// table. Solves "lease lost mid-flight" bugs (T-6f55, the
		// 2026-05-08 mount-vanishing incident, and any future
		// occurrence) — the audit row records before/after values
		// AND the active SQL statement, so the repo function that
		// nuked the lease becomes obvious.
		//
		// Trigger is AFTER UPDATE so it can't slow the write path
		// when nothing changed (early-out via IS DISTINCT FROM).
		// Storage cost is bounded — typical mutation rate is well
		// under 100/sec; even a busy day only writes ~hundreds of
		// rows. Retention TBD: keep until we close the lease-loss
		// investigation, then add a periodic cleanup if it grows.
		Up: `
CREATE TABLE IF NOT EXISTS tasks_lease_audit (
    id            BIGSERIAL PRIMARY KEY,
    task_id       TEXT NOT NULL,
    changed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    old_status    TEXT,
    new_status    TEXT,
    old_lease_id  TEXT,
    new_lease_id  TEXT,
    -- current_query() captures the SQL that triggered the
    -- update — surfaces which repo function (LeaseTask /
    -- ReleaseLease / TransitionConditional / etc.) did it.
    sql_text      TEXT
);

CREATE INDEX IF NOT EXISTS idx_tasks_lease_audit_task_time
    ON tasks_lease_audit (task_id, changed_at DESC);

CREATE OR REPLACE FUNCTION tasks_lease_audit_trigger() RETURNS trigger AS $$
BEGIN
    IF (OLD.lease_id IS DISTINCT FROM NEW.lease_id)
       OR (OLD.status   IS DISTINCT FROM NEW.status) THEN
        INSERT INTO tasks_lease_audit (
            task_id, old_status, new_status, old_lease_id, new_lease_id, sql_text
        ) VALUES (
            NEW.id,
            OLD.status::text, NEW.status::text,
            OLD.lease_id, NEW.lease_id,
            current_query()
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tasks_lease_audit_after_update ON tasks;
CREATE TRIGGER tasks_lease_audit_after_update
AFTER UPDATE ON tasks
FOR EACH ROW EXECUTE FUNCTION tasks_lease_audit_trigger();
`,
		Down: `
DROP TRIGGER IF EXISTS tasks_lease_audit_after_update ON tasks;
DROP FUNCTION IF EXISTS tasks_lease_audit_trigger();
DROP INDEX IF EXISTS idx_tasks_lease_audit_task_time;
DROP TABLE IF EXISTS tasks_lease_audit;
`,
	},
	{
		Version: 28,
		Name:    "telegram_task_threads",
		// Persistent map from Telegram Forum Topics to tasks.
		// Replaces the in-memory notifTracker for reply routing so
		// the mapping survives bot restarts. UNIQUE(chat_id,
		// thread_id) is the race guard — concurrent first-
		// notification handlers may both call createForumTopic;
		// only one INSERT wins, the loser re-resolves via
		// GetByTask. See https://docs.vornik.io
		Up: `
CREATE TABLE IF NOT EXISTS telegram_task_threads (
    id          TEXT PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    chat_id     BIGINT NOT NULL,
    thread_id   BIGINT NOT NULL,
    topic_name  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at   TIMESTAMPTZ,
    UNIQUE (chat_id, thread_id)
);

CREATE INDEX IF NOT EXISTS idx_telegram_task_threads_task
    ON telegram_task_threads(task_id);
`,
		Down: `
DROP INDEX IF EXISTS idx_telegram_task_threads_task;
DROP TABLE IF EXISTS telegram_task_threads;
`,
	},
	{
		Version: 29,
		Name:    "project_memory_chunks_content_title",
		// Display-only LLM-generated topic label for each chunk.
		// Nullable so existing rows stay valid; backfill runs via
		// `vornikctl memory backfill-titles`. Read path prefers this
		// over the legacy filename/markdown-heading fallback.
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS content_title TEXT;
`,
		Down: `
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS content_title;
`,
	},
	{
		Version: 30,
		Name:    "task_creation_source_route",
		// Split the strict-adaptive routing handoff out of the
		// overloaded DELEGATION enum value. delegateSelectedWorkflow
		// (executor/plan.go) copies the parent's payload verbatim
		// onto the child — same-prompt parent/child pair is correct
		// for ROUTE but suspicious for plain DELEGATION (where the
		// lead is supposed to break work into distinct sub-prompts).
		// Without a separate value, the monitoring query
		//   SELECT parent_task_id, count(*) FROM tasks
		//   WHERE creation_source='ROUTE' GROUP BY 1 HAVING count(*) > 1
		// can't distinguish the healthy 1:1 handoff from the
		// a4b24c5 regression (one autonomy tick → 20+ children).
		Up: `
ALTER TYPE task_creation_source ADD VALUE IF NOT EXISTS 'ROUTE';
`,
		// No-op down: removing an enum value would orphan rows.
		Down: ``,
	},
	{
		Version: 31,
		Name:    "project_memory_chunks_utility_score",
		// Per-chunk learnable utility score derived from
		// memory_retrieval_audit. The background recompute job
		// (internal/memory.UtilityScorer) writes this column; the
		// search-side SQL multiplies it into the RRF score so chunks
		// repeatedly used in successful retrievals climb. Default 0
		// is the neutral starting value — search treats unset rows
		// as "no signal" rather than "low quality".
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS utility_score DOUBLE PRECISION NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_project_memory_chunks_utility_score
    ON project_memory_chunks (project_id, utility_score DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_project_memory_chunks_utility_score;
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS utility_score;
`,
	},
	{
		Version: 32,
		Name:    "create_api_keys",
		// Per-project bearer tokens. Replaces the operator-trusted
		// X-Vornik-Project-ID header + static YAML api_keys map for
		// new deployments; both legacy paths stay live during the
		// 2026.6.0 → 2026.8.0 deprecation window. Hash storage (sha256
		// hex) means a DB dump never yields usable keys. The partial
		// index narrows the auth-hot-path lookup to active rows only.
		Up: `
CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_by   TEXT
);
CREATE INDEX IF NOT EXISTS idx_api_keys_project ON api_keys(project_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_active_hash
    ON api_keys(key_hash) WHERE revoked_at IS NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_api_keys_active_hash;
DROP INDEX IF EXISTS idx_api_keys_project;
DROP TABLE IF EXISTS api_keys;
`,
	},
	{
		Version: 33,
		Name:    "create_project_gists",
		// Per-project term-frequency summary written by the
		// periodic consolidation worker. PK on project_id: only the
		// latest gist matters; each tick UPSERTs. terms_json carries
		// []TermFrequency as JSON so the API + UI can forward it
		// without parsing on the daemon side.
		Up: `
CREATE TABLE IF NOT EXISTS project_gists (
    project_id     TEXT PRIMARY KEY,
    terms_json     JSONB NOT NULL,
    chunks_scanned INTEGER NOT NULL,
    generated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    duration_ms    INTEGER NOT NULL DEFAULT 0
);
`,
		Down: `
DROP TABLE IF EXISTS project_gists;
`,
	},
	{
		Version: 34,
		Name:    "create_intent_verdicts",
		// Per-tool-call risk verdicts from the two-tier intent
		// judge. Heuristic tier (internal/intentjudge) fires
		// sync before every tool call; the LLM tier refines
		// async. Both verdicts persist here so future calibration
		// can measure heuristic-vs-LLM agreement.
		//
		// Indexed on (project_id, created_at DESC) for the
		// "show me recent high-risk calls" operator query, and
		// on tool_name for per-tool calibration heatmaps.
		Up: `
CREATE TABLE IF NOT EXISTS intent_verdicts (
    id                TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    task_id           TEXT,
    execution_id      TEXT,
    chat_id           BIGINT,
    tool_name         TEXT NOT NULL,
    tool_args         TEXT NOT NULL DEFAULT '',
    heuristic_risk    TEXT NOT NULL,
    heuristic_conf    DOUBLE PRECISION NOT NULL DEFAULT 0,
    heuristic_rec     TEXT NOT NULL,
    heuristic_reason  TEXT NOT NULL DEFAULT '',
    heuristic_lat_ms  INTEGER NOT NULL DEFAULT 0,
    llm_risk          TEXT,
    llm_conf          DOUBLE PRECISION,
    llm_rec           TEXT,
    llm_reason        TEXT,
    llm_lat_ms        INTEGER,
    llm_model         TEXT,
    -- final_risk / final_rec are the verdict the dispatcher
    -- actually acted on (heuristic when LLM hasn't returned yet,
    -- LLM when it overrode). Lets a query find "what we did"
    -- without walking the heuristic + llm columns each time.
    final_risk        TEXT NOT NULL,
    final_rec         TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    refined_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_intent_verdicts_project_time
    ON intent_verdicts (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intent_verdicts_tool
    ON intent_verdicts (tool_name);
CREATE INDEX IF NOT EXISTS idx_intent_verdicts_final_risk
    ON intent_verdicts (final_risk)
    WHERE final_risk IN ('high', 'critical');
`,
		Down: `
DROP INDEX IF EXISTS idx_intent_verdicts_final_risk;
DROP INDEX IF EXISTS idx_intent_verdicts_tool;
DROP INDEX IF EXISTS idx_intent_verdicts_project_time;
DROP TABLE IF EXISTS intent_verdicts;
`,
	},
	{
		Version: 35,
		Name:    "api_keys_rate_limit_columns",
		// Per-API-key rate limits — first slice of the rate-limit
		// hardening backlog item. Token-bucket primitive lives in
		// internal/ratelimit/keybucket.go; AuthMiddleware reads
		// these columns after a successful DB-key match. NULL on
		// either column means "no limit configured" — request
		// passes without touching the bucket.
		Up: `
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS rate_limit_rps   INTEGER,
    ADD COLUMN IF NOT EXISTS rate_limit_burst INTEGER;
`,
		Down: `
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS rate_limit_burst,
    DROP COLUMN IF EXISTS rate_limit_rps;
`,
	},
	{
		Version: 36,
		Name:    "task_llm_usage_cache_columns",
		// LLM-caching phase A (observation-only) — see
		// https://docs.vornik.io Two new
		// columns on task_llm_usage so the spend dashboard can
		// distinguish fresh prompt-token writes from cache-cheap
		// reads. Bedrock + Anthropic populate these from
		// provider-native cache metadata; sub-providers without
		// caching surface (Vertex/HTTP/Ollama) leave them at 0.
		// Schema-only — phase A reads the data, doesn't emit
		// cache annotations; phase B turns annotations on.
		Up: `
ALTER TABLE task_llm_usage
    ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cache_read_tokens     BIGINT NOT NULL DEFAULT 0;
`,
		Down: `
ALTER TABLE task_llm_usage
    DROP COLUMN IF EXISTS cache_read_tokens,
    DROP COLUMN IF EXISTS cache_creation_tokens;
`,
	},
	{
		Version: 37,
		Name:    "project_memory_chunks_url_liveness",
		// URL liveness flagging — prevents dead URLs indexed long
		// ago from being returned as authoritative "live" hits to
		// downstream agents (researcher, dispatcher). NULL on both
		// columns means "never checked" — search treats those as
		// "unknown, surface anyway" which preserves today's
		// behaviour. Operators can run `vornikctl memory
		// recheck-urls --project <id>` to populate the columns on
		// demand. A periodic auto-worker is deferred to a follow-
		// up so the schema lands as a clean read-side change.
		//
		// Partial index narrows the "find chunks whose URLs need
		// rechecking" worker query (last_checked_at IS NULL OR
		// last_checked_at < cutoff) to a small fraction of the
		// corpus.
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS last_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS is_alive        BOOLEAN;

CREATE INDEX IF NOT EXISTS idx_project_memory_chunks_url_recheck
    ON project_memory_chunks (project_id, last_checked_at NULLS FIRST);
`,
		Down: `
DROP INDEX IF EXISTS idx_project_memory_chunks_url_recheck;
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS is_alive,
    DROP COLUMN IF EXISTS last_checked_at;
`,
	},
	{
		Version: 38,
		Name:    "create_admin_audit",
		// Daemon-level admin action log — see
		// https://docs.vornik.io §3.3. Every
		// admin-surface POST (config reload, MCP refresh,
		// danger-zone confirmation) writes one row here before
		// returning. Distinct from tool_audit_log, which logs
		// agent-side tool invocations; admin_audit captures
		// operator actions against the daemon itself.
		//
		// before_state / after_state are JSONB so config-edit rows
		// can carry pre/post values for diff rendering without the
		// admin code needing to know each value's typed shape.
		Up: `
CREATE TABLE IF NOT EXISTS admin_audit (
    id           TEXT PRIMARY KEY,
    ts           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    principal    TEXT NOT NULL,
    source       TEXT NOT NULL,
    action       TEXT NOT NULL,
    target       TEXT NOT NULL DEFAULT '',
    before_state JSONB,
    after_state  JSONB,
    ip           INET,
    user_agent   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_admin_audit_ts
    ON admin_audit (ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_action_ts
    ON admin_audit (action, ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_principal_ts
    ON admin_audit (principal, ts DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_target
    ON admin_audit (target) WHERE target <> '';
`,
		Down: `
DROP INDEX IF EXISTS idx_admin_audit_target;
DROP INDEX IF EXISTS idx_admin_audit_principal_ts;
DROP INDEX IF EXISTS idx_admin_audit_action_ts;
DROP INDEX IF EXISTS idx_admin_audit_ts;
DROP TABLE IF EXISTS admin_audit;
`,
	},
	{
		Version: 39,
		Name:    "create_chat_audit_log",
		// Per-turn dispatcher activity for the
		// /ui/admin/chat-audit operator surface. One row per inbound
		// user message processed through the LLM tool loop — system
		// prompt hash, model, tool calls, response excerpt, cost.
		// Triggered by the 2026-05-18 janka CV-delivery investigation
		// which took 20+ minutes of journald grepping to determine
		// the dispatcher wasn't calling send_artifact at all.
		//
		// chat_system_prompts is content-addressed by sha256 hex so
		// the prompt body (5-10 KB typically) is stored once per
		// distinct prompt regardless of how many turns reference it.
		// chat_audit_log.system_prompt_hash is the foreign key.
		Up: `
CREATE TABLE IF NOT EXISTS chat_system_prompts (
    hash TEXT PRIMARY KEY,
    body TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS chat_audit_log (
    id                  TEXT PRIMARY KEY,
    ts                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
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
    duration_ms         BIGINT NOT NULL DEFAULT 0,
    cost_usd            DOUBLE PRECISION NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_chat_audit_ts
    ON chat_audit_log (ts DESC);
CREATE INDEX IF NOT EXISTS idx_chat_audit_chat_ts
    ON chat_audit_log (chat_id, ts DESC) WHERE chat_id <> '';
CREATE INDEX IF NOT EXISTS idx_chat_audit_project_ts
    ON chat_audit_log (project_id, ts DESC) WHERE project_id <> '';
`,
		Down: `
DROP INDEX IF EXISTS idx_chat_audit_project_ts;
DROP INDEX IF EXISTS idx_chat_audit_chat_ts;
DROP INDEX IF EXISTS idx_chat_audit_ts;
DROP TABLE IF EXISTS chat_audit_log;
DROP TABLE IF EXISTS chat_system_prompts;
`,
	},
	{
		Version: 40,
		Name:    "project_gists_add_narrative",
		// Slice-3 LLM-tier consolidation: a short natural-language
		// summary layered on top of the existing term-frequency
		// gist. The LLM tier runs on a slower cadence (default 1h)
		// than the LLM-free term pass (default 10m) and writes
		// only the narrative_* columns — never touches terms_json.
		// Nullable so the LLM tier stays opt-in; absent values
		// surface as empty strings in the API/UI without a
		// migration-time backfill.
		Up: `
ALTER TABLE project_gists ADD COLUMN IF NOT EXISTS narrative TEXT;
ALTER TABLE project_gists ADD COLUMN IF NOT EXISTS narrative_model TEXT;
ALTER TABLE project_gists ADD COLUMN IF NOT EXISTS narrative_generated_at TIMESTAMPTZ;
`,
		Down: `
ALTER TABLE project_gists DROP COLUMN IF EXISTS narrative_generated_at;
ALTER TABLE project_gists DROP COLUMN IF EXISTS narrative_model;
ALTER TABLE project_gists DROP COLUMN IF EXISTS narrative;
`,
	},
	{
		Version: 41,
		Name:    "create_embedding_cache",
		// LLM caching design Phase D — content-hash → embedding
		// cache. Same vector(1024) shape as project_memory_chunks
		// + knowledge_entities so the existing pgvector extension
		// covers it; primary key is (content_hash, model) so
		// identical content across projects shares one row per
		// model (embeddings are model-bound). last_hit_at lets a
		// retention sweep evict cold entries; created_at lets
		// operators see the table's growth rate.
		Up: `
CREATE TABLE IF NOT EXISTS embedding_cache (
    content_hash  TEXT NOT NULL,
    model         TEXT NOT NULL,
    embedding     vector(1024) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_hit_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (content_hash, model)
);
CREATE INDEX IF NOT EXISTS idx_embedding_cache_last_hit
    ON embedding_cache (last_hit_at);
`,
		Down: `
DROP INDEX IF EXISTS idx_embedding_cache_last_hit;
DROP TABLE IF EXISTS embedding_cache;
`,
	},
	{
		Version: 42,
		Name:    "create_ratelimit_counters",
		// Sub-item 5 of the rate-limit hardening backlog — durable
		// counter storage for multi-daemon SaaS deployments. The
		// existing in-process Limiter is fine for single-daemon
		// boxes (counter resets on restart, caps are defensive);
		// when two daemons share a project, each enforces its own
		// copy and the effective rate doubles. Postgres-backed
		// counters serialise both daemons against the same row.
		//
		// scope_kind is the limiter dimension ("project" today;
		// "key", "ip", "tool" follow as those limiters move off
		// the in-process map). scope_key is the dimension's
		// identifier (project_id, key_id, etc). window_start is
		// the truncated time bucket (start of the minute or hour
		// — caller normalises). count is the number of events
		// recorded in that bucket.
		//
		// TTL is enforced by a periodic sweeper (separate code
		// path) — DELETE WHERE window_start < NOW() - retention.
		// Single-table for now; pg_partman is overkill at our scale.
		Up: `
CREATE TABLE IF NOT EXISTS ratelimit_counters (
    scope_kind   TEXT NOT NULL,
    scope_key    TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    count        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope_kind, scope_key, window_start)
);

CREATE INDEX IF NOT EXISTS idx_ratelimit_counters_window
    ON ratelimit_counters (window_start);
`,
		Down: `
DROP INDEX IF EXISTS idx_ratelimit_counters_window;
DROP TABLE IF EXISTS ratelimit_counters;
`,
	},
	{
		Version: 43,
		Name:    "create_memory_eviction_audit",
		// Hard-eviction tombstone for project memory chunks. Soft-refute
		// (Repository.MarkRefutedByIDs) keeps the chunk row and flips
		// validation_status='refuted'; that's fine for "this record is
		// wrong, demote it in search." For GDPR-style "forget this"
		// requests + cleanup of confirmed-bad records that the soft path
		// leaves cluttering the search index, the daemon needs a DELETE
		// path. The DELETE itself cascades through memory_embed_queue,
		// memory_embed_dlq, and entity_mentions automatically (all FK
		// ON DELETE CASCADE) and sets project_memory_quarantine.
		// released_chunk_id to NULL where it pointed at the deleted
		// chunk. memory_retrieval_audit.chunk_ids is an array column
		// (no FK) so historical retrieval records preserve the original
		// chunk_id reference — correct for the audit trail.
		//
		// This table records WHO evicted WHAT WHEN and WHY so the
		// deletion itself is auditable (the deletion of records
		// without a record of the deletion is not GDPR-compliant; the
		// tombstone is the compliance hook). Fields are denormalised
		// snapshots taken at eviction time — content_hash + source_name
		// + content_class + producer_role survive even though the
		// referenced chunk is gone. No FK back to project_memory_chunks
		// (the row no longer exists by the time this insert lands).
		Up: `
CREATE TABLE IF NOT EXISTS memory_eviction_audit (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    chunk_id      TEXT NOT NULL,
    content_hash  TEXT NOT NULL DEFAULT '',
    source_name   TEXT NOT NULL DEFAULT '',
    content_class TEXT NOT NULL DEFAULT '',
    producer_role TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    evicted_by    TEXT NOT NULL DEFAULT '',
    evicted_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_memory_eviction_project_time
    ON memory_eviction_audit (project_id, evicted_at DESC);

COMMENT ON TABLE memory_eviction_audit IS 'GDPR-style hard-eviction tombstones for project_memory_chunks. Soft-refute lives in chunk_memory.validation_status; this table records who deleted what, when, why.';
`,
		Down: `
DROP INDEX IF EXISTS idx_memory_eviction_project_time;
DROP TABLE IF EXISTS memory_eviction_audit;
`,
	},
	{
		Version: 44,
		Name:    "chat_audit_hallucination_signals",
		// Per-turn hallucination signals on chat replies. The
		// detector fires in dispatcher/agent_process.go on every
		// reply but signals were observed in Prometheus +
		// (sometimes) prepended to the reply text and then DROPPED.
		// With this column the /ui/admin/chat-audit page surfaces
		// a "signals fired" badge + drill-down detail so operators
		// who get "the bot said X and it was wrong" reports can
		// see which detector flagged it (or didn't).
		//
		// Storage: TEXT JSON (array of hallucination.Signal).
		// Empty string = no signals (the common case). Not JSONB
		// because the list is read-only / rendered-whole — same
		// shape as tool_calls_json + system_prompt next to it.
		Up: `
ALTER TABLE chat_audit_log
    ADD COLUMN IF NOT EXISTS hallucination_signals_json TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN chat_audit_log.hallucination_signals_json IS 'JSON array of hallucination.Signal; empty string when no detector fired';
`,
		Down: `
ALTER TABLE chat_audit_log
    DROP COLUMN IF EXISTS hallucination_signals_json;
`,
	},
	{
		Version: 45,
		Name:    "extracted_documents",
		// Phase 0 of the document-extraction pipeline
		// (https://docs.vornik.io). The
		// extracted_documents table caches the result of running a
		// MIME-keyed extractor over an INPUT artifact (EPUB, PDF,
		// audio, etc.). Sections live on disk under
		// storage_path/sections/; metadata + outline are JSONB columns
		// so memory_search can cite "from chapter N of <title>".
		//
		// The UNIQUE (source_artifact_id, extractor_name,
		// extractor_version) constraint makes re-extraction
		// idempotent — re-running the same extractor returns the
		// existing row, and upgrading the extractor naturally
		// versions side-by-side instead of overwriting.
		//
		// The two project_memory_chunks columns add provenance:
		// once a chunk derives from an extracted document, the
		// retrieval layer can walk back to the source artifact and
		// surface the section title alongside the snippet.
		Up: `
CREATE TABLE IF NOT EXISTS extracted_documents (
    id                     TEXT PRIMARY KEY,
    project_id             TEXT NOT NULL,
    source_artifact_id     TEXT NOT NULL,
    extractor_name         TEXT NOT NULL,
    extractor_version      TEXT NOT NULL,
    mime_type              TEXT NOT NULL,
    storage_path           TEXT NOT NULL,
    metadata_blob          JSONB NOT NULL DEFAULT '{}'::jsonb,
    outline_blob           JSONB NOT NULL DEFAULT '[]'::jsonb,
    section_count          INTEGER NOT NULL DEFAULT 0,
    total_text_bytes       BIGINT NOT NULL DEFAULT 0,
    extraction_duration_ms BIGINT,
    status                 TEXT NOT NULL DEFAULT 'OK',
    extracted_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_artifact_id, extractor_name, extractor_version)
);

CREATE INDEX IF NOT EXISTS idx_extdoc_project
    ON extracted_documents (project_id, extracted_at DESC);
CREATE INDEX IF NOT EXISTS idx_extdoc_source
    ON extracted_documents (source_artifact_id);

COMMENT ON TABLE extracted_documents IS
    'Cached output of MIME-keyed extractors over INPUT artifacts (EPUB, PDF, audio, video, ...). Sections live under storage_path/sections/; per-document metadata + outline in JSONB so memory_search can cite source structure.';

ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS derived_from_extracted_document_id TEXT,
    ADD COLUMN IF NOT EXISTS derived_from_section_id            TEXT;

COMMENT ON COLUMN project_memory_chunks.derived_from_extracted_document_id IS
    'Provenance: the extracted_documents.id this chunk was chunked from (NULL for chunks ingested via the legacy markdown-OUTPUT path).';
COMMENT ON COLUMN project_memory_chunks.derived_from_section_id IS
    'Provenance: the section_id within the extracted document; matches an entry in extracted_documents.outline_blob.';
`,
		Down: `
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS derived_from_section_id,
    DROP COLUMN IF EXISTS derived_from_extracted_document_id;

DROP INDEX IF EXISTS idx_extdoc_source;
DROP INDEX IF EXISTS idx_extdoc_project;
DROP TABLE IF EXISTS extracted_documents;
`,
	},
	{
		Version: 46,
		Name:    "tasks_chat_turn_id",
		// Soft link from a task back to the chat-audit row of the
		// dispatcher turn that created it. Lets us answer "which
		// tasks did this turn spawn?" (audit), "did I already create
		// this work in this turn?" (in-conversation dedup), and
		// "have all the tasks this turn awaited terminated?"
		// (follow-up coalescing). Before this column, dispatcher-
		// created tasks were indistinguishable from API-created
		// USER-source root tasks — the only weak link was the
		// in-memory pendingFollowups map plus the task_watchers row.
		//
		// Nullable: API and autonomous-tick tasks have no originating
		// turn. No FK to chat_audit_log because the audit row is
		// inserted at end-of-turn (after tasks may already exist);
		// the soft reference + index is enough for the queries
		// above, and a UTF-8-truncation insert failure on the audit
		// row doesn't orphan the tasks it points at.
		Up: `
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS chat_turn_id TEXT;

CREATE INDEX IF NOT EXISTS idx_tasks_chat_turn_id
    ON tasks (chat_turn_id) WHERE chat_turn_id IS NOT NULL;

COMMENT ON COLUMN tasks.chat_turn_id IS 'Soft FK to chat_audit_log.id — the dispatcher turn that spawned this task. NULL for API/autonomous tasks.';
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_chat_turn_id;
ALTER TABLE tasks
    DROP COLUMN IF EXISTS chat_turn_id;
`,
	},
	{
		Version: 47,
		Name:    "create_llm_response_cache",
		// LLM caching design Phase E — full-response memoization for
		// the memory background trio (memory_titler, memory_classifier,
		// memory_kg_extract). All three call sites have a stable
		// system prompt and deterministic-ish output for a given
		// input; the same chunk replayed by `vornikctl memory
		// reclassify` / `vornikctl memory backfill-titles` today
		// re-pays full input-token cost. Cache key bundles the model
		// + a purpose tag + a SHA-256 of the system + user messages,
		// so an unrelated prompt revision invalidates naturally.
		//
		// purpose is the call-site tag (memory_titler, memory_
		// classifier, memory_kg_extract). Stored separately from the
		// hashed key so stats / eviction can scope by purpose without
		// scanning the whole table. prompt_tokens + completion_tokens
		// preserve the original billed token counts so the dashboard
		// can quote "$ saved by cache" from pricing × hit_count.
		// last_hit_at drives LRU eviction; hit_count surfaces
		// hot-vs-cold rows for operator visibility.
		Up: `
CREATE TABLE IF NOT EXISTS llm_response_cache (
    cache_key         TEXT PRIMARY KEY,
    model             TEXT NOT NULL,
    purpose           TEXT NOT NULL,
    response_content  TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_hit_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    hit_count         BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_llm_response_cache_last_hit
    ON llm_response_cache (last_hit_at);
CREATE INDEX IF NOT EXISTS idx_llm_response_cache_purpose
    ON llm_response_cache (purpose, model);
`,
		Down: `
DROP INDEX IF EXISTS idx_llm_response_cache_purpose;
DROP INDEX IF EXISTS idx_llm_response_cache_last_hit;
DROP TABLE IF EXISTS llm_response_cache;
`,
	},
	{
		Version: 48,
		Name:    "executions_fork_lineage",
		// Failure-forensics Feature #1 Phase B — the fork primitive.
		// Operators picking a step from the replay timeline ("fork
		// from here") spawn a NEW task that starts a new execution at
		// the chosen step. The new execution carries soft FKs back to
		// the source so the replay page can render the lineage chain.
		//
		// parent_execution_id — the source execution being forked from.
		//   Nullable; only forks set it. Soft FK (no constraint) so a
		//   deleted parent doesn't break the fork; the index is partial
		//   since most executions aren't forks.
		// forked_from_step_id — the workflow step the fork starts at.
		//   The executor's pre-step hook reads this to know where to
		//   inject the prompt override on the first iteration.
		// forked_prompt_override — optional operator-supplied prompt
		//   text prepended to the forked step's prompt on the first
		//   iteration. Stored as TEXT (no length cap; the operator
		//   wrote it, they can read it).
		Up: `
ALTER TABLE executions
    ADD COLUMN IF NOT EXISTS parent_execution_id TEXT,
    ADD COLUMN IF NOT EXISTS forked_from_step_id TEXT,
    ADD COLUMN IF NOT EXISTS forked_prompt_override TEXT;

CREATE INDEX IF NOT EXISTS idx_executions_parent
    ON executions (parent_execution_id) WHERE parent_execution_id IS NOT NULL;

COMMENT ON COLUMN executions.parent_execution_id IS
    'Soft FK to the source execution this row was forked from. NULL when not a fork.';
COMMENT ON COLUMN executions.forked_from_step_id IS
    'Workflow step ID the fork starts at. NULL when not a fork.';
COMMENT ON COLUMN executions.forked_prompt_override IS
    'Operator-supplied prompt prefix prepended to the forked step on iteration 1.';
`,
		Down: `
DROP INDEX IF EXISTS idx_executions_parent;
ALTER TABLE executions
    DROP COLUMN IF EXISTS forked_prompt_override,
    DROP COLUMN IF EXISTS forked_from_step_id,
    DROP COLUMN IF EXISTS parent_execution_id;
`,
	},
	{
		Version: 49,
		Name:    "create_project_wizard_sessions",
		// Feature #2 — conversational project setup wizard. Each
		// operator's wizard conversation is one row; the transcript
		// is appended as JSONB so the LLM has the full prior context
		// on every turn. current_proposal carries the latest
		// validated ProjectYAML so the right-pane preview renders
		// without reparsing the entire transcript. committed_project_id
		// + committed_at flip on the commit endpoint; once non-NULL
		// the session is read-only.
		//
		// Operator-scoped via operator_id (free-form so OAuth subjects /
		// Telegram user IDs / API key IDs all fit). The session
		// retention sweeper purges rows older than configurable TTL
		// (default 30 days from updated_at).
		Up: `
CREATE TABLE IF NOT EXISTS project_wizard_sessions (
    id                   TEXT PRIMARY KEY,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    operator_id          TEXT NOT NULL,
    transcript           JSONB NOT NULL DEFAULT '[]'::jsonb,
    current_proposal     JSONB,
    suggested_template   TEXT,
    ready_to_commit      BOOLEAN NOT NULL DEFAULT FALSE,
    committed_project_id TEXT,
    committed_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_pw_sessions_operator
    ON project_wizard_sessions (operator_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_pw_sessions_uncommitted
    ON project_wizard_sessions (updated_at DESC) WHERE committed_project_id IS NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_pw_sessions_uncommitted;
DROP INDEX IF EXISTS idx_pw_sessions_operator;
DROP TABLE IF EXISTS project_wizard_sessions;
`,
	},
	{
		Version: 111,
		Name:    "create_installation_onboarding_sessions",
		// Installation-scoped onboarding for the first-run setup guide.
		// Unlike project_wizard_sessions, this table is global to the
		// daemon install and its committed row is the durable source of
		// truth for "already onboarded".
		Up: `
CREATE TABLE IF NOT EXISTS installation_onboarding_sessions (
    id                   TEXT PRIMARY KEY,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    operator_id          TEXT NOT NULL,
    current_step         TEXT NOT NULL,
    selected_use_case    TEXT NOT NULL,
    transcript           JSONB NOT NULL DEFAULT '[]'::jsonb,
    proposed_config      JSONB,
    proposed_project     JSONB,
    validation_results   JSONB,
    committed_project_id TEXT,
    committed_at         TIMESTAMPTZ,
    cancelled_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_onboarding_sessions_operator
    ON installation_onboarding_sessions (operator_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_onboarding_sessions_committed
    ON installation_onboarding_sessions (updated_at DESC) WHERE committed_project_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_onboarding_sessions_committed;
DROP INDEX IF EXISTS idx_onboarding_sessions_operator;
DROP TABLE IF EXISTS installation_onboarding_sessions;
`,
	},
	{
		Version: 50,
		Name:    "create_execution_hints",
		// Feature #3 Phase C — operator-injected hints. While an
		// execution is live, the operator can POST a hint to
		// /api/v1/executions/{id}/hints and the executor reads
		// any pending rows at the start of the next agent step,
		// prepending them to the user message as
		// <operator-hint>...</operator-hint> blocks. Hints are
		// one-shot — applied_at flips from NULL to NOW() on
		// consume, so retries of the same step don't see them
		// again.
		//
		// step_id NULL means "apply to whichever step runs next";
		// step_id set means "only the named step". Operators use
		// the former for general nudges, the latter to steer a
		// specific upcoming step.
		Up: `
CREATE TABLE IF NOT EXISTS execution_hints (
    id           TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL,
    step_id      TEXT,
    content      TEXT NOT NULL,
    applied_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_execution_hints_pending
    ON execution_hints (execution_id, applied_at)
    WHERE applied_at IS NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_execution_hints_pending;
DROP TABLE IF EXISTS execution_hints;
`,
	},
	{
		Version: 52,
		Name:    "create_cross_project_orchestration",
		// Inter-project orchestration Phase A (LLD:
		// https://docs.vornik.io).
		//
		// cross_project_calls is the durable ledger of every
		// call_project step: who called whom with what payload,
		// what envelope came back, and (for timed-out / rejected
		// rows) why. Status transitions:
		//   pending → running   when callee task is leased
		//   running → completed when callee terminates COMPLETED
		//   running → failed    when callee terminates FAILED/CANCELLED
		//   any     → rejected  when validation or acceptCallsFrom blocks
		//   any     → timed_out when timeout scanner fires
		//
		// schema_registry holds the JSON-Schema bodies callers
		// declare via `expect.schema`. Read at boot + SIGHUP from
		// configs/schemas/*.json; the runtime never writes to it
		// (write path is the file system). Kept as a table so we
		// can index + cross-reference from audit rows.
		//
		// tasks.cross_project_call_id closes the loop: a callee
		// task carries its CPC id so the executor's terminal-
		// status handler can resolve the matching CPC row.
		// tasks.result_envelope stores the validated outcome JSON
		// — used by the caller to resume with structured data.
		Up: `
CREATE TABLE IF NOT EXISTS cross_project_calls (
    id              TEXT PRIMARY KEY,
    caller_task_id  TEXT NOT NULL,
    caller_step_id  TEXT NOT NULL,
    caller_project  TEXT NOT NULL,
    callee_project  TEXT NOT NULL,
    callee_workflow TEXT NOT NULL,
    callee_task_id  TEXT,
    payload         JSONB NOT NULL,
    expected_schema TEXT NOT NULL,
    status          TEXT NOT NULL,
    result_envelope JSONB,
    error_message   TEXT,
    timeout_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_cpc_caller       ON cross_project_calls (caller_task_id);
CREATE INDEX IF NOT EXISTS idx_cpc_callee       ON cross_project_calls (callee_task_id);
CREATE INDEX IF NOT EXISTS idx_cpc_pending      ON cross_project_calls (status, timeout_at)
    WHERE status IN ('pending', 'running');

CREATE TABLE IF NOT EXISTS schema_registry (
    id          TEXT PRIMARY KEY,
    body        JSONB NOT NULL,
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS cross_project_call_id TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS result_envelope JSONB;
CREATE INDEX IF NOT EXISTS idx_tasks_cpc ON tasks (cross_project_call_id)
    WHERE cross_project_call_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_cpc;
ALTER TABLE tasks DROP COLUMN IF EXISTS result_envelope;
ALTER TABLE tasks DROP COLUMN IF EXISTS cross_project_call_id;
DROP TABLE IF EXISTS schema_registry;
DROP INDEX IF EXISTS idx_cpc_pending;
DROP INDEX IF EXISTS idx_cpc_callee;
DROP INDEX IF EXISTS idx_cpc_caller;
DROP TABLE IF EXISTS cross_project_calls;
`,
	},
	{
		Version: 53,
		Name:    "create_project_spawns",
		// Inter-project orchestration Phase B — spawn_project step
		// (LLD: https://docs.vornik.io
		// design.md §6.2). One row per materialised spawn; preserves
		// the parent-task lineage even after the spawned project
		// is deleted (project_spawns row stays as audit history).
		//
		// UNIQUE on spawned_project enforces idempotence at the DB
		// layer: re-running the same spawn_project step (retry-from-
		// step, scheduler recovery) can't double-create a project.
		// The executor handler short-circuits before the INSERT
		// when it observes an existing row; this is the safety net.
		//
		// idx_ps_per_day powers the maxSpawnsPerDay rate-limit:
		// SELECT COUNT(*) WHERE parent_project = X AND created_at >
		// NOW() - INTERVAL '1 day'. The partial index keeps the
		// scan cheap as historical rows accumulate.
		Up: `
CREATE TABLE IF NOT EXISTS project_spawns (
    id                TEXT PRIMARY KEY,
    parent_task_id    TEXT NOT NULL,
    parent_project    TEXT NOT NULL,
    parent_step_id    TEXT NOT NULL,
    spawned_project   TEXT NOT NULL UNIQUE,
    template_slug     TEXT NOT NULL,
    params            JSONB NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ps_parent    ON project_spawns (parent_task_id);
CREATE INDEX IF NOT EXISTS idx_ps_per_day   ON project_spawns (parent_project, created_at DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_ps_per_day;
DROP INDEX IF EXISTS idx_ps_parent;
DROP TABLE IF EXISTS project_spawns;
`,
	},
	{
		Version: 54,
		Name:    "cross_project_calls_cancel_on_timeout",
		// Inter-project orchestration Phase D follow-on — the
		// per-call cascade-cancel flag (LLD §8.1). When true,
		// the timeout scanner cancels the callee task in
		// addition to resolving the CPC. Default false
		// preserves the v1 "callee work may still be useful"
		// semantic.
		Up: `
ALTER TABLE cross_project_calls ADD COLUMN IF NOT EXISTS cancel_on_timeout BOOLEAN NOT NULL DEFAULT FALSE;
`,
		Down: `
ALTER TABLE cross_project_calls DROP COLUMN IF EXISTS cancel_on_timeout;
`,
	},
	{
		Version: 55,
		Name:    "dispatcher_reminders",
		// Scheduled reminders / dispatcher heartbeat (LLD §3).
		// Durable record of "remind operator at time T". The
		// reminders package polls WHERE status='pending' AND
		// fire_at <= NOW() every 30s; a partial index on
		// (fire_at) filtered to pending rows keeps the scanner
		// query cheap as terminal rows accumulate.
		//
		// Status enum:
		//   pending   → not yet due, waiting on the clock
		//   firing    → leased by a heartbeat tick; intermediate
		//               state distinguishing "claimed" from
		//               "in DB awaiting claim"
		//   fired     → delivered successfully via channel.Send
		//   cancelled → operator-cancelled before fire
		//   expired   → past fire_at but channel unavailable;
		//               terminal for v1 (no retry policy yet)
		Up: `
CREATE TABLE IF NOT EXISTS dispatcher_reminders (
    id            TEXT PRIMARY KEY,
    operator_id   TEXT NOT NULL,
    channel       TEXT NOT NULL,
    channel_ref   TEXT NOT NULL,
    project_id    TEXT,
    fire_at       TIMESTAMPTZ NOT NULL,
    content       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    fired_at      TIMESTAMPTZ,
    cancelled_at  TIMESTAMPTZ,
    created_via   TEXT NOT NULL DEFAULT 'chat',
    error_count   INT NOT NULL DEFAULT 0,
    last_error    TEXT,
    CONSTRAINT dispatcher_reminders_status_check
        CHECK (status IN ('pending','firing','fired','cancelled','expired'))
);
CREATE INDEX IF NOT EXISTS idx_dispatcher_reminders_fire_at
    ON dispatcher_reminders (fire_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_dispatcher_reminders_operator
    ON dispatcher_reminders (operator_id, status);
CREATE INDEX IF NOT EXISTS idx_dispatcher_reminders_project
    ON dispatcher_reminders (project_id, status, fire_at)
    WHERE project_id IS NOT NULL;
`,
		Down: `
DROP TABLE IF EXISTS dispatcher_reminders;
`,
	},
	{
		Version: 56,
		Name:    "execution_step_outcomes_context_source",
		// Context-discovery hardening LLD Phase B. Records which
		// canonical-context convention resolved at workspace prep
		// for the step. Operators query the column to spot
		// projects still on the legacy `autonomy/` layout
		// (plain_autonomy) vs migrated (`dot_autonomy`) vs
		// half-migrated (mixed). Nullable so legacy rows + steps
		// where the convention isn't used carry NULL rather than
		// a misleading "active" sentinel.
		Up: `
ALTER TABLE execution_step_outcomes ADD COLUMN IF NOT EXISTS context_source TEXT;
`,
		Down: `
ALTER TABLE execution_step_outcomes DROP COLUMN IF EXISTS context_source;
`,
	},
	{
		Version: 57,
		Name:    "daemon_leader_locks",
		// Horizontal-scaling prep for 2026.8.0 (BACKLOG MUST-HAVE).
		// Per-worker singleton lock table: only the holding daemon
		// runs the worker; other replicas skip their tick.
		//
		// Schema choices:
		//   - worker_id PK so an INSERT … ON CONFLICT DO UPDATE
		//     can atomically take over an expired lock without a
		//     second round trip.
		//   - expires_at drives takeover: any acquirer whose
		//     conditional UPDATE matches a row with
		//     expires_at < NOW() wins. The departing leader's
		//     Release sets expires_at = NOW() explicitly so
		//     successors don't have to wait the full TTL.
		//   - renewed_at is informational — operators query it
		//     when a worker stops making progress to see whether
		//     its leader is still ticking.
		//   - No foreign keys: a daemon-instance identifier
		//     isn't a first-class table. holder_id is a string
		//     of the operator's choosing (typically
		//     hostname+pid+boot-uuid).
		Up: `
CREATE TABLE IF NOT EXISTS daemon_leader_locks (
    worker_id   TEXT PRIMARY KEY,
    holder_id   TEXT NOT NULL,
    acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    renewed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_daemon_leader_locks_expires
    ON daemon_leader_locks (expires_at);
`,
		Down: `
DROP TABLE IF EXISTS daemon_leader_locks;
`,
	},
	{
		Version: 58,
		Name:    "channel_sessions",
		// Horizontal-scaling prep (BACKLOG MUST-HAVE follow-on to
		// leader-election). Per-channel session state — conversation
		// history + active project — moves out of per-process maps
		// into Postgres so a rolling restart no longer drops user
		// conversations and replicas can pick up traffic seamlessly.
		//
		// Single table for all channel kinds (webchat / email / slack
		// / github / future). The (kind, session_id) composite PK
		// keeps the namespaces isolated — webchat's cookie hash and
		// email's RFC822 message-id can't collide.
		//
		// Schema choices:
		//   - history is JSONB so a Postgres consumer (admin UI,
		//     analytics) can inspect or filter without parsing.
		//     chat.Message round-trips through encoding/json.
		//   - active_project may be NULL (channel hasn't pinned a
		//     project yet — e.g. webchat /chat landing).
		//   - updated_at is indexed for the future stale-session
		//     sweeper. expires_at is a per-row override (NULL =
		//     no per-session TTL, fall back to channel default).
		Up: `
CREATE TABLE IF NOT EXISTS channel_sessions (
    kind           TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    active_project TEXT,
    history        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ,
    PRIMARY KEY (kind, session_id)
);
CREATE INDEX IF NOT EXISTS idx_channel_sessions_updated
    ON channel_sessions (kind, updated_at);
CREATE INDEX IF NOT EXISTS idx_channel_sessions_expires
    ON channel_sessions (expires_at) WHERE expires_at IS NOT NULL;
`,
		Down: `
DROP TABLE IF EXISTS channel_sessions;
`,
	},
	{
		Version: 59,
		Name:    "execution_live_events",
		// Cross-replica live-execution fanout (horizontal-scaling
		// follow-on). Per-execution event log that lets a replica
		// other than the one running the execution serve
		// /api/v1/executions/{id}/live without losing events. The
		// emitting replica also fires Postgres NOTIFY on the
		// vornik_live channel so listening replicas can stream
		// events to their local subscribers in sub-millisecond
		// time; persistence here is for replay (late subscribers,
		// post-restart reconnect, audit).
		//
		// Schema choices:
		//   - id BIGSERIAL is the global primary key for stable
		//     pagination on the future audit / debug views.
		//   - seq is the per-execution monotonic counter the wire
		//     format already exposes; clients use it for gap
		//     detection. Unique (execution_id, seq) keeps the
		//     constraint in the DB so a race between two
		//     attempted publishes can't produce duplicate seqs
		//     (leader-election should prevent this anyway, but
		//     belt-and-suspenders).
		//   - payload is JSONB so post-mortem queries can drill
		//     into specific event shapes without parsing.
		//   - The created_at index drives the future stale-event
		//     sweeper (events on completed executions can be
		//     dropped after, say, 7 days — not part of this
		//     commit).
		Up: `
CREATE TABLE IF NOT EXISTS execution_live_events (
    id           BIGSERIAL PRIMARY KEY,
    execution_id TEXT NOT NULL,
    seq          BIGINT NOT NULL,
    kind         TEXT NOT NULL,
    payload      JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_live_events_execution_seq
    ON execution_live_events (execution_id, seq);
CREATE INDEX IF NOT EXISTS idx_live_events_created
    ON execution_live_events (created_at);
`,
		Down: `
DROP TABLE IF EXISTS execution_live_events;
`,
	},
	{
		Version: 60,
		Name:    "operator_profile",
		// Per-operator persistent profile (roadmapped under
		// "Later — Per-operator persistent profile / memory").
		// Read-path-first slice: schema + repo + dispatcher
		// prompt injection. Write tool deferred to a follow-up.
		//
		// Schema choices:
		//   - operator_id PK so a single SELECT covers the
		//     dispatcher's per-turn read. operator_id is the
		//     stable "<channel>:<channel-specific-id>" string
		//     ("telegram:42", "webchat:abc123") that the
		//     session-store layer already exposes.
		//   - structured JSONB so the dispatcher can read
		//     well-known keys (`tone`, `verbosity`,
		//     `time_zone`, `communication_style`,
		//     `preferred_channel`) without LLM parsing.
		//   - notes TEXT for free-form context the assistant
		//     accumulates over time.
		//   - operator_identity_link maps platform user ids to
		//     a single canonical operator so a cross-channel
		//     /link command consolidates the profile space.
		//     Both columns indexed for the lookup direction
		//     the consolidator needs.
		Up: `
CREATE TABLE IF NOT EXISTS operator_profile (
    operator_id   TEXT PRIMARY KEY,
    structured    JSONB NOT NULL DEFAULT '{}'::jsonb,
    notes         TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS operator_identity_link (
    platform_id   TEXT NOT NULL,
    operator_id   TEXT NOT NULL,
    linked_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (platform_id)
);
CREATE INDEX IF NOT EXISTS idx_operator_identity_link_operator
    ON operator_identity_link (operator_id);
`,
		Down: `
DROP TABLE IF EXISTS operator_identity_link;
DROP TABLE IF EXISTS operator_profile;
`,
	},
	{
		Version: 61,
		Name:    "telegram_poller_state",
		// Cluster-deploy follow-on to leader-election (migration 57).
		// The telegram_poller leader-lock now gates the long-poll
		// loop so only one replica calls getUpdates. This table
		// persists the confirmed offset across replica failover —
		// without it, the new leader would start at offset=0 and
		// Telegram would replay every queued update (bounded ~24h
		// but visibly duplicated for the user).
		//
		// Schema choices:
		//   - bot_id PK so single-bot deployments (the common case)
		//     have one row. Multi-bot deployments (one daemon
		//     proxying multiple BotFather tokens) keyed by
		//     @username or operator-supplied label.
		//   - offset_value (not "offset" — reserved word in many
		//     SQL parsers and a footgun in ad-hoc queries) is
		//     int64; matches Telegram's confirmed-offset
		//     semantics.
		//   - The Set upsert has a guard (only advance, never
		//     rewind) so a stale write from a deposed leader
		//     can't reset a successor's progress. See
		//     telegram_poller_state_repository.go.
		Up: `
CREATE TABLE IF NOT EXISTS telegram_poller_state (
    bot_id       TEXT PRIMARY KEY,
    offset_value BIGINT NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`,
		Down: `
DROP TABLE IF EXISTS telegram_poller_state;
`,
	},
	{
		Version: 62,
		Name:    "task_creation_source_a2a",
		// Adds the 'A2A' value to the task_creation_source enum so
		// tasks submitted via the new A2A protocol surface
		// (POST /a2a/v1/agents/<p>/<wf>/tasks) record their
		// provenance distinctly from human-driven REST submissions.
		// See https://docs.vornik.io
		Up: `
ALTER TYPE task_creation_source ADD VALUE IF NOT EXISTS 'A2A';
`,
		// No-op down: removing an enum value would orphan rows
		// (same convention as migration 30 / ROUTE).
		Down: ``,
	},
	{
		Version: 63,
		Name:    "operator_identity_link_schema_align",
		// Aligns the operator_identity_link schema with the
		// Phase-A repo shape: platform_id → channel_speaker_id
		// rename + linked_by column add. The original migration
		// 60 created the table speculatively before the repo
		// existed; the repo (Phase A) needs the explicit
		// channel namespace + audit trail of who authorised
		// the link (self / cli / auto).
		// See https://docs.vornik.io
		Up: `
ALTER TABLE operator_identity_link
    RENAME COLUMN platform_id TO channel_speaker_id;
ALTER TABLE operator_identity_link
    ADD COLUMN IF NOT EXISTS linked_by TEXT NOT NULL DEFAULT 'self';
`,
		Down: `
ALTER TABLE operator_identity_link
    DROP COLUMN IF EXISTS linked_by;
ALTER TABLE operator_identity_link
    RENAME COLUMN channel_speaker_id TO platform_id;
`,
	},
	{
		Version: 64,
		Name:    "profile_use_audit",
		// Per-turn audit log of operator-profile use. One row
		// per chat turn whose dispatcher injected a non-empty
		// <operator_profile> block. Used by `vornikctl operator
		// audit <id>` so operators can answer "when did the
		// model start citing my 'prefers Czech' preference, and
		// is that the right call?". Schema is deliberately
		// thin: keys is JSONB so the validator allow-list can
		// grow without a schema change; used_notes is a
		// boolean so the consumer doesn't need to crack open
		// the keys list to spot "the assistant relied on my
		// free-form note this turn".
		// See https://docs.vornik.io (Phase B).
		Up: `
CREATE TABLE IF NOT EXISTS profile_use_audit (
    id          BIGSERIAL PRIMARY KEY,
    operator_id TEXT NOT NULL,
    task_id     TEXT,
    used_keys   JSONB NOT NULL,
    used_notes  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_profile_use_audit_operator_time
    ON profile_use_audit (operator_id, created_at DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_profile_use_audit_operator_time;
DROP TABLE IF EXISTS profile_use_audit;
`,
	},
	{
		Version: 65,
		Name:    "workflow_proposals",
		// Slice 2 of the self-evolving-workflows arc (codename
		// "memetic workflows"). The architect agent reads the
		// per-workflow telemetry rollup (Slice 1, commit d0fa66d)
		// and proposes structural YAML edits. Each proposal lands
		// here for operator review; the apply path (Slice 4)
		// flips status to 'applied' + stamps the git commit.
		//
		// Status lifecycle (state machine pinned by the
		// repository methods, not by a CHECK constraint, so future
		// status additions don't require a migration):
		//
		//   pending     → architect emitted, awaiting operator decision
		//   approved    → operator approved; apply path will run next tick
		//   rejected    → operator rejected; terminal
		//   applied     → apply path succeeded; applied_commit stamped
		//   rolled_back → operator reverted via UI; rollback_commit stamped
		//   regressed   → post-apply auto-flag (Slice 5) detected
		//                 failure-rate spike vs baseline
		//
		// evidence_run_ids is a TEXT[] (the architect MUST cite
		// ≥3 execution IDs that motivated the proposal —
		// enforced at insertion). proposal_yaml carries the full
		// proposed workflow YAML (not a diff); the UI computes
		// the diff against the current file at render time so
		// the operator sees what changed.
		Up: `
CREATE TABLE IF NOT EXISTS workflow_proposals (
    id               TEXT PRIMARY KEY,
    workflow_id      TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    proposal_yaml    TEXT NOT NULL,
    motivation       TEXT NOT NULL,
    evidence_run_ids TEXT[] NOT NULL,
    confidence       REAL NOT NULL,
    architect_model  TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at       TIMESTAMPTZ,
    decided_by       TEXT,
    applied_at       TIMESTAMPTZ,
    applied_commit   TEXT,
    rollback_commit  TEXT,
    notes            TEXT
);

CREATE INDEX IF NOT EXISTS idx_workflow_proposals_status
    ON workflow_proposals (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_workflow_proposals_workflow
    ON workflow_proposals (workflow_id, created_at DESC);

-- One pending proposal per workflow at any time (rate-limit
-- enforced at insertion). Partial unique index lets the
-- application layer treat this as a "first writer wins" race.
CREATE UNIQUE INDEX IF NOT EXISTS uq_workflow_proposals_pending
    ON workflow_proposals (workflow_id)
    WHERE status = 'pending';
`,
		Down: `
DROP INDEX IF EXISTS uq_workflow_proposals_pending;
DROP INDEX IF EXISTS idx_workflow_proposals_workflow;
DROP INDEX IF EXISTS idx_workflow_proposals_status;
DROP TABLE IF EXISTS workflow_proposals;
`,
	},
	{
		Version: 66,
		Name:    "execution_hints_task_scope",
		// 2026-05-26: task-level hint scope. Pre-fix, hints were
		// scoped to execution_id only — when a task was requeued
		// (new execution_id), any hints left on the prior execution
		// were orphaned, so operators steering a task across
		// retries had to re-submit after every failure. Adding a
		// nullable task_id lets operators post hints to the TASK
		// instead, and any subsequent execution's first step
		// consumes them.
		//
		// Existing rows keep task_id NULL; the executor's
		// ConsumePending path falls back cleanly when no task_id
		// exists. The CHECK enforces "at least one scope set" so
		// we don't accidentally accept a row with neither.
		Up: `
ALTER TABLE execution_hints
    ADD COLUMN IF NOT EXISTS task_id TEXT;

ALTER TABLE execution_hints
    ALTER COLUMN execution_id DROP NOT NULL;

ALTER TABLE execution_hints
    ADD CONSTRAINT execution_hints_scope_chk
    CHECK (task_id IS NOT NULL OR execution_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_execution_hints_task_pending
    ON execution_hints (task_id, applied_at)
    WHERE applied_at IS NULL AND task_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_execution_hints_task_pending;
ALTER TABLE execution_hints DROP CONSTRAINT IF EXISTS execution_hints_scope_chk;
ALTER TABLE execution_hints ALTER COLUMN execution_id SET NOT NULL;
ALTER TABLE execution_hints DROP COLUMN IF EXISTS task_id;
`,
	},
	{
		Version: 67,
		Name:    "dispatcher_reminders_cron",
		// Natural-language cron extension for dispatcher_reminders
		// (roadmap "Later — Natural-language reminder + cron
		// scheduling"). Recurring reminders re-arm on fire instead
		// of going terminal — the runner stamps a fresh fire_at
		// computed from cron_expr and flips status back to pending.
		// recurrence_until bounds the loop (NULL = unbounded).
		//
		// Both columns nullable + additive: pre-67 rows stay one-
		// shot with cron_expr=NULL, and the existing partial index
		// on (fire_at) WHERE status='pending' still covers re-armed
		// rows because they return to pending after each fire.
		Up: `
ALTER TABLE dispatcher_reminders
    ADD COLUMN IF NOT EXISTS cron_expr TEXT;
ALTER TABLE dispatcher_reminders
    ADD COLUMN IF NOT EXISTS recurrence_until TIMESTAMPTZ;
`,
		Down: `
ALTER TABLE dispatcher_reminders DROP COLUMN IF EXISTS recurrence_until;
ALTER TABLE dispatcher_reminders DROP COLUMN IF EXISTS cron_expr;
`,
	},
	{
		Version: 68,
		Name:    "blackbox_trace_cache",
		// Autonomy Black Box Phase A — read-side assembled-trace
		// memoization. Each row is the canonicalised payload of
		// one task's unified trace assembled from the 9 audit
		// tables (task_messages, tool_audit_log, task_llm_usage,
		// executions, execution_step_outcomes, task_judge_verdicts,
		// memory_retrieval_audit, profile_use_audit, chat_audit_log).
		//
		// The trace_digest is a sha256 over the canonicalised event
		// sequence — two operators reading the same trace get the
		// same hash to cite in tickets. expires_at is the
		// background-eviction marker (24h TTL by default); a
		// schema-version bump invalidates all rows lazily via a
		// constant baked into the digest.
		Up: `
CREATE TABLE IF NOT EXISTS blackbox_trace_cache (
    task_id      TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    trace_digest TEXT NOT NULL,
    schema_ver   INTEGER NOT NULL DEFAULT 1,
    payload      JSONB NOT NULL,
    assembled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_blackbox_trace_cache_project
    ON blackbox_trace_cache (project_id, assembled_at DESC);
CREATE INDEX IF NOT EXISTS idx_blackbox_trace_cache_expires
    ON blackbox_trace_cache (expires_at)
    WHERE expires_at IS NOT NULL;
`,
		Down: `
DROP TABLE IF EXISTS blackbox_trace_cache;
`,
	},
	{
		Version: 69,
		Name:    "workflow_healing_triggers",
		// Autonomy Black Box Phase B — self-healing detector
		// output. The Phase A folded-in detector
		// (internal/blackbox/detector.go) compares the last
		// 24h's run roll-up against a 7-day baseline and writes
		// a row here when failure-rate or cost regresses
		// beyond threshold.
		//
		// The trigger row is "operator triage state" — open by
		// default, transitioned to dismissed by the operator or
		// generated_candidate when fed to the memetic architect.
		// proposal_id stays nullable until the operator clicks
		// "generate candidate" in the UI, at which point the
		// resulting workflow proposal's ID is stamped here.
		//
		// The partial unique index enforces "only one open
		// trigger per (project, workflow, class)" so the
		// hourly detector tick doesn't pile up duplicates while
		// the operator is reviewing.
		Up: `
CREATE TABLE IF NOT EXISTS workflow_healing_triggers (
    id                     TEXT PRIMARY KEY,
    project_id             TEXT NOT NULL,
    workflow_id            TEXT NOT NULL,
    trigger_class          TEXT NOT NULL,
    baseline_start         TIMESTAMPTZ NOT NULL,
    baseline_end           TIMESTAMPTZ NOT NULL,
    comparison_start       TIMESTAMPTZ NOT NULL,
    comparison_end         TIMESTAMPTZ NOT NULL,
    metric_name            TEXT NOT NULL,
    baseline_value         DOUBLE PRECISION NOT NULL,
    comparison_value       DOUBLE PRECISION NOT NULL,
    threshold_value        DOUBLE PRECISION NOT NULL,
    evidence_execution_ids TEXT[] NOT NULL DEFAULT '{}',
    status                 TEXT NOT NULL DEFAULT 'open',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at            TIMESTAMPTZ,
    proposal_id            TEXT,
    CONSTRAINT workflow_healing_triggers_status_check
        CHECK (status IN ('open','dismissed','generated_candidate'))
);
CREATE INDEX IF NOT EXISTS idx_healing_triggers_status
    ON workflow_healing_triggers (status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_healing_triggers_workflow
    ON workflow_healing_triggers (project_id, workflow_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_healing_triggers_open
    ON workflow_healing_triggers (project_id, workflow_id, trigger_class)
    WHERE status = 'open';
`,
		Down: `
DROP TABLE IF EXISTS workflow_healing_triggers;
`,
	},
	{
		Version: 70,
		Name:    "api_keys_companion_scope",
		// Companion-plugin scope columns (LLD 21). Adds four nullable
		// fields to api_keys so an admin-minted key can declare:
		//   - allowed_workflows: JSON array of workflow IDs the key
		//     may invoke via delegate(); NULL means "every workflow
		//     the project permits". Stored as TEXT (JSON-encoded)
		//     rather than TEXT[] to keep the schema portable to the
		//     SQLite mirror in internal/persistence/sqlite/schema.go.
		//   - budget_cap_usd: lifetime ceiling for this key's
		//     delegated work; NULL means uncapped. Cost-aware MCP
		//     server reads this before accepting a delegate() call.
		//   - client_kind: free-form label for the host client
		//     ("claude-code", "codex", "opencode", "gemini-cli").
		//     Used to filter list/audit views; NULL on
		//     non-companion keys leaves the existing
		//     /api/v1/admin/audit surface unchanged.
		//   - session_label: operator-friendly note ("vadim/laptop").
		//     Pure UX; never authoritative.
		//
		// All columns nullable + default unset → migration is a
		// pure additive change. Existing key-issue and lookup paths
		// keep working without code awareness of the new columns
		// (they're surfaced only when the companion grant handler
		// or the companion-aware MCP server explicitly reads them).
		Up: `
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS allowed_workflows TEXT,
    ADD COLUMN IF NOT EXISTS budget_cap_usd    NUMERIC,
    ADD COLUMN IF NOT EXISTS client_kind       TEXT,
    ADD COLUMN IF NOT EXISTS session_label     TEXT;
CREATE INDEX IF NOT EXISTS idx_api_keys_client_kind
    ON api_keys (client_kind) WHERE client_kind IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_api_keys_client_kind;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS session_label,
    DROP COLUMN IF EXISTS client_kind,
    DROP COLUMN IF EXISTS budget_cap_usd,
    DROP COLUMN IF EXISTS allowed_workflows;
`,
	},
	{
		Version: 71,
		Name:    "task_creation_source_companion",
		// Adds the 'COMPANION' value to the task_creation_source
		// enum so tasks delegated by a host-LLM companion plugin
		// (LLD 21) record their provenance distinctly from A2A.
		// Same no-op Down convention as migrations 30 / 62 (we
		// never remove enum values — they'd orphan existing rows).
		Up: `
ALTER TYPE task_creation_source ADD VALUE IF NOT EXISTS 'COMPANION';
`,
		Down: ``,
	},
	{
		Version: 72,
		Name:    "companion_memory_capabilities",
		// LLD 22: companion RAG (recall + remember). Two adjacent
		// changes that ship together:
		//
		//   1. api_keys.memory_read / api_keys.memory_write — per-key
		//      grants for the new `recall` / `remember` MCP tools.
		//      Default false so existing companion keys cannot
		//      escalate just because the daemon was upgraded.
		//
		//   2. memory_retrieval_audit.actor_kind / actor_id — splits
		//      "who issued this search" into an indexable agent vs
		//      companion dimension. Existing `role` column stays
		//      populated for backwards-compat; the new columns let
		//      "show me every companion-origin recall this week" be
		//      a cheap index-backed query without LIKE 'companion:%'.
		//
		// No project_memory_chunks change — companion-origin chunks
		// carry their provenance in source_name (LLD 22 §Provenance).
		// No project_ingest_queue / project_memory_quarantine change —
		// the existing producer_role text column carries the
		// "companion:<client_kind>" prefix unchanged.
		Up: `
ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS memory_read  BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS memory_write BOOLEAN NOT NULL DEFAULT FALSE;

-- Fresh-install safety: memory_retrieval_audit lives only in
-- deployments/postgres/schema/001_initial.sql (no prior migration
-- creates it). On a deployment that bootstrapped via the migration
-- runner WITHOUT sourcing 001_initial.sql, the ALTER below would
-- error with "relation memory_retrieval_audit does not exist" —
-- silently aborting the rest of the migration chain. CREATE TABLE
-- IF NOT EXISTS makes this migration self-healing without changing
-- behaviour on installs that did source the bootstrap SQL.
-- (Broader fix: a fresh-install integration test that runs
-- migrations from scratch — see https://docs.vornik.io.)
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
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_project_time
    ON memory_retrieval_audit (project_id, retrieved_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_chunks
    ON memory_retrieval_audit USING gin (chunk_ids);
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_execution
    ON memory_retrieval_audit (execution_id) WHERE execution_id IS NOT NULL;

ALTER TABLE memory_retrieval_audit
    ADD COLUMN IF NOT EXISTS actor_kind TEXT,
    ADD COLUMN IF NOT EXISTS actor_id   TEXT;
`,
		Down: `
ALTER TABLE memory_retrieval_audit
    DROP COLUMN IF EXISTS actor_id,
    DROP COLUMN IF EXISTS actor_kind;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS memory_write,
    DROP COLUMN IF EXISTS memory_read;
`,
	},
	{
		Version: 74,
		Name:    "memory_ingest_audit_companion_direct_deposits",
		// LLD-22 § "Schema and DB changes" intentionally chose to
		// reuse `project_ingest_queue.producer_role` +
		// `project_memory_quarantine.producer_role` for ingest-side
		// provenance ("No project_ingest_queue / project_memory_quarantine
		// changes (producer_role already exists)"). That contract
		// holds for AGENT deposits — they go through the queue, so
		// `producer_role` + the queue's lifecycle columns capture
		// who/when/decision.
		//
		// COMPANION-direct deposits via Indexer.IngestCompanionNote
		// bypass the queue: they run the gate stack synchronously and
		// land chunks (or quarantine, or get rejected) inline. The
		// queue therefore never sees them — confirmed against live DB
		// 2026-05-27: zero rows in project_ingest_queue with
		// producer_role LIKE 'companion:%' despite a successful
		// companion ingest path.
		//
		// Net effect: REJECTED companion deposits leave NO trace at
		// all (no chunk row, no quarantine row, no queue row). For
		// SaaS / multi-tenant compliance the operator needs a
		// per-call ingest audit: "key X deposited content-hash Y at
		// time T; the gate stack returned decision D". This table
		// is that record.
		//
		// Append-only. Low volume relative to chunks (one row per
		// remember() call, not per chunk). Indexed for the two
		// dashboard queries: "recent activity by project" and
		// "what has key K deposited".
		Up: `
CREATE TABLE IF NOT EXISTS memory_ingest_audit (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    actor_kind      TEXT,           -- "companion:<client_kind>", "agent", NULL for legacy
    actor_id        TEXT,           -- api_keys.id for companion; role name for agent
    source_name     TEXT NOT NULL,
    content_hash    TEXT NOT NULL,
    content_bytes   BIGINT NOT NULL,
    proposed_class  TEXT,
    decision        TEXT NOT NULL,  -- 'admitted' | 'quarantined' | 'rejected'
    gate_failed     TEXT,           -- populated when decision <> 'admitted'
    chunks_admitted INTEGER NOT NULL DEFAULT 0,
    ingested_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT memory_ingest_audit_decision_chk
        CHECK (decision IN ('admitted','quarantined','rejected'))
);
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_project_time
    ON memory_ingest_audit (project_id, ingested_at DESC);
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_actor
    ON memory_ingest_audit (actor_id)
    WHERE actor_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_memory_ingest_audit_actor;
DROP INDEX IF EXISTS idx_memory_ingest_audit_project_time;
DROP TABLE IF EXISTS memory_ingest_audit;
`,
	},
	{
		Version: 73,
		Name:    "retrieval_audit_actor_kind_companion_client_suffix",
		// LLD-22 §"Provenance and audit" specifies actor_kind on
		// memory_retrieval_audit as "companion:<client_kind>" so
		// dashboards can split recalls by host LLM (claude-code,
		// codex, gemini-cli, opencode, …). The 2026.5.x companion
		// MCP server shipped writing bare "companion" — every recall
		// audit row from migration 72 forward landed with no client
		// suffix, leaving any LLD-spec-format filter empty.
		//
		// Code is fixed in the same release this migration ships in;
		// this migration retags historical rows by joining on
		// actor_id (which is api_keys.id for companion rows).
		// Legacy keys with a NULL/empty client_kind keep the bare
		// "companion" tag so the fallback contract stays explicit.
		//
		// Idempotent: re-running the UPDATE on an already-suffixed
		// row is a no-op because the WHERE clause requires
		// actor_kind = 'companion' exactly.
		Up: `
UPDATE memory_retrieval_audit r
   SET actor_kind = 'companion:' || k.client_kind
  FROM api_keys k
 WHERE r.actor_kind = 'companion'
   AND r.actor_id   = k.id
   AND k.client_kind IS NOT NULL
   AND k.client_kind <> '';
`,
		// Down: no-op. Reverting the retag would require remembering
		// the original bare-companion rows; the cost is keeping
		// downgrade-then-upgrade-then-downgrade idempotent. Since the
		// retag is semantically a fix (the LLD-22 spec was always
		// "companion:<client_kind>"), reverting just resurrects the
		// drift and is intentionally not supported. Operators rolling
		// back should run their own SQL.
		Down: ``,
	},
	{
		Version: 75,
		Name:    "repo_scope_on_memory_tables",
		// Adds repo_scope (text, nullable) to the four memory-side
		// tables so a single project's RAG can be partitioned by the
		// repo / software / deployment the deposit pertains to. Solves
		// the "one operator, many repos, shared RAG" pollution problem
		// where VORNIK LLDs dilute N8N or OpenPlatform recall results.
		//
		// Semantics:
		// - NULL = "uncategorized" (pre-migration data, deposits that
		//   couldn't resolve a scope).
		// - "*" = "cross-cutting" — surfaces in every scoped query.
		// - any other string = a scope token, typically the git remote
		//   URL's <host>/<path> form or the repo basename.
		//
		// recall() with no scope filter returns everything (project-
		// wide); with a scope filter returns (matching scope OR '*' OR
		// NULL). The NULL surfacing keeps legacy chunks discoverable
		// during the transition window; a bulk-retag CLI lands later.
		//
		// Depends on migration 74 (memory_ingest_audit must exist
		// before we add a column to it).
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS repo_scope TEXT;
CREATE INDEX IF NOT EXISTS idx_memory_chunks_project_scope
    ON project_memory_chunks (project_id, repo_scope, created_at DESC);

ALTER TABLE project_memory_quarantine
    ADD COLUMN IF NOT EXISTS repo_scope TEXT;
CREATE INDEX IF NOT EXISTS idx_quarantine_project_scope
    ON project_memory_quarantine (project_id, repo_scope, quarantined_at DESC)
    WHERE released_at IS NULL AND dropped_at IS NULL;

ALTER TABLE memory_ingest_audit
    ADD COLUMN IF NOT EXISTS repo_scope TEXT;
CREATE INDEX IF NOT EXISTS idx_memory_ingest_audit_project_scope
    ON memory_ingest_audit (project_id, repo_scope, ingested_at DESC);

ALTER TABLE memory_retrieval_audit
    ADD COLUMN IF NOT EXISTS repo_scope TEXT;
CREATE INDEX IF NOT EXISTS idx_memory_retrieval_audit_project_scope
    ON memory_retrieval_audit (project_id, repo_scope, retrieved_at DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_memory_retrieval_audit_project_scope;
ALTER TABLE memory_retrieval_audit DROP COLUMN IF EXISTS repo_scope;

DROP INDEX IF EXISTS idx_memory_ingest_audit_project_scope;
ALTER TABLE memory_ingest_audit DROP COLUMN IF EXISTS repo_scope;

DROP INDEX IF EXISTS idx_quarantine_project_scope;
ALTER TABLE project_memory_quarantine DROP COLUMN IF EXISTS repo_scope;

DROP INDEX IF EXISTS idx_memory_chunks_project_scope;
ALTER TABLE project_memory_chunks DROP COLUMN IF EXISTS repo_scope;
`,
	},
	{
		Version: 76,
		Name:    "repo_scope_on_ingest_queue",
		// B-4 of the migration-75 arc. The async ingest worker
		// drains project_ingest_queue and runs Indexer.IngestText
		// out-of-band from the executor that enqueued the artifact.
		// To carry the deposit-time repo_scope across that boundary,
		// the queue row needs its own column — the executor stamps
		// it at enqueue (reading task.payload.repo_scope), the
		// worker stamps it on the resulting chunks via
		// PatchScopeByArtifact after IngestText returns.
		//
		// Nullable. Workflow tasks without a companion-set scope
		// land NULL (uncategorized), matching the existing chunk-
		// level convention.
		Up: `
ALTER TABLE project_ingest_queue
    ADD COLUMN IF NOT EXISTS repo_scope TEXT;
`,
		Down: `
ALTER TABLE project_ingest_queue DROP COLUMN IF EXISTS repo_scope;
`,
	},
	{
		Version: 77,
		Name:    "cascade_execution_id_fks_for_project_wipe",
		// The archive sweeper's project-wide cleanup fails with
		//   "pq: update or delete on table \"executions\" violates
		//    foreign key constraint \"task_messages_execution_id_fkey\""
		// because two FKs to executions(id) were created without an
		// ON DELETE clause and default to NO ACTION:
		//   - task_messages.execution_id
		//   - task_scratchpad.last_execution_id
		//
		// Sibling FKs already have the right semantics:
		//   artifacts.execution_id                              → CASCADE
		//   corpus_epochs.ingest_execution_id                    → SET NULL
		//   project_ingest_queue.ingest_execution_id             → SET NULL
		//   project_memory_quarantine.ingest_execution_id        → SET NULL
		//
		// SET NULL is the right choice for these two: they're
		// task-scoped tables that already cascade-delete via
		// task_id→tasks(id), so a project-wide wipe still
		// removes the rows; setting the execution back-ref to NULL
		// preserves the audit history when an exec is deleted for
		// other reasons (the orphan sweeper marks FAILED but
		// doesn't delete today — but a future cleanup might).
		//
		// IF EXISTS on the drops keeps the migration idempotent
		// across partially-applied state.
		Up: `
ALTER TABLE task_messages
    DROP CONSTRAINT IF EXISTS task_messages_execution_id_fkey;

ALTER TABLE task_messages
    ADD CONSTRAINT task_messages_execution_id_fkey
        FOREIGN KEY (execution_id) REFERENCES executions(id) ON DELETE SET NULL;

ALTER TABLE task_scratchpad
    DROP CONSTRAINT IF EXISTS task_scratchpad_last_execution_id_fkey;

ALTER TABLE task_scratchpad
    ADD CONSTRAINT task_scratchpad_last_execution_id_fkey
        FOREIGN KEY (last_execution_id) REFERENCES executions(id) ON DELETE SET NULL;
`,
		Down: `
ALTER TABLE task_messages
    DROP CONSTRAINT IF EXISTS task_messages_execution_id_fkey;

ALTER TABLE task_messages
    ADD CONSTRAINT task_messages_execution_id_fkey
        FOREIGN KEY (execution_id) REFERENCES executions(id);

ALTER TABLE task_scratchpad
    DROP CONSTRAINT IF EXISTS task_scratchpad_last_execution_id_fkey;

ALTER TABLE task_scratchpad
    ADD CONSTRAINT task_scratchpad_last_execution_id_fkey
        FOREIGN KEY (last_execution_id) REFERENCES executions(id);
`,
	},
	{
		Version: 78,
		Name:    "counterfactual_provenance_on_executions",
		// Phase C of the Autonomy Black Box arc
		// (https://docs.vornik.io §
		// "Counterfactual link"). A counterfactual is a derived
		// task created by re-running the original with exactly
		// one variable changed. The provenance columns sit on
		// the executions row so the assembler can render
		// original + counterfactual side-by-side and so the
		// blackbox detector can recognise + filter
		// counterfactual runs out of its baseline windows
		// (we don't want a counterfactual that explored a
		// "what if the model were cheaper" question to inflate
		// the cost-regression numerator).
		//
		// All three columns nullable. Native (non-counterfactual)
		// runs land NULL. Partial index keeps the lookup
		// "every counterfactual of task X" cheap without
		// burdening the much larger native-runs slice.
		Up: `
ALTER TABLE executions
    ADD COLUMN IF NOT EXISTS counterfactual_of_task_id TEXT,
    ADD COLUMN IF NOT EXISTS counterfactual_variable   TEXT,
    ADD COLUMN IF NOT EXISTS counterfactual_label      TEXT;

CREATE INDEX IF NOT EXISTS idx_executions_counterfactual_of
    ON executions (counterfactual_of_task_id)
    WHERE counterfactual_of_task_id IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_executions_counterfactual_of;
ALTER TABLE executions
    DROP COLUMN IF EXISTS counterfactual_label,
    DROP COLUMN IF EXISTS counterfactual_variable,
    DROP COLUMN IF EXISTS counterfactual_of_task_id;
`,
	},
	{
		Version: 79,
		Name:    "memory_firewall_policy_columns",
		// Policy-Aware Memory Firewall Phase A (LLD:
		// https://docs.vornik.io
		// § "Data Model — Extending project_memory_chunks").
		//
		// All new columns nullable. Existing chunks land with
		// defaults at first retrieval (lazy backfill via the
		// evaluator's DefaultPolicyForSource) or via the operator
		// backfill CLI. The legacy single-tenant deployment treats
		// every NULL column as "anyone, all purposes, forever" —
		// behaviour identical to pre-firewall.
		//
		// tenant_id is forward-compat with the 2026.8.0 multi-
		// tenancy migration; that migration will backfill +
		// enforce NOT NULL. Here it lands nullable so single-
		// tenant deployments need no operator action.
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS tenant_id            TEXT,
    ADD COLUMN IF NOT EXISTS sensitivity_tier     TEXT,
    ADD COLUMN IF NOT EXISTS provenance_source    TEXT,
    ADD COLUMN IF NOT EXISTS provenance_producer  TEXT,
    ADD COLUMN IF NOT EXISTS provenance_trust     INT,
    ADD COLUMN IF NOT EXISTS provenance_url       TEXT,
    ADD COLUMN IF NOT EXISTS firewall_expires_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS permitted_roles      TEXT[],
    ADD COLUMN IF NOT EXISTS allowed_purposes     TEXT[],
    ADD COLUMN IF NOT EXISTS policy_digest        TEXT;

-- Tenant-scoped recall lookup (2026.8.0 enforcement). Partial
-- so single-tenant deployments don't carry an empty index.
CREATE INDEX IF NOT EXISTS idx_memory_tenant
    ON project_memory_chunks (tenant_id, project_id)
    WHERE tenant_id IS NOT NULL;

-- "Chunks expiring soon" — firewall sweeper + operator UI.
CREATE INDEX IF NOT EXISTS idx_memory_firewall_expires
    ON project_memory_chunks (firewall_expires_at)
    WHERE firewall_expires_at IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_memory_firewall_expires;
DROP INDEX IF EXISTS idx_memory_tenant;
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS policy_digest,
    DROP COLUMN IF EXISTS allowed_purposes,
    DROP COLUMN IF EXISTS permitted_roles,
    DROP COLUMN IF EXISTS firewall_expires_at,
    DROP COLUMN IF EXISTS provenance_url,
    DROP COLUMN IF EXISTS provenance_trust,
    DROP COLUMN IF EXISTS provenance_producer,
    DROP COLUMN IF EXISTS provenance_source,
    DROP COLUMN IF EXISTS sensitivity_tier,
    DROP COLUMN IF EXISTS tenant_id;
`,
	},
	{
		Version: 80,
		Name:    "memory_policy_evaluations",
		// Per-retrieval audit row for the firewall. One row per
		// (chunk, request) pair regardless of decision — allow
		// rows are the proof trail, block rows are the
		// compliance trail. See LLD § "New
		// memory_policy_evaluations table".
		//
		// Indexes:
		//   - (project_id, evaluated_at DESC) for the operator's
		//     recent-blocks view
		//   - (decision, evaluated_at) PARTIAL on decision != 'allow'
		//     so the compliance review query stays fast even as
		//     allow rows dominate
		//   - (trace_id) PARTIAL for the Black Box assembler join
		//
		// The allow-only partial index supports the aggressive
		// allow-row retention sweep (default 30 days vs 365 for
		// block rows).
		Up: `
CREATE TABLE IF NOT EXISTS memory_policy_evaluations (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    tenant_id        TEXT,
    chunk_id         TEXT NOT NULL,
    request_role     TEXT,
    request_purpose  TEXT,
    request_operator TEXT,
    trace_id         TEXT,
    decision         TEXT NOT NULL,
    policy_digest    TEXT,
    reason_detail    TEXT,
    evaluated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_policy_eval_project_recent
    ON memory_policy_evaluations (project_id, evaluated_at DESC);

CREATE INDEX IF NOT EXISTS idx_policy_eval_decision_recent
    ON memory_policy_evaluations (decision, evaluated_at DESC)
    WHERE decision <> 'allow';

CREATE INDEX IF NOT EXISTS idx_policy_eval_trace
    ON memory_policy_evaluations (trace_id)
    WHERE trace_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_policy_eval_evaluated_at_allow
    ON memory_policy_evaluations (evaluated_at)
    WHERE decision = 'allow';
`,
		Down: `
DROP INDEX IF EXISTS idx_policy_eval_evaluated_at_allow;
DROP INDEX IF EXISTS idx_policy_eval_trace;
DROP INDEX IF EXISTS idx_policy_eval_decision_recent;
DROP INDEX IF EXISTS idx_policy_eval_project_recent;
DROP TABLE IF EXISTS memory_policy_evaluations;
`,
	},
	{
		Version: 81,
		Name:    "workflow_healing_overrides",
		// Per-(project, workflow, trigger_class) operator overrides
		// for the Black Box Phase B detector. Unifies two operator
		// needs:
		//   - threshold_override: replace the detector's default
		//     relative-delta cap (0.25 for failure_rate, 0.40 for
		//     cost) for a specific tuple
		//   - muted_until: snooze the detector for a tuple while
		//     a known regression is being triaged outside the loop
		//
		// Composite primary key on (project_id, workflow_id,
		// trigger_class) — one row per tuple, Upsert overwrites.
		// Either column can be NULL; a row with both NULL is
		// effectively a no-op (the UI's Delete form removes it).
		Up: `
CREATE TABLE IF NOT EXISTS workflow_healing_overrides (
    project_id          TEXT NOT NULL,
    workflow_id         TEXT NOT NULL,
    trigger_class       TEXT NOT NULL,
    threshold_override  DOUBLE PRECISION,
    muted_until         TIMESTAMPTZ,
    notes               TEXT NOT NULL DEFAULT '',
    created_by          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, workflow_id, trigger_class)
);

CREATE INDEX IF NOT EXISTS idx_healing_overrides_updated
    ON workflow_healing_overrides (updated_at DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_healing_overrides_updated;
DROP TABLE IF EXISTS workflow_healing_overrides;
`,
	},
	{
		Version: 82,
		Name:    "tasks_companion_api_key_index",
		// Expression index on the companion API key embedded in a
		// task's payload. Powers the per-key budget-cap gate
		// (TaskLLMUsageRepository.SumCostByAPIKey): the companion
		// delegate handler sums prior LLM spend for a key by joining
		// task_llm_usage → tasks on this expression before admitting a
		// new delegate. Partial (key IS NOT NULL) so only
		// companion-created tasks carry an index entry — keeps it small.
		// See https://docs.vornik.io finding #2 /
		// mitigation plan §7.2.
		Up: `
CREATE INDEX IF NOT EXISTS idx_tasks_companion_api_key
    ON tasks ((payload->'companion'->>'api_key_id'))
    WHERE payload->'companion'->>'api_key_id' IS NOT NULL;
`,
		Down: `
DROP INDEX IF EXISTS idx_tasks_companion_api_key;
`,
	},
	{
		Version: 83,
		Name:    "workflow_proposals_kind",
		// Additive `kind` enum column on workflow_proposals so
		// operators can filter / analyse architect proposals by class
		// (add_step, change_timeout, …). The closed set lives in
		// https://docs.vornik.io §
		// "What the architect can propose". Existing rows + any
		// proposal the architect doesn't yet tag default to the
		// 'unspecified' sentinel — the column is non-breaking
		// (mitigation plan §10). See https://docs.vornik.io
		// (self-evolving-workflows medium) / mitigation plan §8.5.
		//
		// NB: populating kind from the architect's LLM output is a
		// separate follow-on (the model's ArchitectOutput must emit a
		// `kind` field); until that lands, new proposals also default
		// to 'unspecified'. The read path + filter ship here.
		Up: `
ALTER TABLE workflow_proposals
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'unspecified';

CREATE INDEX IF NOT EXISTS idx_workflow_proposals_kind
    ON workflow_proposals (kind, created_at DESC);
`,
		Down: `
DROP INDEX IF EXISTS idx_workflow_proposals_kind;
ALTER TABLE workflow_proposals DROP COLUMN IF EXISTS kind;
`,
	},
	{
		Version: 84,
		Name:    "project_wizard_sessions_cancelled_at",
		// Terminal `cancelled_at` state on project_wizard_sessions so an
		// operator can cancel an in-progress wizard session and free the
		// per-operator active-session slot (cap MaxActiveSessions=5, counted
		// as sessions with committed_project_id IS NULL AND cancelled_at IS
		// NULL). Additive nullable column — non-breaking.
		Up: `
ALTER TABLE project_wizard_sessions ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ;
`,
		Down: `
ALTER TABLE project_wizard_sessions DROP COLUMN IF EXISTS cancelled_at;
`,
	},
	{
		Version: 85,
		Name:    "create_instincts",
		// Continuous-learning instinct layer, slice 1 (schema). An
		// instinct is an atomic, confidence-scored learned pattern —
		// "in situation T, action/observation A held" — mined by the
		// leader-elected extraction worker (internal/instinct) from
		// the existing audit spine. See
		// https://docs.vornik.io
		//
		// Slice 1 populates this table read-only (no consumers wired);
		// the whole subsystem is gated behind instinct.enabled (default
		// false), so this migration is pure-additive and inert until an
		// operator opts in.
		//
		// NB: the column is trigger_json, NOT `trigger` — TRIGGER is a
		// reserved word in Postgres and an unquoted column of that name
		// would fail to parse. The LLD's frontmatter `trigger` maps to
		// this column at the export/import boundary.
		//
		// support_count / contradict_count are DERIVED columns: the
		// worker recomputes them from instinct_evidence on every
		// upsert, which makes re-scanning a time window idempotent
		// (the dedup is enforced by instinct_evidence's PK). The
		// confidence column is the materialised Wilson-lower-bound ×
		// recency-decay value, also recomputed on upsert / lazily on
		// read.
		//
		// idx_instincts_dedup is the atomicity key: a recurring
		// situation updates one row's confidence rather than spawning
		// duplicates. Scoped by (scope, project_id) so the same
		// trigger_key can later exist as both a project-local and a
		// promoted global instinct.
		Up: `
CREATE TABLE IF NOT EXISTS instincts (
    id               TEXT PRIMARY KEY,
    scope            TEXT NOT NULL DEFAULT 'project',
    project_id       TEXT NOT NULL DEFAULT '',
    domain           TEXT NOT NULL,
    trigger_key      TEXT NOT NULL,
    trigger_json     JSONB,
    action           TEXT NOT NULL,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0,
    support_count    INTEGER NOT NULL DEFAULT 0,
    contradict_count INTEGER NOT NULL DEFAULT 0,
    source           TEXT NOT NULL DEFAULT 'observer',
    status           TEXT NOT NULL DEFAULT 'candidate',
    distill_model    TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_instincts_dedup
    ON instincts (scope, project_id, trigger_key);
CREATE INDEX IF NOT EXISTS idx_instincts_project_domain
    ON instincts (project_id, domain);
CREATE INDEX IF NOT EXISTS idx_instincts_status_confidence
    ON instincts (status, confidence DESC);

COMMENT ON TABLE instincts IS 'Continuous-learning instinct layer: atomic, confidence-scored learned patterns mined from the audit spine. Advisory-only; behaviour change is operator/architect-gated. See continuous-learning-instinct-layer-design.md.';
COMMENT ON COLUMN instincts.trigger_json IS 'Structured trigger {role?, error_class?, task_type?, step_id?, model?, …}. trigger_key is its canonical hash and the dedup key.';
COMMENT ON COLUMN instincts.domain IS 'recovery | cost | quality | retrieval | workflow';
COMMENT ON COLUMN instincts.source IS 'observer | operator | architect-reject';
COMMENT ON COLUMN instincts.status IS 'candidate | active | promoted | retired';
`,
		Down: `
DROP TABLE IF EXISTS instincts;
`,
	},
	{
		Version: 86,
		Name:    "create_instinct_evidence_and_applications",
		// Slice 1 companion tables for the instinct layer.
		//
		// instinct_evidence is the provenance + idempotency spine. Each
		// row links one instinct to one corroborating/contradicting
		// execution_step_outcomes row (outcome_id). The composite PK
		// (instinct_id, outcome_id) is what makes the extraction
		// worker safe to re-run over an overlapping window: re-seeing
		// the same outcome is an ON CONFLICT DO NOTHING no-op, so
		// support_count (= COUNT(polarity='support')) never
		// double-counts. polarity ∈ {support, contradict}.
		//
		// instinct_applications records when an instinct was
		// surfaced/used and what happened next — the feedback loop the
		// consumers (slices 3+) will close. No consumer writes it in
		// slice 1; the table lands now so the schema is stable before
		// the behaviour-affecting slices build on it.
		Up: `
CREATE TABLE IF NOT EXISTS instinct_evidence (
    instinct_id  TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    outcome_id   TEXT NOT NULL,
    polarity     TEXT NOT NULL DEFAULT 'support',
    created_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instinct_id, outcome_id)
);
CREATE INDEX IF NOT EXISTS idx_instinct_evidence_instinct
    ON instinct_evidence (instinct_id);

COMMENT ON TABLE instinct_evidence IS 'Per-outcome provenance for instincts. PK (instinct_id, outcome_id) makes worker re-runs idempotent. polarity ∈ {support, contradict}.';

CREATE TABLE IF NOT EXISTS instinct_applications (
    id           TEXT PRIMARY KEY,
    instinct_id  TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    task_id      TEXT NOT NULL DEFAULT '',
    surface      TEXT NOT NULL,
    result       TEXT NOT NULL,
    applied_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_instinct_applications_instinct
    ON instinct_applications (instinct_id, applied_at DESC);

COMMENT ON TABLE instinct_applications IS 'Records when an instinct was surfaced/used and the result, closing the feedback loop. surface ∈ {failed_task_ui, lead_recovery, architect_evidence}; result ∈ {accepted, rejected, succeeded, failed, ignored}.';
`,
		Down: `
DROP TABLE IF EXISTS instinct_applications;
DROP TABLE IF EXISTS instinct_evidence;
`,
	},
	{
		Version: 87,
		Name:    "create_workflow_healing_candidates",
		// Self-Healing Workflow Genome v1 — candidate ledger.
		//
		// A candidate is a trial-tracking record that LINKS to a
		// memetic WorkflowProposal (proposal_id, a soft string
		// reference — same convention as workflow_healing_triggers
		// .proposal_id; no hard FK so a proposal purge doesn't
		// orphan-cascade the candidate). trigger_id FK-cascades from
		// the trigger that spawned it. baseline/candidate genome
		// hashes are the registry.Workflow.Hash fingerprints so the
		// scorecard can prove which structure it compared.
		//
		// status is CHECK-constrained: promotion is ALWAYS a manual
		// operator action, so there is no autonomous path into
		// 'promoted'. candidate_class / risk_level are descriptive
		// free text (not CHECK-constrained) so later phases can add
		// recipe classes without a schema change.
		Up: `
CREATE TABLE IF NOT EXISTS workflow_healing_candidates (
    id                    TEXT PRIMARY KEY,
    trigger_id            TEXT NOT NULL REFERENCES workflow_healing_triggers(id) ON DELETE CASCADE,
    project_id            TEXT NOT NULL,
    workflow_id           TEXT NOT NULL,
    proposal_id           TEXT NOT NULL,
    baseline_genome_hash  TEXT NOT NULL DEFAULT '',
    candidate_genome_hash TEXT NOT NULL DEFAULT '',
    candidate_class       TEXT NOT NULL,
    proposal_diff         TEXT NOT NULL DEFAULT '',
    motivation            TEXT NOT NULL DEFAULT '',
    expected_effect       TEXT NOT NULL DEFAULT '',
    risk_level            TEXT NOT NULL DEFAULT 'medium',
    status                TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft','trial_running','trial_passed','trial_failed','rejected','promoted')),
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    promoted_at           TIMESTAMP WITH TIME ZONE,
    promoted_by           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_healing_candidates_trigger
    ON workflow_healing_candidates (trigger_id);
CREATE INDEX IF NOT EXISTS idx_healing_candidates_project_time
    ON workflow_healing_candidates (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_healing_candidates_proposal
    ON workflow_healing_candidates (proposal_id);

COMMENT ON TABLE workflow_healing_candidates IS 'Self-Healing Workflow Genome v1: trial-tracking record linking a workflow regression trigger to a memetic WorkflowProposal. status ∈ {draft,trial_running,trial_passed,trial_failed,rejected,promoted}; promotion is always a manual operator action.';
COMMENT ON COLUMN workflow_healing_candidates.proposal_id IS 'Soft reference to workflow_proposals.id (no hard FK; the architect owns the proposal content / apply path).';
`,
		Down: `
DROP INDEX IF EXISTS idx_healing_candidates_proposal;
DROP INDEX IF EXISTS idx_healing_candidates_project_time;
DROP INDEX IF EXISTS idx_healing_candidates_trigger;
DROP TABLE IF EXISTS workflow_healing_candidates;
`,
	},
	{
		Version: 88,
		Name:    "create_workflow_healing_trials",
		// Self-Healing Workflow Genome v1 — trial ledger.
		//
		// One row per trial run of a candidate. mode + verdict are
		// CHECK-constrained to the LLD enums. baseline_summary /
		// candidate_summary / scorecard are JSONB blobs the trial
		// runner writes verbatim (TrialSummary / Scorecard). verdict
		// allows 'inconclusive' so a low-fidelity replay can surface
		// its limitations rather than being forced into pass/fail.
		// candidate_id FK-cascades from the candidate.
		Up: `
CREATE TABLE IF NOT EXISTS workflow_healing_trials (
    id                     TEXT PRIMARY KEY,
    candidate_id           TEXT NOT NULL REFERENCES workflow_healing_candidates(id) ON DELETE CASCADE,
    mode                   TEXT NOT NULL
        CHECK (mode IN ('static','replay','shadow')),
    evidence_execution_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    baseline_summary       JSONB NOT NULL DEFAULT '{}'::jsonb,
    candidate_summary      JSONB NOT NULL DEFAULT '{}'::jsonb,
    scorecard              JSONB NOT NULL DEFAULT '{}'::jsonb,
    verdict                TEXT NOT NULL DEFAULT 'pending'
        CHECK (verdict IN ('pending','passed','failed','inconclusive','errored')),
    started_at             TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    finished_at            TIMESTAMP WITH TIME ZONE
);
CREATE INDEX IF NOT EXISTS idx_healing_trials_candidate
    ON workflow_healing_trials (candidate_id, started_at DESC);

COMMENT ON TABLE workflow_healing_trials IS 'Self-Healing Workflow Genome v1: one trial run (static or replay) of a candidate against an evidence set. mode ∈ {static,replay,shadow}; verdict ∈ {pending,passed,failed,inconclusive,errored}.';
`,
		Down: `
DROP INDEX IF EXISTS idx_healing_trials_candidate;
DROP TABLE IF EXISTS workflow_healing_trials;
`,
	},
	{
		Version: 89,
		Name:    "rollback_supersession_provenance",
		// Memory rollback x supersession (2026-06-04 bug-sweep
		// critical finding). See https://docs.vornik.io
		// memory-rollback-supersession-design.md.
		//
		// superseded_in_epoch / pre_supersede_status record WHY and
		// FROM-WHAT a chunk was superseded, so RollbackTo can restore
		// the prior version when the superseding epoch is rolled back
		// (pre-fix both versions became unretrievable). A chunk is
		// superseded at most once between restores, so row columns
		// carry the same information as the pair ledger the original
		// ingest design sketched (its section 9 failure mode #3).
		//
		// corpus_epochs.deactivated_at/_by tombstone explicit operator
		// deactivations so RollbackTo's re-activation pass cannot
		// resurrect them. corpus_rollbacks.chunks_restored is the
		// audit counter for the restore pass.
		Up: `
ALTER TABLE project_memory_chunks
    ADD COLUMN IF NOT EXISTS superseded_in_epoch  TEXT,
    ADD COLUMN IF NOT EXISTS pre_supersede_status TEXT;

CREATE INDEX IF NOT EXISTS idx_memory_chunks_superseded_in_epoch
    ON project_memory_chunks (project_id, superseded_in_epoch)
    WHERE superseded_in_epoch IS NOT NULL;

ALTER TABLE corpus_epochs
    ADD COLUMN IF NOT EXISTS deactivated_at TIMESTAMP WITH TIME ZONE,
    ADD COLUMN IF NOT EXISTS deactivated_by TEXT;

ALTER TABLE corpus_rollbacks
    ADD COLUMN IF NOT EXISTS chunks_restored INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN project_memory_chunks.superseded_in_epoch IS 'Epoch whose ingest run caused this chunk''s supersession; NULL = not superseded or pre-migration history (non-restorable). Rollback restores chunks whose causing epoch is deactivated.';
COMMENT ON COLUMN project_memory_chunks.pre_supersede_status IS 'validation_status at the moment of supersession; restore puts this back (COALESCE unverified).';
COMMENT ON COLUMN corpus_epochs.deactivated_at IS 'Explicit operator deactivation tombstone; rollback re-activation skips tombstoned epochs. Cleared by Activate.';
COMMENT ON COLUMN corpus_rollbacks.chunks_restored IS 'Superseded chunks restored by this rollback''s supersession-revert pass.';
`,
		Down: `
DROP INDEX IF EXISTS idx_memory_chunks_superseded_in_epoch;
ALTER TABLE project_memory_chunks
    DROP COLUMN IF EXISTS superseded_in_epoch,
    DROP COLUMN IF EXISTS pre_supersede_status;
ALTER TABLE corpus_epochs
    DROP COLUMN IF EXISTS deactivated_at,
    DROP COLUMN IF EXISTS deactivated_by;
ALTER TABLE corpus_rollbacks
    DROP COLUMN IF EXISTS chunks_restored;
`,
	},
	{
		Version: 90,
		Name:    "identity_core",
		Up: `
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    disabled_at   TIMESTAMP WITH TIME ZONE
);
COMMENT ON TABLE users IS 'Human principals for the identity core (oidc-identity-permissions-design.md §3.1). Machine credentials stay in api_keys.';

CREATE TABLE IF NOT EXISTS groups (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    description   TEXT,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE groups IS 'Permission groups (oidc-identity-permissions-design.md §3.1): role admin = instance-wide; role user = scoped by group_projects.';

CREATE TABLE IF NOT EXISTS group_projects (
    group_id      TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    project_id    TEXT NOT NULL,
    PRIMARY KEY (group_id, project_id)
);
COMMENT ON TABLE group_projects IS 'role=admin groups ignore this table (admin is instance-wide); project_id ''*'' = all projects for user-role groups.';

CREATE TABLE IF NOT EXISTS group_members (
    group_id      TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_group_members_user ON group_members (user_id);

CREATE TABLE IF NOT EXISTS user_identities (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel       TEXT NOT NULL,
    external_id   TEXT NOT NULL,
    display       TEXT,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMP WITH TIME ZONE,
    revoked_at    TIMESTAMP WITH TIME ZONE,
    UNIQUE (channel, external_id)
);
CREATE INDEX IF NOT EXISTS idx_user_identities_user ON user_identities (user_id);
COMMENT ON TABLE user_identities IS 'Channel bindings (google|github|microsoft|gitlab|telegram|slack|...). UNIQUE(channel, external_id) spans revoked rows: rebinding repoints the existing row.';

CREATE TABLE IF NOT EXISTS link_codes (
    code_hash             TEXT PRIMARY KEY,
    user_id               TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at            TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at            TIMESTAMP WITH TIME ZONE NOT NULL,
    used_at               TIMESTAMP WITH TIME ZONE,
    used_by_channel       TEXT,
    used_by_external_id   TEXT
);
COMMENT ON TABLE link_codes IS 'Self-service channel-link codes (Phase 4 consumes; schema ships with the core). Raw codes never stored — sha256 only, same hygiene as api_keys.key_hash.';
CREATE INDEX IF NOT EXISTS idx_link_codes_user ON link_codes (user_id);
`,
		Down: `
DROP TABLE IF EXISTS link_codes;
DROP TABLE IF EXISTS user_identities;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS group_projects;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS users;
`,
	},
	{
		Version: 91,
		Name:    "ui_sessions",
		Up: `
CREATE TABLE IF NOT EXISTS ui_sessions (
    id            TEXT PRIMARY KEY,
    token_hash    TEXT NOT NULL UNIQUE,
    user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at    TIMESTAMP WITH TIME ZONE,
    ip            TEXT,
    user_agent    TEXT
);
CREATE INDEX IF NOT EXISTS idx_ui_sessions_hash ON ui_sessions (token_hash) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_ui_sessions_user ON ui_sessions (user_id);
COMMENT ON TABLE ui_sessions IS 'Browser login sessions (oidc-identity-permissions-design.md §4.3). Cookie carries the raw 256-bit token; only the sha256 lands here. Sessions reference user_id and re-resolve permissions per request (60s TTL) — no frozen permission snapshot.';
`,
		Down: `
DROP TABLE IF EXISTS ui_sessions;
`,
	},
	{
		Version: 92,
		Name:    "workflow_proposals_instinct_ids",
		Up: `
ALTER TABLE workflow_proposals ADD COLUMN IF NOT EXISTS instinct_ids TEXT[];
COMMENT ON COLUMN workflow_proposals.instinct_ids IS 'Instinct-layer priors that supported this proposal (2026-06-07 review: only the priors'' action text survived in motivation, so proposals could not be traced back to their instincts). NULL = no priors / pre-column row.';
`,
		Down: `
ALTER TABLE workflow_proposals DROP COLUMN IF EXISTS instinct_ids;
`,
	},
	{
		Version: 93,
		Name:    "task_status_awaiting_approval",
		// Autonomy manual-approval gate. Autonomy tasks created under a
		// project with requireApproval previously landed in PENDING,
		// which the scheduler never leases (WHERE status='QUEUED') and
		// no UI surfaced — they waited forever (operator report
		// 2026-06-09). They now land in a dedicated AWAITING_APPROVAL
		// status that joins the awaiting-action inbox surface and the
		// operator resolves via approve (→ QUEUED) or reject
		// (→ CANCELLED). See
		// https://docs.vornik.io
		//
		// ALTER TYPE ... ADD VALUE IF NOT EXISTS is idempotent and fast
		// (no table rewrite). Mirrors the v24 enum extension. NOTE: a
		// partial index whose predicate references this new value cannot
		// be created in the same transaction that adds it (Postgres
		// "unsafe use of new enum value") — defer any such index to a
		// later migration, exactly as v24→v25 split the inbox indexes.
		Up: `
ALTER TYPE task_status ADD VALUE IF NOT EXISTS 'AWAITING_APPROVAL';
`,
		// Postgres has no native DROP VALUE for enum types; the value is
		// left in the type, matching the v24 Down note. A clean removal
		// would require recreating the enum and rewriting every
		// dependent column — not worth it for an additive value.
		Down: ``,
	},
	{
		Version: 94,
		Name:    "instinct_applications_execution_step_link",
		// Slice 7 surfacing: link a surfaced lead_recovery application
		// back to the execution + step it was attached to, so the
		// RecoveryResolver can later match it against the step's outcome
		// and flip the still-'ignored' row to succeeded/failed in place.
		//
		// Both columns are plain TEXT (NOT NULL DEFAULT '') and the
		// partial index predicate references only the plain string
		// columns surface/result — no enum is involved, so (unlike v93)
		// the index is safe to create in the same transaction as the
		// column additions.
		Up: `
ALTER TABLE instinct_applications ADD COLUMN IF NOT EXISTS execution_id TEXT NOT NULL DEFAULT '';
ALTER TABLE instinct_applications ADD COLUMN IF NOT EXISTS step_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_instinct_applications_pending ON instinct_applications (execution_id, step_id) WHERE surface = 'lead_recovery' AND result = 'ignored';
`,
		// Additive policy (matches migration 93): leave the columns and
		// index in place on Down.
		Down: ``,
	},
	{
		Version: 95,
		Name:    "cluster_nodes",
		Up: `
CREATE TABLE IF NOT EXISTS cluster_nodes (
    instance_id  TEXT PRIMARY KEY,
    profile      TEXT NOT NULL,
    version      TEXT NOT NULL,
    address      TEXT NOT NULL DEFAULT '',
    capabilities JSONB NOT NULL DEFAULT '{}',
    last_seen    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cluster_nodes_last_seen ON cluster_nodes (last_seen);`,
		Down: `DROP TABLE IF EXISTS cluster_nodes;`,
	},
	{
		Version: 96,
		Name:    "daemon_leader_locks_epoch",
		// Additive — rolling-deploy safe: an old binary ignores
		// the column; a new binary's Acquire sets it. The DEFAULT 0
		// preserves today's behaviour for any rows inserted before
		// the first new-binary Acquire runs.
		//
		// The epoch is a monotonic fence token that increments on
		// every takeover (a different holder wins) and is preserved
		// on a same-holder renew. See review finding B1 + the
		// cluster-slice-D plan.
		Up:   `ALTER TABLE daemon_leader_locks ADD COLUMN IF NOT EXISTS epoch BIGINT NOT NULL DEFAULT 0;`,
		Down: `ALTER TABLE daemon_leader_locks DROP COLUMN IF EXISTS epoch;`,
	},
	{
		Version: 97,
		Name:    "ingest_queue_active_idempotency",
		// Hardening 2026-06-15 (memory LLD review batch 2): a duplicate
		// enqueue of the same (project, source_artifact) — e.g. a replayed
		// ingest-trigger step or a re-processed execution — created a
		// SECOND active queue row that drained independently, re-ingesting
		// identical content. DedupHashGate catches the published near-dup,
		// but the processing is wasted and the near_dup_supersede path is
		// exercised needlessly. A partial unique index makes at most one
		// row ACTIVE (queued|processing) per key; terminal rows
		// (done|failed) stay unconstrained so the same artifact can be
		// legitimately re-ingested once its prior run finishes. Enqueue
		// uses ON CONFLICT DO NOTHING against this index (race-safe
		// idempotency). Genuinely-new content carries a new artifact ID,
		// so it is never collapsed.
		//
		// Collapse any pre-existing active duplicates first (keep the
		// earliest-enqueued row per key) so the index build can't fail on
		// historical data.
		//
		// The whole Up runs in one transaction (applyMigration); the
		// EXCLUSIVE table lock additionally blocks concurrent writers for
		// its duration so a rolling MULTI-INSTANCE deploy (an old-binary
		// pod still doing plain INSERTs while a new pod migrates) can't
		// slip a duplicate active row between the backfill and the index
		// build and fail it. Single-instance migrates at boot before
		// serving, so this is belt-and-braces; the queue is tiny and the
		// lock is held only for the brief migration tx. (Review
		// 2026-06-15: migration-97 concurrency.)
		Up: `
LOCK TABLE project_ingest_queue IN EXCLUSIVE MODE;

UPDATE project_ingest_queue
SET state = 'failed',
    finished_at = NOW(),
    last_error = 'superseded duplicate (migration 97 idempotency backfill)'
WHERE state IN ('queued','processing')
  AND id NOT IN (
    SELECT DISTINCT ON (project_id, source_artifact_id) id
    FROM project_ingest_queue
    WHERE state IN ('queued','processing')
    ORDER BY project_id, source_artifact_id, enqueued_at ASC, id ASC
  );

CREATE UNIQUE INDEX IF NOT EXISTS uq_ingest_queue_active
    ON project_ingest_queue (project_id, source_artifact_id)
    WHERE state IN ('queued','processing');`,
		Down: `DROP INDEX IF EXISTS uq_ingest_queue_active;`,
	},
	{
		Version: 98,
		Name:    "knowledge_edges_integrity",
		// Hardening 2026-06-15 (memory LLD review batch 4): enforce edge
		// integrity at WRITE time, not only via the read-path project
		// filter. (1) No self-loops — a relationship from an entity to
		// itself is meaningless and pollutes graph walks. (2) from/to
		// entities must belong to the edge's project_id; a CHECK can't
		// express a cross-table predicate, so a BEFORE INSERT/UPDATE
		// trigger enforces it. Self-loops are deleted first so the CHECK
		// can validate against historical data.
		//
		// VALIDATE CONSTRAINT scans the table under SHARE UPDATE EXCLUSIVE
		// (does not block reads/writes, but conflicts with concurrent DDL
		// / VACUUM). On a large knowledge_edges table this can take a
		// while; run during a low-churn window if the table is big. It is
		// small on the current single-customer deployment. (Review
		// 2026-06-15: migration-98 lock duration.)
		Up: `
DELETE FROM knowledge_edges WHERE from_entity = to_entity;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'knowledge_edges_no_self_loop') THEN
        ALTER TABLE knowledge_edges
            ADD CONSTRAINT knowledge_edges_no_self_loop
            CHECK (from_entity <> to_entity) NOT VALID;
        ALTER TABLE knowledge_edges VALIDATE CONSTRAINT knowledge_edges_no_self_loop;
    END IF;
END $$;

CREATE OR REPLACE FUNCTION knowledge_edges_same_project_trigger() RETURNS trigger AS $$
BEGIN
    -- Reject only when a referenced entity demonstrably belongs to a
    -- DIFFERENT project. (Non-existent ids are already barred by the FK;
    -- phrasing it this way keeps the check a pure cross-project guard.)
    IF EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.from_entity AND project_id <> NEW.project_id)
       OR EXISTS (SELECT 1 FROM knowledge_entities WHERE id = NEW.to_entity AND project_id <> NEW.project_id) THEN
        RAISE EXCEPTION 'knowledge_edges: from/to entity must belong to edge project_id %', NEW.project_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS knowledge_edges_same_project ON knowledge_edges;
CREATE TRIGGER knowledge_edges_same_project
BEFORE INSERT OR UPDATE ON knowledge_edges
FOR EACH ROW EXECUTE FUNCTION knowledge_edges_same_project_trigger();`,
		Down: `
DROP TRIGGER IF EXISTS knowledge_edges_same_project ON knowledge_edges;
DROP FUNCTION IF EXISTS knowledge_edges_same_project_trigger();
ALTER TABLE knowledge_edges DROP CONSTRAINT IF EXISTS knowledge_edges_no_self_loop;`,
	},
	{
		Version: 99,
		Name:    "memory_tsv_vornik_english_config",
		// Hardening 2026-06-15 (memory LLD review): full-text ranking
		// referenced the bare 'english' text-search config on BOTH the
		// stored tsv generated column and every query site. 'english' is
		// tied to the server's snowball dictionaries, which shift across
		// PG major upgrades — and because tsv is STORED, an upgrade would
		// leave old rows stemmed differently from new rows + queries,
		// drifting ranking silently. Move to a vornik-OWNED config
		// (a COPY of pg_catalog.english: identical lexemes today) used by
		// the column AND the queries, so the dependency is explicit and
		// the operator reindexes deliberately on a PG upgrade rather than
		// inheriting new stemming by surprise.
		//
		// Rebuilding the STORED generated column rewrites the table under
		// ACCESS EXCLUSIVE — fast on the current corpus, but run in a
		// maintenance window if project_memory_chunks is large.
		Up: `
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = 'vornik_english') THEN
    CREATE TEXT SEARCH CONFIGURATION vornik_english (COPY = pg_catalog.english);
  END IF;
END $$;

ALTER TABLE project_memory_chunks DROP COLUMN IF EXISTS tsv;
ALTER TABLE project_memory_chunks
  ADD COLUMN tsv TSVECTOR GENERATED ALWAYS AS (to_tsvector('vornik_english', content)) STORED;
CREATE INDEX IF NOT EXISTS idx_memory_tsv ON project_memory_chunks USING GIN (tsv);`,
		Down: `
ALTER TABLE project_memory_chunks DROP COLUMN IF EXISTS tsv;
ALTER TABLE project_memory_chunks
  ADD COLUMN tsv TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', content)) STORED;
CREATE INDEX IF NOT EXISTS idx_memory_tsv ON project_memory_chunks USING GIN (tsv);
DROP TEXT SEARCH CONFIGURATION IF EXISTS vornik_english;`,
	},
	{
		Version: 100,
		Name:    "memory_supersession_epoch_consistency",
		// Hardening 2026-06-15 (memory LLD review): a recorded
		// supersession epoch (superseded_in_epoch) only makes sense on a
		// chunk that is actually superseded — the rollback restore pass
		// keys on BOTH (validation_status='superseded' AND
		// superseded_in_epoch IS NOT NULL). Enforce that one direction so
		// a future code change can't set the epoch without the status and
		// leave a chunk the restore pass would resurrect into the wrong
		// state.
		//
		// The REVERSE is deliberately NOT constrained: a chunk superseded
		// with an empty epochID (ingest had no epoch) or pre-migration
		// history is legitimately status='superseded' with a NULL epoch
		// (see migration 89's column comment + repository.go's
		// SupersedeBySameSource, which sets superseded_in_epoch =
		// NULLIF($epoch,'')). All existing rows therefore satisfy the
		// check (pre-migration rows have NULL epoch), so VALIDATE passes.
		Up: `
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ck_memory_superseded_epoch_status') THEN
        ALTER TABLE project_memory_chunks
            ADD CONSTRAINT ck_memory_superseded_epoch_status
            CHECK (superseded_in_epoch IS NULL OR validation_status = 'superseded') NOT VALID;
        ALTER TABLE project_memory_chunks VALIDATE CONSTRAINT ck_memory_superseded_epoch_status;
    END IF;
END $$;`,
		Down: `ALTER TABLE project_memory_chunks DROP CONSTRAINT IF EXISTS ck_memory_superseded_epoch_status;`,
	},
	{
		Version: 101,
		Name:    "instinct_per_action_evidence_and_versioning",
		// W6 (2026-06-15, continuous-learning instinct layer): per-action
		// evidence partitioning + action-version history.
		//
		// Per-action evidence: instinct_evidence gains `action` — the
		// instinct action the outcome corroborated AT RECORD TIME.
		// RecomputeConfidence now counts only evidence whose action matches
		// the instinct's CURRENT action, so when a cross-project conflict
		// replaces the global action (maybePromoteGlobal "replaced") the new
		// action no longer inherits the displaced action's evidence — closing
		// the deferred correctness hole WITHOUT deleting evidence (audit
		// preserved). Existing rows are backfilled from their parent
		// instinct's current action so the count is unchanged for every
		// already-converged instinct.
		//
		// instinct_action_history: append-only ledger snapshotting a
		// displaced action's final confidence/support/contradict before a new
		// action takes the slot — the versioning/rollback substrate.
		//
		// Both objects are additive + idempotent (IF NOT EXISTS, guarded
		// backfill), so the migration is a single safe transaction on the
		// live corpus.
		Up: `
ALTER TABLE instinct_evidence ADD COLUMN IF NOT EXISTS action TEXT NOT NULL DEFAULT '';

UPDATE instinct_evidence ie
SET action = i.action
FROM instincts i
WHERE ie.instinct_id = i.id AND ie.action = '';

CREATE INDEX IF NOT EXISTS idx_instinct_evidence_instinct_action
    ON instinct_evidence (instinct_id, action);

CREATE TABLE IF NOT EXISTS instinct_action_history (
    id               TEXT PRIMARY KEY,
    instinct_id      TEXT NOT NULL REFERENCES instincts(id) ON DELETE CASCADE,
    action           TEXT NOT NULL,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0,
    support_count    INTEGER NOT NULL DEFAULT 0,
    contradict_count INTEGER NOT NULL DEFAULT 0,
    reason           TEXT NOT NULL,
    recorded_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_instinct_action_history_instinct
    ON instinct_action_history (instinct_id, recorded_at DESC);

COMMENT ON COLUMN instinct_evidence.action IS 'W6: instinct action this outcome corroborated at record time; RecomputeConfidence counts only evidence matching the instinct''s current action.';
COMMENT ON TABLE instinct_action_history IS 'W6: append-only action-transition ledger; one row per action change snapshotting the displaced action''s final confidence/support/contradict. reason ∈ {action_change, w6_replace}.';

-- v2 auto-apply: the pending-recovery-applications index must also cover
-- 'auto_applied' rows (a prompt-level directive is pending until the resolver
-- flips it), so the RecoveryResolver's pending scan stays index-backed.
DROP INDEX IF EXISTS idx_instinct_applications_pending;
CREATE INDEX IF NOT EXISTS idx_instinct_applications_pending
    ON instinct_applications (execution_id, step_id)
    WHERE surface = 'lead_recovery' AND result IN ('ignored', 'auto_applied');`,
		Down: `
DROP INDEX IF EXISTS idx_instinct_applications_pending;
CREATE INDEX IF NOT EXISTS idx_instinct_applications_pending
    ON instinct_applications (execution_id, step_id)
    WHERE surface = 'lead_recovery' AND result = 'ignored';
DROP TABLE IF EXISTS instinct_action_history;
DROP INDEX IF EXISTS idx_instinct_evidence_instinct_action;
ALTER TABLE instinct_evidence DROP COLUMN IF EXISTS action;`,
	},
	{
		Version: 102,
		Name:    "memory_policy_eval_chunk_index",
		// Admin chunk-detail "recent evaluations" panel queries
		// memory_policy_evaluations by chunk_id (ListByChunk). The table has
		// ~36k rows and no chunk_id index — the page previously scanned
		// cross-project (ListRecent over 30 days) and filtered in Go,
		// discarding most of what it read. This index makes the per-chunk
		// lookup a bounded index scan. CONCURRENTLY would avoid the write
		// lock, but it can't run in the migration's single transaction; the
		// table's writes are async audit batches that tolerate the brief lock.
		Up: `CREATE INDEX IF NOT EXISTS idx_policy_eval_chunk
    ON memory_policy_evaluations (chunk_id, evaluated_at DESC);`,
		Down: `DROP INDEX IF EXISTS idx_policy_eval_chunk;`,
	},
	{
		Version: 103,
		Name:    "create_budget_reservations",
		// Budget-reservation ledger (trading-hardening §1): closes the
		// read-then-spend TOCTOU in the hard-cap admission check. A
		// reservation is an in-flight claim on budget — inserted atomically
		// at task admission, settled when the task terminates. The admission
		// txn sums unsettled reservations + committed spend so concurrent
		// admissions can't all see the same headroom and overshoot the hard
		// cap.
		//
		// No FK on task_id: the cap gate (and thus the reservation insert)
		// runs BEFORE the task row exists, so a FK would be an ordering
		// deadlock. The watchdog sweep reaps reservations whose task went
		// terminal (settlement missed, e.g. crash) or that went stale (task
		// row never created). The partial index serves the hot unsettled-sum
		// query; the task_id index serves settlement.
		Up: `
CREATE TABLE IF NOT EXISTS budget_reservations (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    estimated_usd DOUBLE PRECISION NOT NULL,
    reserved_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_budget_reservations_unsettled
    ON budget_reservations (project_id) WHERE settled_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_budget_reservations_task
    ON budget_reservations (task_id);`,
		Down: `DROP TABLE IF EXISTS budget_reservations;`,
	},
	{
		Version: 104,
		Name:    "create_a2a_push_configs",
		// A2A push-notification config (steering-notifications design,
		// pushNotificationConfig follow-up): when an A2A caller submits a task
		// with a webhook url, we persist it so the daemon can POST task-state
		// updates (input-required / completed / failed / canceled) to that url
		// even when the caller isn't holding an open SSE stream. Keyed by
		// task_id (one active config per task, last-write-wins). No FK — the
		// config is written right after the task row exists, but keeping it
		// FK-free matches budget_reservations and avoids a delete-cascade
		// surprise. token is the optional bearer the caller wants echoed back
		// on the webhook (A2A authenticates the PUSH to the client).
		Up: `
CREATE TABLE IF NOT EXISTS a2a_push_configs (
    task_id    TEXT PRIMARY KEY,
    url        TEXT NOT NULL,
    token      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);`,
		Down: `DROP TABLE IF EXISTS a2a_push_configs;`,
	},
	{
		Version: 105,
		Name:    "create_recovery_events",
		// Append-only marker for graceful-recovery exits: one row each time an
		// execution reaches a terminal flagged Recovery:true (an intentional
		// on_fail→recovery exit, e.g. dev-pipeline's `checkpoint`). Recovery
		// terminals are COMPLETED-status, so the task-level outcome can't
		// distinguish them — this table makes recovery observable for trends.
		// Mirrors tool_audit_log (append-only); FK-free like the other audit tables.
		Up: `
CREATE TABLE IF NOT EXISTS recovery_events (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    task_id       TEXT NOT NULL,
    execution_id  TEXT NOT NULL,
    workflow_id   TEXT NOT NULL DEFAULT '',
    terminal_id   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_recovery_events_created_at ON recovery_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recovery_events_project ON recovery_events(project_id);
`,
		Down: `
DROP TABLE IF EXISTS recovery_events;
`,
	},
	{
		Version: 106,
		Name:    "execution_step_outcomes_budget_stamp",
		// Instinct ↔ tool-budget seam (LLD instinct-tool-budget-seam-design.md §4).
		// Stamps the resolved complexity tier, effective tool-iteration budget,
		// and actual tool-call count on every agent step outcome row so the
		// budget instinct extractor can mine over/under-provisioning as a pure
		// function over outcome rows alone (no tool_audit_log cross-join).
		// All three columns are NULL-by-default: non-agent steps (system/gate/
		// approval) leave them NULL, and rows written before this migration
		// are unminable (correct — the feature didn't exist then).
		Up: `
ALTER TABLE execution_step_outcomes ADD COLUMN IF NOT EXISTS complexity_tier TEXT;
ALTER TABLE execution_step_outcomes ADD COLUMN IF NOT EXISTS effective_tool_budget INTEGER;
ALTER TABLE execution_step_outcomes ADD COLUMN IF NOT EXISTS tool_calls_used INTEGER;
`,
		Down: `
ALTER TABLE execution_step_outcomes DROP COLUMN IF EXISTS complexity_tier;
ALTER TABLE execution_step_outcomes DROP COLUMN IF EXISTS effective_tool_budget;
ALTER TABLE execution_step_outcomes DROP COLUMN IF EXISTS tool_calls_used;
`,
	},
	{
		Version: 107,
		Name:    "artifact_origin",
		// Artifact provenance for outputguard first-party content
		// (outputguard-provenance-design.md §4.3, Slice 2).
		// origin classifies each artifact's content source so the
		// outputguard can skip injection-class rules on agent-authored
		// content (task_output). Default 'unknown' is the safe fallback
		// (treated as third-party — full rule set runs). Idempotent:
		// ADD COLUMN IF NOT EXISTS is a no-op on re-run.
		Up:   `ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'unknown';`,
		Down: `ALTER TABLE artifacts DROP COLUMN IF EXISTS origin;`,
	},
	{
		Version: 108,
		Name:    "api_keys_allow_push",
		// git-over-HTTPS workspace access (LLD slice 2). Adds a per-key
		// allow_push BOOLEAN so push is opt-in and read-only is the safe
		// default for existing keys. Default false: legacy keys cannot
		// push after a daemon upgrade unless explicitly granted.
		// Idempotent: ADD COLUMN IF NOT EXISTS is a no-op on re-run.
		Up:   `ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS allow_push BOOLEAN NOT NULL DEFAULT false;`,
		Down: `ALTER TABLE api_keys DROP COLUMN IF EXISTS allow_push;`,
	},
	{
		Version: 109,
		Name:    "trading_exec_reconcile_columns",
		// Exec-reconcile slice (commit f361e99d) added FilledQty to
		// TradingOrder and ExecID/AccountID/Source/SourceDetail to
		// TradingFill on the Go model + the SQLite schema, but shipped
		// NO Postgres migration. On the prod (Postgres) DB the columns
		// never existed, so the reconcile loop's persist failed every
		// tick with `column "filled_qty" of relation "trading_orders"
		// does not exist` (observed 2026-06-27). The shadow-reconcile
		// table the PG fill repository writes to (trading_fills_shadow)
		// was likewise SQLite-only. This migration brings Postgres to
		// parity with the model + SQLite schema. Additive and
		// idempotent (ADD COLUMN / CREATE TABLE IF NOT EXISTS); the
		// NOT NULL columns carry defaults so existing rows are valid.
		Up: `
ALTER TABLE trading_orders ADD COLUMN IF NOT EXISTS filled_qty NUMERIC(18,6) NOT NULL DEFAULT 0;
ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS exec_id TEXT;
ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS account_id TEXT;
ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'reconcile';
ALTER TABLE trading_fills ADD COLUMN IF NOT EXISTS source_detail TEXT;
CREATE TABLE IF NOT EXISTS trading_fills_shadow (
    id                  TEXT PRIMARY KEY,
    order_id            TEXT,
    project_id          TEXT NOT NULL,
    symbol              TEXT NOT NULL,
    qty                 NUMERIC(18,6) NOT NULL,
    price               NUMERIC(18,6) NOT NULL,
    commission_usd      NUMERIC(18,6),
    exec_id             TEXT,
    account_id          TEXT,
    source              TEXT NOT NULL DEFAULT 'reconcile',
    source_detail       TEXT,
    filled_at           TIMESTAMP WITH TIME ZONE NOT NULL,
    recorded_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_trading_fills_shadow_project_time
    ON trading_fills_shadow(project_id, recorded_at DESC);
COMMENT ON TABLE trading_fills_shadow IS 'Shadow-reconcile fill records (VORNIK_BROKER_EXEC_RECONCILE_SHADOW) — broker-side fills logged without affecting the authoritative trading_fills ledger.';
`,
		Down: `
DROP TABLE IF EXISTS trading_fills_shadow;
ALTER TABLE trading_fills DROP COLUMN IF EXISTS source_detail;
ALTER TABLE trading_fills DROP COLUMN IF EXISTS source;
ALTER TABLE trading_fills DROP COLUMN IF EXISTS account_id;
ALTER TABLE trading_fills DROP COLUMN IF EXISTS exec_id;
ALTER TABLE trading_orders DROP COLUMN IF EXISTS filled_qty;
`,
	},
	{
		Version: 110,
		Name:    "api_keys_default_repo_scope",
		// Per-key default repo_scope for companion memory. Scope on a
		// companion deposit was entirely client-driven: the MCP handlers
		// read repo_scope ONLY from the caller's argument, so any client
		// without a SessionStart scope injector (Codex ships none) that
		// omitted the arg produced NULL-scoped chunks. This column gives
		// the server a per-key fallback the MCP memory surface stamps when
		// the caller omits repo_scope. Nullable additive column; existing
		// keys keep NULL (= no default) so behaviour is unchanged until an
		// operator sets one via `vornikctl companion grant --repo-scope`.
		Up: `ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS default_repo_scope TEXT;`,
		// Postgres down path. SQLite has no migrations table — its schema is
		// the canonical CREATE TABLE in internal/persistence/sqlite/schema.go,
		// and a SQLite rollback would need ALTER TABLE ... DROP COLUMN
		// (>= 3.35.0, and SQLite's DROP COLUMN has no IF EXISTS clause) or a
		// table-recreate. See the schema.go comment on default_repo_scope.
		Down: `ALTER TABLE api_keys DROP COLUMN IF EXISTS default_repo_scope;`,
	},
}
