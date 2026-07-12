---
workflowId: "deep-research"
displayName: "Deep Research → Report"
description: "Long-running decomposed research, mirroring issue-fix for code. A decompose step (lead) splits a complex research request into self-contained sub-questions and emits them as delegatedTasks (SEQUENTIAL, each pinned to research-subtask) — the delegation engine schedules and runs each as its own durable task, so a large investigation can span hours or DAYS and survives daemon restarts. Each subtask writes a findings file to the shared workspace (no git merge — findings accumulate as files); on resume a writer SYNTHESIZES all findings into one report, then a publisher shares it. For a simple question the lead emits ONE subtask and it behaves like plain research."
version: "1.0.0"
# 2026-07-11: gives the assistant a decomposed research path so complex,
# potentially multi-day research can be SCHEDULED rather than crammed into a
# single researcher container (which the tool-budget/timeout caps kill). Same
# shape as issue-fix: decompose → delegated SEQUENTIAL chain → resume →
# synthesize → publish. Accumulation is file-based (assistant is non-git, so the
# shared workspace is never reset between subtasks). maxWallClock is generous
# (multi-day) because the parent waits on a long serial chain; per-hour cost is
# still bounded by the project budget caps.
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
                { "workflow": "research-subtask", "prompt": "<self-contained sub-question + the exact findings file to write>" }
              ]
            }
        - `delegatedTasks` MUST be a non-empty array. If you cannot form a plan,
          say why in `message` — do NOT return an empty array.

      Each `delegatedTasks` entry:
        - "workflow": "research-subtask"
        - "prompt": a self-contained instruction for ONE sub-question — the child
          does NOT see this request or your reasoning, so state exactly what to
          investigate AND name the exact findings file it must write, numbered so
          the order is clear and files don't collide:
            `artifacts/out/deep-research/NN-<short-slug>.md`
          (NN = 01, 02, 03… in the order you list them). Example prompt:
            "Investigate the regulatory landscape for X in the EU (GDPR, AI Act).
             Write your findings with sources to
             artifacts/out/deep-research/02-eu-regulation.md."

      Rules:
        - Order sub-questions sensibly: later ones may build on earlier findings
          (the child shares the workspace and can read earlier files).
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
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
    prompt: |
      Every research subtask has run and written a findings file under
      `artifacts/out/deep-research/`. Your job is to SYNTHESIZE them into one
      coherent report — you do NOT do fresh research here.

      1. List `artifacts/out/deep-research/` and READ EVERY `.md` file in it
         (they are numbered 01, 02, …). Each is one sub-question's findings.
      2. Write a single, well-structured report to
         `artifacts/out/deliverable.md`: an executive summary, then a section
         per sub-question weaving the findings into a narrative, then a
         consolidated `## Sources` section merging the per-file citations
         (deduped). Do NOT drop any sub-question's material.
      3. If a findings file is missing or thin, note the gap explicitly in the
         report rather than inventing content.

      Do NOT publish here (the next step does) and do NOT touch vornik-internal
      files (.autonomy/, CURRENT_TASK.md).

      Respond with:
      `{"write":{"report_file":"artifacts/out/deliverable.md","sections":N,"sources":M}}`
  publish:
    type: "agent"
    role: "publisher"
    on_success: "done"
    on_fail: "failed"
    timeout: "15m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 3
      backoff: "exponential"
      initial_delay: "20s"
    prompt: |
      Read `artifacts/out/deliverable.md` — the synthesized deep-research report.
      Publish it as a shareable page with PageDrop (it is Markdown, so use
      `mcp__pagedrop__pagedrop_publish_doc`), give it a descriptive title, and
      return the resulting link so the user can open it. If publishing is not
      available, say so and return the report path instead.

      Respond with:
      `{"publish":{"url":"<page link or empty>","title":"<title>"}}`
terminals:
  done:
    status: "COMPLETED"
    message: "Deep research complete — sub-questions researched, synthesized, and published."
  failed:
    status: "FAILED"
    message: "Deep research incomplete — a subtask, synthesis, or publish step failed."
---

# Deep Research → Report (decomposed, mirrors issue-fix)

For research requests too large for a single researcher pass — a survey, a
multi-angle investigation, something you'd want to run for hours or days.

1. **`decompose`** (lead) splits the request into self-contained sub-questions
   and emits `delegatedTasks` (SEQUENTIAL), each pinned to **`research-subtask`**
   and each told the exact findings file to write
   (`artifacts/out/deep-research/NN-slug.md`).
2. The **delegation engine** runs the serial chain — each sub-question is its
   own durable, leased task, so the whole investigation can span **days** and
   survives daemon restarts; the parent waits in `WAITING_FOR_CHILDREN`.
   Findings **accumulate as files in the shared workspace** (the assistant is
   non-git, so the workspace is never reset between subtasks — no git merge).
3. On resume, **`synthesize`** (writer) reads every findings file and writes one
   consolidated report to `artifacts/out/deliverable.md`.
4. **`publish`** (publisher) renders it to a shareable PageDrop page and returns
   the link.

A simple request decomposes to ONE subtask and behaves like plain `research`;
the win is for large scopes, where decomposition keeps each container small and
the work resumable. Cost stays bounded by the project's budget caps even though
wall-clock is generous (`maxWallClock: 72h`).

This is to research what `issue-fix` is to code. See `issue-fix.md` and
`research-subtask.md`.
