---
workflowId: "issue-subtask"
displayName: "Issue Subtask"
description: "Implement ONE self-contained subtask of an issue fix plus its test. Used as the SEQUENTIAL child workflow that issue-fix's decompose step delegates to — each child merges its change to the project clone before the next subtask starts."
version: "1.0.0"
maxStepVisits: 2
maxIterations: 30
entrypoint: "implement"
maxWallClock: "30m"
steps:
  implement:
    type: "agent"
    role: "coder"
    on_success: "complete"
    on_fail: "failed"
    timeout: "20m"
    prompt: |
      Implement EXACTLY the subtask described in your task input — one focused
      change plus a test that covers it. Nothing more, nothing less.

      This is typically a Python project: `python`, `pip` and `pytest` are
      available; run the project's test command to verify your change passes
      before finishing. Keep the diff minimal.

      Do NOT create or modify vornik-internal files (.autonomy/, CURRENT_TASK.md,
      BACKLOG.md, COVERAGE_REPORT.md). Report what you changed and the test you added.
terminals:
  complete:
    status: "COMPLETED"
    message: "Subtask implemented and tested."
  failed:
    status: "FAILED"
    message: "Subtask could not be implemented."
---

# Issue Subtask

One chunk of an issue fix. `issue-fix`'s `decompose` step delegates a SEQUENTIAL
chain of these (via `delegatedTasks` with `workflow: issue-subtask`); the
executor commits + merges each subtask's worktree to the project clone before the
next runs, so subtasks build on one another. A subtask FAILED bubbles up: the
issue-fix parent routes to its fail terminal and no PR is opened.

See `https://docs.vornik.io`.
