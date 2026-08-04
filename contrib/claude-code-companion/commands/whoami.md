---
description: Show which RAG project/scope this session is currently using
allowed-tools: mcp__vornik__whoami
---

# Which vornik RAG project/scope am I in?

The operator runs multiple Claude Code sessions a day against different
projects and repo scopes. This command answers "what scope are MY calls
using right now" without relying on the SessionStart digest, which only
fires once and may have scrolled out of context.

Call `mcp__vornik__whoami` with no arguments. Report back plainly:

- **Project**: `project_id` — this key's bound vornik project (not a
  repo_scope; keys are minted per-project via `vornikctl companion grant`).
- **Repo scope in use**: `effective_repo_scope` — this is what
  `recall` / `remember` / `recent_memory` / `delegate` resolve to right
  now when a call omits `repo_scope`. If it's empty, calls without an
  explicit `repo_scope` are project-wide (uncategorized).
- **Configured default**: `default_repo_scope` — the operator-set floor
  on this key (`vornikctl companion grant --repo-scope`). Note this can
  differ from the SessionStart hook's cwd-detected token: the hook
  passes `repo_scope` explicitly on every call, which always overrides
  this key default when present.
- **Client**: `client_kind` / `session_label` if present, so the user
  can tell this session's key apart from others they've granted.

If the user wants the full list of scopes that exist in this project
(not just the one currently in use), that's a different tool —
`mcp__vornik__list_scopes` — which has no dedicated slash command yet.
Call it directly if asked.
