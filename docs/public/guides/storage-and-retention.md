---
sources:
    - path: docs/operator/storage-and-retention.md
      sha256: 8530705d6e3b5de3dda517434f7b3b61a8b8bd356b0174020436b7307388692b
---
# Storage and Retention

vornik produces two kinds of data that grow over time: **artifacts** (files
agents read and write) and **operational history** (task records, execution
logs, cost rows). This guide covers where artifacts live, how to keep history
from growing without bound, and how to back up and restore a deployment.

All settings here live in your daemon config
(`~/.config/vornik/vornik.yaml`). See
[Configuration reference](../reference/configuration.md) for the full key
list.

---

## Artifact storage

By default vornik stores artifacts on the local filesystem. You can instead
point it at an S3-compatible object store — AWS S3, MinIO, or Ceph RGW — which
is the better choice for durability and for deployments that may scale beyond
a single host.

### Filesystem (default)

```yaml
storage:
  artifacts_path: /var/lib/vornik/artifacts
  # backend: "filesystem"   # default; may be omitted
```

### AWS S3

```yaml
storage:
  artifacts_path: /var/lib/vornik/artifacts   # still used for staging
  backend: "s3"
  s3:
    region: "us-east-1"
    bucket: "vornik-prod-artifacts"
    prefix: "vornik/prod"
    # Leave endpoint and credentials empty to use the AWS SDK's
    # default resolver and credential chain (IAM role, ~/.aws, env).
```

For production, prefer an IAM role over static credentials. Run the daemon
under an instance profile that grants only the permissions it needs:
`s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`, and `s3:ListBucket` on your
bucket and its contents.

### MinIO (local development)

```yaml
storage:
  artifacts_path: /var/lib/vornik/artifacts
  backend: "s3"
  s3:
    endpoint: "http://localhost:9000"
    region: "us-east-1"          # MinIO accepts any non-empty value
    bucket: "vornik-dev"
    access_key_id: "minioadmin"
    secret_access_key: "minioadmin"
    use_path_style: true         # required for MinIO
    force_ssl: false             # only safe for localhost development
```

### Credentials via environment

You can leave the credential fields blank in YAML and supply them via
environment variables instead — the recommended pattern for production:

```bash
export VORNIK_STORAGE_S3_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
export VORNIK_STORAGE_S3_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
```

!!! warning "Create the bucket first, and mind TLS"
    vornik does not create buckets for you. The bucket must exist before the
    daemon starts, or boot fails with a clear `bucket not found` error.
    Versioning, object lock, and lifecycle policies are yours to configure and
    are recommended for production.

    `force_ssl` defaults to `true` and refuses plain-HTTP endpoints. Only set
    it to `false` for local MinIO development.

---

## Retention

The retention sweeper prunes old operational data on a schedule. It is **off
by default** — nothing is deleted until you enable it. Each category of data
has its own age threshold in days.

```yaml
retention:
  enabled: true            # default: false - opt in
  interval: "6h"
  task_llm_usage_days: 90  # cost history
  tool_audit_days: 30      # tool/debug records
  tasks_days: 60           # finished tasks
  executions_days: 60      # finished executions
  artifacts_days: 60       # artifact records and their files
  task_messages_days: 30   # chat history (0 = keep with the parent task)
  memory_chunks_days: 0    # long-term memory (0 = keep forever)
```

What the defaults mean:

- `memory_chunks_days: 0` keeps long-term memory forever, which is the right
  posture for most deployments. Long-term memory is product data, not
  throwaway logs — set a finite value only if storage pressure forces it.
- `task_messages_days: 0` means chat history is pruned only when its parent
  task is pruned. Set it explicitly to trim chat faster than tasks.

### Per-project overrides

Any of these thresholds can be set on an individual project's config to
override the daemon default. A common pattern is keeping cost data longer for
billing while trimming chat sooner:

```yaml
# In a project config:
retention:
  task_llm_usage_days: 365
  task_messages_days: 14
```

The minimum effective retention is one day. Setting a field to `0` means
"inherit the default," not "delete immediately."

### Previewing and applying

`vornikctl retention` runs in preview mode by default — it shows what *would*
be pruned without deleting anything. Add `--apply` to actually delete.

```bash
# Preview across every project (no deletes)
vornikctl retention

# Preview a single project
vornikctl retention --project personal-assistant

# Actually prune
vornikctl retention --project personal-assistant --apply

# Machine-readable output
vornikctl retention --json
```

!!! danger "Artifact pruning is permanent"
    Pruning artifacts removes both the database record and the underlying file
    (or object, on S3). It cannot be undone from within vornik — recovery
    means restoring from a backup. Run a preview first.

---

## Backup and restore

`vornikctl backup` captures a complete snapshot you can restore later.

```bash
# Auto-named archive in the current directory
vornikctl backup

# Explicit output path
vornikctl backup --out /backups/vornik-2026-05-19.tar.gz
```

The archive contains a full database dump, every artifact file (for the
filesystem backend), and all project workspaces.

!!! note "S3 backends are backed up separately"
    If you use the S3 artifact backend, `vornikctl backup` does **not** copy
    your S3 objects — back the bucket up with native S3 tooling (for example,
    `aws s3 sync`). The database dump and workspaces are still captured.

To restore, stop the daemon first, then:

```bash
vornikctl restore --from /backups/vornik-2026-05-19.tar.gz --force
```

The restore runs as a single transaction: it either imports completely or
rolls back cleanly, never leaving a half-applied database.

As a safety check, restore refuses to overwrite a database that already
contains projects or tasks, so you don't accidentally clobber a live system:

```text
error: target database is not empty (1 project, 47 tasks)
       (pass --allow-non-empty to override)
```

If you genuinely intend to re-apply into a populated database, add
`--allow-non-empty`.

!!! warning "Stop the daemon before restoring"
    Restoring into a database the daemon is actively using has undefined
    results. The `--force` flag is a contract that you have stopped the
    daemon, not a way to skip doing so.

---

## Context budget and tier

Separate from disk storage, each chat session has a token budget — the model's
context window. As a conversation fills that budget, vornik progressively
tightens how many tools it exposes per turn, recovering prompt space exactly
when it matters. This keeps long conversations working instead of failing once
they approach the window limit.

This behavior only turns on when you tell vornik how large the context window
is:

```yaml
chat:
  # Set to your model's context window in tokens to enable tier-aware
  # tool management. 0 (the default) leaves it off.
  context_size: 200000
```

With this set, chat replies carry an `X-Vornik-Context-Tier` header
(`peak`, `good`, `degrading`, or `poor`) that the UI uses to colour the
session's context badge so you can see at a glance how much headroom remains.

If you want guaranteed full-tool turns, either raise the budget or shorten the
conversation with `/summarize`.

---

## Where to go next

- [Cost and caching](cost-and-caching.md) — keep LLM spend predictable.
- [Memory and RAG](../features/memory-rag.md) — what long-term memory stores.
- [Configuration reference](../reference/configuration.md) — every key and its
  default.
