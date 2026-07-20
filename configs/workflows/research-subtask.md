---
workflowId: "research-subtask"
displayName: "Research Subtask"
description: "The leaf unit of deep-research: a researcher investigates ONE self-contained sub-question and writes its findings to a top-level output artifact (artifacts/out/findings.md), harvested into the durable artifact store scoped to this task. Analogous to issue-subtask for code. Subtasks are INDEPENDENT — each runs in its own isolated task workspace and does NOT read sibling subtasks' findings; the deep-research synthesize step aggregates them all via the artifact store. The prompt IS the sub-question; there is no decomposition or publishing here."
version: "1.1.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
# 2026-07-11: leaf workflow for deep-research decomposition. A single
# researcher pass, bounded, that produces one findings file.
# 2026-07-20 (LLD 2026-07-20-delegated-child-artifact-handoff): findings are
# written to TOP-LEVEL artifacts/out/ (the ephemeral per-execution workspace),
# which persistArtifacts harvests into the durable artifact store keyed to this
# subtask's task id. The parent deep-research synthesize step then pulls this
# job's children's findings from the store into its artifacts/in/ (job-scoped
# by lineage). Subtasks no longer share a filesystem dir and no longer read
# each other — that shared-workspace path was the T-06b5 cross-job leak.
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
      Your task input is ONE self-contained research sub-question. Research it
      thoroughly using your tools, then write your findings to exactly this file:

        `artifacts/out/findings.md`

      (Write to that TOP-LEVEL path — NOT a subdirectory, and NOT under
      `project/`. The engine stores it as this subtask's durable output artifact
      and hands it to the final synthesis automatically; you do not name or
      number the file, and you do not read any other subtask's file — each
      subtask is independent.)

      The file must start with an H2 heading naming the sub-question, then your
      findings in clear prose, then a `### Sources` list of the URLs/citations
      you actually used (every material claim must trace to a source).

      Do NOT publish anything, do NOT write a final report, and do NOT touch
      vornik-internal files (.autonomy/, CURRENT_TASK.md). Your ONLY output
      artifact is the single findings file.

      Respond with:
      `{"research":{"subquestion":"<the question>","findings_file":"artifacts/out/findings.md","sources":N}}`
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
findings file to top-level `artifacts/out/findings.md`. This is to
`deep-research` what `issue-subtask` is to `issue-fix`: the deterministic unit
the delegation engine schedules, one per sub-question, in a SEQUENTIAL chain.

Findings are harvested into the **durable artifact store**, scoped to this
subtask's task id — not left in a shared filesystem. Each subtask is
**independent** (its own isolated task workspace; it does not read siblings);
the parent `deep-research` `synthesize` step pulls this job's children's
findings from the store into its `artifacts/in/` (job-scoped by task lineage),
so a prior or concurrent job's findings can never bleed in.
