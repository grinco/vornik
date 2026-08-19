---
workflowId: "companion-research-gather"
displayName: "Companion: Research gather"
description: "Gathers sourced information on a topic. Host LLM delegates when it needs context it doesn't already have, without spending its own tokens browsing."
version: "1.0.0"
author: "Vadim Grinco <vadim@grinco.eu>"
license: "Proprietary"
entrypoint: "gather"
maxStepVisits: 1
maxIterations: 15
maxWallClock: "30m"
cleanup_artifacts:
  - artifacts/out/findings.md
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
  gather:
    type: "agent"
    # Output contract (customer report 2026-08-03): the role schema permits a
    # declared refusal, so without this a step that writes nothing still
    # counted as success. At least one file matching this glob must be written
    # DURING this step or the step fails into on_fail. Filename-specific on
    # purpose — a wildcard would be satisfied by an upstream artifact re-staged
    # into artifacts/out/ while this step runs.
    require_output_glob: "artifacts/out/findings.md"
    role: "analyst"
    on_success: "done"
    on_fail: "failed"
    timeout: "30m"
terminals:
  done:
    status: "COMPLETED"
  failed:
    status: "FAILED"
    message: "Research gather failed"
---

# Companion: Research gather

One-shot research pass. The host LLM passes a `topic:` (often a
question phrased exactly as the user asked it) plus optional
`constraints:` (sources to prefer, date bounds, depth).

The analyst gathers from `memory_search` first, then proceeds to
file/repo reads. External fetch is intentionally not in the
default tool allowlist — operators who want web research can add
`web_fetch` per-project after weighing the egress and prompt-injection
posture; the swarm template ships without it.

## Prompts

### gather

Read the task payload's `topic:` and `constraints:`. Query
`memory_search` first for relevant prior context; expand from
there into local files / repo paths the topic references.

Produce `artifacts/out/findings.md` per the analyst role's
contract. Cite every non-obvious claim by URL or file path.
Flag anything you couldn't verify under a "Confidence" section
at the bottom rather than omitting it.
