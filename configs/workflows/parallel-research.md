---
workflowId: "parallel-research"
displayName: "Parallel Research → Synthesize"
description: "Declarative parallel fan-out example: a `parallel` step launches three fixed research legs concurrently (as PARALLEL delegated child tasks via the existing delegation engine), then a `synthesize` join step consolidates whichever legs succeeded. join_policy quorum:2 lets the workflow proceed with 2-of-3 legs, calling out any missing leg via inputArtifactsSummary rather than failing the whole run. Unlike deep-research (LLM-decided leg count via delegatedTasks), the legs here are declared statically and validated at load. See https://docs.vornik.io"
version: "1.1.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
# 2026-07-23 (LLD 2026-07-23-workflow-parallel-fanout): first consumer of the
# declarative `parallel` step type. Parallelism stays at the TASK level — each
# branch is its own PARALLEL child task — so the executor's single-threaded
# invariants are untouched. The parent pauses WAITING_FOR_CHILDREN and resumes
# at the join step once the join policy is satisfied.
resume_after_children: true
maxStepVisits: 4
maxIterations: 40
entrypoint: "research_fanout"
maxWallClock: "12h"
steps:
  research_fanout:
    type: "parallel"
    # 1..fan-out-limit statically-declared legs. Each becomes one PARALLEL
    # delegated child task; failed legs are simply absent from the fan-in.
    branches:
      - id: "market"
        role: "researcher"
        prompt: |
          Research current market conditions relevant to the task's subject.
          Write your findings to artifacts/out/ as a concise brief with sources.
      - id: "competitors"
        role: "researcher"
        prompt: |
          Research competitor positioning relevant to the task's subject.
          Write your findings to artifacts/out/ as a concise brief with sources.
      - id: "technical"
        role: "researcher"
        prompt: |
          Assess the technical feasibility relevant to the task's subject.
          Write your findings to artifacts/out/ as a concise brief with sources.
    # The non-parallel consumer step the parent resumes at once the join policy
    # is satisfied. A parallel step's ONLY forward edge is `join` — it sets no
    # on_success/on_fail/gates (a proceed-false join reuses the runtime
    # child-failure bubble-up: parent retry budget, then terminal FAILED).
    join: "synthesize"
    # Proceed once at least 2 of the 3 legs succeed. Evaluated only AFTER all
    # legs are terminal (no early short-circuit). all | quorum:<n> | best_effort.
    join_policy: "quorum:2"
  synthesize:
    type: "agent"
    role: "analyst"
    on_success: "publish"
    on_fail: "failed"
    timeout: "30m"
    # Stage the succeeded legs' output artifacts into artifacts/in/ and inject
    # inputArtifactsSummary (expected/staged/missing/empty). A leg that did not
    # succeed appears under `missing` — call it out, do not invent its content.
    stage_child_artifacts: true
    # Output contract (added 2026-07-28, T-1089): the deliverable must exist,
    # freshly written by THIS step, or the step fails loud instead of handing
    # publish nothing. Same hole deep-research fell into — an analyst that
    # reports "I could not write it" is a schema-VALID success, so without this
    # the chain completed with no report. TOP-LEVEL path (no `project/` prefix):
    # outputGlobSatisfied resolves it against the ephemeral workspaceDir that
    # persistArtifacts harvests from.
    require_output_glob: "artifacts/out/deliverable.md"
    prompt: |
      The parallel research legs have run. The executor has staged each
      SUCCEEDED leg's findings into `artifacts/in/` (one file per leg) and set
      `inputArtifactsSummary` in your task context.

      1. Read `inputArtifactsSummary`: it lists `expected` / `staged` legs plus
         `missing` (a leg that never succeeded) and `empty` (succeeded but
         produced nothing). You MUST explicitly call out every `missing` and
         `empty` leg as a gap — do NOT paper over them or invent their content.
      2. Read every `.md` file in `artifacts/in/`.
      3. Write one consolidated brief to `artifacts/out/deliverable.md`: an
         executive summary, a section per available leg, a `## Gaps` section
         naming any missing/empty leg, and a merged `## Sources` section.

      Respond with:
      `{"write":{"report_file":"artifacts/out/deliverable.md"}}`
  publish:
    type: "agent"
    role: "publisher"
    # Success routes through a GATE, never straight to `done` (T-1089).
    on_success: "confirm_published"
    on_fail: "failed"
    timeout: "15m"
    prompt: |
      Read `artifacts/out/deliverable.md` and publish it as a shareable page
      with PageDrop (`mcp__pagedrop__pagedrop_publish_doc`); return the link.

      If publishing genuinely fails, report `published.ok: false` with a
      `published.reason` saying why. Do NOT claim success you didn't achieve:
      the report file is preserved either way, and the task will be marked
      failed so the operator can retry just this step.

      Respond with:
      `{"published":{"ok":true,"url":"<page link>","title":"<title>"},"message":"<one line>"}`
  confirm_published:
    type: "gate"
    # T-1089: `published.ok: false` is a schema-VALID publisher result, so an
    # honest "I did not publish" is a step SUCCESS and on_success fires. Without
    # this gate the task reported COMPLETED having shared nothing. A gate rather
    # than a retry — re-running the publisher risks a double-publish, and a
    # declared failure is not a shape failure a different model would fix.
    # on_success is intentionally UNSET so a malformed result with no
    # `published.ok` key cannot fall through to `done`.
    gates:
      - condition: "published.ok == true"
        target: "done"
      - condition: "published.ok == false"
        target: "publish_failed"
    on_fail: "publish_failed"
terminals:
  done:
    status: "COMPLETED"
    message: "Parallel research complete — legs fanned out, join policy satisfied, synthesized, and published."
  failed:
    status: "FAILED"
    message: "Parallel research incomplete — the join policy was not satisfied, or synthesis/publish failed."
  publish_failed:
    status: "FAILED"
    message: "Parallel research synthesized the brief but could NOT publish it — the deliverable artifact is preserved on the task; fork the publish step to retry sharing it."
---

# Parallel Research → Synthesize (declarative fan-out)

A worked example of the declarative `parallel` step type. Use it when you know
the parallel legs up front and want them to run concurrently, then converge.

1. **`research_fanout`** (`type: parallel`) declares three fixed research legs.
   At runtime each branch becomes its own **PARALLEL delegated child task** via
   the existing delegation engine — so parallelism lives at the task level and
   the executor's single-threaded state machine is untouched. The parent pauses
   `WAITING_FOR_CHILDREN`.
2. Once **all** legs are terminal, the **join policy** (`quorum:2`) is
   evaluated: with ≥2 successes the parent resumes at the join step; otherwise
   it bubbles up through the parent's retry budget to a terminal failure. There
   is no early short-circuit — the policy is evaluated only after every leg
   terminates.
3. **`synthesize`** (the join step) declares `stage_child_artifacts`, so the
   executor stages the **succeeded** legs' findings into `artifacts/in/` and
   sets `inputArtifactsSummary` — a **failed leg shows up under `missing`**, not
   silently dropped. The analyst consolidates them, calling out any gaps.
4. **`publish`** renders the brief to a shareable page.

Contrast with `deep-research.md`, where an LLM `decompose` step decides the leg
count at runtime via `delegatedTasks`. Here the legs are **static and validated
at load** (branch ids unique, `join` resolves to a non-parallel step,
`join_policy` well-formed, cumulative fan-out within the limit). See
`https://docs.vornik.io`.
