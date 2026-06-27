<!-- Generated from source — do not edit by hand. -->

# vornikctl CLI reference

vornikctl inspects and controls a running vornik daemon.

## vornikctl backup

Create a one-file archive of the vornik deployment

Create a tarball containing:
  - pg_dump of the database
  - the artifacts directory
  - the project workspaces directory (per-project git repos)
  - a snapshot of the main config file + configs/ directory

Output path defaults to ./vornik-backup-YYYYMMDD-HHMMSS.tgz. The archive
is portable across hosts when restored via 'vornikctl restore'.

Requires: pg_dump on PATH with credentials resolved from config.

Examples:
  vornikctl backup
  vornikctl backup --out /backups/daily.tgz


```
vornikctl backup [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--out` |  | output archive path (default auto-generated) |

## vornikctl config

Inspect and control daemon configuration

## vornikctl config reload

Trigger a configuration reload

Trigger a manual reload of the daemon config and registry. Equivalent
to sending SIGHUP to the vornik process. Useful when the file watcher
doesn't pick up a change (network filesystem, edit-in-place with
overwrite semantics, etc).

```
vornikctl config reload [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--force` | `false` | Reload even when validation errors are present |

## vornikctl config reload-status

Show last reload outcome and any validation errors

```
vornikctl config reload-status [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output instead of the human summary |

## vornikctl config show

Dump effective daemon config (secrets redacted)

```
vornikctl config show [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `true` | JSON output (the only supported shape — default) |

## vornikctl doctor

Diagnose and repair common issues

Run diagnostic checks against the vornik daemon and optionally fix issues.

Operational state:
  stale_leases          Tasks stuck in LEASED/RUNNING with expired leases
  orphaned_watchers     Telegram watchers for tasks already completed/failed
  stuck_executions      Executions in RUNNING/PENDING for over 1 hour
  task_state_audit      Terminal tasks with leaked lease fields

Schema & storage:
  config_validation     Validate all project/swarm/workflow YAML configs
  workflow_swarm_compat Flag (project, workflow) pairs whose swarm can't satisfy the workflow's roles
  role_prompt_sanity    Lint swarm role prompts: tool refs vs allowedTools, output shape, untrusted_content awareness
  eval_suite_lint       Parse configs/evals/*.json; flag suites with missing/incompatible project/workflow/swarm
  database_schema       Verify expected tables and indexes exist (incl. 2026.4.11+ additions)
  orphan_fk_rows        Detect orphan rows in audit / llm_usage / watchers referencing missing tasks
  orphan_worktrees      .worktrees/ subdirs with no matching live task
  config_crlf           Config files with CRLF line endings (UI YAML-writer drift); --fix normalizes to LF

Runtime:
  podman_config         Check podman availability and rootless configuration
  agent_images          Verify agent images referenced in swarm configs are available
  env_file_freshness    Flag EnvironmentFile= entries modified after daemon
                        start (systemd reads them only at ExecStart, so
                        post-edit secrets are invisible until restart)

Security:
  api_security_posture  Flag API auth disabled with non-loopback listen address
  api_key_strength      Detect weak / placeholder API keys
  secrets_permissions   Secrets files/dirs with world-readable permissions
  config_secret_hygiene Config.yaml plaintext secrets or loose permissions (recommends ${ENV_VAR})

Models:
  model_health          Role-pinned models with high recent failure rate or
                        degenerate output; RECOMMENDS the role's modelFallback
                        (diagnostic only — never auto-switches a model)
  model_route_coverage  Role-pinned models that don't resolve to a chat
                        model_route prefix or are missing from pricing.yaml

Cost & budget:
  pricing_coverage      Models in swarm configs missing from pricing.yaml
  autonomy_budget_guard Autonomy-enabled projects with no hard $ cap
  budget_utilisation    Projects at ≥80%% of daily or monthly hard cap
  dispatcher_role       When telegram.dispatcher_project_id is set,
                        the chosen project's swarm should declare a
                        "dispatcher" role so dashboard role+model
                        aggregation rows align with the swarm catalogue.

Use --fix to automatically repair stale_leases, orphaned_watchers,
stuck_executions, task_state_audit, orphan_fk_rows, orphan_worktrees,
secrets_permissions, and dispatcher_role findings. Schema, runtime,
security posture, pricing, and budget checks are diagnostic only
because they require operator config changes or external runtime actions.

```
vornikctl doctor [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--fix` | `false` | Automatically repair detected issues |
| `--json` | `false` | Output in JSON format |

## vornikctl doctor feature

Show feature-doctor diagnoses (all features, or one by id)

Query the daemon's feature-doctor surface and render a status table.

Without an id argument, all registered features are listed with their
current status (ok/ready/blocked/degraded/unknown).

With an id argument, full prereq detail (including remediation hints for
unmet prereqs) is shown for that feature.

Exit code 1 when any feature is blocked or degraded.

```
vornikctl doctor feature [id] [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl doctor feature enable

Propose (or apply) enabling a feature via the feature doctor

Compute the gate changes required to enable a feature and optionally apply them.

Without --apply (dry-run): prints the proposed gate changes and apply
mechanism. No config is mutated.

With --apply: sends the changes to the daemon, which:
  1. Backs up config.yaml
  2. Writes all gate changes (comment-preserving)
  3. Triggers a config reload
  4. Runs the feature's verify check
  5. Rolls back to the backup on any failure

This command requires admin credentials (VORNIK_API_KEY must be an admin key
when auth is enabled).

```
vornikctl doctor feature enable <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--apply` | `false` | Apply the gate changes (default: dry-run only) |

## vornikctl execution

Manage executions

Inspect and list workflow executions in the vornik control plane.

## vornikctl execution inspect

Inspect an execution

Get detailed information about an execution by ID.

```
vornikctl execution inspect <executionId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl execution list

List executions

List executions for a project, optionally filtered by task or status.

```
vornikctl execution list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--all` | `false` | Show every execution instead of aggregating retries by task |
| `--json` | `false` | Output in JSON format |
| `-p`, `--project` |  | Project ID (required) |
| `-s`, `--status` |  | Filter by status (PENDING, RUNNING, COMPLETED, FAILED, CANCELLED) |
| `-t`, `--task` |  | Filter by task ID |

## vornikctl init

Initialize vornik resources

## vornikctl init project

Create a project YAML config

Create a project config under configs/projects, validate it against the registry, and optionally print it with --dry-run.

```
vornikctl init project <name> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--autonomy` | `false` | Enable autonomy in the generated project |
| `--config-dir` |  | Registry config directory (default: VORNIK_CONFIGS_DIR or ./configs) |
| `--display-name` |  | Human-readable project name |
| `--dry-run` | `false` | Print generated YAML instead of writing it |
| `--force` | `false` | Overwrite an existing project file |
| `--goal` |  | Autonomy goal |
| `--param` | `[]` | Template parameter (repeatable). Format: name=value |
| `--swarm` | `basic-swarm` | Swarm ID |
| `--template` |  | Materialise from a template slug (e.g. personal-assistant, news-feed). Bypasses the built-in YAML generator; use --param name=value to set template parameters. |
| `--workflow` | `adaptive` | Default workflow ID |

## vornikctl init swarm

Create a SWARM.md config from a preset template

Generate a new swarm config by copying one of the built-in preset
templates and rewriting its swarmId to the name you pass. The generated
file is a WORKFLOW.md-style SWARM.md (YAML frontmatter + Markdown body)
and is validated against the full registry before it lands on disk.

Available presets (use --list to see the one-line description of each):
  basic            lead + coder + reviewer — minimal code swarm
  dev              lead + feasibility + analyst + coder + tester + reviewer + scout + architect — full code stack
  research         lead + researcher + writer — good for digest / scan / summary projects
  companion-ingest reviewer + analyst + summarizer + rag-ingester — companion swarm with async-ingest role

Examples:
  vornikctl init swarm my-swarm --template basic
  vornikctl init swarm itpe-triage --template research --dry-run
  vornikctl init swarm my-ingest-swarm --template companion-ingest --force
  vornikctl init swarm --list

```
vornikctl init swarm <name> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--config-dir` |  | Registry config directory (default: VORNIK_CONFIGS_DIR or ./configs) |
| `--dry-run` | `false` | Print generated SWARM.md instead of writing it |
| `--force` | `false` | Overwrite an existing swarm file |
| `--list` | `false` | List available preset templates and exit |
| `--template` | `basic` | Preset template: basic \| dev \| research \| companion-ingest |

## vornikctl instinct

Inspect, retire, export, and import learned instincts

Browse the continuous-learning instinct layer.

Instincts are confidence-scored learned patterns ("in situation T,
action A held") mined from the audit spine. They are advisory: surfaced
as evidence behind their own gates, never auto-applied.

list / show / retire operate against the running daemon's
/api/v1/instincts surface. export / import are the cross-deployment
sharing primitive — export pulls matching instincts into a portable
frontmatter file; import parses + validates such a file.

## vornikctl instinct export

Export matching instincts to a portable frontmatter file

Export pulls instincts matching the filter out of the running daemon
and writes them in the LLD frontmatter shape (a YAML document with a
top-level instincts: list). The structured trigger is emitted as a
nested map; it round-trips to the trigger_json column on import.

  vornikctl instinct export --domain recovery -o recovery-instincts.yaml

```
vornikctl instinct export [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--domain` |  | Filter by domain |
| `--min-confidence` | `0` | Only instincts with confidence >= this (0-1) |
| `--project` |  | Filter by project ID |
| `--scope` |  | Filter by scope |
| `--status` |  | Filter by status |
| `-n`, `--limit` | `1000` | Maximum rows to export (1-1000) |
| `-o`, `--output` |  | Write to file instead of stdout |

## vornikctl instinct import

Parse + validate a portable instinct frontmatter file

Import reads a SWARM instinct frontmatter file (as produced by
'vornikctl instinct export'), validates every entry, and reports the
parsed instincts.

Verify-only: this slice has no instinct-create write path (instincts are
mined by the daemon's extraction worker, not hand-injected), so import
never mutates the daemon — it confirms the file is well-formed and shows
what it carries.

```
vornikctl instinct import <file> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Parse + validate only (default behaviour; kept for symmetry) |
| `--json` | `false` | Output the parsed instincts as JSON |

## vornikctl instinct list

List instincts (filterable by domain, scope, project, status, confidence)

Show instincts from the running daemon, highest confidence first.

Filters compose with AND. Default limit 100; max 1000.

  vornikctl instinct list --domain recovery --status active
  vornikctl instinct list --project assistant --min-confidence 0.6

```
vornikctl instinct list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--domain` |  | Filter by domain (recovery\|cost\|quality\|retrieval\|workflow) |
| `--json` | `false` | Output JSON instead of table |
| `--min-confidence` | `0` | Only instincts with confidence >= this (0-1) |
| `--project` |  | Filter by project ID |
| `--scope` |  | Filter by scope (project\|global) |
| `--status` |  | Filter by status (candidate\|active\|promoted\|retired) |
| `-n`, `--limit` | `100` | Maximum rows to return (1-1000) |

## vornikctl instinct retire

Retire an instinct (advisory — it stays for audit)

Flip an instinct to status=retired. Advisory only: the row stays
for audit and nothing about agent behaviour changes — it is simply
removed from the advisory surfaces. Idempotent.

```
vornikctl instinct retire <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl instinct show

Show a single instinct by id

```
vornikctl instinct show <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl key

Manage per-project API keys

Create, list, rotate, and revoke DB-backed bearer tokens scoped to a
single project. The token's bound project is the authoritative cost-row
target — X-Vornik-Project-ID header overrides are IGNORED for DB-backed
keys, closing the cross-project billing leak that the legacy static-key
path allowed.

The secret returned by 'create' and 'rotate' is shown ONCE on stdout.
Capture it now; the daemon stores only a sha256 hash and cannot recover
the raw key later.

## vornikctl key create

Mint a new API key for a project

Generate a new sk-vornik-<project>.<random> token. The raw secret is printed ONCE.

```
vornikctl key create [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--expires` |  | Expiration (RFC3339 or duration like 30d, 6m, 1y). Empty = never. |
| `--json` | `false` | Emit JSON instead of human text |
| `--name` |  | Operator-friendly label (required) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl key list

List API keys for a project (no secrets returned)

```
vornikctl key list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON instead of a table |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl key revoke

Soft-delete an API key (idempotent)

```
vornikctl key revoke <keyID> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-p`, `--project` |  | Project ID (required) |

## vornikctl key rotate

Mint a new key with the same name + expiry; revoke the old

```
vornikctl key rotate <keyID> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON instead of human text |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl key update

Update an existing key's allowed_workflows list or push capability

Add, remove, or replace workflows on a key's allowed_workflows
list without minting a new secret. Three modes, mutually exclusive
per invocation:

  --add-workflow X[,Y]      append X (and Y) to the current list
  --remove-workflow X[,Y]   drop X (and Y) from the current list
  --set-workflows X,Y,Z     replace the list wholesale; pass '' to
                            mean "every workflow the project permits"

To flip git-push access on a key use one of the mutually exclusive flags:

  --allow-push              grant git-push access over HTTPS
  --disallow-push           revoke git-push access

Add / remove modes fetch the current list, mutate, and PUT the
result — last writer wins on concurrent edits. Set mode is the
raw PUT.

Examples:
  vornikctl key update -p my-project --add-workflow=rag-ingest <keyID>
  vornikctl key update -p my-project --remove-workflow=doc-review <keyID>
  vornikctl key update -p my-project --set-workflows=research-gather,rag-ingest <keyID>
  vornikctl key update -p my-project --set-workflows='' <keyID>   # = every project workflow
  vornikctl key update -p my-project --allow-push <keyID>
  vornikctl key update -p my-project --disallow-push <keyID>

```
vornikctl key update <keyID> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--add-workflow` | `[]` | Workflow ID(s) to append to allowed_workflows. Repeatable / comma-separated. |
| `--allow-push` | `false` | Grant git-push access over HTTPS to this key. |
| `--disallow-push` | `false` | Revoke git-push access over HTTPS from this key. |
| `--json` | `false` | Emit JSON instead of human text |
| `--remove-workflow` | `[]` | Workflow ID(s) to drop from allowed_workflows. Repeatable / comma-separated. |
| `--set-workflows` | `[]` | Workflow ID(s) replacing allowed_workflows. Empty value (--set-workflows='') clears the list = every project workflow. |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl mcp

Inspect and call MCP tools wired to a project

## vornikctl mcp call

Invoke one MCP tool directly (debug path; skips the LLM)

Invoke one MCP tool by its qualified name (mcp__{server}__{tool}) and
print the result. Arguments are a JSON object supplied via --args. Use
this to prove a scraper / gmail / github connection is live without
spinning up an agent container and paying for an LLM call.

Examples:
  vornikctl mcp tools -p janka
  vornikctl mcp servers
  vornikctl mcp call -p janka --tool mcp__scraper__web_fetch \
      --args '{"url":"https://example.com","project_id":"janka","allowed_hosts":["*"],"text_only":true,"max_bytes":2000}'

```
vornikctl mcp call [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--args` | `{}` | Arguments as a JSON object (default "{}") |
| `--json` | `false` | JSON output instead of the plain-text response body |
| `--tool` |  | Qualified tool name: mcp__{server}__{tool} (required) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl mcp servers

List the daemon-level MCP server inventory

List every MCP server declared at the daemon level (top-level
mcp.servers block) with its reachability state and the tool catalog
each advertises. Listing a server here does NOT grant any project
access to its tools — that still requires editing the project's own
mcp.servers list.

```
vornikctl mcp servers [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output instead of the table |

## vornikctl mcp tools

List tools a project's MCP servers advertise

```
vornikctl mcp tools [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output instead of the table |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory

Operate on project memory chunks

Administrative commands for per-project RAG memory (project_memory_chunks).

## vornikctl memory audit

List unvalidated chunks (default: unverified + legacy)

Show chunks awaiting validation review. The default filter targets
the two states an operator cares about for content quality:
- unverified: freshly ingested chunks the system hasn't validated yet
- legacy:     chunks pre-dating the 2026.4 memory-hardening migration

Pass --status to narrow to a specific subset (e.g. --status legacy to
hand-audit the legacy backlog). The output is project-scoped so a typo
can't dump the whole deployment.

For per-chunk correction, use the dispatcher's memory_correct chat
tool (faster — embed similarity finds the wrong claim automatically).

```
vornikctl memory audit [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--full` | `false` | Show full chunk preview (~200 chars) instead of the one-line summary |
| `--json` | `false` | Emit JSON instead of a table |
| `--status` | `[]` | Filter by validation_status (repeatable). Default: unverified,legacy. |
| `-n`, `--limit` | `100` | Max rows (1-500) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory backfill-titles

Generate LLM topic labels for chunks with NULL content_title

Walk project_memory_chunks rows where content_title IS NULL and ask the
configured Titler LLM (memory.titler.model in vornik.yaml) to generate a
short topic label for each. The label powers the operator vector-cloud
UI: chunks without one fall back to their first markdown heading and
then to the source filename, which is usually noise.

The daemon does the work — this command just drives it batch by batch
and prints progress. Safe to interrupt and re-run; it always resumes
from wherever it left off because the query selects WHERE content_title
IS NULL.

Examples:
  vornikctl memory backfill-titles                    # title everything
  vornikctl memory backfill-titles --dry-run          # count only
  vornikctl memory backfill-titles --batch-size 5     # smaller batches
  vornikctl memory backfill-titles --max 100          # stop after 100

```
vornikctl memory backfill-titles [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--batch-size` | `10` | Chunks per LLM-call batch (1-100) |
| `--dry-run` | `false` | Only report how many chunks are missing a title |
| `--json` | `false` | Emit JSON summary instead of human-readable progress |
| `--max` | `0` | Stop after this many chunks have been processed (0 = unlimited) |

## vornikctl memory cache-stats

Show LLM cache effectiveness (embedding + response)

Report row counts, lifetime hits, $ saved, and on-disk size for the
embedding cache (Phase D — keyed on content_hash+model) and the
response cache (Phase E — keyed on model+purpose+prompt). Both
caches must be enabled in the daemon config to populate; either
or both may render as "disabled" on a fresh deployment.

Example:
  vornikctl memory cache-stats
  vornikctl memory cache-stats --json

```
vornikctl memory cache-stats [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output instead of the short-form table |

## vornikctl memory dlq

Inspect and replay chunks in the memory embed DLQ

The memory DLQ (memory_embed_dlq table) holds chunks the embed worker
couldn't store on its own — embedder unavailable, dimension mismatch,
oversized content, etc. The worker auto-retries rows whose retry_after
has lapsed; permanently-parked rows (retry_count = -1) need an operator
to either fix the underlying cause and replay, or delete the chunk.

## vornikctl memory dlq list

List DLQ entries

```
vornikctl memory dlq list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of table |
| `-n`, `--limit` | `100` | Max rows (1-1000) |
| `-p`, `--project` |  | Filter by project ID |

## vornikctl memory dlq replay

Move one or more chunks back to the embed queue

```
vornikctl memory dlq replay <chunkId...>
```

## vornikctl memory epochs

List recent corpus epochs (snapshots) for a project

```
vornikctl memory epochs [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--limit` | `20` | Max epochs to show |
| `--project` |  | Project ID (required) |

## vornikctl memory evict

Permanently delete memory chunks (GDPR-style hard eviction)

Permanently delete the named project_memory_chunks rows. Cascades
through memory_embed_queue + memory_embed_dlq + entity_mentions (FK
ON DELETE CASCADE) and nulls out project_memory_quarantine.
released_chunk_id where it pointed at the evicted chunk. A per-chunk
tombstone row lands in memory_eviction_audit so the deletion itself
is auditable (the GDPR compliance hook — deletion without record of
the deletion is itself non-compliant).

Eviction is DESTRUCTIVE and IRREVERSIBLE. For "this record is wrong,
demote it in search" use the soft-refute path (vornikctl via
memory_correct dispatcher tool) instead. Use evict only for:

  - GDPR / privacy-driven "forget this" requests.
  - Cleanup of confirmed-bad records that soft-refute leaves
    cluttering the index.
  - Cascading cleanup tied to a hard-deleted source artifact.

Requires --confirm so it can't fire from a typo. --reason is
recorded on the audit row; pass a short prose justification
(e.g. "GDPR DSAR 2026-05-20-12" or "operator: confirmed wrong
ticker"). Empty --reason still records the row but flags the
operator: get-it-right-on-the-first-call gap.

Two selectors (exactly one required):
  --chunks  explicit comma-separated chunk IDs
  --scope   every chunk under a repo_scope; --scope="" targets the
            UNTAGGED (NULL) bucket — e.g. memories ingested before
            scopes existed. Running without --confirm prints the
            match count and refuses (a built-in dry run).

Examples:
  # explicit IDs:
  vornikctl memory evict --project assistant \
      --chunks mc_abc123,mc_def456 \
      --reason "GDPR DSAR 2026-05-20-12" --confirm

  # preview, then evict the untagged (pre-scope) bucket:
  vornikctl memory evict -p acme --scope ""            # dry run (count only)
  vornikctl memory evict -p acme --scope "" \
      --reason "pre-scope cleanup" --confirm

  # evict everything under a specific (wrong/dead) scope:
  vornikctl memory evict -p acme --scope github.com/old/repo --confirm

Note: the project filter is the IDOR guard. Chunk IDs that exist
under a different project will be silently ignored — the command
reports the count actually deleted so an operator who pastes the
wrong IDs notices the discrepancy.

```
vornikctl memory evict [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--chunks` |  | Comma-separated chunk IDs to evict (one of --chunks / --scope) |
| `--confirm` | `false` | REQUIRED safety gate — refuses to delete without this |
| `--reason` |  | Audit-trail reason recorded on the tombstone row |
| `--scope` |  | Evict EVERY chunk under this repo_scope (one of --chunks / --scope). Pass --scope="" for the untagged (NULL) bucket — e.g. pre-scope memories. |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory feedback

Show chunk-utility analytics for a project's memory

Renders memory retrieval feedback for one project: how many chunks
are indexed, how many were actually retrieved at least once in the
window, and a sample of unretrieved chunk IDs that are auto-prune
candidates.

Sources: project_memory_chunks (indexed) + memory_retrieval_audit
(per-search rows). Empty values usually mean the audit repo isn't
wired or the schema migration hasn't run yet — see
deployments/postgres/schema/001_initial.sql.

```
vornikctl memory feedback [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--days` | `30` | Window length in days (capped at 365) |
| `--json` | `false` | Output in JSON format |
| `--sample` | `20` | Number of unretrieved chunk IDs to print (capped at 200) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory firewall

Policy-Aware Memory Firewall — inspect evaluations + mode

Read-only operator surface for the Policy-Aware Memory Firewall.

The firewall attaches policy metadata (provenance / sensitivity /
expiry / tenant / role / purpose) to every memory chunk and
emits an audit row per retrieval decision. These verbs surface
the audit trail + the daemon's current enforcement mode.

Requires an admin-scoped API key.

## vornikctl memory firewall evaluations

List recent firewall evaluations for a project

Page through memory_policy_evaluations rows, newest first.

Filter by decision class to surface a specific compliance pattern:

  vornikctl memory firewall evaluations --project p1 --decision block_role_not_permitted
  vornikctl memory firewall evaluations --project p1 --decision block_expired --since 2026-05-01
  vornikctl memory firewall evaluations --project p1 --decision allow --limit 200

```
vornikctl memory firewall evaluations [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--csv` | `false` | Stream RFC 4180 CSV instead of table (default 30-day window for compliance exports) |
| `--decision` |  | Filter by decision class (allow \| block_expired \| block_tenant_mismatch \| block_role_not_permitted \| block_purpose_not_allowed \| block_sensitivity_tier) |
| `--json` | `false` | Output JSON instead of table |
| `--project` |  | Project ID (required) |
| `--since` |  | Lower bound timestamp (YYYY-MM-DD or RFC3339); default last 7 days |
| `-n`, `--limit` | `50` | Maximum rows (1-500) |

## vornikctl memory firewall mode

Print the daemon's current firewall enforcement mode

```
vornikctl memory firewall mode [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl memory firewall set-policy

Mutate per-chunk firewall policy (admin-only)

Edit one chunk's firewall policy. Only the flags you pass are
applied — others stay at their stored values. The policy_digest is
recomputed server-side; the response carries the new value so you
can verify the round-trip.

Examples:

  vornikctl memory firewall set-policy c1 --sensitivity restricted
  vornikctl memory firewall set-policy c1 --permitted-roles coder,analyst
  vornikctl memory firewall set-policy c1 --permitted-roles ''           # clear (deny-all)
  vornikctl memory firewall set-policy c1 --allowed-purposes operational,audit_review
  vornikctl memory firewall set-policy c1 --expires-at 2026-12-31T23:59:59Z
  vornikctl memory firewall set-policy c1 --tenant-id tenant-a

```
vornikctl memory firewall set-policy <chunk_id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--allowed-purposes` |  | Comma-separated list (operational\|training_data\|audit_review\|compliance_export); empty = deny-all |
| `--expires-at` |  | RFC3339 timestamp; empty = clear |
| `--json` | `false` | Output JSON instead of human-readable |
| `--permitted-roles` |  | Comma-separated list of allowed roles; empty = deny-all |
| `--sensitivity` |  | Sensitivity tier (public\|internal\|confidential\|restricted) |
| `--tenant-id` |  | Tenant ID (empty = clear) |

## vornikctl memory gist

Show the periodic LLM-free per-project term-frequency gist

Print the latest gist (top-N ranked terms by raw frequency over the
project's chunk corpus) produced by the consolidate worker. The worker
runs every ~10 minutes by default; a 404 means the loop hasn't fired
for this project yet.

```
vornikctl memory gist <projectID> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON instead of a table |

## vornikctl memory prune-candidates

List chunks that haven't been retrieved in --since (auto-prune candidates)

Print chunk IDs that are indexed for --project but haven't appeared in
any memory_retrieval_audit row since now() - --since. These are the
candidates for the memory feedback loop's prune signal: chunks that
the corpus stores but nothing actually reads.

The command DOES NOT delete anything — it surfaces the list so an
operator can decide. To actually prune, follow up with project-side
tooling or 'vornikctl memory wipe' for the whole project.

Examples:
  vornikctl memory prune-candidates --project assistant
  vornikctl memory prune-candidates --project assistant --since 90d
  vornikctl memory prune-candidates --project assistant --json --limit 500

```
vornikctl memory prune-candidates [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON instead of a table |
| `--limit` | `100` | Max candidates to return |
| `--since` | `720h0m0s` | Retrieval lookback window (default 30d) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory reassign

Move memory chunks from one project to another

Move all project_memory_chunks rows from --from to --to in a single
transaction, handling the (project_id, content_hash) unique constraint by
dropping source rows whose hashes already exist at the destination.

Example:
  vornikctl memory reassign --from old-project --to new-project
  vornikctl memory reassign --from old-project --to new-project --dry-run

Use --dry-run first to see counts without making changes.

```
vornikctl memory reassign [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Report what would change without writing |
| `--from` |  | source project ID (required) |
| `--to` |  | destination project ID (required) |

## vornikctl memory recheck-urls

HEAD-ping every URL in a project's memory chunks and flag dead ones

Walk project_memory_chunks for --project, extract URLs from each chunk's
content, and issue a short-timeout HEAD against every URL. Chunks whose URLs
are all dead are flagged (is_alive=false); chunks with at least one alive URL
are confirmed (is_alive=true). Dead URLs stay indexed — they're just flagged
so consuming agents (researcher, dispatcher) can prefer live hits and warn
when only dead ones survive.

The command is the operator-actionable MVP for URL liveness. A periodic
auto-worker that runs this on a schedule is a follow-up; for now operators
run this on demand when a recent E2E shows agents pulling stale URLs.

Examples:
  vornikctl memory recheck-urls --project assistant
  vornikctl memory recheck-urls --project assistant --limit 100
  vornikctl memory recheck-urls --project assistant --timeout 3s --json

```
vornikctl memory recheck-urls [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON summary instead of human-readable output |
| `--limit` | `0` | Max chunks to check (0 = all) |
| `--timeout` | `5s` | Per-URL HEAD timeout (default 5s) |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory reclassify

Re-derive content_class for unclassified chunks

Walk every chunk in --project whose content_class is 'unclassified'
(or empty) and re-derive the class.

Default (deterministic): apply ClassifyByRole to each chunk's
producer_role. Chunks whose producer_role is empty or maps to
ClassUnclassified are left alone.

--use-llm: chunks the deterministic pass leaves unclassified are
sent to the LLM classifier (POST /api/v1/memory/reclassify-llm).
Requires the daemon to be running with memory.classifier.enabled=true
in config. Costs one LLM call per chunk; the CLI loops batches
until the queue drains.

--llm-only: SKIP the deterministic pass entirely. Useful when an
operator has manually reset chunks to 'unclassified' specifically
to force LLM reclassification — without this flag the deterministic
pass would immediately re-stamp them based on producer_role, leaving
nothing for the LLM to see. Implies --use-llm. Requires the daemon.

The deterministic mapping is the canonical Phase-2 table
(researcher → research, analyst → spec, reviewer → decision,
coder/etc. → commit_msg, tester/etc. → diagnostic, lead → spec,
vision → external_fetch, strategist → spec, risk-officer → decision,
executor → commit_msg).

Examples:
  vornikctl memory reclassify --project assistant --dry-run
  vornikctl memory reclassify --project assistant
  vornikctl memory reclassify --project assistant --use-llm
  vornikctl memory reclassify --project assistant --llm-only
  vornikctl memory reclassify --project assistant --use-llm --batch-size 5
  vornikctl memory reclassify --project assistant --json

```
vornikctl memory reclassify [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--batch-size` | `10` | Chunks per LLM batch when --use-llm is set (1-50) |
| `--dry-run` | `false` | Report what would change without writing |
| `--json` | `false` | Emit JSON summary instead of human-readable output |
| `--llm-only` | `false` | Skip the deterministic pass; send every unclassified chunk straight to the LLM classifier. Implies --use-llm. |
| `--use-llm` | `false` | After the deterministic pass, send remaining unclassified chunks to the LLM classifier |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory reembed

Re-enqueue every chunk in a project for embedding

Push every chunk in --project back onto memory_embed_queue so the
worker re-embeds them with the currently-configured model + dimension.
Use after upgrading the embedding model or changing
memory.embedding_dimension in vornik.yaml.

The command only inserts queue rows — it doesn't touch existing
embeddings; the worker overwrites those one batch at a time. Safe to
interrupt and re-run; queued rows for chunks that already came around
are skipped via ON CONFLICT.

By default the command stays attached after enqueueing and reports
progress every few seconds until the queue drains. Pass --no-watch
to detach immediately (e.g. for use inside scripts where you want
the parent process to handle the polling).

Examples:
  vornikctl memory reembed --project assistant
  vornikctl memory reembed --project assistant --no-watch
  vornikctl memory reembed --project assistant --interval 1s
  vornikctl memory reembed --project assistant --json

```
vornikctl memory reembed [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--interval` | `3s` | Progress poll interval (with --watch) |
| `--json` | `false` | Emit machine-readable JSON summary (implies --no-watch) |
| `--no-watch` | `false` | Detach after enqueueing instead of polling for progress |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory regraph

Re-flag chunks with zero edges so the KG worker reprocesses them

Re-flag every chunk in --project that produced zero published
edges, so the daemon's KG extraction worker picks them up on its next
tick and re-runs the four-stage pipeline (extractor → resolver →
relationship → validator) with whatever logic is currently shipping.

Use after a KG-pipeline fix to make existing isolated entities benefit
from the change. Without this, the fix only helps NEW chunks.

Idempotent: re-running against the same project re-flags the same
chunk set minus any that the latest pass DID manage to extract edges
from. --dry-run reports the candidate count without writing.

Examples:
  vornikctl memory regraph --project assistant --dry-run
  vornikctl memory regraph --project assistant
  vornikctl memory regraph --project assistant --json

```
vornikctl memory regraph [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Report candidate count without writing |
| `--json` | `false` | Emit JSON summary instead of human-readable output |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory rollback

Roll back the corpus to a prior snapshot

Atomically deactivate every epoch newer than --to and re-activate every
epoch up to and including --to for the given project. Default is preview;
pass --apply to execute. Records the action in corpus_rollbacks.

Example:
  vornikctl memory rollback --project assistant --to epoch_xxx
  vornikctl memory rollback --project assistant --to epoch_xxx --apply

Use the epoch listing first to pick a target:
  vornikctl memory epochs --project assistant

```
vornikctl memory rollback [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--apply` | `false` | Actually perform the rollback (default: dry-run) |
| `--project` |  | Project ID (required) |
| `--reason` |  | Optional rollback reason |
| `--to` |  | Target epoch ID (required) |

## vornikctl memory scope

Inspect and bulk-edit the per-chunk repo scope

Two subcommands for managing the per-chunk repo-scope tag. The
scope partitions a single project's RAG so one operator's many repos
don't cross-pollute each other's recall results.

  list   — distinct scopes + chunk counts (the namespace inventory)
  retag  — bulk update chunks from one scope (or NULL) to another

The chunk-side semantics are:
  NULL        = uncategorized (untagged chunks; visible under
                 every scoped recall during the transition window)
  "*"         = cross-cutting (surfaces in every scoped recall)
  any string  = repo token (typically a git remote, e.g.
                 "github.com/acme/myrepo")

## vornikctl memory scope list

List distinct repo_scope values in a project, with chunk counts

```
vornikctl memory scope list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Emit JSON instead of a table |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory scope retag

Bulk-promote chunks from one scope to another

Update the repo scope on every chunk in a project that matches the
selectors. Typical use is bulk-promoting a backlog of untagged chunks:

  # Tag every uncategorized chunk in a project as a given repo:
  vornikctl memory scope retag -p my-project --to=github.com/acme/myrepo

  # Fix a typo across a previously-scoped batch:
  vornikctl memory scope retag -p my-project --from=github.com/acme/old-repo --to=github.com/acme/myrepo

  # Narrow with a source_name LIKE filter (only re-tag a subset):
  vornikctl memory scope retag -p my-project \
      --from=                   \
      --to=github.com/acme/myrepo \
      --source-name-like='docs-%'

Defaults: --from is empty (NULL / uncategorized chunks). Pass --from=X
to migrate a specific scope; pass --source-name-like to narrow.

Always runs against the live daemon's chunk store.
Use --dry-run to preview row counts before commit.

```
vornikctl memory scope retag [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Show affected count without writing |
| `--from` |  | Source scope to promote from. Empty / unset = NULL (uncategorized chunks). Pass '*' to retag cross-cutting chunks. |
| `--source-name-like` |  | Optional source_name SQL LIKE pattern to narrow which chunks get retagged (e.g. 'lld-%'). |
| `--to` |  | Target scope to stamp on matched chunks (required). '*' = cross-cutting; '' is rejected (would un-tag). |
| `--yes` | `false` | Skip the interactive confirmation prompt |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl memory search

Search project memory (RAG index)

Query a project's hybrid RAG index and print the top matches. Wraps
GET /api/v1/projects/{project}/memory/search. Useful for verifying what
the researcher role is actually retrieving before you pin a behaviour
down in prompts.

```
vornikctl memory search [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output instead of the short-form table |
| `-n`, `--limit` | `10` | Max results (1-50) |
| `-p`, `--project` |  | Project ID (required) |
| `-q`, `--query` |  | Search query (required) |

## vornikctl memory stats

Show per-project RAG chunk counts and embedding coverage

Report total chunks, embedded chunks, and embed-queue depth for every
project (or one, with --project). Embedding coverage = embedded/total;
100% means the embed worker has caught up. A non-zero queue depth with
100% coverage means the worker is about to re-embed something (e.g.
after a model change).

```
vornikctl memory stats [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | JSON output |
| `-p`, `--project` |  | Show only this project (default: all) |

## vornikctl memory wipe

Delete every memory artifact for one project

Wipe one project's memory state in a single transaction:

  - project_memory_chunks            (chunk content + embeddings)
  - memory_embed_queue / dlq         (cascade from chunks)
  - knowledge_entities / edges       (KG nodes + relationships, project-scoped)
  - entity_mentions                  (cascade from entities + chunks)
  - project_memory_quarantine        (rejected chunks; --keep-quarantine to preserve)
  - project_ingest_queue             (pending ingest items)
  - corpus_epochs / corpus_epochs_active (snapshot history)
  - memory_retrieval_audit           (search history; --keep-audit to preserve)

This is irreversible. Use --dry-run first to see counts, then run
without it (you'll get a confirmation prompt unless --yes is set).

Examples:
  vornikctl memory wipe --project assistant --dry-run
  vornikctl memory wipe --project assistant
  vornikctl memory wipe --project assistant --yes --keep-quarantine

What it does NOT touch:
  - tasks, executions, artifacts (use those tools separately)
  - chunks already reassigned to other projects (only filters by project_id)
  - Source files on disk (this is purely a DB wipe)

```
vornikctl memory wipe [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--dry-run` | `false` | Show counts without deleting |
| `--keep-audit` | `false` | Preserve memory_retrieval_audit rows |
| `--keep-graph` | `false` | Preserve knowledge_entities / edges / mentions |
| `--keep-quarantine` | `false` | Preserve project_memory_quarantine rows |
| `--yes` | `false` | Skip confirmation prompt |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl models

Discover and inspect chat-provider models

List models served by each enabled chat sub-provider.

The daemon walks every enabled chat sub-provider that supports
discovery (HTTP gateway, Vertex, claude/codex subscriptions, claude/codex
CLIs) and aggregates the results. Each row is crosswalked against
configs/pricing.yaml so you can see at a glance which models have
explicit cost entries — those without will accrue spend at the
configured 'default' rate (or zero) until pricing.yaml is updated.

Sources:
  live   — fetched from the provider's /v1/models endpoint
  static — curated list (Claude / Codex OAuth surfaces don't expose
           a public model list, so the daemon ships a hardcoded one)

## vornikctl models list

List discoverable models across all chat sub-providers

```
vornikctl models list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |
| `--provider` |  | Filter to one sub-provider (e.g. vertex, http, claude-subscription) |
| `--unpriced` | `false` | Only show models without a pricing.yaml entry |

## vornikctl project

Inspect projects loaded by the daemon

## vornikctl project archive

Archive a project (schedule it for deletion after a grace window)

Flip a project's lifecycle to archived. The daemon stops
dispatching new work for the project immediately. After the
grace window elapses the archive-sweeper hard-deletes the
project YAML, every project-scoped DB row, and every artifact
blob on disk.

Unarchive any time during the grace window to restore the
project. Default grace is 7 days.

```
vornikctl project archive <projectId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--grace` | `7d` | Grace window before deletion (e.g. 1d, 7d, 30d, 90d, 12h) |
| `--json` | `false` | Output JSON instead of human-readable |
| `--reason` |  | Optional operator-visible reason recorded in the YAML |

## vornikctl project delete-now

Skip the grace window — wipe an archived project on the next sweeper tick

Rewind the scheduledDeleteAt timestamp to ~now and kick the
archive-sweeper. The project's YAML, DB rows, and artifact
blobs are wiped within seconds.

Requires the project to be archived first (use 'project
archive' if it isn't). Cannot be undone.

```
vornikctl project delete-now <projectId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl project list

List projects the daemon is serving

```
vornikctl project list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl project show

Show one project's full config

```
vornikctl project show <projectId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl project unarchive

Restore an archived project to active

Clear the lifecycle block on a previously-archived project.
The grace window resets, new tasks can dispatch again, and the
sweeper stops tracking the project for deletion.

```
vornikctl project unarchive <projectId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl reminders

Inspect / cancel scheduled reminders

List and cancel rows in the dispatcher_reminders ledger.

Reminders are created by the dispatcher's set_reminder tool when an
operator asks the bot for one in chat. This CLI exists for terminal-
only operators (no chat session) and for cleaning up stuck rows.

Calls are served by the daemon's /api/v1/reminders endpoints.

## vornikctl reminders cancel

Cancel a pending reminder

Flip a pending or firing dispatcher_reminders row to status=cancelled.
The heartbeat will skip it on subsequent ticks.

Idempotent: cancelling an already-terminal row is a no-op.

```
vornikctl reminders cancel <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl reminders delete

Physically remove a reminder row

Delete the dispatcher_reminders row entirely. Distinct from
'cancel' which preserves the row for audit. Intended for operator
cleanup of stale rows — e.g. reminders that survived a project
deletion (B-12), recurring rules gone awry, test data lingering.

The row is gone after this — no recovery, no audit trail of the
content (only an admin_audit_log entry naming the deletion). Use
'cancel' instead if you want the row preserved.

Returns 404 if the id doesn't exist; idempotent for scripts that
need to ignore "already gone".

```
vornikctl reminders delete <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |
| `--yes` | `false` | Skip the y/N confirmation prompt |

## vornikctl reminders list

List reminders (filterable by status, operator, project)

Show rows from the dispatcher_reminders ledger, fire-time ascending.

Filters compose with AND. Default limit 50; max 500.

Common queries:
  vornikctl reminders list --status pending
  vornikctl reminders list --status pending --project assistant
  vornikctl reminders list --operator telegram:42 --status fired

```
vornikctl reminders list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of table |
| `--operator` |  | Filter by operator id (e.g. 'telegram:42') |
| `--project` |  | Filter by project id |
| `--status` |  | Filter by status (pending\|firing\|fired\|cancelled\|expired) |
| `-n`, `--limit` | `50` | Maximum rows to return (1-500) |

## vornikctl reminders schedule

Create a one-shot or recurring reminder from natural language

Parse free-form text into a reminder via the daemon's
natural-language parser, confirm, and commit. Supports both
one-shot ("tomorrow at 9") and recurring ("every Monday at 9am")
schedules; recurring rows carry a 5-field POSIX cron expression
and the heartbeat re-arms them after every fire.

Examples:
  vornikctl reminders schedule "remind me in 3 hours to check the deploy" \
      --operator telegram:42 --channel telegram --channel-ref 42
  vornikctl reminders schedule "every Monday at 9am send the news digest" \
      --operator telegram:42 --channel telegram --channel-ref 42
  vornikctl reminders schedule "every weekday morning until June 1 send a tick" \
      --operator webchat:abc --channel webchat --channel-ref abc \
      --timezone Europe/Prague

By default the CLI prints the parsed reminder, asks for y/N confirmation,
then commits. Pass --yes to skip the prompt (scripted use).

```
vornikctl reminders schedule <natural-language text> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--channel-ref` |  | Channel-specific delivery ref (chat_id, thread, message-id) — required |
| `--channel` |  | Delivery channel kind (telegram\|slack\|email\|webchat\|github) — required |
| `--json` | `false` | Output JSON instead of human-readable |
| `--operator` |  | Operator id (e.g. telegram:42) — required |
| `--project` |  | Project id (optional) |
| `--timezone` |  | Operator timezone (IANA, e.g. Europe/Prague). Defaults to UTC. |
| `--yes` | `false` | Skip the y/N confirmation prompt |

## vornikctl reminders show

Show a single reminder by id

```
vornikctl reminders show <id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output JSON instead of human-readable |

## vornikctl restore

Restore a vornik deployment from a backup archive

Restore the database, artifacts directory, project workspaces, and
configs from an archive produced by 'vornikctl backup'. The daemon
MUST NOT be running. Requires psql on PATH. Refuses to run unless
--force is set.

Two safety gates run before the restore proceeds:
  1. Schema-presence gate (B-8): if the target already carries a
     vornik-owned schema (migrations rows OR canonical PG types like
     artifact_class), the restore is refused. Override with --clean
     (drops the schema first) or --allow-non-empty (proceeds anyway,
     usually fails on CREATE TYPE collisions).
  2. Row-count gate: refuses targets with non-empty projects/tasks
     tables. Override with --allow-non-empty.

Examples:
  # Fresh target — no vornik schema yet
  vornikctl restore --from vornik-backup-20260501-040000.tgz --force

  # Daemon already migrated the target; wipe + restore
  vornikctl restore --from <archive> --force --clean

  # Explicitly accept collisions (rarely useful)
  vornikctl restore --from <archive> --force --allow-non-empty

See https://docs.vornik.io "Backup and Restore" for the full matrix.


```
vornikctl restore [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--allow-non-empty` | `false` | permit restore into a DB with existing project/task rows |
| `--clean` | `false` | DROP SCHEMA public CASCADE before restore — wipes any vornik schema already loaded on the target |
| `--force` | `false` | required — confirms the daemon is stopped and the DB can be overwritten |
| `--from` |  | archive path (required) |

## vornikctl retention

Preview or apply retention pruning

Prune historical operational state older than the configured retention
windows. Defaults to preview mode — counts what would be pruned without
deleting anything. Add --apply to actually delete.

Windows default to:
  task_llm_usage  = 90 days
  tool_audit_log  = 30 days
  terminal tasks  = 60 days
  terminal execs  = 60 days
  artifacts       = 60 days  (DB + file on disk)

Minimum floor is 1 day regardless of config. project_memory_chunks is
NEVER pruned.

Examples:
  vornikctl retention                          # preview all projects
  vornikctl retention --project janka          # preview one project
  vornikctl retention --project janka --apply  # actually prune janka


```
vornikctl retention [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--apply` | `false` | actually delete rows (default: preview only) |
| `--json` | `false` | emit machine-readable JSON instead of a table |
| `--project` |  | operate on a single project (default: all) |

## vornikctl skill

Export, import, and validate portable SWARM-SKILL.md files

Portable SKILL.md interop layer.

A SWARM-SKILL.md is a single Markdown file that bundles a workflow
plus the roles its steps reference, with YAML frontmatter shaped
after the agentskills.io SKILL.md spec. The file is publishable
(one file, one curl) and ingestable (one vornikctl call materialises
the workflow + roles into the deployed config tree).

## vornikctl skill export

Export a workflow + its roles as a portable SWARM-SKILL.md

Export packages the named workflow and the roles its steps reference
into a single SWARM-SKILL.md file with agentskills.io-shaped frontmatter.

Argument is always <project>/<workflow>:
  - <project> resolves to that project's swarm (where the roles live);
  - <workflow> selects which workflow in the registry to bundle.

The standard flag drops the metadata.vornik.* block so the resulting
file is consumable by non-vornik SKILL.md tools. Standard files are
one-way: they cannot be re-imported.

```
vornikctl skill export <project>/<workflow> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Set the canonical author field |
| `--license` |  | Set the canonical license field (SPDX identifier) |
| `--standard` | `false` | Drop metadata.vornik.* — produce a clean agentskills.io SKILL.md |
| `--version` |  | Override the canonical version field |
| `-o`, `--output` |  | Write to file instead of stdout |

## vornikctl skill import

Materialise a SWARM-SKILL.md into the deployed configs tree

Import a SWARM-SKILL.md, writing a fresh WORKFLOW.md plus the
merged target swarm.

A target swarm is required — either the project's swarm (via
--project), an explicit swarm (--into-swarm), or a brand-new
swarm to create (--as-swarm).

Conflict detection is up-front: workflow IDs that already exist or
role names that collide are surfaced together before any write.

Use --dry-run to preview the writes without touching disk.

```
vornikctl skill import <file> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--as-swarm` |  | Create a new swarm with the imported roles (overrides --into-swarm) |
| `--configs-dir` |  | Configs directory (default: VORNIK_CONFIGS_DIR or ~/.config/vornik/configs) |
| `--dry-run` | `false` | Print the would-be writes without touching disk |
| `--into-swarm` |  | Merge the imported roles into this existing swarm (overrides --project) |
| `--project` |  | Resolve target swarm from this project's config |
| `--rename-role` | `[]` | Rewrite imported role names; repeat for multiple, format old=new |
| `--rename-workflow` |  | Change the imported workflow's ID |

## vornikctl skill info

Show detail about an installed skill

```
vornikctl skill info <handle>/<skill> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output as JSON |

## vornikctl skill install

Install a SWARM-SKILL.md from a local path, HTTPS URL, git URL, or registry handle

Resolve a source to a SWARM-SKILL.md, materialise its workflow + roles
into the deployed configs tree under a namespaced ID prefix, and record
the install in the ledger.

Source forms:
  ./skill.md                       local file
  file:///abs/path/skill.md        local file (URL form)
  https://example.com/skill.md     direct HTTPS GET
  git+https://github.com/h/r.git   git clone, picks SKILL.md at root
  vadim/research                   resolved via the registry index

--handle / --skill let an operator override the (handle, skill) tuple
the ledger records when installing from a non-registry source where
the canonical identity isn't otherwise known.

```
vornikctl skill install <source> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--allow-source-change` | `false` | Proceed even if the resolved source commit/content differs from what was pinned (supply-chain drift) |
| `--dry-run` | `false` | Resolve + validate without writing anything |
| `--force` | `false` | Re-install (overwrite materialised files) even when the ledger already has this skill |
| `--handle` |  | Override ledger handle when installing from a non-registry source |
| `--registry` |  | Override registry index URL (defaults to VORNIK_SKILL_REGISTRY_URL or https://skills.vornik.io) |
| `--skill` |  | Override ledger skill name when installing from a non-registry source |

## vornikctl skill list

List installed skills

```
vornikctl skill list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output as JSON |

## vornikctl skill rate

Submit a rating for an installed skill

POST {handle, skill, stars} to <registry>/rating.

Best-effort: a 404 / network failure is reported but doesn't
return a non-zero exit (so a busted registry doesn't break
scripts).

```
vornikctl skill rate <handle>/<skill> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--registry` |  | Override registry URL |
| `--stars` | `0` | Stars (1-5) |

## vornikctl skill register

Print PR-submit instructions for the registry index

Build a skills.yaml row for <handle>/<skill> and print
either:

  - The exact YAML snippet to append to the registry repo's
    skills.yaml (default), so the operator can submit a PR
    manually; OR
  - With --gh, run "gh pr create" against the registry's
    SkillRegistryRepo (default skills.vornik.io). Requires
    the gh CLI on PATH.

```
vornikctl skill register <handle>/<skill> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--description` |  | One-line description |
| `--gh` | `false` | Open a PR via gh CLI instead of printing the snippet |
| `--git-url` |  | Git URL the new skill installs from (required) |
| `--homepage` |  | Project homepage URL |
| `--registry` |  | Override registry index URL |
| `--tag` | `[]` | Tag (repeat for multiple) |

## vornikctl skill remove

Remove an installed skill (deletes materialised files + ledger row)

```
vornikctl skill remove <handle>/<skill> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--keep-mirror` | `false` | Keep the source mirror copy (default: delete) |

## vornikctl skill search

Search the registry index

Fetch <registry>/index.json and filter by query.

Query matches case-insensitively against handle, skill name,
description, and tag list. Empty query lists every skill in the
index. The registry URL defaults to https://skills.vornik.io;
override with --registry or VORNIK_SKILL_REGISTRY_URL.

```
vornikctl skill search [<query>] [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output as JSON |
| `--registry` |  | Override registry index URL |

## vornikctl skill update

Re-fetch the source for an installed skill and re-materialise

```
vornikctl skill update [<handle>/<skill>] [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--all` | `false` | Update every installed skill |

## vornikctl skill validate

Validate a SWARM-SKILL.md file or directory of files

Validate a SWARM-SKILL.md file or every *.md immediate child of a directory.

Enforces the agentskills.io / SKILL.md frontmatter shape plus the
vornik payload consistency rules:
  - name (required, lowercase-hyphens, ≤64 chars)
  - description (required, ≤1024 chars)
  - version (required, semver shape)
  - author / license (recommended; warnings only)
  - metadata.vornik.schema_version must be 1 (when present)
  - every workflow step has a prompt
  - every step's role exists in metadata.vornik.roles
  - file size ≤100k chars; warns over 15k

Exit code 0 on clean or warnings-only; 1 on any ERROR finding.

```
vornikctl skill validate <path> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--fix` | `false` | Print suggested replacements for findings that have a mechanical hint |
| `--json` | `false` | Output the validation report as JSON |

## vornikctl swarm

Inspect swarm definitions

## vornikctl swarm list

List swarms

```
vornikctl swarm list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl swarm show

Show one swarm's full definition (roles, prompts, permissions)

```
vornikctl swarm show <swarmId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl tail

Tail task logs (alias for `vornikctl task tail`)

Alias for `vornikctl task tail`. Prints current container logs for a running task, or the latest persisted failure/result excerpt after completion.

```
vornikctl tail <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | `false` | Poll and print appended log lines |
| `-n`, `--lines` | `200` | Number of lines to show |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task

Manage tasks

Inspect, list, cancel, and retry tasks in the vornik control plane.

## vornikctl task amend

Amend the task brief; re-queues from non-running state

```
vornikctl task amend <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Author identity (operator handle, defaults to OS user) |
| `--new-brief` |  | New brief text |
| `--reason` |  | Optional reason for the amendment |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task answer

Reply to an open checkpoint and re-queue the task

```
vornikctl task answer <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Author identity (operator handle, defaults to OS user) |
| `--checkpoint` |  | Checkpoint message id (required) |
| `--choice` |  | Selected option id (for decision checkpoints) |
| `--content` |  | Message content |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task cancel

Cancel a task

Cancel a task by ID. Only tasks in QUEUED, LEASED, RUNNING, or PENDING status can be cancelled.

```
vornikctl task cancel <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task close

Close a task (operator-confirmed terminal)

```
vornikctl task close <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Author identity (operator handle, defaults to OS user) |
| `--reason` |  | Optional closure reason |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task directive

Post a directive (course correction) — re-queues the task on non-running state

```
vornikctl task directive <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Author identity (operator handle, defaults to OS user) |
| `--content` |  | Message content |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task explain

Generate a post-mortem explanation for a terminal task

Joins the task's failure context (last error, step outcomes, recent
tool calls, container log tail) and asks the configured chat
provider for an operator-friendly paragraph explaining what went
wrong and what to try next.

By default prints just the summary paragraph. Pass --show-data to
also dump the structured inputs the model saw, or --json for the
full machine-readable response.

```
vornikctl task explain <task-id> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |
| `--show-data` | `false` | Print the structured inputs the model saw alongside the summary |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task get

Get a single task's details

Fetch one task by ID. Prints the task envelope (status, workflow, timestamps) and, with --json, the full API response including payload.

```
vornikctl task get <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task list

List tasks

List tasks for a project, optionally filtered by status.

```
vornikctl task list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |
| `-p`, `--project` |  | Project ID (required) |
| `-s`, `--status` |  | Filter by status (PENDING, QUEUED, RUNNING, COMPLETED, FAILED, CANCELLED) |

## vornikctl task message

Post a message to a task's conversation thread

```
vornikctl task message <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--author` |  | Author identity (operator handle, defaults to OS user) |
| `--content` |  | Message content |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task messages

List a task's conversation messages

```
vornikctl task messages <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task pause

Pause an active task

```
vornikctl task pause <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task resume

Resume a paused task

```
vornikctl task resume <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task retry

Retry a task

Retry a failed or cancelled task by ID. The task is re-queued for execution.

```
vornikctl task retry <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--reset-attempts` | `false` | Reset attempt counter to 1 |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task submit

Submit a new task

Submit a new task to a project's queue. The --prompt flag is the
operator-friendly shortcut for the context.prompt payload shape every
researcher role already reads; for custom shapes use --context-json.

Examples:
  vornikctl task submit -p janka --prompt "Summarise yesterday's scans"
  vornikctl task submit -p snake --task-type feature --prompt "Implement X"
  vornikctl task submit -p n8n-agents --workflow adaptive --prompt "..." --priority 10

```
vornikctl task submit [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--attach` | `[]` | Attach a file as an input artifact (snapshotted + auto-extracted into project memory). Repeatable; e.g. --attach book.epub --attach paper.pdf |
| `--context-json` |  | Raw JSON object to set as the task context (mutually exclusive with --prompt) |
| `--idempotency-key` |  | Optional idempotency key; duplicate submits return the existing task |
| `--json` | `false` | Output the raw API response instead of the human summary |
| `--priority` | `0` | Priority 0-100; 0 uses the project's default |
| `--prompt` |  | Task prompt — shorthand for --context-json '{"prompt":"..."}' |
| `--task-type` | `research` | Task type label (free-form; surfaces in `task list`) |
| `--workflow` |  | Override the project's default workflow |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl task tail

Tail task logs

Print current container logs for a running task, or the latest persisted failure/result excerpt after completion.

```
vornikctl task tail <taskId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | `false` | Poll and print appended log lines |
| `-n`, `--lines` | `200` | Number of lines to show |
| `-p`, `--project` |  | Project ID (required) |

## vornikctl version

Print the version number

```
vornikctl version
```

## vornikctl workflow

Inspect workflow definitions

## vornikctl workflow list

List workflows

```
vornikctl workflow list [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl workflow show

Show one workflow's full definition

```
vornikctl workflow show <workflowId> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Output in JSON format |

## vornikctl workflow validate

Validate a WORKFLOW.md file or a directory of workflows against the SKILL.md shape

Validate a WORKFLOW.md file or every *.md immediate child of a directory.

Enforces the agentskills.io / SKILL.md frontmatter shape:
  - name (required, lowercase-hyphens, ≤64 chars)
  - description (required, ≤1024 chars)
  - version (required, semver shape)
  - author / license (recommended; warnings only)
  - metadata.related_skills (optional list of name-shaped entries)
  - file size ≤100k chars; warns over 15k
  - body must have a '## Prompts' section when frontmatter
    declares agent steps with no inline 'prompt:'

Exit code 0 on clean or warnings-only; 1 on any ERROR finding.

The --fix flag prints suggested replacements inline; writing
them back is out of scope and remains a manual edit.

```
vornikctl workflow validate <path> [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--fix` | `false` | Print suggested fixes for findings that have a mechanical hint |
| `--json` | `false` | Output the validation report as JSON |

