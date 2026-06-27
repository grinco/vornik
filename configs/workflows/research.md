---
workflowId: "research"
displayName: "Research and Write"
description: "Two-step research workflow: a researcher gathers information into research.md, then a writer turns it into a polished deliverable."
version: "1.1"
entrypoint: "research"
maxStepVisits: 2
maxIterations: 10
# Hard ceiling on wall-clock duration. Two-step linear pipeline
# (research → write); 1h is generous for typical one-shot
# research while bounding a stuck scraper or runaway iteration.
maxWallClock: "1h"
# Defense-in-depth: wipe canonical artifacts at workflow start so a
# researcher that fails to overwrite (early tool error, exit before
# write) can't bleed prior-task content into the writer. The prompts
# already say "OVERWRITE the file"; this guarantees the file is gone.
cleanup_artifacts:
  - artifacts/out/research.md
  - artifacts/out/deliverable.md
  - artifacts/out/summary.txt
steps:
  research:
    type: "agent"
    role: "researcher"
    on_success: "write"
    # Swarm recovery (2026-05-18): on researcher failure (verifier
    # block, paywalled sources, agent error), route to the recover
    # step rather than fail the task outright. The lead reads
    # context.recovery, proposes 1-3 alternative approaches via a
    # decision checkpoint, and the operator picks one. Projects
    # with pedantic: true (e.g. ibkr-trader) skip this hop and
    # fall through to terminal failure as before. See
    # https://docs.vornik.io
    on_fail: "recover"
    # 45m base (raised from 30m, 2026-06-09): the researcher's iteration
    # budget grew (base 120 + dynamic tool-budget), so 30m no longer fit a
    # full-budget research run and the container hit the podman-wait
    # deadline. With dynamic tool_budget enabled this base is the COMPLEX
    # (1.0x) reference; smaller tasks scale it down, open_ended up. See
    # https://docs.vornik.io
    timeout: "45m"
    # Encode the observed recovery pattern where container_non_zero_exit
    # failures self-resolve on retry (confidence 0.61 in production). This
    # captures the implicit infrastructure retry behavior shown in telemetry.
    # Increase attempts from 3 to 5 to reduce residual failure volume while
    # maintaining fast failure detection.
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
  write:
    type: "agent"
    role: "writer"
    on_success: "done"
    # Writer errors also offer alternatives now (drop PDF, retry
    # with a different pandoc engine, ship Markdown only). Same
    # pedantic-mode opt-out applies.
    on_fail: "recover"
    timeout: "15m"
    # Explicit retry policy for container_non_zero_exit (confidence 0.57),
    # matching observed self-resolution pattern in write step.
    # Increase attempts from 3 to 5 given the high frequency of container_non_zero_exit
    # failures (51 total) and the clear self-resolution pattern.
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "15s"
  recover:
    # type:plan routes through executePlanStep which recognises the
    # lead's checkpoint outcome envelope and transitions the task
    # to AWAITING_INPUT (the lead-handoff path). type:agent would
    # parse result.json as a plain agent emission and miss the
    # checkpoint surfacing. The lead's recovery-mode systemPrompt
    # forbids emitting role steps from this hop — it MUST output
    # outcome=checkpoint kind=decision.
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
    message: "Research failed"
---

# Research and Write

Two-step workflow: gather information, then produce a polished
document. Use this with `assistant-swarm` for research tasks,
comparisons, and reports.

Use the `adaptive` workflow instead when the task is ambiguous or
might need a planner step.

## Prompts

### research

Gather comprehensive information on the topic in the task.
Write findings to `artifacts/out/research.md` with key facts, sources, and
caveats. Keep it concise enough for a smaller writer model to reuse.

### write

Read `artifacts/out/research.md`. Write a polished document to
`artifacts/out/<short-slug>.md` and a 2-3 sentence summary to
`artifacts/out/summary.txt`.

Follow the writer role's output contract — your response must
include the role's required `writing` and `produced_files`
keys plus a top-level `message` field carrying the 2-3
sentence summary (the UI and autonomy notifier read that
field). The role's systemPrompt has the full shape; don't
replace it with a `{message}`-only response.