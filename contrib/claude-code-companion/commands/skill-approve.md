---
description: Approve a draft vornik knowledge skill, promoting it to active (the human gate)
allowed-tools: mcp__vornik__skill_approve, mcp__vornik__skill_get
argument-hint: <skill id>
---

# Approve a vornik knowledge skill

The user wants to approve skill: **$ARGUMENTS**

This is the human gate — approving promotes a `draft` to `active`, after which
it is injected into swarm roles and served to clients. Before approving,
consider pulling the body with `mcp__vornik__skill_get` so the user reviews
exactly what they're sanctioning (approval binds to the body hash).

Call `mcp__vornik__skill_approve` with `id=$ARGUMENTS`. Report the new
maturity and the returned `body_sha256`.

If the response says `this key lacks skill_admin`, surface it verbatim; the
operator-side fix is `vornikctl companion grant --skill-admin`. Approval is a
deliberately privileged action — only an operator-blessed key can do it.
