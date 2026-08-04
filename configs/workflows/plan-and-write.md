---
workflowId: "plan-and-write"
displayName: "Research, Plan, and Write"
description: "Three-step linear pipeline (research → plan → write) for prose deliverables: gather material, draft a structured plan, then produce the final document."
version: "1.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
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
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/research.md"
    role: "researcher"
    # Outcome gates (CUSTOMER REPORT 2026-08-03). The role schema legitimately
    # permits `written: false` + a reason — the correct way for this role to
    # decline — and an unconditional on_success advanced that refusal all the way
    # to the COMPLETED terminal, so tasks finished with `artifacts: []`. A refusal
    # now routes to the lead's recovery hop instead. NOTE: gates and on_success
    # are mutually exclusive on an agent step (Validate rejects both, because the
    # executor short-circuits on on_success BEFORE evaluating gates), so the
    # transition lives entirely in these gates.
    gates:
      - condition: "research.written == true"
        target: "plan"
      - condition: "research.written == false"
        target: "recover"
    # Recovery hop on failure (see research.md for the rationale).
    on_fail: "recover"
    timeout: "30m"
  plan:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/plan.md"
    role: "planner"
    # Outcome gates (CUSTOMER REPORT 2026-08-03). The role schema legitimately
    # permits `written: false` + a reason — the correct way for this role to
    # decline — and an unconditional on_success advanced that refusal all the way
    # to the COMPLETED terminal, so tasks finished with `artifacts: []`. A refusal
    # now routes to the lead's recovery hop instead. NOTE: gates and on_success
    # are mutually exclusive on an agent step (Validate rejects both, because the
    # executor short-circuits on on_success BEFORE evaluating gates), so the
    # transition lives entirely in these gates.
    gates:
      - condition: "planning.written == true"
        target: "write"
      - condition: "planning.written == false"
        target: "recover"
    # A failed planner output (missing plan.md, schema mismatch) is
    # exactly the kind of failure the lead can propose alternatives
    # for (downgrade to writer-direct, retry with corrective hint).
    on_fail: "recover"
    timeout: "20m"
  write:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/summary.txt"
    role: "writer"
    # Outcome gates (CUSTOMER REPORT 2026-08-03). The role schema legitimately
    # permits `written: false` + a reason — the correct way for this role to
    # decline — and an unconditional on_success advanced that refusal all the way
    # to the COMPLETED terminal, so tasks finished with `artifacts: []`. A refusal
    # now routes to the lead's recovery hop instead. NOTE: gates and on_success
    # are mutually exclusive on an agent step (Validate rejects both, because the
    # executor short-circuits on on_success BEFORE evaluating gates), so the
    # transition lives entirely in these gates.
    gates:
      - condition: "writing.written == true"
        target: "done"
      - condition: "writing.written == false"
        target: "recover"
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
