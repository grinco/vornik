---
sources:
    - path: https://docs.vornik.io
      sha256: dd3c0c7cc1fe6b09a221865df381426829fb58f8b1e9796adb8283fdca681b72
---
# Troubleshooting

This page walks through the problems you are most likely to hit while running
vornik, and how to resolve them yourself. Work top-to-bottom: the quick checks
below resolve the majority of issues, and the sections after them cover
specific symptoms.

If a fix here references a setting, see
[Configuration reference](../reference/configuration.md) for the full schema, and
[vornikctl reference](../reference/vornikctl.md) for command details.

## Quick checks

Run these first whenever something looks wrong. They tell you, in order,
whether the daemon is alive, whether it can reach its database, whether the
container runtime is healthy, and what the built-in diagnostics think.

```bash
# Is the daemon alive?
curl -s http://localhost:8080/healthz

# Can it reach its database?
curl -s http://localhost:8080/readyz

# Is the container runtime working?
podman info

# Run the built-in diagnostics
vornikctl doctor
```

`vornikctl doctor` runs a battery of checks (configuration validity, database
schema, container runtime, agent images, stuck tasks) and prints actionable
results. It is the fastest way to localize a problem. A few checks are worth
knowing by name:

- **`model_health`** — looks at the last 24 hours and flags a model that is
  failing too often or returning degenerate (near-empty) output, and
  recommends the role's configured fallback model. It never switches models
  for you — swapping a model under a live swarm is left to you.
