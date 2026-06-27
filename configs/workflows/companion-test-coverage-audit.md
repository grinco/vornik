---
workflowId: "companion-test-coverage-audit"
displayName: "Companion: Test coverage audit"
description: "Audits test coverage on a set of touched files. Returns missing-test list, untested branches, and a coverage-delta summary the host LLM can act on."
version: "1.0.0"
entrypoint: "audit"
maxStepVisits: 1
maxIterations: 10
maxWallClock: "25m"
cleanup_artifacts:
  - artifacts/out/review.md
steps:
  audit:
    type: "agent"
    role: "reviewer"
    on_success: "done"
    on_fail: "failed"
    timeout: "25m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Coverage audit failed"
---

# Companion: Test coverage audit

One-shot audit of test coverage on changed files. The host LLM
passes a `files:` list (typically the diff's touched-file set);
the reviewer enumerates each file's symbols, locates existing
tests, and identifies gaps.

## Prompts

### audit

For each file in the task payload's `files:` list:

  1. Read the file via `file_read`. Enumerate exported and
     package-private symbols (functions, methods, types).
  2. Find existing tests via `grep` for the symbol name across
     the repo's test directories.
  3. For untested or weakly-tested symbols, record under "Gaps"
     with the file:line and a short note on what the test should
     cover (golden path + 1-2 edge cases).

Produce `artifacts/out/review.md` with:

  - "Coverage summary" — files audited, gaps found.
  - "Gaps" — symbol-by-symbol list.
  - "Recommended test order" — which gap to close first and why.
