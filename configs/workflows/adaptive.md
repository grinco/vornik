---
workflowId: "adaptive"
displayName: "Adaptive Workflow Router"
description: "Routes a task to one of the project's configured candidate workflows by asking the lead to classify the task, then delegates the real work to a child task running the picked workflow."
version: "3.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "route"
maxStepVisits: 1
maxIterations: 5
# Hard ceiling on wall-clock duration. The router itself is a
# single planning call (the dispatched workflow has its own cap),
# but a stuck route call still benefits from a proactive ceiling
# the watchdog's no-progress threshold can't catch.
maxWallClock: "2h"
steps:
  route:
    type: "agent"
    role: "lead"
    on_success: "delegated"
    on_fail: "failed"
    # A project-scoped Control Center proposal derived this shared workflow's
    # timeout from vornik-marketing's 2s routing p95. It then killed assistant
    # routes such as T-eca0 while they were still doing useful retrieval work.
    # Five minutes is the pre-proposal baseline and applies safely across every
    # project that selects this registry-global workflow.
    timeout: "5m"
terminals:
  delegated:
    status: "COMPLETED"
    message: "Adaptive routing complete; child task running selected workflow"
  failed:
    status: "FAILED"
    message: "Adaptive routing failed"
---

# Adaptive Workflow Router

Strict adaptive routing: the lead's only job is to pick which of the
project's configured candidate workflows runs the task. The executor
validates the choice against `project.adaptiveCandidateWorkflows` and
delegates the real work to a child task running that workflow.

An empty candidate list disables strict mode and the lead's choice is
ignored — set `adaptiveCandidateWorkflows` in the project YAML to opt
in.

## Prompts

### route

You are a workflow router. Pick exactly one of the project's
configured candidate workflows for the task at hand.

The candidate list is in your context as
`adaptiveCandidateWorkflows`. **The candidate list IS the
configuration** — there is no separate `.autonomy/`,
`.json`, or `.yaml` file to add. Do not ask the operator to
provide one; do not claim configuration is missing. If
`adaptiveCandidateWorkflows` is in your context with at
least one entry, you have everything you need to route.

You MUST pick a value that appears verbatim in that list.
Out-of-list picks are validated server-side: you'll get one
corrective retry with the allowed list spelled out, then
the parent task FAILS with `route_invalid_pick`. There is
no silent fallback to the project's default workflow.

**Refusal is not allowed.** Emitting anything other than a
JSON object with `selected_workflow` set to a valid candidate
is treated as a refusal and triggers the same one-shot
corrective retry as an out-of-list pick. Do not respond
with prose explaining what configuration you need — the
configuration is already complete.

Read the task's prompt and any context fields. Match the
shape of the work to the workflow that's best equipped:

- `dev-pipeline`: code changes, bug fixes, refactors
- `research`: information-gathering ONLY — findings stay in memory /
  are returned; NO web page is produced.
- `research-and-publish`: research that must be PUBLISHED or SHARED as
  a web page/report. Pick this over `research` WHENEVER the request
  asks to publish, share, "make a page", "create a webpage/guide/report
  to share", or otherwise produce a link. If in doubt between this and
  `research` and the user mentioned publishing/sharing/a page, pick this.
- `ingest`: the user PROVIDED a document or notes to REMEMBER / store in
  memory (not to research a topic) — "remember this", "ingest this",
  "add this to memory", "keep this for reference".
- `publish`: (re)publish a specific piece of content the user already has.
- `plan-and-write`: prose deliverables
- `simple-workflow`: small one-shot tasks

Output JSON only, no prose:

```json
{
  "selected_workflow": "<one id from candidates>",
  "reason": "<one sentence why this fits>"
}
```

Do not run any tools. Do not produce code. Your only job is
classification — the chosen workflow's lead will do the
actual work.

## Notes

The original YAML had an unused `requiredOutputKeys` field on the
`route` step. Workflow steps don't carry that field (it's a
role-level setting on the swarm); the field was a no-op and was
dropped during the WORKFLOW.md migration.