- **`config_crlf`** — finds config files saved with Windows-style CRLF line
  endings, a common cause of phantom config drift (see
  [Configuration drift after editing in the UI](#configuration-drift-after-editing-in-the-ui)).
  Run `vornikctl doctor --fix` to normalize them to LF in place.
- **`model_route_coverage`** — confirms every model a role uses resolves to a
  configured route and has a pricing entry, so nothing routes to an
  unintended catch-all or has its cost silently estimated.

### Collect evidence before asking for help

When a problem is not obvious, make `vornikctl support-report` your **first**
step before opening a support thread. It produces one self-contained,
**redacted-by-default** `tar.gz` bundle so you do not have to hand-assemble
logs, audit, config, and a doctor diagnosis:

```bash
# Everything tied to one task
vornikctl support-report --task <task-id>

# Or a time window (a timestamp, or a duration like 2h / 90m)
vornikctl support-report --since 2h

# Preview what would be collected — writes nothing
vornikctl support-report --task <task-id> --dry-run
```

Exactly one of `--task` or `--since` is required. Everything runs through
vornik's secret detector before it is written, so the redacted bundle is safe
to share — send it to support@vornik.io.

## The daemon will not start

A daemon that exits immediately on launch almost always reports the reason on
its first few log lines. The common causes:

| What you see | Cause | Fix |
|---|---|---|
| `failed to load configuration` | The config file is missing or has invalid YAML | Confirm the path vornik is reading and validate the YAML syntax. See [Configuration reference](../reference/configuration.md). |
| `failed to connect to PostgreSQL` | The database is not reachable | See [Database problems](#database-problems) below. |
| `failed to initialize runtime manager` | The container runtime is not installed or not found | See [Containers will not run](#containers-will-not-run) below. |
| `scheduler already started` | A second vornik process is already running | Stop the duplicate process, then start vornik once. |

If the daemon starts but then keeps restarting, the cause is usually that the
database was not ready when vornik launched. Make sure your database is up
before starting vornik.

### The daemon stops when you log out

!!! note "Symptom"
    vornik becomes unreachable after you close your SSH session, then
    "magically" works again the next time you log in.

When vornik runs as a per-user service, the operating system stops it together
with your login session unless you enable lingering. Enable it once:

```bash
sudo loginctl enable-linger <your-username>
```

After this, vornik keeps running whether or not you are logged in.

### The daemon will not stop, or is killed on restart

!!! note "Symptom"
    `systemctl --user restart vornik` hangs for the full stop timeout and the
    journal shows vornik was force-killed (SIGABRT) mid-shutdown.

This happens when shutdown waits on an in-flight long request — a chat
completion held open for an agent's whole tool-loop can outlast systemd's
stop timeout, at which point systemd escalates to a hard kill.

vornik now bounds the drain: graceful shutdown gets a fixed budget, then any
still-open connections are force-closed so the process always exits in time.
To give that bounded drain headroom, set `TimeoutStopSec=90s` on the systemd
unit (a user drop-in works).

Even with the fix, pick a good moment to restart:

- Do not restart while tasks are `RUNNING` or `LEASED`, or while a chat
  conversation is active — in-flight work is force-dropped at the budget.
- Check for live agent containers first, and wait for an idle window:
  ```bash
  podman ps --filter label=vornik.managed=true
  ```

## Tasks are not running

### Tasks stay QUEUED forever

If tasks sit in `QUEUED` and never start, the scheduler has nothing to run
them on. Check, in order:

1. **Is the container runtime healthy?** Run `podman info`. If it errors, fix
   the runtime first (see [Containers will not run](#containers-will-not-run)).
2. **Are you at your concurrency limit?** vornik runs a bounded number of tasks
   at once. If that many are already `RUNNING`, new tasks wait their turn. This
   is expected; let the running tasks finish or raise the limit in your config.
3. **Is the agent image available?** A task cannot start if its container image
   is missing. Pull it manually to confirm it exists.

### Tasks show RUNNING or LEASED but nothing is happening

This usually means a task was interrupted (for example, vornik restarted
mid-execution) and the work was not picked back up cleanly. vornik recovers
these automatically, but you can clear a known-stuck task immediately:

```bash
vornikctl task cancel <task-id> --project <project>
vornikctl task retry  <task-id> --project <project>
```

### A task failed instantly

A task that goes straight to `FAILED` with no run time almost always failed
before the agent did any work. Inspect the execution to see the exact error:

```bash
vornikctl execution list --project <project> --status FAILED
vornikctl execution inspect <execution-id>
```

The most common causes are a missing container image, a mount path that does
not exist, or a permissions problem in the container runtime.

### A failed task that you want to resume mid-way

You do not have to re-run a long task from scratch. From the task detail page
in the web UI, use **Retry** to re-run a failed execution. Multi-step
workflows can be retried from the step that failed rather than from the
beginning.

### "file does not exist" in a multi-step workflow

!!! note "Resolved"
    If a later step of a multi-step workflow (for example a writer following a
    researcher) once failed with a `file does not exist` error for a file the
    previous step had produced, this is **fixed** — no action needed.

This was a handoff bug, not flaky storage or a broken mount, so there is no
need to chase it. A step's outputs are now persisted into the durable
artifact store and re-staged into the next step's workspace, so a later step
reliably sees what an earlier one produced. If you are running a current
release, you should not hit it.

## Containers will not run

vornik runs every agent in a container. If the runtime is unavailable, tasks
queue but never execute.

```bash
# Is it installed and on your PATH?
which podman

# Does it actually run?
podman info

# A rootless setup also needs its socket active
systemctl --user status podman.socket
systemctl --user start  podman.socket
```

### Image pull failures

If an execution error mentions pulling an image, pull it by hand to see the
real error (a typo in the image name, a registry you are not logged in to, or
no network):

```bash
podman pull <image-name>
podman images          # confirm what you already have locally
```

### Permission or user-mapping errors

Rootless container errors that mention `newuidmap`, `newgidmap`, or a "pause
process" usually mean the runtime's user namespace mapping needs a refresh:

```bash
podman system migrate
```

If problems persist, confirm your user has subordinate UID/GID ranges
configured. As a last resort you can run agents in the host user namespace via
the runtime settings in your config — see
[Configuration reference](../reference/configuration.md).

### Leftover containers

vornik labels the containers it manages, so you can list and clean them up
without touching anything else on the host:

```bash
# List vornik-managed containers
podman ps -a --filter label=vornik.managed=true

# Remove the ones that have exited
podman rm $(podman ps -a --filter label=vornik.managed=true --filter status=exited -q)
```

## Database problems

### The database is unreachable

If `/readyz` returns an error, vornik cannot reach its database. Confirm the
database is running and accepting connections, then start it if it is down.
Once the database recovers, vornik reconnects on its own — re-check `/readyz`
to confirm.

### Upgrade or migration errors

If startup fails with a migration error after an upgrade, the most common cause
is that the database was previously written by a *newer* version of vornik than
the one you are now running (a downgrade). Run a matching or newer vornik
build, or restore from a backup. Always take a backup before upgrading; see
[vornikctl reference](../reference/vornikctl.md) for the backup and restore
commands.

## Configuration drift after editing in the UI

!!! note "Symptom"
    A swarm, project, or workflow you edited through the dashboard keeps
    showing up as drifted between your source and deployed config even though
    the content looks identical, and the drift comes back after you reconcile
    it.

The cause is invisible Windows-style CRLF (`\r\n`) line endings. vornik parses
them fine, but `git` and diff tooling treat the deployed copy as changed
against an LF source tree, so drift keeps reappearing. Saving through the UI
now normalizes line endings to LF, but to clean up files that already drifted:

```bash
# See which files carry CRLF (the config_crlf check)
vornikctl doctor

# Normalize CRLF to LF in place
vornikctl doctor --fix
```

## Memory and semantic search

vornik's [project memory](../features/memory-rag.md) supports semantic
(meaning-based) search in addition to keyword search.

### Search only ever returns keyword matches

!!! note "Symptom"
    Memory search works, but never returns semantic ("similar meaning")
    results — only literal keyword hits.

Semantic search needs the `pgvector` extension in your PostgreSQL database.
Without it, vornik silently falls back to keyword-only search. Install
`pgvector` (the simplest path is a PostgreSQL image that ships with it
pre-installed), enable the extension in your database, and restart vornik. New
content is embedded automatically once the extension is present.

### Search errors mention "dimensions" mismatch

This means the embedding model you configured produces vectors of a different
size than the one your memory store was first set up with. Pick a single
embedding model and keep it consistent. If you intend to switch models, you
will need to re-embed your existing memory with the new model; see
[Memory and RAG](../features/memory-rag.md) and
[Configuration reference](../reference/configuration.md).

## Conversation channels

If a chat channel (for example a messaging bot) stops responding or replies
look wrong, see [Conversation channels](../guides/conversation-channels.md) for
channel-specific setup. Two quick things to check first:

- **Replies time out on long requests.** Some operations (large analyses,
  bulk memory reclassification) legitimately take longer than the default
  client timeout. Raise the request timeout for those calls — see
  [Workflows and LLM controls](../guides/workflows-and-llm-controls.md).
- **A channel double-replies.** If you run more than one vornik instance, only
  one should own each polling channel at a time. Webhook-based channels are
  safe behind a load balancer without extra configuration.

## Costs look wrong or higher than expected

vornik tracks per-project LLM spend and enforces optional budgets. If spend is
climbing unexpectedly:

- Check the spend panel on the project page for a per-role breakdown.
- A model that frequently produces malformed output costs more than its
  headline price because it gets retried. Compare models by their *effective*
  cost (spend per successful step), not their list price.
- Set soft and hard budget limits so runaway loops are stopped automatically.

See [Cost and caching](../guides/cost-and-caching.md) for the full picture.

## Still stuck?

- Re-run `vornikctl doctor` and read every warning, not just the failures.
- Reproduce the problem in the web UI first — it often surfaces a clear error
  message that the command line hides.
- Check the [Release notes](../release-notes/index.md) in case the behavior
  you are seeing changed in a recent version.
- Review [Configuration reference](../reference/configuration.md) to confirm
  the relevant setting is what you expect.
