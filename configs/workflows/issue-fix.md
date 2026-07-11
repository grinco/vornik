---
workflowId: "issue-fix"
displayName: "Issue Fix"
description: "Top-level workflow for a labeled GitHub issue. A decompose step splits the issue's full scope into self-contained subtasks and emits them as delegatedTasks (SEQUENTIAL) — the engine schedules and runs each in order, merging to the project clone. On resume a tester ACTUALLY RUNS the aggregate test suite (gate testing.passed) before a reviewer checks the aggregate diff. On rejection/red tests it loops to a remediate step that delegates a surgical fix subtask, re-tests and re-reviews (max 2 rounds via per-step maxVisits); on green + approval a system step opens a DRAFT PR. Exhausted rounds / any subtask failure → FAILED, no PR. Writes no vornik-internal files."
version: "3.2.0"
resume_after_children: true
maxStepVisits: 4
maxIterations: 40
entrypoint: "decompose"
maxWallClock: "2h"
steps:
  decompose:
    type: "agent"
    role: "lead"
    on_success: "test"
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

      HOW TO EMIT (read this carefully — getting the channel wrong drops every
      subtask and fails the task):
        - Return your plan as a SINGLE JSON object that is your FINAL message.
        - Do NOT call file_write, and do NOT create or write .autonomy/result.json
          yourself — the engine captures the JSON object from your final message
          automatically. (Writing files here pollutes the repo and, when the
          write fails, silently loses your whole plan.)
        - The object has exactly two keys:
            {
              "delegationMode": "SEQUENTIAL",
              "delegatedTasks": [
                { "workflow": "issue-subtask", "prompt": "<self-contained instruction for ONE chunk>" }
              ]
            }
        - `delegatedTasks` MUST be a non-empty array. If you cannot form a plan,
          say why in `message` — do NOT return an empty array.

      Each `delegatedTasks` entry:
        - "workflow": "issue-subtask"
        - "prompt": a self-contained instruction for ONE chunk — the child does
          NOT see this issue or your reasoning, so state what to implement and
          which test to add. Keep it focused: if the issue references a spec/plan
          file already in the repo (e.g. `https://docs.vornik.io`), the child
          SHARES the workspace and CAN read it — cite the file + the relevant
          task/section and summarise the acceptance criteria instead of pasting
          the whole spec. Do NOT paste large file contents into the prompt.

      Rules:
        - If the issue asks for N of something ("test at least 5 functions"), emit
          N subtasks (one per function/item) — NOT one.
        - A genuinely single-step issue is ONE subtask; that's fine.
        - Subtasks run SEQUENTIALLY and each builds on the previous one's merged
          change, so order them sensibly.
        - Do NOT implement anything here and do NOT write any files (no .autonomy/,
          CURRENT_TASK.md, BACKLOG.md, or result.json). Your ONLY output is the
          delegatedTasks JSON object described above.
  test:
    type: "agent"
    role: "tester"
    # NO on_success — an agent step's inline gates are evaluated ONLY when
    # on_success is empty (workflow.go: `nextStepID := step.OnSuccess`
    # short-circuits the gate block). Setting it would make the gates dead code
    # and let a red suite reach review/publish — the exact 2026-07-09 incident
    # this step guards against (headmatch #36 → red PR #37). The gates decide
    # routing; on_fail is the hard-error catch-all (tester crashed / emitted no
    # parseable testing.passed → failed → no PR).
    on_fail: "failed"
    timeout: "15m"
    gates:
      - condition: "testing.passed == true"
        target: "review"
      # Red suite → hand it to the bounded remediation loop, same as a review
      # rejection. remediate loops back through THIS step, so a fix is always
      # re-tested before it can be reviewed/published.
      - condition: "testing.passed == false"
        target: "remediate"
    prompt: |
      Every subtask has run and merged to the project branch. Your ONE job is to
      RUN THE PROJECT'S TEST SUITE against the aggregate change and report the
      real result — do NOT judge the diff, do NOT infer pass/fail from the code.

      This is typically a Python project: `python`, `pip` and `pytest` are
      available. Run the project's actual test command (e.g. `pytest -q`, or the
      command the repo documents). Capture the summary line.

      Set testing = { passed: <bool>, summary: "<the pytest summary line>" }:
        - passed=true ONLY if the run is GREEN: zero failures, zero errors, and
          zero collection/import errors. `xfail`, `xpass`, `skip`, and
          `deselected` are ACCEPTABLE and do NOT make it red.
        - passed=false if ANY test FAILS or ERRORS, or the suite fails to collect
          (ImportError / ModuleNotFoundError / missing symbol the tests import).
          Tests that reference functions the implementation never added are a
          RED suite, NOT an intentional "xfail" — the two land together or the
          suite is red.
      ALSO set a top-level `message` with the failing test names / import errors
      and the summary line, so the remediation step can fix exactly those.
      Do NOT create or modify vornik-internal files (.autonomy/, CURRENT_TASK.md,
      BACKLOG.md, COVERAGE_REPORT.md).
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
    # maxVisits caps the review→remediate loopback: initial review + 2
    # re-reviews. Paired with remediate.maxVisits=2 (design
    # 2026-07-09-issue-fix-remediation-loopback). A rejected re-review beyond
    # this routes on the remediate cap, not here.
    maxVisits: 3
    gates:
      - condition: "review.approved == true"
        target: "publish"
      # Rejected → hand the findings to a remediation subtask (was: failed).
      # The loop is bounded by remediate.maxVisits; exhaustion → failed with
      # the final review attached.
      - condition: "review.approved == false"
        target: "remediate"
    prompt: |
      Every subtask has run and merged to the project branch. Review the AGGREGATE
      change against the GitHub issue in your task input. FIRST inspect the real
      diff (do not review from metadata):
        `git --no-pager diff origin/HEAD...HEAD`   # all subtask commits vs upstream
      (also `git --no-pager log --oneline origin/HEAD..HEAD`; if git is restricted,
      read the changed files with the file tools).
      The preceding `test` step already RAN the suite and it was GREEN
      (testing.passed==true is the only path here); its summary is in
      `context.previousStepResult`. Do NOT re-litigate pass/fail from the diff.
      Judge ONLY against the issue:
        - SCOPE: every item the issue asked for is delivered (e.g. all 5 functions,
          not 1).
        - Tests cover the change; the diff is relevant and minimal.
        - A test that references a function the diff never implements is NOT an
          acceptable "xfail" — the tests and their implementation must land
          together. REJECT if any required behaviour is tested but unimplemented.
        - REJECT if scope is incomplete, the diff is empty/irrelevant, or it
          contains any vornik-internal file (.autonomy/, CURRENT_TASK.md,
          BACKLOG.md, COVERAGE_REPORT.md).
      Emit review = { approved: <bool>, summary, remaining: [...] }.
      Set approved=true ONLY if the FULL issue is satisfied with tests.
      ALSO set a top-level `message` field (this is forwarded to the fixer if you
      reject): when approved=false, `message` MUST contain your concrete findings
      (what is broken/missing, with file:symbol references) AND the `remaining`
      items, specific enough to fix without re-reading your reasoning. When
      approved=true, a one-line summary is enough.
  remediate:
    type: "agent"
    role: "lead"
    # Deterministically run the fix under issue-subtask (clean coder-only
    # workflow), same as decompose — never fall back to the project default.
    delegated_workflow: "issue-subtask"
    # Re-test after the fix (was: review) — a remediation must pass the test
    # gate before it can be reviewed/published; it can never skip it.
    on_success: "test"
    on_fail: "failed"
    timeout: "10m"
    # 2 fix rounds. The 3rd entry (i.e. a 3rd rejection) trips this cap and
    # routes to on_fail=failed, carrying the final review. See design doc.
    maxVisits: 2
    prompt: |
      The reviewer REJECTED the current branch for the GitHub issue in your task
      input. Their findings are in `context.previousStepResult` (what is broken or
      missing, plus the remaining items).

      Emit a `delegatedTasks` array (delegationMode "SEQUENTIAL") with EXACTLY ONE
      entry that orders a SURGICAL fix of those findings:
        - "workflow": "issue-subtask"
        - "prompt": a self-contained instruction that RESTATES the reviewer's
          findings + remaining items and tells the coder to fix ONLY those, on the
          code ALREADY on the branch — do NOT re-implement from scratch, do NOT
          widen scope beyond the findings. Instruct it to re-run the relevant
          tests. The child shares the workspace and can read any repo file (cite
          paths rather than pasting large content).

      HOW TO EMIT: return the JSON object as your FINAL message — do NOT call
      file_write and do NOT write .autonomy/result.json. Emit exactly one
      delegatedTasks entry (never zero, never a re-decomposition of the whole
      issue). Do NOT implement anything here yourself.
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
3. On resume (a fresh worktree off the now-updated clone), **`test`** (tester)
   ACTUALLY RUNS the project's test suite on the aggregate change and gates on
   `testing.passed`. A red suite never reaches review/publish. The resume guard
   stops decompose from re-running / re-spawning.
4. **green** → **`review`** inspects the aggregate `origin/HEAD...HEAD` diff for
   scope/quality (it does not re-run tests — the suite is already green here).
5. **approved** → **`publish`** (`forge.open_change_request`) opens a **draft** PR.
6. **red tests OR review rejection** → **`remediate`** (lead) delegates ONE
   `issue-subtask` that surgically fixes the findings (forwarded via
   `context.previousStepResult`) on the current branch, then loops back through
   **`test`** (re-run the suite) → **`review`**. Bounded to **2 rounds** by
   per-step `maxVisits` (review 3 / remediate 2); the 3rd rejection trips
   remediate's cap → `FAILED` with the final review attached. **Any subtask
   failure** → `FAILED`, no PR.

The `test` gate is the regression guard for the 2026-07-09 "red PR" incident
(headmatch #36 → PR #37): the old path never ran the suite, and the diff-only
reviewer rationalised 28 hard failures as intentional "xfail" and published. See
`https://docs.vornik.io`.

See `https://docs.vornik.io` and
`https://docs.vornik.io`.
