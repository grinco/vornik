---
description: List vornik knowledge skills for review (by maturity)
allowed-tools: mcp__vornik__skill_list
argument-hint: [maturity: draft|active|trusted|retired]
---

# List vornik knowledge skills

The user wants to review the project's knowledge skills: **$ARGUMENTS**

Call `mcp__vornik__skill_list`. Pass `maturity` when the user asks for a
specific state (e.g. `draft` to see what's awaiting approval). Optional
`repo_scope` / `domain` / `limit`. Render as a table:
`name | maturity | version | description`. When listing drafts, remind the
user they can approve with `/skill-approve <id>` (needs a `skill_admin` key).

If the response says `this key lacks skill_read`, surface it verbatim.
