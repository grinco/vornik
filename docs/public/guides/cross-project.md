---
sources:
    - path: internal/executor/call_project.go
      sha256: af067e625043247a2b3ee0b433fb2cac8191ab3b7944d5b5259b21db18e10206
    - path: internal/executor/spawn_project.go
      sha256: 5ece68e724d864e53a5f78e85f2091c66a917fdbe2ac72c7be9fcfb6f91cacdc
    - path: internal/executor/a2a_call.go
      sha256: 4cc5de01462f829a382c99aa4a8e9f595243d2df2db693702fbf305239f2b7ed
    - path: internal/registry/project.go
      sha256: a6b13685371ebdecfddfa9870a8ae7fc37653f83374519c7d9cb9dc6ec0aeb82
---
# Cross-project orchestration & agent federation

A workflow in one project can hand work to another project, stand up a brand
new project on demand, or call a remote vornik-compatible agent over the open
**A2A** (agent-to-agent) protocol. This guide covers all three: `call_project`,
`spawn_project`, and `a2a_call`.

## Enabling cross-project calls

`call_project` and `spawn_project` are **off by default** and require Postgres
(they persist call lineage and spawn records, which the SQLite backend can't
provide). Turn them on with:

```bash
VORNIK_INTER_PROJECT_ENABLED=true
```

With the flag off, both step types fail fast — route their `on_fail` branch so
a workflow degrades gracefully rather than erroring out. `a2a_call` is governed
separately (see [A2A federation](#a2a-federation-calling-remote-agents) below).

## `call_project`: invoke another project's workflow

`call_project` is a workflow **step type**. The caller blocks until the callee
workflow reaches a terminal state, then continues with the callee's result
available to later steps.

```yaml
- id: enrich
  type: call_project
  target_project: research-desk
  target_workflow: summarize
  payload:
    url: "${outputs.fetch.url}"      # interpolate an earlier step's output
  expect:
    schema: summary-v1               # optional result-schema check
  timeout: 5m                        # optional
  cancel_on_timeout: true            # optional; cancel the callee if it overruns
  on_success: publish
  on_fail: handle-error
```

`payload` values support `${outputs.<step-id>.<field>}` interpolation from
earlier steps in the calling workflow.

### Who may call whom (allowlists)

Cross-project calls are governed by **two** allowlists in project YAML — one on
each side:

| Field | Side | Meaning | Default |
|---|---|---|---|
| `canCallProjects` | caller | Outbound allowlist — this project may call only these projects. | empty = may call any project |
| `acceptCallsFrom` | callee | Inbound allowlist — accept calls only from these projects (supports `team-*` / `*` globs). | empty = **refuse all** |

The callee side is **closed by default**: a project accepts no inbound calls
until it lists the callers it trusts. So both projects must opt in — the caller
(unless it allows all) and, always, the callee:

```yaml
# research-desk project — allow the reporting project to call it
acceptCallsFrom:
  - reporting
```

### Depth and cycle safety

Call chains are bounded so a misconfigured graph can't recurse forever:

- **Depth cap** — `maxCallDepth` per project (default **8**). A call that would
  push the chain past the cap is refused with `DEPTH_EXCEEDED`.
- **Cycle detection** — a project that is already an ancestor of the current
  call chain cannot be called again; the attempt is refused with
  `CYCLE_DETECTED`.

Both refusals are written to the audit log, so you can see exactly which call
was blocked and why.

## `spawn_project`: create a new project on demand

`spawn_project` instantiates a fresh project from a registered template and,
optionally, kicks off its first task:

```yaml
- id: onboard
  type: spawn_project
  template: customer-workspace
  params:
    name: acme-corp
  initial_task:                       # optional
    workflow: bootstrap
    payload:
      tier: standard
  on_success: notify
  on_fail: handle-error
```

Spawning is gated per project by `allowSpawn`, which is **closed by default**:

```yaml
allowSpawn:
  templates:                          # empty = no spawns allowed
    - customer-workspace
  maxSpawnsPerDay: 5
```

For safety, a freshly spawned project **cannot** pre-authorize its spawner in
`acceptCallsFrom` at spawn time — consent to receive calls must be granted
explicitly afterward, so spawning can't be used to bypass the inbound allowlist.

## A2A federation: calling remote agents

The A2A step talks to any agent that speaks the A2A protocol — another vornik
instance, or a third-party agent — over HTTP + SSE. Unlike `call_project`, it is
not bound by the inter-project flag; it's available wherever the A2A handler is
enabled.

```yaml
- id: delegate-remote
  type: a2a_call
  agent_url: https://partner.example.com/a2a/v1/agents/research/summarize
  prompt: "Summarize the attached filing in five bullet points."
  api_key_env: PARTNER_A2A_KEY        # optional; sent as the auth header
  timeout: 5m                         # optional (default 5m)
  on_success: publish
  on_fail: handle-error
```

The step POSTs the prompt to the agent's `tasks` endpoint, then streams the
agent's progress over SSE until it reaches a terminal state. A stream that goes
idle for 90s is treated as a failure.

### Exposing your own agents (inbound A2A)

When the A2A handler is enabled, each project+workflow is published as an
addressable agent:

| Route | Auth | Purpose |
|---|---|---|
| `GET /.well-known/agent.json` | public | Index of available agent cards. |
| `GET /a2a/v1/agents/<project>/<workflow>/card` | public | One agent's capability card. |
| `POST /a2a/v1/agents/<project>/<workflow>/tasks` | key | Submit a task to the agent. |
| `GET /a2a/v1/agents/<project>/<workflow>/tasks/{id}` | key | Stream task progress (SSE). |

Point the `api_key_env` of a remote caller at a key scoped to the callee
project. A few protocol features are intentionally not wired yet: result-schema
validation against `expect`, and `input-required` prompts (currently treated as
a failed task rather than a checkpoint).

## Troubleshooting

- **Step fails immediately with "disabled"** — set
  `VORNIK_INTER_PROJECT_ENABLED=true` and confirm you're on Postgres.
- **`CYCLE_DETECTED` / `DEPTH_EXCEEDED`** — inspect the call chain; raise
  `maxCallDepth` only if the depth is legitimate, never to mask a cycle.
- **Callee refuses the call** — add the caller to the callee's
  `acceptCallsFrom` (it's closed by default).
- **Spawn refused** — add the template to the spawner's `allowSpawn.templates`
  and check `maxSpawnsPerDay`.
