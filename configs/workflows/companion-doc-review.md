---
workflowId: "companion-doc-review"
displayName: "Companion: Doc review"
description: "Reviews documentation for freshness, clarity, link rot, and divergence from the code it describes. Host LLM delegates when it touches docs or before a release."
version: "1.0.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "review"
# Reviews the STAGED input artifact (the doc under review). Declaring this makes
# the companion delegate handler reject an artifact-less delegation at submit
# instead of failing opaquely at file_read(/app/input/task.json). Attach via the
# /upload skill or delegate inputArtifacts.
require_input_artifacts: true
maxStepVisits: 1
maxIterations: 10
maxWallClock: "20m"
cleanup_artifacts:
  - artifacts/out/review.md
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
  review:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/review.md"
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
