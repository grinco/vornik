---
description: Find active/trusted vornik knowledge skills relevant to the current work
allowed-tools: mcp__vornik__skill_search
argument-hint: <topic / what you're trying to do>
---

# Search vornik knowledge skills

The user wants approved know-how relevant to: **$ARGUMENTS**

Call `mcp__vornik__skill_search`. Useful args:

- `query` — case-insensitive substring over name/description.
- `repo_scope` — omit for the key default; the search returns the current
  scope plus `*` plus (unless `strict_scope=true`) NULL-scoped skills.
- `domain` / `role` — narrow when the intent is clear.

Only `active`/`trusted` skills are returned (drafts are excluded). Show the
matches as a short list (`name — description`). If one clearly fits, offer to
pull its full body with `/skill-get`. If nothing comes back, say so; the
know-how may not be captured yet (offer `/skill-propose`).

If the response says `this key lacks skill_read`, surface it verbatim
(`vornikctl companion grant --skill-read`).
