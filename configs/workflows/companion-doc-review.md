---
workflowId: "companion-doc-review"
displayName: "Companion: Doc review"
description: "Reviews documentation for freshness, clarity, link rot, and divergence from the code it describes. Host LLM delegates when it touches docs or before a release."
version: "1.0.0"
entrypoint: "review"
maxStepVisits: 1
maxIterations: 10
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
    message: "Doc review failed"
---

# Companion: Doc review

One-shot doc audit. The host LLM passes a `docs:` path (file or
directory) and optionally a `code:` path the docs are meant to
describe. The reviewer checks freshness (last-edit dates, stale
version refs), clarity (jargon, broken sentence structure), link
integrity (mark obvious dead URLs), and code-doc divergence.

## Prompts

### review

Read the doc set listed in the task payload via `read_many_files`.
If a `code:` reference path is supplied, also read that and check
for divergence (functions/flags named in docs that no longer
exist; behavior described that the code contradicts).

Produce `artifacts/out/review.md` with:

  - "Verdict" — one line: ship / fix-then-ship / rewrite.
  - "Freshness" — stale version pins, outdated screenshots,
    last-edit-vs-code-edit timestamp mismatches.
  - "Clarity" — passages a new reader would miss.
  - "Link rot" — URLs that look obviously broken (don't fetch;
    just flag patterns: localhost, deleted-org GitHub paths,
    deprecated domains).
  - "Code-doc divergence" — anchored on file:line in both sides.
