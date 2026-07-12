---
workflowId: "research-subtask"
displayName: "Research Subtask"
description: "The leaf unit of deep-research: a researcher investigates ONE self-contained sub-question and writes its findings to the exact file named in the task prompt (under artifacts/out/deep-research/). Analogous to issue-subtask for code. Findings accumulate in the shared project workspace across the SEQUENTIAL chain — no git merge — so each subtask builds on the previous and the deep-research synthesize step reads them all. The prompt IS the sub-question; there is no decomposition or publishing here."
version: "1.0.0"
# 2026-07-11: leaf workflow for deep-research decomposition. A single
# researcher pass, bounded, that appends one findings file. Because the
# assistant project is non-git, the shared workspace is never reset between
# tasks (executor: resetWorkspace is a no-op when the snapshot ref is empty for
# a repo with no .git), so sequential subtasks' findings files persist for the
# synthesize step.
entrypoint: "research"
maxStepVisits: 2
maxIterations: 12
maxWallClock: "1h"
steps:
  research:
    type: "agent"
    role: "researcher"
    on_success: "done"
    on_fail: "failed"
    timeout: "45m"
    retry:
      on: ["container_non_zero_exit", "context_timeout"]
      max_attempts: 5
      backoff: "exponential"
      initial_delay: "30s"
    prompt: |
      Your task input is ONE self-contained research sub-question, and it names
      the EXACT findings file to write (e.g. `artifacts/out/deep-research/03-market-sizing.md`).

      Research that sub-question thoroughly using your tools. Then WRITE your
      findings to the exact file path named in the task input:
        - Create the `artifacts/out/deep-research/` directory if needed.
        - Write ONLY that one file. Do NOT overwrite, delete, or edit any other
          file in that directory — earlier subtasks' findings live there and the
          final synthesis depends on them.
        - Start the file with an H2 heading naming the sub-question, then your
          findings in clear prose, and a `### Sources` list of the URLs/citations
          you actually used (every material claim must trace to a source).
      If the workspace already contains earlier findings files under
      `artifacts/out/deep-research/`, you MAY read them for context/continuity,
      but stay in scope: answer ONLY your assigned sub-question.

      Do NOT publish anything, do NOT write a final report, and do NOT touch
      vornik-internal files (.autonomy/, CURRENT_TASK.md). Your ONLY output
      artifact is the single findings file.

      Respond with:
      `{"research":{"subquestion":"<the question>","findings_file":"<the path you wrote>","sources":N}}`
terminals:
  done:
    status: "COMPLETED"
    message: "Sub-question researched — findings written."
  failed:
    status: "FAILED"
    message: "Research subtask failed."
---

# Research Subtask (deep-research leaf)

One researcher pass on a single self-contained sub-question, writing one
findings file under `artifacts/out/deep-research/`. This is to `deep-research`
what `issue-subtask` is to `issue-fix`: the deterministic unit the delegation
engine schedules, one per sub-question, in a SEQUENTIAL chain.

Findings **accumulate in the shared project workspace** — the assistant project
is non-git, so the executor never resets the workspace between tasks, and each
subtask's file persists for the next subtask and for the final synthesis. The
subtask writes exactly the file named in its prompt and leaves the others
alone.
