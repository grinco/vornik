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
      (base branch → PR head) as your input. Your working tree is checked out
      to the PR head only as CONTEXT; the diff — spanning EVERY commit in the
      PR, base branch → PR head — is the review scope. Review the ENTIRE
      diff, not just the most recent commit. Do NOT run git or gh — the
      daemon handles all git state, and do NOT treat HEAD or the latest
      commit as the thing under review.

      Produce the review in this order:
      1. INTENT — reconstruct what the change is supposed to do from the PR
         title/body/linked issue. If you cannot state the intent in one
         sentence, that is itself a finding.
      2. DESIGN CHALLENGE — is this the right approach at all? Name at least
         one alternative and say why the PR's approach is or isn't better.
         Check fit with the project's existing architecture and conventions
         (read the surrounding code, do not guess).
      3. CORRECTNESS HUNT — walk EVERY file in the diff. Each finding must
         cite file:line and state the concrete failure scenario (inputs/state
         → wrong outcome). No finding without evidence; do not invent file
         contents — read the actual files.
      4. TEST ADEQUACY — not "tests exist": name the specific edge cases the
         tests miss. New behaviour without a test that fails before the
         change is a finding.
      5. SECURITY & ROBUSTNESS — input validation, secrets in code/config,
         injection surfaces, error paths, resource leaks.
      6. WHAT WOULD BREAK THIS — before any verdict, actively construct a
         breaking scenario and try it against the code you read. Approve only
         if the attempt fails, and say what you attempted.
      7. VERDICT — list findings by severity (blocker/major/minor/nit). If
         you found nothing, state explicitly what you checked and how.

      Pre-existing problems NOT introduced by this diff: record them with the
      backlog_deposit tool (if available) instead of blocking the PR — the
      verdict must be scoped to the diff.

      Your message becomes the review comment verbatim. End with the
      structured verdict {"review":{"approved":true|false,"feedback":"...",
      "summary":"..."}} — approved=true only with zero blocker/major findings
      AND a failed break attempt (step 6). approved=true submits a real
      GitHub APPROVE, approved=false a REQUEST_CHANGES.
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
2. `review` (reviewer agent) reads the diff and writes the review prose,
   following a hardened adversarial rubric (intent → design challenge →
   correctness hunt → test adequacy → security/robustness → an explicit
   "what would break this" attempt → severity-ranked verdict). This is the
   only LLM step; it touches no git state.
3. `forge.post_review` (system) posts the prose as a review via the project's
   forge provider, mapping the neutral review event onto the provider's API.
   With `gating_reviews: true` (set above) the reviewer's verdict is submitted as
   a real review state — `approved: true` → **APPROVE**, `approved: false` →
   **REQUEST_CHANGES** — so the automation can satisfy branch protection / trigger
   auto-merge instead of leaving a non-gating comment. Set `gating_reviews: false`
   (or omit it) to keep comment-only posting where a human approval is still
   required.

See `https://docs.vornik.io`.
