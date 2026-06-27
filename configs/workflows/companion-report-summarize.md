---
workflowId: "companion-report-summarize"
displayName: "Companion: Report summarize"
description: "Distills a long input into an executive brief. Host LLM delegates when it has a verbose source (transcript, log, report) and only wants the signal."
version: "1.0.0"
entrypoint: "summarize"
maxStepVisits: 1
maxIterations: 6
maxWallClock: "15m"
cleanup_artifacts:
  - artifacts/out/summary.md
steps:
  summarize:
    type: "agent"
    role: "summarizer"
    on_success: "done"
    on_fail: "failed"
    timeout: "15m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Summarize failed"
---

# Companion: Report summarize

One-shot distiller. The host LLM passes an `input:` path (or
inline content) and optionally a `target_audience:` hint. The
summarizer produces a tight brief.

## Prompts

### summarize

Your input is EITHER inline content in the task payload OR an uploaded
file staged at `artifacts/in/<name>` — when `context.inputArtifacts` is
non-empty, read it there with `file_read` (do NOT use a bare/host path
or glob `/app/workspace`). Produce `artifacts/out/summary.md` per the
summarizer role's contract: 3-5 sentence brief, key-points bullet list
(≤7), caveats section.

If `target_audience:` is set ("operator", "engineer", "exec",
"end-user"), tune vocabulary and depth to match — operators want
file paths and command names; execs want outcomes and risks; etc.
