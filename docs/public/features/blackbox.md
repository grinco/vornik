---
sources:
    - path: internal/cli/blackbox.go
      sha256: a3ab2f66a79f53977a42f9e7de67d4c0a7b2f05f837902a8be56adc7a0ec7ad9
    - path: internal/enterprise/blackbox/engine/sideeffects.go
      sha256: e7661d65ea2a5326fe7035ab7c1e391d4430670c397ddb19467a6d2eb238a135
---
# Autonomy Black Box

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.


When a task does something you didn't expect — a wrong answer, a surprising
cost, a tool call you didn't anticipate — you need the full record of what
happened, and ideally an answer to *"what would have happened if one thing had
been different?"* The Autonomy Black Box gives you both: a single unified
**trace** per task, and **counterfactual replay** that re-runs a task with one
variable changed so you can compare the two side by side.

Everything here is **admin-gated** — the trace and replay surfaces require an
admin-scoped API key, the same gate that protects the audit log.

## Unified trace

Every task vornik runs scatters evidence across many places: the messages
exchanged, each LLM call and its token cost, every tool call and its result,
memory chunks retrieved, judge verdicts, step outcomes, operator actions. The
Black Box **assembles** all of it into one chronologically-ordered trace for a
task, so you read it once instead of stitching it together by hand.

```bash
vornikctl blackbox trace <task_id>
```

```
Task:        task_20260622_ab12cd
Project:     research-desk
Workflow:    daily-digest
Status:      COMPLETED
Cost (USD):  $0.0431
Digest:      9f2c…e7a1

Events: 38 (LLM=7 Tool=12 Memory=9 Steps=6 Judge=4)

Use --json for the full event stream.
```

Add `--json` for the complete event stream — every LLM call, tool input/output,
and retrieved memory chunk in order. The web UI at `/ui/admin/blackbox` is the
richer reader: it renders the trace as a clickable event log with type badges
and a cost roll-up, so you can drill into the exact prompt, tool input, or
memory chunk that mattered.

### Trace digest

Each trace carries a **digest** — a SHA-256 over the canonicalised event
sequence. Two people who assemble the same task's trace get the same digest, so
you can cite it in a ticket or an audit and know you're both looking at the same
record. The digest is the trace's tamper-evident fingerprint.

## Counterfactual replay

A counterfactual re-runs a task with **exactly one variable changed** and
records a new trace. The engine refuses to vary more than one thing at a time —
multi-variate replays are too hard to reason about.

```bash
vornikctl blackbox replay <task_id> \
    --variable model \
    --value openai-gpt-oss-120b \
    --label "would gpt-oss have been cheaper?"
```

Supported variables today:

| Variable | What it changes | Notes |
|----------|-----------------|-------|
| `model`  | The chat-router model target | `--role` optionally narrows the swap to one workflow role; omitted = router-level |
| `prompt` | The system/user prompt for a role | `--role` is **required** |

`--label` is required — it's the operator-readable description recorded on the
new run and shown in the compare view. Further variables (budget, tool-result
injection, excluding a specific memory chunk) are on the roadmap.

The replay creates a **new task** that runs to completion under normal
scheduling. Poll it like any task, then compare:

```bash
vornikctl task status <new_task_id>
vornikctl blackbox scorecard <original_task_id> <new_task_id>
```

### Replay is side-effect safe by default

A counterfactual must never fire a real-world action — it can't place a broker
order, send a message, or write a file just because you asked "what if?". The
replay engine is therefore **deny-by-default**: during a replay a tool runs live
only when it is **both** on the replay-safe allow-list **and** was actually
called in the original run. Every other tool call is short-circuited with a
synthesized `skipped` response, so the replay proceeds without any external
effect.

The replay-safe allow-list covers read-only and idempotent tools — read-only
broker queries (account summary, positions, orders, quotes), market-data and
technical-indicator lookups, file reads, web fetch/search, and memory recall.
Anything that can change the world (order placement, outbound messaging, file
writes, shell, cluster control) — and anything nobody has classified yet — is
stubbed.

Inspect the live allow-list:

```bash
vornikctl blackbox sideeffects
```

Operators tune it with the `blackbox.replay_safe_tools` config key followed by a
daemon reload. Because the policy is deny-by-default, you opt a tool *into* live
replay (a safe, explicit edit) rather than having to remember to deny a
dangerous one — a newly added tool is stubbed until you decide otherwise.

## Scorecard

The scorecard assembles two traces and prints the differences — status change,
cost delta, latency delta, step/LLM/tool-call count deltas, and per-step
divergences (a different tool called at a step, a different judge verdict, a
different outcome):

```bash
vornikctl blackbox scorecard <task_a> <task_b>
```

```
Trace 1            Trace 2            Delta
-------            -------            -----
task_ab12cd        task_ef34gh
status=COMPLETED   status=COMPLETED   same
$0.0431            $0.0123            -0.0308 (-71.5%)

Findings:
  - step 3: different model produced a shorter tool plan
  - llm calls: 7 → 4
```

The comparison is symmetric — swapping the two task IDs only flips the sign of
the deltas. When a large share of a replay's tool calls had to be stubbed, the
scorecard flags the comparison as low-confidence so you don't over-read a
divergence that's really an artifact of stubbing.

## Where to find it

| Surface | Location |
|---------|----------|
| CLI | `vornikctl blackbox {trace, replay, scorecard, sideeffects}` |
| Web UI | `/ui/admin/blackbox` — Traces, Compare, Counterfactuals (and regression Triggers) |
| API | `/api/v1/admin/blackbox/*` (admin-scoped key required) |

All surfaces require admin scope; without it the daemon returns `403`.

The regression **Triggers** tab and operator-gated repair flow are part of the
[self-healing workflow genome](self-healing.md), which builds on the same trace
and replay machinery.
