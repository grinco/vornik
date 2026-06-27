---
description: List your outstanding vornik delegations
allowed-tools: mcp__vornik__list
---

# Peek at outstanding vornik tasks

Call `mcp__vornik__list` to fetch every companion task this key has
delegated in the last 14 days. Render the result as a compact table with
columns: `task_id`, `status`, `workflow`, `created_at`.

If `$ARGUMENTS` is a known status (QUEUED, RUNNING, COMPLETED, FAILED,
CANCELLED), pass it as the `status` filter. Otherwise list everything.

After rendering, point the user at the next step:
- If anything is COMPLETED but they haven't pulled the result yet,
  suggest `/result <task_id>`.
- If everything is in flight, just say "all in flight; nothing to pull
  yet."
