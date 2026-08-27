---
workflowId: "research"
displayName: "Research and Write"
description: "Two-step research workflow: a researcher gathers information into research.md, then a writer turns it into a polished deliverable."
version: "1.1"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
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
# Scores this workflow's DECLARED output obligations — the
# require_output_glob entries above — as met/declared (2026-08-18).
# Chosen over pinned_case_validation because that kind needs the verifier to
# emit testing.cases[], which the local benchmark model managed 15% of the
# time against 100% for a 397B; "write the file you promised" is a contract a
# small model can actually keep. It measures delivery, NOT correctness: a
# workflow writing a valid but empty file scores 1.0. See
# https://docs.vornik.io
qualityScoring:
  kind: "contract_satisfaction"
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
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
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
    on_success: "done"
    # Writer errors also offer alternatives now (drop PDF, retry
    # with a different pandoc engine, ship Markdown only). Same
    # pedantic-mode opt-out applies.
    on_fail: "recover"
    timeout: "15m"
    # failures (51 total) and the clear self-resolution pattern.
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
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
Do NOT publish or call any `pagedrop`/publish tool — plain research never
publishes; publishing a page is the `research-and-publish` workflow's job.

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

Do NOT publish or call any `pagedrop`/publish tool — plain research never
publishes; publishing a page is the `research-and-publish` workflow's job.