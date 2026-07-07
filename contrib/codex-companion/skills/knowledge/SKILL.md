---
name: knowledge
description: |
  Teaches Codex to use vornik's daemon-owned knowledge-skill store — propose
  reusable know-how, search/apply approved skills, and (with an admin key)
  approve drafts. Use when the vornik MCP tools are available and the user
  wants to capture or reuse a procedure. Distinct from the delegate skill.
---

# vornik knowledge skills

A vornik **knowledge skill** is instructional know-how (a SKILL.md-style
procedure) stored on the vornik daemon. Once an operator approves it, it is
served to vornik swarm roles AND every companion client — write it once in
Codex, and it's available everywhere.

This is NOT the same as:
- **`SWARM-SKILL.md` capability skills** (workflow + roles you install) — those
  use `vornikctl skill`, not these tools.
- **RAG memory** (`remember`/`recall`) — that's for facts. Knowledge skills are
  for procedures the agent should apply.

## Tools

- `mcp__vornik__skill_propose` — create a DRAFT skill (needs `skill_write`).
- `mcp__vornik__skill_search` — find active/trusted skills (needs `skill_read`).
- `mcp__vornik__skill_get` — fetch one skill's full body (needs `skill_read`).
- `mcp__vornik__skill_list` — enumerate by maturity for review (needs `skill_read`).
- `mcp__vornik__skill_approve` / `skill_reject` — the human gate (needs `skill_admin`).
- `mcp__vornik__skill_set_global` — set/clear a skill's GLOBAL reach (needs `skill_admin`).

## When to propose

Propose when you and the user just worked out a reusable procedure — a
troubleshooting flow, a deploy sequence, a non-obvious fix. Provide:

- `name` — kebab-case slug, unique within the repo scope.
- `description` — one line: WHEN to apply it (the trigger).
- `body` — actionable Markdown (steps, checks, anti-patterns), under 64 KiB.
- `roles` — optional; limits which swarm roles the skill injects into.

The skill lands as a `draft` and does not fire until an operator approves it.
Tell the user it needs approval; don't imply it's live.

## Cross-project (global) skills

By default a skill is project-scoped — it only injects into its home project's
roles. To make a skill reach EVERY project (e.g. so the janka and assistant
autonomy roles pick up a procedure you captured here), either:

- propose it with `global=true` (honored only on a fresh create), or
- flip an existing skill with `skill_set_global` (`id`, `global`).

Both need `skill_admin` and only affect a skill in this key's own project.
Changing reach never changes maturity. A global draft's approval prompt is
labelled "affects ALL projects" so the operator decides knowingly.

## Search before re-deriving

Before working through a procedure the project may already know, call
`skill_search` with a topic query. If a strong match comes back, `skill_get`
its body and apply it rather than reinventing it.

## Repo scope

Codex ships no SessionStart hook, so — as with the memory tools — you MUST
resolve `repo_scope` yourself and pass it on every skill call, or rely on the
key's default scope. Derive the canonical `<host>/<path>` token from
`git config --get remote.origin.url` (strip `.git`, scheme, and any leading
`user@`; replace the first `:` with `/`). Pass `repo_scope="*"` only for a
genuinely cross-repo procedure.

## Capability errors

If a tool returns `this key lacks skill_read` / `skill_write` / `skill_admin`,
surface it verbatim and give the operator-side fix:
`vornikctl companion grant --skill-read` / `--skill-write` / `--skill-admin`
(or `--skill-all`). Approval (`skill_admin`) is deliberately opt-in — a client
can propose but not self-approve.
