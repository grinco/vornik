---
workflowId: "deep-research"
displayName: "Deep Research → Report"
description: "Long-running decomposed research, mirroring issue-fix for code. A decompose step (lead) splits a complex research request into self-contained sub-questions and emits them as delegatedTasks (SEQUENTIAL, each pinned to research-subtask) — the delegation engine schedules and runs each as its own durable task, so a large investigation can span hours or DAYS and survives daemon restarts. Each subtask writes its findings as a durable output artifact (stored per-task in the artifact store); on resume the writer's synthesize step declares stage_child_artifacts, so the executor deterministically stages every child's findings into artifacts/in/ (job-scoped by task lineage — never another job's), and the writer synthesizes them into one report before a publisher shares it. For a simple question the lead emits ONE subtask and it behaves like plain research."
version: "1.2.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
# 2026-07-11: gives the assistant a decomposed research path so complex,
# potentially multi-day research can be SCHEDULED rather than crammed into a
# single researcher container (which the tool-budget/timeout caps kill). Same
# shape as issue-fix: decompose → delegated SEQUENTIAL chain → resume →
# synthesize → publish.
# 2026-07-20 (LLD 2026-07-20-delegated-child-artifact-handoff): cross-task
# handoff goes through the DURABLE ARTIFACT STORE, not a shared filesystem.
# Each subtask writes its findings to TOP-LEVEL artifacts/out/ (ephemeral →
# harvested to the store, scoped to that child task). On resume, synthesize
# declares `stage_child_artifacts: true`; the executor gathers this job's
# delegated children's output artifacts (by ParentTaskID lineage) into
# synthesize's artifacts/in/ — isolated by construction, so a prior or
# concurrent deep-research job's findings can never leak in (fixes T-06b5).
# maxWallClock is generous (multi-day) because the parent waits on a long
# serial chain; per-hour cost is still bounded by the project budget caps.
resume_after_children: true
maxStepVisits: 4
maxIterations: 40
entrypoint: "decompose"
maxWallClock: "72h"
steps:
  decompose:
    type: "agent"
    role: "lead"
    on_success: "synthesize"
    on_fail: "failed"
    timeout: "15m"
    # Deterministically run every delegated subtask under research-subtask,
    # regardless of whether the lead emits the per-task `workflow` field.
    delegated_workflow: "research-subtask"
    prompt: |
      Your task input is a RESEARCH REQUEST. Split its full scope into the
      smallest sensible, SELF-CONTAINED research sub-questions (distinct angles,
      subtopics, or entities) and delegate them so the engine schedules each one
      deterministically as its own durable task. This lets a large investigation
      run over hours or days without any single container carrying all of it.

      HOW TO EMIT (getting the channel wrong drops every subtask and fails the
      task):
        - Return your plan as a SINGLE JSON object that is your FINAL message.
        - Do NOT call file_write and do NOT write .autonomy/result.json — the
          engine captures the JSON object from your final message automatically.
        - The object has exactly two keys:
            {
              "delegationMode": "SEQUENTIAL",
              "delegatedTasks": [
                { "workflow": "research-subtask", "prompt": "<self-contained sub-question>" }
              ]
            }
        - `delegatedTasks` MUST be a non-empty array. If you cannot form a plan,
          say why in `message` — do NOT return an empty array.

      Each `delegatedTasks` entry:
        - "workflow": "research-subtask"
        - "prompt": a self-contained instruction for ONE sub-question — the child
          does NOT see this request or your reasoning, so state exactly what to
          investigate. The child writes its own findings file automatically;
          you do NOT assign filenames or numbering. Example prompt:
            "Investigate the regulatory landscape for X in the EU (GDPR, AI Act),
             with authoritative sources."

      Rules:
        - Each sub-question is INDEPENDENT — a subtask does NOT read other
          subtasks' findings (they run in isolated task workspaces; the
          synthesize step is what aggregates them). Order them logically for the
          reader, but do not make one depend on another's output.
        - A genuinely simple request is ONE subtask; that's fine.
        - Prefer more, smaller sub-questions over a few huge ones — small
          subtasks fit their container budget and make the days-long chain
          resumable at fine granularity.
        - Do NOT research or write anything here yourself. Your ONLY output is the
          delegatedTasks JSON object.
  synthesize:
    type: "agent"
    role: "writer"
    on_success: "publish"
    on_fail: "failed"
    timeout: "30m"
    # Stage this job's delegated research subtasks' output artifacts (their
    # findings files) into artifacts/in/. The executor does this
    # deterministically, scoped to THIS job's children by task lineage — never
    # another deep-research job's findings (LLD 2026-07-20-delegated-child-
    # artifact-handoff §3, fixes the T-06b5 cross-job leak).
    stage_child_artifacts: true
    # Stage ONLY the subtasks' findings files (T-1089). Without this every child
    # also contributes the executor's own `<step>-response-*.md` transcript, plus
    # another per shape retry: the incident run staged 26 entries for 10
    # subtasks — the findings AND verbose transcripts that largely duplicate
    # them. That roughly doubled the writer's input and helped exhaust its
    # prompt-token budget before it could write the deliverable. research-subtask
    # writes a fixed `artifacts/out/findings.md`, which the store harvests as
    # `findings-<date>-<short>.md`, so this glob is exact rather than heuristic.
    # A subtask whose files ALL fail the glob still shows up in
    # inputArtifactsSummary.empty[], so an over-narrow glob can't hide.
    stage_child_artifacts_include: "findings-*.md"
    # Output contract (2026-07-12, re-pathed 2026-07-28): the deliverable must
    # exist, freshly written by THIS step, or the step fails loud (one
    # corrective shape retry, then on_fail) instead of handing publish nothing.
    # The path is TOP-LEVEL (no `project/` prefix) since the 2026-07-20
    # re-platforming moved deliverables to the ephemeral per-execution
    # artifacts/out/ that persistArtifacts harvests.
    # T-1089 (2026-07-28) is the recurrence this restores: the re-platforming
    # dropped the contract line along with the old `project/`-prefixed path, so
    # a synthesize that wrote no file "succeeded", publish had nothing to
    # publish, and the task reported COMPLETED with no deliverable.
    require_output_glob: "artifacts/out/deliverable.md"
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
    prompt: |
      Every research subtask has run; the executor has staged each one's findings
      file into `artifacts/in/` (one file per sub-question). Your job is to
      SYNTHESIZE them into one coherent report — you do NOT do fresh research
      here, and you do NOT read `artifacts/out/` (that is for your own output).

      1. Read `inputArtifactsSummary` in your task context: it reports
         `expected` / `staged` sub-questions plus `missing` (a subtask that
         never completed) and `empty` (a subtask that completed but produced no
         findings). You MUST call out every `missing` and `empty` sub-question
         as an explicit gap in the report — do NOT paper over them.
      2. Read EVERY `.md` file in `artifacts/in/`. Each is one sub-question's
         findings.
      3. Write a single, well-structured report to
         `artifacts/out/deliverable.md`: an executive summary, then a section
         per sub-question weaving the findings into a narrative, then a
         consolidated `## Sources` section merging the per-file citations
         (deduped). Do NOT drop any sub-question's material, and do NOT invent
         content for a missing/empty one — note the gap instead.

      Do NOT publish here (the next step does) and do NOT touch vornik-internal
      files (.autonomy/, CURRENT_TASK.md).

      Respond with:
      `{"write":{"report_file":"artifacts/out/deliverable.md","sections":N,"sources":M}}`
  publish:
    type: "agent"
    role: "publisher"
    # Success routes through a GATE, never straight to `done` — see
    # confirm_published (T-1089).
    on_success: "confirm_published"
    on_fail: "failed"
    timeout: "15m"
    # Retry these classes. Attempts/backoff/delay are deliberately
    # OMITTED so they inherit the executor defaults: this block never
    # executed before 2026-08-27, so its former "max_attempts: 5" was
    # never in force (the real behaviour was infraRetryMaxAttempts=6).
    # Writing 5 here would be a silent retune disguised as a fix.
    retry:
      on: ["unclassified", "llm_call_failed", "container_start_failed", "container_wait_failed", "container_killed", "context_timeout"]
    prompt: |
      Read `artifacts/out/deliverable.md` — the synthesized deep-research report.
      Publish it as a shareable page with PageDrop (it is Markdown, so use
      `mcp__pagedrop__pagedrop_publish_doc`), give it a descriptive title, and
      return the resulting link so the user can open it.

      If publishing genuinely fails, report `published.ok: false` with a
      `published.reason` saying why. Do NOT claim success you didn't achieve:
      the report file itself is preserved either way, and the task will be
      marked failed so the operator can retry just this step.

      Respond with:
      `{"published":{"ok":true,"url":"<page link>","title":"<title>"},"message":"<one line>"}`
  confirm_published:
    type: "gate"
    # T-1089: the publisher's outputSchema deliberately treats
    # `published.ok: false` as a schema-VALID result (its plausibility rule only
    # requires a `reason` in that case), so an honest "I did not publish" is a
    # step SUCCESS and `on_success` fires. Routing publish.on_success straight
    # to `done` therefore reported COMPLETED for a task that shared nothing.
    # This gate is the only thing standing between a declared publish failure
    # and a COMPLETED terminal.
    #
    # Deliberately a gate, not a retry: re-running the publisher risks a
    # double-publish (see the publisher role's own prompt warning), and a
    # declared failure is not a shape failure a different model would fix.
    #
    # NOTE: on_success is intentionally UNSET. runGateStep treats "no condition
    # matched" + on_success as a clean default fall-through, so setting it would
    # let a malformed result carrying no `published.ok` key at all reach `done`
    # — reopening the exact hole. Everything that is not an explicit
    # ok == true lands on publish_failed via the second gate or on_fail.
    gates:
      - condition: "published.ok == true"
        target: "done"
      - condition: "published.ok == false"
        target: "publish_failed"
    on_fail: "publish_failed"
