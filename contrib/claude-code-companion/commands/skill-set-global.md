---
description: Set (or clear) a vornik knowledge skill's GLOBAL reach — a global skill injects into every project's roles
allowed-tools: mcp__vornik__skill_set_global, mcp__vornik__skill_get, mcp__vornik__skill_list
argument-hint: <skill id> [global|project]
---

# Set a vornik knowledge skill's cross-project reach

The user wants to change the reach of skill: **$ARGUMENTS**

A **global** skill injects into EVERY project's roles, not just its home
project — so a skill captured in this companion project can reach the janka
and assistant autonomy roles. A **project-only** skill injects only into its
home project. Changing reach does NOT change maturity: an approved skill stays
approved.

Parse `$ARGUMENTS` as `<skill id> [global|project]`. Default to **global** if
only an id is given (promotion is the common case). Then call
`mcp__vornik__skill_set_global` with `id=<the id>` and `global=true` (for
`global`) or `global=false` (for `project`).

Because a global skill fires everywhere, this is a deliberately privileged
action gated on `skill_admin`, and you can only flip a skill in THIS key's own
project. If the response says:

- `this key lacks skill_admin` — surface it verbatim; the operator-side fix is
  `vornikctl companion grant --skill-admin`.
- `skill not found in this project` — the id belongs to another project; you
  can only change reach on your own project's skills.

Report the returned `is_global` and the note (which states the new reach).
