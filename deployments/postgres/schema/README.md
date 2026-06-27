# vornik PostgreSQL Schema

This directory contains the database schema for vornik's persistence layer.

## Overview

vornik uses **PostgreSQL** for all transactional state. The schema is designed for:

- **Durability**: State survives process restarts
- **Atomic operations**: Safe task claiming via leasing
- **Resumability**: Workflow executions can be recovered from checkpoints
- **Queue management**: Efficient scheduling queries with priority ordering
- **Migration tracking**: Schema versions are recorded in `migrations`

## Schema Files

| File | Description |
|------|-------------|
| `001_initial.sql` | Bootstrap tables: migrations, tasks, executions, artifacts |

## Tables

### `tasks`

The transactional heart of the queue system. Each row represents a unit of work progressing through a defined lifecycle.

**Key Fields:**
- `id` - Unique identifier (UUID v7 recommended)
- `project_id` - Project association (tenancy boundary)
- `workflow_id` - Workflow governing this task
- `parent_task_id` - Delegation lineage tree
- `status` - Lifecycle state (enum)
- `priority` - Scheduling priority (0-100; lower numbers run first)
- `payload` - JSON blob with prompt/inputs/context
- `dependencies` - Task IDs that must complete first

**Lease Fields:**
- `lease_id` - Unique lease identifier
- `leased_at` - When lease was acquired
- `leased_by` - Lease holder identifier
- `lease_expires_at` - Deadline for automatic recovery

**Retry Fields:**
- `attempt` - Current execution attempt
- `max_attempts` - Maximum retries allowed
- `last_error` - Most recent failure message

### `executions`

Tracks runtime state of workflow executions. The `state_snapshot` field enables durable checkpointing and resumption.

**Key Fields:**
- `id` - Unique identifier
- `task_id` - Associated triggering task
- `workflow_id` / `workflow_revision` - Version-pinned workflow
- `status` - Execution state (enum)
- `current_step_id` - Current workflow step
- `completed_steps` - Array of finished step IDs
- `state_snapshot` - Full resumable state (JSONB)

### `artifacts`

Index of durable outputs and intermediate products. Content is stored on the filesystem; this table tracks metadata.

**Key Fields:**
- `id` - Unique identifier
- `project_id` / `execution_id` / `task_id` - Associations
- `name` - User-provided filename
- `artifact_class` - Classification (enum)
- `storage_path` - Relative path to content
- `size_bytes` / `content_hash_sha256` - Integrity metadata

## Enums

### `task_status`

```
PENDING → QUEUED → LEASED → RUNNING → (WAITING_FOR_CHILDREN | COMPLETED | FAILED | CANCELLED)
```

| State | Description |
|-------|-------------|
| `PENDING` | Awaiting approval before schedulable |
| `QUEUED` | Ready for scheduler pickup |
| `LEASED` | Claimed, reserved for execution |
| `RUNNING` | Currently executing |
| `WAITING_FOR_CHILDREN` | Blocked on delegated child tasks |
| `COMPLETED` | Successfully finished |
| `FAILED` | Failed after retry exhaustion |
| `CANCELLED` | Cancelled by operator |

### `execution_status`

```
PENDING → RUNNING → (COMPLETED | FAILED | CANCELLED)
```

### `task_creation_source`

| Source | Description |
|--------|-------------|
| `USER` | Direct submission (API, CLI, chat) |
| `DELEGATION` | Created by parent task |
| `AUTONOMOUS` | Created by swarm lead autonomously |

### `delegation_mode`

| Mode | Description |
|------|-------------|
| `SEQUENTIAL` | Parent waits for completion |
| `PARALLEL` | Child runs independently |
| `FAN_OUT` | Multiple children, parent waits for all |

### `artifact_class`

| Class | Description |
|-------|-------------|
| `OUTPUT` | Final deliverable |
| `INTERMEDIATE` | Work-in-progress |
| `SNAPSHOT` | Checkpoint for retry/recovery |
| `LOG` | Execution log or trace |
| `METADATA` | Supplementary metadata file |

## Indexes

### Critical Path Indexes

| Index | Purpose |
|-------|---------|
| `idx_tasks_queue_lookup` | Scheduler priority queue query (`priority ASC`, then FIFO by queue timestamp) |
| `idx_tasks_lease_expired` | Lease recovery scan |
| `idx_tasks_project` | Project-scoped queries |

### Supporting Indexes

| Index | Purpose |
|-------|---------|
| `idx_tasks_status` | Status filtering |
| `idx_tasks_parent` | Delegation tree traversal |
| `idx_tasks_workflow` | Workflow association |
| `idx_tasks_dependencies` | GIN index for dependency array |
| `idx_executions_task` | Task→Execution lookup |
| `idx_executions_running` | Running execution count |
| `idx_artifacts_hash` | Content deduplication |

## Views

### `active_tasks`

All tasks currently being processed or queued (not in terminal state).

### `failed_tasks`

Failed tasks requiring operator attention. Serves as the dead-letter queue view.

## Query Patterns

### Lease a Task (Atomic)

```sql
UPDATE tasks
SET
    status = 'LEASED',
    lease_id = gen_random_uuid()::text,
    leased_at = NOW(),
    leased_by = 'scheduler-1',
    lease_expires_at = NOW() + INTERVAL '5 minutes'
WHERE id = (
    SELECT id FROM tasks
    WHERE status = 'QUEUED'
    ORDER BY priority ASC, created_at ASC
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING *;
```

### Find Expired Leases

```sql
SELECT id, lease_id, lease_expires_at
FROM tasks
WHERE status IN ('LEASED', 'RUNNING')
  AND lease_expires_at < NOW();
```

### Check Dependencies Met

```sql
SELECT t.id, t.dependencies
FROM tasks t
WHERE t.status = 'QUEUED'
  AND array_length(t.dependencies, 1) > 0
  AND NOT EXISTS (
    SELECT 1 FROM unnest(t.dependencies) AS dep_id
    JOIN tasks dep ON dep.id = dep_id
    WHERE dep.status != 'COMPLETED'
  );
```

### Get Execution State

```sql
SELECT 
    e.id, e.status, e.current_step_id, 
    e.state_snapshot, e.completed_steps
FROM executions e
WHERE e.task_id = $1
ORDER BY e.created_at DESC
LIMIT 1;
```

### List Project Artifacts

```sql
SELECT a.id, a.name, a.artifact_class, a.size_bytes, a.created_at
FROM artifacts a
WHERE a.project_id = $1
ORDER BY a.created_at DESC;
```

## Migration Strategy

New migrations should be numbered sequentially:
- `002_add_project_settings.sql`
- `003_add_audit_log.sql`
- etc.

Each migration should:
1. Be idempotent where practical
2. Include rollback comments
3. Update this README if adding new tables/enums

## Design Decisions

1. **UUID v7 for IDs**: Time-orderable for natural sorting without exposing sequence gaps
2. **JSONB for flexible fields**: `payload` and `state_snapshot` need schema flexibility
3. **Array type for dependencies**: Efficient GIN indexing for dependency checks
4. **Partial indexes**: Index only relevant rows (e.g., `WHERE status = 'QUEUED'`)
5. **Enum types**: Type-safe status values with clear documentation
6. **Explicit lease fields**: Enables proper distributed locking semantics

## References

- Architecture: `/https://docs.vornik.io`
- Queue Manager LLD: `/https://docs.vornik.io`
- Persistence LLD: `/https://docs.vornik.io`
