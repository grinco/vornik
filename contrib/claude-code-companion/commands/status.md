---
description: Check status of a specific vornik task
allowed-tools: mcp__vornik__status
argument-hint: <task_id>
---

# Status of a vornik task

The user wants the current state of task `$ARGUMENTS`.

Call `mcp__vornik__status` with `task_id=$ARGUMENTS`. Surface the result
in plain English: which workflow it's running, what state it's in, when
it was created, and any `last_error`.

If the status is COMPLETED / FAILED / CANCELLED, remind the user they
can pull the final artifacts with `/result $ARGUMENTS`.
