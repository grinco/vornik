---
name: validate-install
description: |
  Teaches Codex how to validate a Vornik deployment against the published
  reference architecture. Use this skill when the user asks whether an install
  is set up correctly, wants a post-install or post-upgrade check, asks for a
  health check / audit / review of a deployment, or reports that Vornik
  "isn't working" with no specific error to route on. This is the
  configured-vs-observed discipline: a config value is a statement of intent,
  never evidence of behaviour, so every check reads the daemon's RESOLVED
  state and then the ledger that proves it ran. Strictly read-only — never
  restart, reload, or "just try" a fix on a system you are auditing. Hand off
  to troubleshoot-vornik once a specific symptom emerges, and to
  report-problem when the deployment is defective rather than misconfigured.
---

# Validating a Vornik install

The reference shape is published at
**<https://docs.vornik.io/reference/reference-architecture/>** — read it before
reporting. This skill is the *procedure*; that page is the *yardstick*, and it is
the thing that gets updated when the product changes. If the user has a local
checkout, `docs/public/reference/reference-architecture.md` is the same page.

Validation is not troubleshooting. Troubleshooting starts from a symptom and works
toward a cause; validation starts from a reference shape and looks for divergence.
If the user already has a specific failure — daemon down, a task that failed, a
feature dark — use `troubleshoot-vornik` instead, which routes by symptom.

## Two rules that override everything else

**1. Read-only. Always.** You are inspecting a system whose behaviour you do not
own, often a customer's. Never restart, reload, drop, migrate, or "just try" a fix
mid-audit. If you find something that needs changing, report it and let the
operator decide; `configure-vornik` is the skill for actually changing it.

The trap with teeth: `POST /api/v1/config/reload` looks like the natural way to ask
*is the registry valid?* and it answers by **applying** the tree — which on a live
system can push a half-edited file into service. Your audit would then have caused
the outage it was sent to find. Ask instead:

```
GET /api/v1/config/reload-status
```

It reports the same errors, warnings and blocked reason from the last attempt
without triggering one.

**2. `vornikctl doctor` first.** It already encodes many of these checks with
remediations attached. Run it, read it out, and route to it. A validator that
re-implements its checks will drift from the product and start contradicting it.

```
vornikctl doctor                  # broad sweep
vornikctl doctor feature          # per-feature resolved health
vornikctl doctor --offline        # when the daemon is down
```

Note that `--offline` resolves config paths from *your shell*, not from the dead
daemon's unit — so on a non-default install it may be reading a different tree than
the daemon would.

## Order of work

Work outside-in. Each step can invalidate the ones after it, so do not gather
everything up front and reason at the end.

**1. Is it running, and which binary?**

```
systemctl --user is-active vornik
systemctl --user cat vornik | grep ExecStart
```

An Enterprise deployment running a binary built without the enterprise build flag
has EE features silently off, and everything below will look inexplicably absent.
Check this before concluding a feature is broken.

**2. Which config tree is it reading?** The boot log's `config watcher started`
line lists the resolved paths.

```
journalctl --user -u vornik | grep -m1 'config watcher started'
```

**This is ground truth.** Get it before looking at any config file, or you will
spend the whole audit reading files the daemon never opened. `VORNIK_CONFIGS_DIR`
is honoured only when the directory already holds all three of `projects/`,
`swarms/`, `workflows/` — otherwise it is skipped with no error at all, which is
the usual cause of "my env var is set but it reads the wrong tree".

**3. Registry health.** `GET /api/v1/projects` for what actually loaded;
`reload-status` for errors, warnings and blocked reason.

**4. Database.**

```sql
SELECT extversion FROM pg_extension WHERE extname = 'vector';
```

pgvector present, migrations at head, and the embedding column's dimension matching
the configured model. A dimension mismatch fails *every* insert, so it presents as
a total memory outage rather than as degraded quality.

