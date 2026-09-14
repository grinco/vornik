# Configuration assistant

!!! note "Community Edition"

    The configuration assistant ships in the **Community Edition**. The
    models behind it are yours to choose and pay for. See
    [Editions](../editions.md).

The configuration assistant lets you change this deployment — project
autonomy settings, swarm configurations, workflows — from a natural-language
request, without hand-editing YAML. It reads the deployed configuration, the
schema this binary accepts and the deployment's own measurements (autonomy feed
lag and cadence breaches, execution ratings with their counterfactual, quality
percentiles, judge verdicts), edits a private copy of the tree the way a
coding agent would, and files the result as a **reviewable, rollbackable
proposal** in the control-plane ledger. It never writes the live tree itself.

```
your request ─▶ grounding ─▶ edit loop (private snapshot) ─▶ deterministic gates
            ─▶ blast-radius class ─▶ judge (a different model) ─▶ proposal ─▶ you
```

## What it will and will not do

- **It edits files; it never runs programs.** The assistant has a coding
  agent's read / grep / glob / edit ability over the config tree and no
  ability to execute anything. That keeps vornik's standing rule that only
  `vornikctl` on the host may spawn a process.
- **It proposes; it never applies on its own** — except for the two safest
  classes of change (tuning and descriptive prose), per project, when you opt
  in and the judge passed the edit.
- **It never deletes a file.** Removing a workflow or a project stays an
  operator action.
- **It never edits a secret.** Secret values are outside its reach by
  construction; it can rename the environment variable a value comes from,
  not read or write the value.
- **It refuses to run at all** for a project whose config tree currently
  contains a raw secret (the `config_secret_hygiene` doctor finding), because
  the assistant's model and its judge are hosted models and a raw secret in
  the tree would leave the box the moment you asked a question.

## Blast-radius classes

Every proposal is classified by the most restrictive thing it touches:

| class | examples | may auto-apply? |
|---|---|---|
| **A — tuning** | feed cadence, step timeout, retry count down | with opt-in and a judge pass |
| **B1 — steering prose** | `autonomy.goal`, a step's `prompt:` | operator doors only, with opt-in |
| **B2 — descriptive prose** | `PROJECT_CONTEXT.md` | with opt-in |
| **C — topology** | add / remove a step, a feed, a workflow | never |
| **D — spend** | budget caps up, cadence down, model changes | never |
| **E — authority** | `allowedTools`, `permissions.*`, MCP grants, `admin.*` | never — always a human approval |

A class-E proposal shows you the **effective permission diff** (which role
gains or loses which tools, computed the way the daemon resolves it), needs a
distinct confirmation of the subject being widened, and is capped per
credential per day. A key it cannot classify is class E.

## Where you can ask

| door | classes available |
|---|---|
| `vornikctl assist` and `POST /api/v1/operator/assist` | all, E human-approved |
| the control-plane console | all, E human-approved |
| a linked chat sender (`/assist …`) | A and B2 only |

The chat door opens only when the [identity core](identity.md) is on, and it
refuses to serve while account resolution is unavailable rather than falling
back to a sender allowlist.

## The judge

A second, cheaper model of a **different family** reads your intent and the
diff (never the assistant's own reasoning) and answers two questions: did the
edit do what you asked, and did it do anything you did not ask for? Its
verdict is attached to the proposal and quotes the changed lines. `abstain`
is not `pass`: an abstaining or unavailable judge means the proposal is filed
NOT JUDGED and cannot auto-apply. The daemon refuses a same-family pair
outright.

## Kill switches

- `config_assistant.paused: true` refuses every request before any model call.
- `config_assistant.disabled_classes: [C, D]` refuses those classes entirely.
- a project, swarm or workflow can carry `config_assistant_enabled: false`.

## Turning it on

```bash
vornikctl doctor feature enable config-assistant
```

The doctor checks three things first and refuses if any fails: a chat provider
is configured; the store honours the durable-commit contract the multi-file
apply journal relies on; and `config_assistant.model` and
`config_assistant.judge_model` resolve to different provider families.

## Configuration

| key | default | meaning |
|---|---|---|
| `config_assistant.enabled` | `false` | open the operator doors |
| `config_assistant.model` | `chat.model` | the assistant's model |
| `config_assistant.judge_model` | — | the judge; must be a different family |
| `config_assistant.paused` | `false` | global kill switch |
| `config_assistant.disabled_classes` | `[]` | classes refused outright |
| `config_assistant.max_output_bytes` | `262144` | overlay cap; exceeding it refuses |
| `config_assistant.max_tool_turns` | `40` | edit-loop cap per request |
| `config_assistant.request_timeout` | `5m` | per-request deadline |
| `config_assistant.class_e_daily_cap` | `5` | class-E proposals per credential per day |
| `config_assistant.auto_apply.<project>.classes` | — | opt-in for A, B1, B2 |
| `config_assistant.chat_door` | `false` | open the chat door (A, B2) |

See also [Architect consultation](architect-consult.md).
