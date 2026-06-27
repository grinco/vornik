---
sources:
    - path: https://docs.vornik.io
      sha256: 69bba1cb257cf7a9abec8f1e0d4d434432716b619dc2b915c89260f9480a78cb
---
# Concepts

vornik orchestrates teams of AI agents to do continuous, asynchronous work
on your behalf. A handful of ideas explain almost everything you will do
with it. This page defines them and shows how they fit together. Once they
click, the [guides](../guides/index.md) and
[reference](../reference/index.md) are straightforward. For the system shape
behind these ideas — control plane vs data plane, the isolation boundary, and
multi-swarm vs multi-node — see [Architecture](architecture.md).

## The mental model

You can read the whole system in one sentence:

> A **project** owns a **swarm** of agents and one or more **workflows**;
> you submit **tasks**, and vornik runs each task through a workflow,
> letting agents **delegate** sub-work and draw on shared **memory**.

Everything below expands one word in that sentence.

## Projects

A **project** is the top-level unit and the boundary of isolation. Each
project is its own little world: its own task queue, its own swarm, its own
workflows, its own stored memory and artifacts, and its own policies. Two
projects on the same host never see each other's work.

A project is where you set the rules of engagement:

- which swarm runs the work
- the default workflow tasks use
- default priority and how many tasks may run at once
- whether the project may create work for itself (autonomy)
- how long history is kept (retention)
- which tools and secrets agents are allowed to use

You can run many projects at once. vornik schedules across all of them and
keeps them operationally separate, so a busy project cannot starve or
interfere with a quiet one beyond competing for the host's resources.

## Swarms

A **swarm** is the team of agents a project runs on. A swarm definition
lists **roles** — for example a `lead`, a `researcher`, and a `writer` —
and for each role says how many agents to run and what runtime they need.

Two ideas matter most:

- **The lead role.** One role is the swarm's lead. The lead plans the work
  and hands pieces off to specialist roles. The other roles do focused
  jobs: research, write, code, review, and so on.
- **Runtime policy.** Each role is either **warm** (kept running, ready to
  pick up work immediately) or **ephemeral** (started on demand and torn
  down when idle). Ephemeral is the default; make a role warm only when
  startup cost or latency justifies it.

Every agent runs in its own isolated Podman container, with a per-role
network policy that controls whether it can reach the network at all. New
installs default to no agent egress, so agents cannot reach outside the
host unless you grant a role that permission.

Swarms can be **reused**: a project can point at a shared swarm template,
or define its own custom one. vornik ships preset swarm templates (a
minimal code swarm, a fuller development stack, a research swarm, and
others) that you can generate and then adapt.

## Workflows

A **workflow** is the recipe for how a task gets done — the ordered steps,
the branches, and the conditions for moving between them. Workflows are not
hardcoded; each project picks or defines its own.

A workflow is a **graph of steps**, not just a straight line. It can
express:

- linear sequences (plan, then implement, then review)
- conditional routing through **gates** — for example, "if the review was
  approved, finish; otherwise loop back and revise"
- parallel branches that later join
- retry and recovery paths
- review or approval steps that pause for a human decision

Each step is assigned to a role from the swarm. A typical step says "run
the `reviewer` role with this instruction, and depending on the outcome,
go here or there." Workflows are **versioned**, so a running task always
stays tied to the exact workflow version that launched it, even if you edit
the workflow later.

When a step fails, a workflow need not simply give up. vornik can present
the lead with a **structured recovery decision** whose options carry typed
actions — re-route the task onto a different (allow-listed) workflow, fall
back to a role's configured backup model, retry, or skip. When one of these
paths reaches a successful end, vornik records that the run *recovered*
rather than completing straight through, so recovery shows up as its own
trend rather than hiding inside the success rate.

When you do not name a workflow on a task, the project's default workflow
runs. See [Workflows and LLM controls](../guides/workflows-and-llm-controls.md)
for how to author and tune them.

## Tasks

A **task** is one unit of work you ask the swarm to do — "summarise these
notes", "review this report and list the risks", "draft a feature plan."
It is the thing you submit and the thing you track.

A task carries:

- the project it belongs to
- a priority (lower numbers run first; the project default applies if you
  leave it at `0`)
- the workflow it will run on (the project default unless you override it)
- its place in the queue and any dependencies
- its delegation lineage (which task spawned it, if any)
- its status and execution history
- links to the artifacts it produced

A task moves through a clear lifecycle: `PENDING` → `QUEUED` → `RUNNING`,
ending in `COMPLETED`, `FAILED`, or `CANCELLED`. Because all of this is
persisted, a daemon restart does not lose your work — vornik recovers
unfinished tasks and continues.

