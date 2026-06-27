---
workflowId: "issue-fix"
displayName: "Issue Fix"
description: "Top-level workflow for a labeled GitHub issue. A decompose step splits the issue's full scope into self-contained subtasks and emits them as delegatedTasks (SEQUENTIAL) — the engine schedules and runs each in order, merging to the project clone. On resume a reviewer checks the aggregate diff, then a system step opens a DRAFT PR. Reject / any subtask failure → FAILED, no PR. Writes no vornik-internal files."
version: "3.0.0"
resume_after_children: true
maxStepVisits: 4
maxIterations: 40
entrypoint: "decompose"
maxWallClock: "2h"
steps:
  decompose:
    type: "agent"
    role: "lead"
    on_success: "review"
    on_fail: "failed"
    timeout: "10m"
    # Deterministically run every delegated subtask under issue-subtask (a clean
    # coder-only workflow), regardless of whether the lead emits the per-task
    # `workflow` field. Without this they fall back to the project default
    # (dev-pipeline), which re-decomposes, tries agent-side git commit on the
    # read-only .git, and times out.
    delegated_workflow: "issue-subtask"
    prompt: |
      You are triaging a GitHub issue for an EXTERNAL customer repo. The issue
      (title + body) is your task input — it is the complete spec.

      Split the issue's FULL scope into the smallest sensible, SELF-CONTAINED
      subtasks and delegate them so the engine schedules each one deterministically.
      Emit a `delegatedTasks` array with `delegationMode: "SEQUENTIAL"`. Each entry:
        - "workflow": "issue-subtask"
        - "prompt": a COMPLETE, standalone instruction for ONE chunk — restate
          exactly what to implement and which test to add. The child does NOT see
          this issue or your reasoning, so the prompt must stand on its own.

      Rules:
        - If the issue asks for N of something ("test at least 5 functions"), emit
          N subtasks (one per function/item) — NOT one.
        - A genuinely single-step issue is ONE subtask; that's fine.
        - Subtasks run SEQUENTIALLY and each builds on the previous one's merged
          change, so order them sensibly.
        - Do NOT implement anything here, and do NOT write .autonomy/,
          CURRENT_TASK.md or BACKLOG.md. Your ONLY output is the delegatedTasks plan.
  review:
    type: "agent"
    role: "reviewer"
    # NO on_success here — it would make the gates below dead code. The engine
    # evaluates an agent step's inline gates ONLY when on_success is empty
    # (workflow.go: `nextStepID := step.OnSuccess` short-circuits the gate
    # block). With on_success set, review would jump there unconditionally and
    # never publish. The gates decide routing; on_fail is the catch-all when the
    # reviewer emits no parseable review.approved (gate eval → "no condition
    # matched" → on_fail → failed, i.e. not-approved → no PR).
    on_fail: "failed"
    timeout: "15m"
    gates:
      - condition: "review.approved == true"
        target: "publish"
      - condition: "review.approved == false"
        target: "failed"
    prompt: |
      Every subtask has run and merged to the project branch. Review the AGGREGATE
      change against the GitHub issue in your task input. FIRST inspect the real
      diff (do not review from metadata):
        `git --no-pager diff origin/HEAD...HEAD`   # all subtask commits vs upstream
      (also `git --no-pager log --oneline origin/HEAD..HEAD`; if git is restricted,
      read the changed files with the file tools).
      Judge ONLY against the issue:
        - SCOPE: every item the issue asked for is delivered (e.g. all 5 functions,
          not 1).
        - Tests cover the change; the diff is relevant and minimal.
        - REJECT if scope is incomplete, the diff is empty/irrelevant, or it
          contains any vornik-internal file (.autonomy/, CURRENT_TASK.md,
          BACKLOG.md, COVERAGE_REPORT.md).
      Emit review = { approved: <bool>, summary, remaining: [...] }.
      Set approved=true ONLY if the FULL issue is satisfied with tests.
  publish:
    type: "system"
    handler: "forge.open_change_request"
    on_success: "complete"
    on_fail: "failed"
    timeout: "10m"
terminals:
  complete:
    status: "COMPLETED"
    message: "Issue fixed — subtasks done, reviewed, draft PR opened."
  failed:
    status: "FAILED"
    message: "Issue fix incomplete or failed review — no PR opened."
---

# Issue Fix (v3 — top-level, deterministic subtask scheduling)

The webhook routes a **labeled issue straight here** (no github-router hop — that
caused a self-routing loop when issue-fix was its own auto-route candidate).

1. **`decompose`** (lead) emits `delegatedTasks` (SEQUENTIAL), one self-contained
   subtask per scope item, each pinned to **`issue-subtask`**.
2. The **delegation engine** runs the serial chain; each subtask merges to the
   project clone before the next. Deterministic — the engine guarantees every
   subtask runs, not a prompt asking the coder to "do all 5".
3. On resume (a fresh worktree off the now-updated clone), **`review`** inspects
   the aggregate `origin/HEAD...HEAD` diff. The resume guard stops decompose from
   re-running / re-spawning.
4. **approved** → **`publish`** (`forge.open_change_request`) opens a **draft** PR;
   **rejected / any subtask failed** → `FAILED`, no PR.

See `https://docs.vornik.io`.
