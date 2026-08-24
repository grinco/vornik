---
sources:
    - path: internal/service/container_scheduler.go
      sha256: f68b1734378e56ec45800767eabcb390d839c897e6e0b93a74a58ae61c487b56
    - path: internal/service/memory_adapter.go
      sha256: b371b60a90ee5fa856e51bef0df88653feb0c2dac9d15730289cd3fb4ff6a40b
    - path: internal/memory/reranker.go
      sha256: fa57cbdcf4bbbb803d0074c2eb3740dc0d509d92a9b91d1cf3495b9cf8dc3ad8
    - path: internal/config/config.go
      sha256: d05c764c8e2bd5764003cb480bb4d77179a84251369d82312476c614659bb211
---
# Reference architecture

This page describes the expected shape of a healthy vornik deployment, so you can
check an install against it. It is written to be *executable*: the
`validate-install` companion skill reads this page, inspects a live deployment, and
reports where the two diverge. Every section states what the reference shape **is**,
how to **observe** it on a running system, and — where the distinction matters —
whether a divergence is a defect or a legitimate choice.

## What this page is not

- **Not a sizing guide.** No host, CPU, memory or network topology. Two installs
  with identical architecture can run on very different hardware.
- **Not an installation runbook.** It describes the destination, not the route. For
  the route, see [Getting started](../getting-started/index.md).
