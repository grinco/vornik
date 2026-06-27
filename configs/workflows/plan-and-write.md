---
workflowId: "plan-and-write"
displayName: "Research, Plan, and Write"
description: "Three-step linear pipeline (research → plan → write) for prose deliverables: gather material, draft a structured plan, then produce the final document."
version: "1.0"
entrypoint: "research"
maxStepVisits: 2
maxIterations: 15
# Hard ceiling on wall-clock duration. Three-step linear pipeline
# (research → plan → write); 1h is generous for typical one-shot
# research output while bounding a stuck scrape.
maxWallClock: "1h"
# Defense-in-depth: wipe canonical artifacts at workflow start so an
# upstream step (researcher / planner) that fails to overwrite can't
# leak prior-task content into the writer. Each step's prompt already
# says OVERWRITE; this is the executor-level fallback.
cleanup_artifacts:
  - artifacts/out/research.md
  - artifacts/out/plan.md
  - artifacts/out/deliverable.md
  - artifacts/out/summary.txt
steps:
  research:
    type: "agent"
    role: "researcher"
    on_success: "plan"
    # Recovery hop on failure (see research.md for the rationale).
    on_fail: "recover"
    timeout: "30m"
  plan:
    type: "agent"
    role: "planner"
    on_success: "write"
    # A failed planner output (missing plan.md, schema mismatch) is
    # exactly the kind of failure the lead can propose alternatives
    # for (downgrade to writer-direct, retry with corrective hint).
    on_fail: "recover"
    timeout: "20m"
  write:
    type: "agent"
    role: "writer"
    on_success: "done"
    # Writer errors (pandoc engine fault, format mismatch) are
    # routinely recoverable — fall back to Markdown, switch engine.
    on_fail: "recover"
    timeout: "15m"
  recover:
    # See research.md::recover for the type:plan rationale and
    # the lead-handoff path that surfaces checkpoint outcomes.
    type: "plan"
    role: "lead"
    on_success: "failed"
    on_fail: "failed"
    timeout: "5m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Plan-and-write failed"
---

# Research, Plan, and Write

Three-step workflow: gather information, create a structured plan or
itinerary, then produce a polished final document. Use this with
`assistant-swarm` for:

- Travel itineraries ("3-day trip to Lisbon in April")
- Project proposals ("mobile app proposal for a fitness startup")
- Event plans ("team offsite agenda for 20 people")
- Structured how-to guides

## Prompts

### research

Gather all information needed to produce the requested plan or itinerary.
Write findings to `artifacts/out/research.md`.

Focus on practical details: locations, times, costs, availability,
logistics, and anything that affects feasibility. Include sources.

### plan

Read `artifacts/out/research.md` and create a structured plan or itinerary
in `artifacts/out/plan.md`.

Be specific: include times, durations, logistics, costs, booking
requirements, and practical tips. Structure it so it can be followed
directly without needing to look anything else up.

### write

Read `artifacts/out/research.md` and `artifacts/out/plan.md`.
Produce the final polished document in `artifacts/out/<short-slug>.md`.
Write a 2-3 sentence summary to `artifacts/out/summary.txt`.

Follow the writer role's output contract — your response must
include the role's required `writing` and `produced_files`
keys plus a top-level `message` field carrying the 2-3
sentence summary (the UI and autonomy notifier read that
field). The role's systemPrompt has the full shape; don't
replace it with a `{message}`-only response.
