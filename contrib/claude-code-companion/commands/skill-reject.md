---
description: Reject or retire a vornik knowledge skill
allowed-tools: mcp__vornik__skill_reject
argument-hint: <skill id> [reason]
---

# Reject / retire a vornik knowledge skill

The user wants to reject or retire skill: **$ARGUMENTS**

Call `mcp__vornik__skill_reject` with `id` (and an optional `reason`). This
retires the skill: terminal for a draft, or a revocation for an active/trusted
one — after which it no longer injects or surfaces in search.

If the response says `this key lacks skill_admin`, surface it verbatim
(`vornikctl companion grant --skill-admin`).