**5. Memory.** `GET /api/v1/memory/stats` for `chunksTotal` / `chunksEmbedded` /
`queueDepth`, then `SELECT count(*) FROM memory_embed_dlq` for failures. A large
corpus mid-embed is a legitimate intermediate state; distinguish a **draining**
queue from an **idle** one before calling anything broken.

**6. Resolved subsystem state** — not configured state. See below.

**7. Execution posture.** The daemon's container-start log lines carry the full
argument vector: confirm capability drops and no-new-privileges on agent
containers.

## Check resolved state, never configured state

This is the single highest-value habit, and it has a canonical case that shipped.
A deployment can have `reranker.enabled` set to `true`, the reranker wired, and the
reranker never having fired once across a six-figure count of recorded model calls
— because the request path never carried the per-request flag. Config said yes.
Behaviour said nothing. Nothing reported the gap.

So for any subsystem, ask two questions in order:

- **Resolved:** the daemon logs it at boot. For the reranker,
  `memory reranker ACTIVE` or `memory reranker INERT`, with the gate that closed.
- **Observed:** the usage ledger proves behaviour.

```sql
SELECT role, count(*) FROM task_llm_usage WHERE role ILIKE '%rerank%' GROUP BY role;
```

Enabled + ACTIVE + zero ledger rows after context-assembly recall has run is a
defect. Enabled + INERT is a defect the boot log already explained — quote its
reason rather than re-deriving it.

The same shape generalises: `vornikctl doctor feature` reports resolved feature
state, and a feature enabled in config but reported dark is the finding.

## Severity

**Defects — report loudly:**

1. A registry file outside every daemon-reported config path. Highest severity:
   the operator's mental model and the system disagree, and nothing says so.
2. pgvector absent, or embedding dimension mismatched against the model.
3. Embeddings far behind with an **idle** queue and a populated dead-letter queue.
4. A subsystem enabled in config whose resolved state is inert.
5. Two daemons on one database, or sharing single-consumer channel credentials —
   a polled bot API cannot have two consumers, and the symptom appears on the
   victim, not the culprit.
6. Agent containers missing capability drops or no-new-privileges.
7. A project with no `maxConcurrentTasks` — one project can starve all others, and
   the daemon starts happily without it.

**Informational — state once, do not repeat:**

- Optional subsystems absent or degraded on a single-node install. A single-node
  install reporting its cluster feature degraded is *expected*: the feature is
  describing an absent capability, not a fault.
- NULL-scoped memory content — the un-migrated tail.
- Individual task or container failures. Agents fail for ordinary reasons — a model
  refusing, a timeout, an unreachable endpoint — and the executor retries and
  classifies them. Persistent failure of one *class* is signal; instances are not.
- Gate-quarantined memory deposits. A gate rejection is normal operation.
- A **draining** embedding queue.

**Never report:**

- Database, project or swarm **names** as wrong. Naming belongs to the operator.
  Installs are routinely named for their history rather than their role, and a name
  that reads like a test database is frequently the production one. Never infer an
  environment from a name, and never offer to drop one on that basis.
- **Model choices.** Heterogeneous models across roles — a cheap one for
  classification, a stronger one for synthesis — is the intended design, not drift.
- Key material, secrets, or any config value from a secrets tree, in any form.
  Never print a key, not even truncated, not even to confirm that it exists.

## How to word findings

Prefer **"configured X, observed Y"** over **"X is wrong"**. State the observation,
the reference expectation, and how you observed it, so the operator can check your
work.

A validator's credibility is spent the first time it reports a healthy install as
broken, and it does not recover — which is why the never-report list matters as much
as the defect list. Absence of an optional subsystem is not a finding; say so once
and move on.

If you could not observe something, say so. An unchecked item reported as passing is
worse than an honest gap.

## Where to go next

- A specific symptom emerged mid-audit → `troubleshoot-vornik`.
- The operator wants a divergence fixed → `configure-vornik`.
- The deployment is defective rather than misconfigured (a check that should pass
  cannot, or the product misbehaves against its own documented shape) →
  `report-problem`, which files an anonymized report the operator reviews and
  submits themselves.
