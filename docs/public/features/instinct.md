---
sources:
    - path: internal/featuredoctor/feature_instinct.go
      sha256: 00cbe484b24d980cb0e7ad9837584a5c318a82837c1c7ee35422dbd4636a436a
    - path: internal/enterprise/instinct/engine/confidence.go
      sha256: 0570ac7a29c04c82b3539f1b5039aac038b6565a40213e74c29c610ae70f104e
---
# Instinct layer

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.


The instinct layer lets vornik learn from its own history. It mines the
telemetry every task already produces, distils recurring patterns into
confidence-scored **instincts** — "in situation T, action A held up" — and
makes those instincts available as *evidence* to the parts of vornik that
decide how the next task runs.

The defining rule: **instincts are advisory. Nothing mutates silently.** An
instinct can raise the confidence of a workflow proposal an operator is already
reviewing, or surface a remediation hint on a failed task — but it never edits a
prompt, swaps a model, deletes a memory, or promotes a workflow on its own.
Every behaviour-affecting decision still passes through the same operator and
review gates it always did. With the layer disabled (the default), behaviour is
unchanged.

## How it learns

A background worker runs on a cadence (30 minutes by default) and reads the
**audit spine** — the step outcomes, tool-call log, and memory-retrieval log
that vornik writes for every task. Deterministic extractors turn finalised
outcomes into atomic patterns; only genuinely ambiguous clusters fall through to
a cheap distillation model. Each pattern is deduplicated by a canonical trigger
key and scored.

### Confidence and decay

An instinct's confidence is a **Wilson lower bound** on its success rate
(so a pattern seen 3 times is trusted far less than one seen 300 times),
multiplied by a **recency decay** with a 30-day half-life (so a pattern that
stops recurring fades rather than lingering forever). An instinct moves through
states as the evidence accumulates:

| State | Reached when |
|-------|--------------|
| `candidate` | freshly mined; not yet trusted |
| `active` | confidence ≥ 0.6 and seen at least 3 times |
| `promoted` | holds across ≥ 2 projects at confidence ≥ 0.8 (becomes a global prior) |
| `retired` | confidence falls below 0.2 (sticky — stays retired) |

These thresholds are tunable (see [Configuration](#configuration)).

## What consumes instincts

Four consumers read instincts, each independently switchable and each strictly
advisory:

- **Failure-class playbooks** — when a task fails, high-confidence remediation
  instincts for that failure class are surfaced on the task's detail page and
  offered to the lead as recovery *context*. The operator's recovery-approval
  gate is unchanged.
- **Architect evidence priors** — when the workflow architect proposes a change,
  matching instincts act as priors. They can only nudge the proposal's
  confidence *up* toward the strongest supporting evidence (never above what the
  model itself reported, never widening the edit's scope); a rejected proposal is
  written back as contrary evidence.
- **Memory hygiene** — instincts suggest which memory scopes to boost and which
  chunks are prune candidates. The consolidation sweeper *surfaces* these; it
  never deletes on their say-so, and firewall-sensitive chunks are screened out.
- **Tool-budget tiers** — when a planner gives no explicit complexity verdict,
  a learned tier can fill the gap. An explicit verdict always wins, and the
  configured budget caps still bound the result.

There is an optional `auto_apply` mode (off by default, gated by its own
confidence floor) that changes only how *strongly* a recovery remediation is
presented to the lead — it does not bypass the approval gate.

## Enabling it

Run the feature doctor to diagnose prerequisites and enable:

```bash
vornikctl doctor feature enable instinct
```

This sets `instinct.enabled` plus the consumer gates and restarts the daemon
during an idle window. Because it is restart-gated, the doctor refuses to apply
while tasks are running and waits for an idle window. (The tool-budget and
`auto_apply` consumers stay manually opt-in.)

## Inspecting what it learned

Everything the layer knows is browsable. Use the CLI:

```bash
vornikctl instinct list --domain recovery --min-confidence 0.8
vornikctl instinct show <instinct_id>
vornikctl instinct export -o instincts.json        # filtered snapshot
vornikctl instinct retire <instinct_id>            # advisory; the row stays for audit
```

`list` filters by `--domain`, `--scope`, `--project`, `--status`, and
`--min-confidence`. The same inventory is available as a read-only browser at
`/ui/admin/instincts`, and failed tasks show their learned remediations inline
on the task detail page. All admin surfaces require an admin-scoped key.

## Configuration

The layer is configured under the `instinct` block. The most useful keys:

| Key | Default | Meaning |
|-----|---------|---------|
| `instinct.enabled` | `false` | master switch |
| `instinct.cadence_seconds` | `1800` | how often the miner runs |
| `instinct.min_support` | `3` | observations before a pattern can leave `candidate` |
| `instinct.active_confidence` | `0.6` | promotion-to-`active` threshold |
| `instinct.promote_confidence` | `0.8` | cross-project global-promotion threshold |
| `instinct.promote_projects` | `2` | projects a pattern must hold across to go global |
| `instinct.retire_floor` | `0.2` | confidence below which an instinct retires |
| `instinct.decay_halflife_days` | `30` | recency-decay half-life |
| `instinct.model` | `chat.model` | distillation model for ambiguous clusters |

The four consumers are toggled under `instinct.consumers`:
`failure_playbooks`, `architect_priors`, `memory_hygiene`,
`application_feedback`, and `tool_budget`. The optional auto-apply mode lives
under `instinct.consumers.auto_apply` (`enabled`, `min_confidence`,
`allowed_error_classes`).