### Artifacts

The durable outputs a task produces — plans, findings, drafts, reports,
review notes — are stored as **artifacts**. Files an agent writes to its
output location are persisted automatically and surfaced through the API,
the Web UI, and connected chat channels. Artifacts are snapshotted across
retries, so an iterative task preserves its history instead of silently
overwriting it.

Two kinds of storage sit behind every step, and the distinction matters:

- An **ephemeral per-step workspace** that exists only for that step. The
  agent writes its deliverables here; it is wiped between steps.
- A **persistent project worktree** that carries a project's files forward.
  In the default mode this is a per-task copy branched from the project, so
  one task's work never disturbs another's.

Because a step's outputs live in the ephemeral tier, vornik bridges them
through the durable artifact store when one step needs to hand work to the
next: each step's outputs are saved to the store and re-staged into the next
step's workspace as inputs. This **store-backed handoff** is why a writer
reliably sees the researcher's files even though each step ran in its own
throwaway container.

## Delegation

**Delegation** is how a swarm decomposes big work. A lead agent (or a
workflow step) can create **child tasks** for other roles to handle, then
gather their results back into the parent. This is what turns a single
request into coordinated teamwork: a lead can plan, fan out research to
several agents, collect their findings, and have a writer assemble the
result.

Delegation preserves **lineage** — every child task records its parent —
so you can always see how a piece of work was broken down. It is also
**bounded by policy**: a project controls who may delegate and limits are
enforced to prevent runaway recursion or a flood of self-created tasks.

Closely related is **autonomy**: a project can be allowed to create tasks
for *itself* based on its goal — generating follow-up work, raising
review tasks, or maintaining a continuous output stream. Autonomy is
off by default and, when enabled, is rate-limited and can be gated behind
approval so nothing runs without oversight.

## Memory

**Memory** lets a project carry knowledge forward across tasks instead of
starting cold every time. vornik can store facts, documents, and prior
results for a project and let agents semantically recall the relevant parts
when they work — so an agent on task #50 can draw on what was learned in
task #3.

Memory is **project-scoped**: each project's memory is private to that
project, just like its tasks and artifacts. Files you attach when
submitting a task are extracted into memory automatically, so the swarm can
reference them later.

Memory is an opt-in capability. See
[Memory & RAG](../features/memory-rag.md) for what it does and how to
enable it.

## Channels

A **channel** is a way for people to reach a project — to submit work and
receive results — without using the CLI, API, or Web UI directly. Channels
are **pluggable**: vornik ships a Telegram bot and an email channel, and the
set is meant to grow. They share one contract for sending replies and one
**channel-agnostic** way of delivering files, so a task's artifacts go out
the same way regardless of where the request came from — as documents in a
Telegram chat, or as attachments on a threaded email reply.

## Operating a fleet

vornik is **local-first**: a node runs the daemon and its agents on one
host. But a deployment can span more than one node, and a couple of
capabilities keep that observable without adding a separate control service:

- **Centralised log forwarding.** vornik can ship its structured logs and
  audit events to external sinks (an HTTP webhook and/or syslog), filtered to
  the scopes you care about. It is best-effort by design — a slow or failing
  sink never blocks or crashes the daemon.
- **Clustering and fleet visibility.** Nodes can register themselves so you
  see the whole fleet at a glance, a live feed surfaces what is running
  across projects in near real time, and an opt-in monitor probes the
  endpoints the cluster is expected to expose and alerts an operator when one
  goes down or recovers.

## How it all runs together

Putting the pieces in motion:

1. You submit a **task** to a **project** (via the CLI, the Web UI, the
   API, or a connected chat channel).
2. vornik writes it to the project's durable **queue** with its priority.
3. The scheduler picks eligible tasks by priority and dependency state,
   respecting each project's concurrency limit.
4. vornik resolves the project's **workflow** and **swarm**, then starts or
   reuses the needed agent containers.
5. The workflow runs asynchronously — agents **delegate**, branch, and draw
   on **memory** as the workflow allows.
6. Outputs are persisted as **artifacts**; status and logs are available
   throughout.
7. New tasks may be created by the lead or by autonomy policy and flow back
   into the queue. The project keeps running as a continuous system, not a
   one-shot request.

## Where to go next

- **[Getting Started](../getting-started/index.md)** — install and run the
  first task hands-on.
- **[Guides](../guides/index.md)** — how-tos for workflows, channels, cost
  and caching, storage and retention.
- **[Features](../features/instinct.md)** — opt-in capabilities such as
  memory, authentication, and the instinct layer.
