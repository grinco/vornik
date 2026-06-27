---
workflowId: "companion-architectural-review"
displayName: "Companion: Architectural review"
description: "Reviews an attached document, diff, or PR for architectural issues. Host LLM clients (Claude Code etc.) delegate this when they want a second opinion without burning their own context. Attach the artifact under review as an input file — the reviewer reads the staged content, never project memory."
version: "1.1.0"
entrypoint: "review"
maxStepVisits: 1
maxIterations: 8
maxWallClock: "20m"
cleanup_artifacts:
  - artifacts/out/review.md
steps:
  review:
    type: "agent"
    role: "reviewer"
    on_success: "done"
    on_fail: "failed"
    timeout: "20m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Architectural review failed"
---

# Companion: Architectural review

One-shot reviewer pass. The host LLM client attaches the artifact
under review (a design doc, a diff, a PR description) as an input
file; the reviewer role reads that staged content and produces
`artifacts/out/review.md` with a verdict + findings + suggestions,
which the plugin returns to the host on its next `result(task_id)`
call.

## Prompts

### review

**Review the STAGED INPUT, not memory.** The artifact under review
is attached to this task as an input file — read it from the staged
inputs (the `inputFiles` / `inputExtractions` in your context,
materialised under `/app/input/uploads/`). Treat that file's content
as the single source of truth for what you are reviewing.

Do **NOT** review from `recall` / project memory. The RAG store can
hold several stale revisions of the same document, and reviewing
those yields contradictory, out-of-date findings (e.g. flagging a
"contradiction" that is just two ingested drafts, or a "missing"
piece that shipped after the snapshot). Use `recall` ONLY to
cross-reference *other* components the reviewed artifact depends on
— never as the source of the artifact itself. You also cannot see
the wider repo: do not assume `read_many_files`/`grep`/`glob` over
source paths will return anything, and never infer "drift from
shipped code" from absent files — say the code wasn't available
instead.

Fallbacks: for a diff or PR passed inline in the task payload
(`diff:`, `pr_url:`), review that text directly. If no input
artifact is staged and the payload carries no inline content, write
a review that says exactly that — "no reviewable content was
provided" — rather than reviewing memory chunks.

Produce `artifacts/out/review.md` per the reviewer role's contract.
Focus on architectural concerns over micro-style: coupling,
cohesion, separation of concerns, error boundaries, observability
hooks, surprise blast radius. Style-only nits go in a single
"Minor" bullet at the bottom — the host LLM already does style.
