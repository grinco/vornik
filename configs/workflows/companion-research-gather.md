---
workflowId: "companion-research-gather"
displayName: "Companion: Research gather"
description: "Gathers sourced information on a topic. Host LLM delegates when it needs context it doesn't already have, without spending its own tokens browsing."
version: "1.0.0"
entrypoint: "gather"
maxStepVisits: 1
maxIterations: 15
maxWallClock: "30m"
cleanup_artifacts:
  - artifacts/out/findings.md
steps:
  gather:
    type: "agent"
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
