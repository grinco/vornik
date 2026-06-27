---
workflowId: "github-router"
displayName: "GitHub Router"
description: "Deterministic issue→change-request router. The forge_job is classified at webhook time; the entrypoint auto-delegates dev-pipeline (single candidate, no LLM), then on resume the forge.open_change_request system step pushes the branch and opens the PR/MR daemon-side. No agent runs git or gh."
version: "1.0.0"
resume_after_children: true
maxStepVisits: 6
maxIterations: 30
entrypoint: "intake"
maxWallClock: "1h"
steps:
  intake:
    type: "agent"
    role: "github-classifier"
    on_success: "publish"
    on_fail: "failed"
    timeout: "5m"
    prompt: |
      This step is the strict-adaptive route entrypoint. It is NOT executed by an
      LLM: with a single adaptiveCandidateWorkflows entry (dev-pipeline) the
      executor auto-delegates deterministically and pauses on the child. On
      resume (child merged) the workflow advances to the publish step.
  publish:
    type: "system"
    handler: "forge.open_change_request"
    on_success: "complete"
    on_fail: "failed"
    timeout: "10m"
terminals:
  complete:
    status: "COMPLETED"
    message: "Change request opened for the issue."
  failed:
    status: "FAILED"
    message: "GitHub issue handling failed."
---

# GitHub Router

Deterministic handling of a labeled issue → change request, with **no LLM in the
router** and **no agent touching git or `gh`**:

1. The webhook is classified at ingest (`forge_job` stamped on the task by the
   daemon's forge classifier).
2. `intake` is the strict-adaptive route entrypoint. With a single
   `adaptiveCandidateWorkflows` entry the executor auto-routes to `dev-pipeline`
   (no LLM call) and spawns it as a child, pausing the parent on
   `WAITING_FOR_CHILDREN`.
3. The `dev-pipeline` child implements the fix in an isolated worktree; the
   executor commits + merges it to the project clone.
4. On resume, the parent runs the `publish` system step
   (`forge.open_change_request`): it pushes the merged commit under a
   deterministic branch (`fix/issue-<n>` / `feat/issue-<n>`) and opens the
   change request — idempotently, so a retry never opens a duplicate.

See `https://docs.vornik.io`.
