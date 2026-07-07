---
description: Propose a knowledge skill (instructional know-how) into vornik's skill store as a draft
allowed-tools: mcp__vornik__skill_propose
argument-hint: <skill name + what it captures>
---

# Propose a knowledge skill

The user wants to capture reusable know-how as a vornik **knowledge skill**:
**$ARGUMENTS**

A knowledge skill is an instructional SKILL.md-style procedure that, once an
operator approves it, is served to vornik swarm roles and every companion
client. It is NOT a `SWARM-SKILL.md` capability bundle (workflow + roles) and
is unrelated to RAG memory — use `/remember` for facts, this for procedures.

Call `mcp__vornik__skill_propose` with:

- `name` — a kebab-case slug (e.g. `trace-bedrock-model-hang`). Unique within
  the repo scope.
- `description` — one line describing WHEN to apply the skill (the trigger).
- `body` — the Markdown procedure. Keep it actionable: steps, checks,
  anti-patterns. Under 64 KiB.
- `repo_scope` — omit to use the key default (the current repo); pass `*` for
  a cross-repo procedure.
- `domain` / `tags` / `roles` — optional. `roles` limits which swarm roles the
  skill injects into (empty = any role).
- `global` — optional (default false). Set `global=true` to propose a
  CROSS-PROJECT skill: once approved it injects into EVERY project's roles, not
  just this one — e.g. a procedure you want the janka and assistant autonomy
  roles to pick up too. Only honored on a fresh create; use `/skill-set-global`
  to change reach on an existing skill. A global skill's approval prompt is
  labelled "affects ALL projects" so the operator decides knowingly.

Draft first: the skill lands as a `draft` and does NOT fire until an operator
approves it (`/skill-approve` with a `skill_admin` key). Report the returned
id, name, and version, and remind the user it needs approval.

If the response says `this key lacks skill_write`, surface it verbatim; the
operator-side fix is `vornikctl companion grant --skill-write`.