- **Not a list of required features.** Most subsystems are optional. A validator
  that treats "absent" as "broken" produces noise, and noise trains operators to
  ignore it. [What to report](#what-a-validator-should-actually-report) states which
  findings are real.

## The shape in one paragraph

A vornik deployment is **one daemon** owning **one PostgreSQL database**, serving an
HTTP API, executing work as **containerised agents** on a runtime it controls, and
carrying long-term state in a **memory subsystem** backed by pgvector. Work is
described by a three-level registry — **projects** choose a **swarm** (who works) and
a **workflow** (how) — read from a config tree on disk. Everything else (chat
channels, autonomy, control plane, MCP servers) is an optional subsystem attached to
that core.

The single most common cause of "my change did nothing" is that the daemon read a
**different config tree** than the one you edited. That is why the configuration
section exists.

## The core: one daemon, one database

### Daemon

A single long-lived process (`vornik-enterprise` for Enterprise, `vornik` for
Community), normally under a user-level systemd unit rather than root. It resolves
its configuration **once at start** — environment changes require a restart, not a
reload.

**Observe:** `systemctl --user is-active vornik` and its `ExecStart` path.

**Reference:** exactly one daemon per database. Two daemons sharing a database is a
misconfiguration, not a scaling strategy — they will contend on leader-elected
workers, channel polling, and the scheduler. A genuine multi-node deployment uses
node roles, not duplicated daemons. See [Clustering](../guides/clustering.md).

### Database

One PostgreSQL instance with the **pgvector** extension, holding all durable state:
tasks, executions, artifact metadata, memory chunks and their embeddings, audit
trails, and the usage ledger.

**Observe:**

```sql
SELECT extversion FROM pg_extension WHERE extname = 'vector';
```

**Reference:** pgvector present; migrations applied to the current head; the
embedding column's dimension matching the configured embedding model. A dimension
mismatch fails **every** insert, so it presents as a total memory outage rather than
as degraded quality.

**The database name carries no meaning.** Installs are routinely named for their
history rather than their role, and a name that reads like a test database is
frequently the production one. Never infer an environment from a name — and never
offer to drop one on that basis.

## Configuration: the two-tree rule

This is the highest-yield thing to check, because it is the most common real failure
and it is silent.

vornik reads configuration from **two separate places**:

|                     | What it holds                                              | Reloadable          |
| ------------------- | ---------------------------------------------------------- | ------------------- |
| `config.yaml`       | daemon-wide settings: database, server, subsystem toggles   | **No** — restart    |
| the **registry tree** | `projects/`, `swarms/`, `workflows/`                      | Yes — `config reload` |

The registry tree is resolved through a **fallback chain**, not a single variable,
and the CLI's chain differs from the daemon's. `VORNIK_CONFIGS_DIR` is honoured only
when the directory already contains all three subdirectories — otherwise it is
skipped **with no error at all**.

The failure mode: you edit a file in a source checkout, the daemon reads a deployed
copy elsewhere, the change appears to do nothing, and nothing anywhere reports a
problem.

**Observe:** the daemon logs its resolved paths at start (`config watcher started`
lists them). That log line is ground truth; a file outside those paths is not read.

**Reference:** the tree the daemon reports is the tree being edited. Where a
source-of-truth checkout and a deployed copy both exist, a drift check between them
runs as part of deployment.

**Finding:** a registry file that exists on disk but not under a daemon-reported path
is a **defect**, and a high-severity one — it means your mental model and the system
disagree. See [Configuration](configuration.md) for the full resolution order.

## Work definition: projects, swarms, workflows

Three levels, each answering one question:

| Level        | Question                                             | Form                                    |
| ------------ | ---------------------------------------------------- | --------------------------------------- |
| **Project**  | *what body of work, with what limits*                | one YAML per project                    |
| **Swarm**    | *who does it* — roles, their models, container image | one Markdown-with-frontmatter per swarm |
| **Workflow** | *how* — steps, their types, timeouts, prompts        | one Markdown-with-frontmatter per workflow |

A project names exactly one swarm and one default workflow. Both must resolve, or the
project fails validation at load — loudly, which is correct.

**Reference project fields:**

```yaml
projectId: "docs-review"              # stable identifier; never reused
displayName: "Documentation review"
swarmId: "docs-review-swarm"          # must resolve
defaultWorkflowId: "architectural-review"
adaptiveCandidateWorkflows:           # bounds adaptive routing
  - "doc-review"
  - "rag-ingest"
defaultPriority: 50
maxConcurrentTasks: 3                 # the per-project concurrency bound
```

**`maxConcurrentTasks` is the load-bearing one.** Without it a single project can
occupy the whole executor and starve every other project. Treat its absence as a
finding even though the daemon starts happily.

**`adaptiveCandidateWorkflows` has a sharp edge:** when the list is empty or missing,
an adaptive route's selection is *silently ignored* and the workflow terminates
without spawning a child task. Present-but-empty behaves differently from absent.

**Reference swarm role:**

```yaml
roles:
  - name: "lead"
    model: "your-model-id"
    runtime:
      image: "localhost/vornik-agent:latest"   # required
```

Every role needs a runtime image. Roles routinely differ in model — a cheap model for
classification, a stronger one for synthesis — and that heterogeneity is normal, not
drift.

**Observe:** `GET /api/v1/projects` for what resolved, and
`GET /api/v1/config/reload-status` for the load errors and warnings of the last
attempt, which name the offending file and field. Do not trigger a reload to find
out — see the [observation table](#observation-quick-reference).

## Memory: the subsystem with the most ways to be quietly wrong

Memory is where a deployment is most likely to be *running* and *not working*.

### Ingest

Content enters memory through several paths, and they differ in caller, cap and gate
behaviour. Deposits pass a gate stack — secret redaction, dedup by content hash,
minimum length, policy class. **A gate rejection is normal operation, not an error**,
and a validator must not report quarantined content as breakage.

**Reference:** secret scanning active on every ingest path. Content below the minimum
length is rejected — expected, and the reason short probe deposits "mysteriously"
fail.

### Embeddings, and the two clocks

Embedding is **asynchronous**. Content is searchable by keyword immediately and
semantically only once its embedding lands. A deployment that has just ingested a
large corpus is in a legitimate intermediate state, not a broken one.

**Observe:** `GET /api/v1/memory/stats` → `chunksTotal`, `chunksEmbedded`,
`queueDepth`, and an `embedder` block giving the **resolved** provider, model and
dimension.

Compare that block against your configured model rather than trusting the config
alone: it is the embedding half of the resolved-state rule below. A divergence means
the daemon resolved something other than what you wrote.

**One caveat.** The block reports the embedder in force for work done *now*, not the
one that produced the vectors already stored. Pointing a deployment at a new
embedding model does not re-embed the existing corpus, and because unchanged content
is deduplicated on re-ingest, old vectors survive a model change silently. If you
change embedding models, re-embed explicitly.

**Reference:** `queueDepth` trending to zero. A **persistently** non-zero queue with
a non-empty dead-letter queue means embeddings are failing — most often a
provider/credential problem or a dimension mismatch.

**Finding:** `chunksEmbedded` far below `chunksTotal` with an idle queue is a
**defect** (embeddings are failing silently). The same ratio with a draining queue is
**informational**.

Chunks carry **two timestamps** and conflating them is a real bug class:

- **ingest time** (`created_at`) — when we learned it
- **event time** (`event_time`, nullable) — when the content pertains to

Temporal filters bound event time, falling back to ingest time when it is unknown.
Recency digests, TTL freshness and backfill ordering deliberately use **ingest**
time: a document about 2019 ingested yesterday is *fresh knowledge*.

### Retrieval, and the reranker trap

Retrieval fuses a semantic arm and a keyword arm by reciprocal rank. Two paths exist
and they are not equivalent:

| Path                 | Ordering                       | Cost                       |
| -------------------- | ------------------------------ | -------------------------- |
| interactive recall   | rank fusion only               | fast, no extra model call   |
| **context assembly** | fusion **plus an LLM reranker** | one extra model call        |

Reranking is opt-in per request. A caller that does not ask for it does not get it —
**however correctly the reranker is configured**.

This is the trap, and it is worth stating plainly because we shipped it: a deployment
can have `reranker.enabled` set to `true`, the reranker wired, and the reranker never
firing once across a six-figure count of recorded model calls, because the request
never carried the flag. Configuration said yes; behaviour said nothing; nothing
reported the gap. The request path was fixed, but the *shape* of the bug is general —
a config value is a statement of intent, not evidence of behaviour.

**Observe:** the daemon logs the **resolved** state at start —
`memory reranker ACTIVE` or `memory reranker INERT` with the reason and the specific
gate that closed. Then confirm behaviour, not intent:

```sql
SELECT role, count(*) FROM task_llm_usage WHERE role ILIKE '%rerank%' GROUP BY role;
```

**Reference:** if `reranker.enabled` is true, the log says ACTIVE **and** the ledger
accumulates reranker rows once context-assembly recall runs. Enabled-plus-zero-rows
is a **defect**.

Scored-sufficiency widening is **reranker-gated** — it cannot activate without a live
reranker, so an enabled sufficiency block on an inert reranker is doing nothing.

### Scope isolation

Memory is partitioned by a repo-scope token so one operator's many repositories do
not dilute each other's recall. A scoped query includes NULL-scoped content by
default (a migration-grace allowance); strict scoping excludes it.

**Finding:** a large NULL-scoped population is **informational** — it is the
un-migrated tail. Content leaking *across* two non-null scopes would be a serious
defect, but requires a deliberate probe to detect.

## Execution: containerised agents

Work runs as short-lived containers, one per role invocation, on a runtime the daemon
manages. The reference posture is deliberately tight:

- no new privileges, all capabilities dropped
- `--network none` for agents that need no egress; those reach the daemon over a
  bind-mounted unix socket instead
- read-only input mount, writable output and workspace mounts
- the project's git worktree mounted per task

**Observe:** the daemon's container-start log lines carry the full argument vector.

**Reference:** capabilities dropped and no-new-privileges present on every agent
container. Their absence is a **security defect**, not a preference.

**Do not confuse a failed container with a broken install.** Agents fail for ordinary
reasons — a model refusing, a timeout, an unreachable endpoint — and the executor
retries and classifies them. Persistent failure of one *class* is the signal;
individual failures are not.

## Optional subsystems

All of these are absent in a valid minimal install. **Absence is never a finding.**
Each is worth checking only for *internal* consistency: configured-but-not-working is
the defect shape.

### Authentication

Per-project API keys with an admin-class key for privileged operations, plus narrower
companion keys carrying explicit capability booleans (memory read, memory write,
skill access).

**Reference when enabled:** every key scoped to one project with the narrowest
capabilities its job needs. **Never print or log a key.**

**Sharp edge:** after enabling auth, an endpoint that returns empty lists rather than
a 401 usually means a visibility filter is missing an admin-class bypass — the data is
there and the caller cannot see it.

### Chat channels, autonomy, control plane

Channels attach conversational front-ends. Autonomy schedules recurring work. The
control plane proposes configuration and tuning changes for operator approval.

**The polling trap:** a channel that polls a single-consumer upstream (a bot API, for
instance) cannot have two consumers. A second daemon — or a test instance sharing
production credentials — will steal updates from the first, and the symptom appears on
the *victim*, not the culprit.

**Finding:** two daemons configured with the same channel credentials is a **defect**
regardless of which one you consider primary.

### Multi-node

Node roles let one deployment span hosts. Background workers are leader-elected, so
adding a node does not duplicate their work.

**Reference:** a single-node install reporting its cluster feature as *degraded* is
**expected**, not a finding — the feature is describing an absent capability, not a
fault.

### MCP servers and automations

Attach external tools; synthesise automations from natural language. Each is
independently switchable. Same rule: check internal consistency, ignore absence.

## What a validator should actually report

The hardest part of validation is not finding divergences — it is not drowning the
operator. Ordered by value:

**Defects (report loudly):**

1. A registry file outside every daemon-reported config path — the change is not
   being read, and nothing says so.
2. pgvector absent, or embedding dimension mismatched against the model.
3. Embeddings persistently unembedded with an idle queue — whether or not the
   dead-letter queue has anything in it. An empty DLQ next to an idle queue is the more
   dangerous shape, not the safer one: it means those chunks were lost from the pipeline
   rather than failed, so nothing will retry them and nothing was logged. Compare
   `chunksTotal` against `chunksEmbedded`; a persistent gap with `queueDepth == 0` is a
   defect whatever the DLQ says. Repair with
   `vornikctl memory reembed --project <p> --only-missing`.
4. A subsystem enabled in config whose resolved state is inert — the reranker is the
   canonical case; the boot log states this directly.
5. Two daemons on one database, or sharing single-consumer channel credentials.
6. Agent containers missing capability drops or no-new-privileges.
7. A project without a concurrency bound — one project can starve all others.

**Informational (state once, do not repeat):**

- Optional subsystems absent or degraded on a single-node install.
- NULL-scoped memory content — the un-migrated tail.
- Individual task or container failures.
- Gate-quarantined memory deposits.
- A draining embedding queue.

**Never report:**

- Database, project or swarm *names* as wrong. Naming is the operator's.
- Model choices. Heterogeneous models across roles is the intended design.
- Key material, secrets, or config values from a secrets tree — in any form.

**The governing principle:** prefer "configured X, observed Y" over "X is wrong". A
validator's credibility is spent the first time it reports a healthy install as
broken, and it does not recover.

## Observation quick reference

Every row is read-only, and that is a hard constraint rather than a convenience: a
validator runs against a system whose behaviour it does not own.
`POST /api/v1/config/reload` is the trap here — it looks like the natural way to ask
"is the registry valid?", and it answers by *applying* the tree, which on a live
system may push a half-edited file into service. Ask `reload-status` instead, which
reports the same errors from the last attempt without triggering one.

| Question                        | How                                                                  |
| ------------------------------- | -------------------------------------------------------------------- |
| Daemon alive, and which binary  | `systemctl --user is-active vornik`; unit `ExecStart`                 |
| Which config tree is read       | boot log: `config watcher started` → `paths`                         |
| pgvector present                | `SELECT extversion FROM pg_extension WHERE extname='vector'`          |
| Projects loaded                 | `GET /api/v1/projects`                                               |
| Registry valid                  | `GET /api/v1/config/reload-status` — errors, warnings, blocked reason |
| Memory volume + embedding health | `GET /api/v1/memory/stats`                                          |
| Resolved embedder (provider/model/dims) | `GET /api/v1/memory/stats` → `embedder` |
| Embedding failures              | `SELECT count(*) FROM memory_embed_dlq`                              |
| Reranker resolved state         | boot log: `memory reranker ACTIVE\|INERT`                            |
| Reranker actually running       | `task_llm_usage` where `role ILIKE '%rerank%'`                       |
| Model spend by role             | `task_llm_usage` grouped by `role`                                   |
| Feature health                  | `vornikctl doctor feature`                                           |
| Broad health sweep              | `vornikctl doctor` (`--offline` when the daemon is down)              |

**`vornikctl doctor` first.** It already encodes many of these checks with
remediations attached, and a validator that re-implements them will drift from the
product. Prefer routing to it over duplicating it. See
[vornikctl](vornikctl.md) and [Feature doctor](../guides/feature-doctor.md).