terminals:
  done:
    status: "COMPLETED"
    message: "Deep research complete — sub-questions researched, synthesized, and published."
  failed:
    status: "FAILED"
    message: "Deep research incomplete — a subtask, synthesis, or publish step failed."
  publish_failed:
    status: "FAILED"
    message: "Deep research synthesized the report but could NOT publish it — the deliverable artifact is preserved on the task; fork the publish step to retry sharing it."
---

# Deep Research → Report (decomposed, mirrors issue-fix)

For research requests too large for a single researcher pass — a survey, a
multi-angle investigation, something you'd want to run for hours or days.

1. **`decompose`** (lead) splits the request into self-contained sub-questions
   and emits `delegatedTasks` (SEQUENTIAL), each pinned to **`research-subtask`**.
2. The **delegation engine** runs the serial chain — each sub-question is its
   own durable, leased task, so the whole investigation can span **days** and
   survives daemon restarts; the parent waits in `WAITING_FOR_CHILDREN`. Each
   subtask writes its findings to **top-level `artifacts/out/`**, harvested into
   the **durable artifact store** scoped to that child task (not a shared
   filesystem).
3. On resume, **`synthesize`** (writer) declares `stage_child_artifacts`, so the
   executor deterministically stages **this job's** children's findings into
   `artifacts/in/` (job-scoped by task lineage — a prior/concurrent job's
   findings can never appear). The writer reads them plus `inputArtifactsSummary`
   (noting any `missing`/`empty` sub-question) and writes one consolidated report
   to `artifacts/out/deliverable.md`.
4. **`publish`** (publisher) renders it to a shareable PageDrop page and returns
   the link.

A simple request decomposes to ONE subtask and behaves like plain `research`;
the win is for large scopes, where decomposition keeps each container small and
the work resumable. Cost stays bounded by the project's budget caps even though
wall-clock is generous (`maxWallClock: 72h`).

This is to research what `issue-fix` is to code. See `issue-fix.md` and
`research-subtask.md`.
