---
workflowId: "github-review"
displayName: "GitHub PR Review"
description: "Deterministic-bracketed change-request review: the daemon fetches the diff (forge.fetch_diff), a reviewer agent writes the review prose + verdict, then forge.post_review submits it as a REAL forge review (APPROVE / REQUEST_CHANGES). No agent runs git or gh."
version: "1.0.0"
maxStepVisits: 4
maxIterations: 20
entrypoint: "fetch_diff"
maxWallClock: "30m"
steps:
  fetch_diff:
    type: "system"
    handler: "forge.fetch_diff"
    on_success: "review"
    on_fail: "failed"
    timeout: "5m"
  review:
    type: "agent"
    role: "reviewer"
    on_success: "post"
    on_fail: "failed"
    timeout: "15m"
    prompt: |
      The previous step provided the change request's COMPLETE unified diff
      (base branch → PR head, spanning EVERY commit in the PR) as your input.
      Review the ENTIRE diff — every changed file and addition — NOT just the
      most recent commit. Your working tree is checked out to the PR head only
      as CONTEXT for reading surrounding code; it is not the review scope (do
      NOT run git or gh, the daemon handles all git state, and do NOT treat HEAD
      or the latest commit as the thing under review — the scope is the whole
      diff). Review for correctness, test coverage, security, and adherence to
      the project's conventions. Write a concise review in prose: specific
      findings first, then a short overall assessment. Your message becomes the
      review comment verbatim. Do not invent file contents — read the actual
      files. End with your verdict: emit a structured object
      {"review":{"approved":true|false,"feedback":"...","summary":"..."}} —
      approved=true submits a real GitHub APPROVE, approved=false a
      REQUEST_CHANGES. Approve only when the change is correct, tested, and
      ready to merge.
  post:
    type: "system"
    handler: "forge.post_review"
    # Submit a REAL forge review state from the reviewer's verdict (APPROVE /
    # REQUEST_CHANGES) rather than a non-gating comment. Set false to revert to
    # comment-only posting (e.g. while branch protection requires a human gate).
    gating_reviews: true
    on_success: "complete"
    on_fail: "failed"
    timeout: "5m"
terminals:
  complete:
    status: "COMPLETED"
    message: "Change-request review posted."
  failed:
    status: "FAILED"
    message: "Change-request review failed."
---

# GitHub PR Review

Deterministic handling of an opened change request:

1. `forge.fetch_diff` (system) fetches the unified diff daemon-side and passes it
   to the reviewer as its input — the agent needs no forge CLI or network access.
2. `review` (reviewer agent) reads the diff and writes the review prose. This is
   the only LLM step; it touches no git state.
3. `forge.post_review` (system) posts the prose as a review via the project's
   forge provider, mapping the neutral review event onto the provider's API.
   With `gating_reviews: true` (set above) the reviewer's verdict is submitted as
   a real review state — `approved: true` → **APPROVE**, `approved: false` →
   **REQUEST_CHANGES** — so the automation can satisfy branch protection / trigger
   auto-merge instead of leaving a non-gating comment. Set `gating_reviews: false`
   (or omit it) to keep comment-only posting where a human approval is still
   required.

See `https://docs.vornik.io`.
