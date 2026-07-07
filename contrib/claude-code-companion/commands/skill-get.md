---
description: Fetch one vornik knowledge skill's full body
allowed-tools: mcp__vornik__skill_get
argument-hint: <skill name or id>
---

# Get a vornik knowledge skill

The user wants the full body of: **$ARGUMENTS**

Call `mcp__vornik__skill_get` with either `id`, or `name` + `repo_scope`
(names are unique only within a scope, so a bare name resolves against the
key's scope). Render the body verbatim so the user can read or apply the
procedure. If not found, say so and offer `/skill-search`.

If the response says `this key lacks skill_read`, surface it verbatim.
