---
workflowId: "dev-pipeline"
displayName: "Development Pipeline"
description: "Full TDD development pipeline: analyst breaks the work into subtasks with pinned test_cases, coder writes tests-first, tester validates each pinned case, reviewer enforces tests-and-impl-in-same-commit."
version: "2.1.0"
# 2026-05-14: TDD rework. Analyst pins concrete test_cases per
# subtask; coder writes tests-first; tester validates each pinned
# case by id; reviewer enforces "tests AND impl in same commit".
# maxStepVisits 5 → 12 and maxIterations 20 → 50 because the
# test-first cycle adds at least one extra step visit per subtask
# (failing-run then passing-run) and the reviewer retry path is
# now more likely to be exercised when pinned cases don't match.
# maxWallClock kept at 2h — bigger ceilings tend to mask stuck
# agents the watchdog can't catch.
maxStepVisits: 12
maxIterations: 50
entrypoint: "analyze"
maxWallClock: "2h"
steps:
  analyze:
    type: "agent"
    role: "analyst"
    on_success: "implement"
    on_fail: "failed"
    timeout: "10m"
  implement:
    type: "agent"
    role: "coder"
    on_success: "test"
    # Hard error (not a gate-driven rework) → park the stuck subtask
    # and checkpoint instead of dead-ending the feature. See
    # https://docs.vornik.io §8.
    on_fail: "recover-checkpoint"
    timeout: "15m"
  test:
    type: "agent"
    role: "tester"
    # testing.passed==false is a gate (below), NOT on_fail — the normal
    # rework loop is unchanged. on_fail fires only on a hard tester
    # error (suite couldn't run, schema survived fallback, timeout).
    on_fail: "recover-checkpoint"
    timeout: "10m"
    gates:
      - condition: "testing.passed == true"
        target: "review"
      - condition: "testing.passed == false"
        target: "implement"
  review:
    type: "agent"
    role: "reviewer"
    # review.approved==false is a gate (below), NOT on_fail — rejections
    # still loop back to implement. on_fail fires only on a hard
    # reviewer error.
    on_fail: "recover-checkpoint"
    timeout: "10m"
    gates:
      - condition: "review.approved == true && review.all_done == true"
        target: "report"
      - condition: "review.approved == true && review.all_done == false"
        target: "checkpoint-report"
      - condition: "review.approved == false"
        target: "implement"
  report:
    type: "agent"
    role: "analyst"
    on_success: "complete"
    # Hard-fail on report error: a feature without its CHANGELOG and
    # patches is incomplete by contract — operators rely on these
    # artifacts for review and merge. Earlier behaviour quietly
    # marked the task COMPLETED and the operator only noticed days
    # later when they went looking for the patches.
    on_fail: "failed"
    timeout: "10m"
  checkpoint-report:
    type: "agent"
    role: "analyst"
    on_success: "checkpoint"
    # Hard-fail on checkpoint-report error: same reasoning as the
    # full report step. The subtask's commits already landed, but
    # without the partial-changelog the operator may not realise
    # work happened this session — and the next session's lead has
    # less context for the continuation. Better to surface the
    # failure loudly so it gets fixed.
    on_fail: "failed"
    timeout: "10m"
  # Graceful-checkpoint recovery: reached only via on_fail from the
  # per-subtask loop (implement/test/review) on a HARD error that
  # survived the executor's shape-retry and modelFallback layers. The
  # analyst reads context.recovery (failed_step + failure_reason),
  # marks the in-flight subtask [!] blocked in CURRENT_TASK.md, writes
  # a partial changelog, commits, and exits via the checkpoint
  # terminal — so the next autonomy tick resumes from the NEXT subtask
  # instead of the operator finding a dead FAILED task. Projects with
  # pedantic:true skip recovery population in the executor and fall
  # straight through to terminal failure. See
  # https://docs.vornik.io §8.
  recover-checkpoint:
    type: "agent"
    role: "analyst"
    on_success: "checkpoint"
    # The analyst itself errored while trying to park the subtask —
    # nothing left to do but fail.
    on_fail: "failed"
    timeout: "10m"
terminals:
  complete:
    status: "COMPLETED"
    message: "Feature implemented, tested, reviewed, and reported"
  # checkpoint terminal: status COMPLETED so the operator sees a
  # clean finish, but the message tells the operator (and any
  # autonomy/UI layer) that the FEATURE is partially done. The
  # lead's next autonomy tick reads CURRENT_TASK.md, sees unchecked
  # subtasks, and schedules a continuation task.
  checkpoint:
    status: "COMPLETED"
    # recovery: this COMPLETED terminal is reached via on_fail (through
    # recover-checkpoint) on a HARD step failure — by design, so the
    # autonomy loop resumes from the next subtask. recovery:true tells
    # the workflow_onfail_masking doctor check this is intentional, not a
    # failure masquerading as success.
    recovery: true
    message: "Subtask landed; feature has remaining subtasks — next autonomy tick or operator picks up CURRENT_TASK.md"
  failed:
    status: "FAILED"
    message: "Pipeline failed"
  cancelled:
    status: "CANCELLED"
    message: "Pipeline was cancelled"
---

# Development Pipeline

Test-driven feature pipeline: analyst breaks features into subtasks
with pinned `test_cases`; coder writes tests-first; tester validates
each pinned case by id; reviewer enforces "tests AND impl in same
commit". After each subtask the pipeline reviews and loops back for
the next one.

Design principle: each step does ONE small, focused thing.

## Prompts

### analyze

Explore the project directory (`project/`) to understand its structure.
If `project/.autonomy/PROJECT_CONTEXT.md` exists, read it for context.
Otherwise, explore directly: list files, read README, check git log.

Read `project/.autonomy/CURRENT_TASK.md` if it exists — it may contain
a feature that's already broken into subtasks with some completed.

Subtask markers: `[ ]` pending, `[x]` done, `[!]` blocked. A `[!]`
subtask was parked by a prior recover-checkpoint after a hard error;
treat it as **skip** — never select it as the next subtask, and never
flip it back to `[ ]` yourself. Only an unblocked `[ ]` subtask is
eligible work.

Your job:

1. If no `CURRENT_TASK.md` exists or all subtasks are done (every entry
   `[x]`), pick the next pending feature from the backlog and create
   `CURRENT_TASK.md`.

   **Termination guard:** if `CURRENT_TASK.md` exists and has NO `[ ]`
   subtasks left but one or more `[!]` blocked subtasks (the feature
   cannot progress), do NOT start a new feature and do NOT loop — mark
   the feature blocked in the backlog with the blocked subtasks' reasons
   and fail the step so the operator can unblock it. This prevents an
   infinite checkpoint→resume→re-block loop.
2. Break the feature into small, independent subtasks (each should be
   implementable in under 10 tool calls — a single file change, one
   function, one test, etc.)
3. For EACH subtask, pin a concrete `test_cases` block (this is the
   TDD contract — read the analyst role's `systemPrompt` for the
   schema and rules). Each pinned case must have `id`, `description`,
   `inputs`, `expected`, and `kind` (unit | integration | manual).
   Vague cases produce vague tests — be precise. Every acceptance
   criterion maps to at least one case; include negative/edge cases
   when the behaviour is non-trivial.
4. Write to `project/.autonomy/CURRENT_TASK.md`:
   - Feature name
   - Subtask checklist with `[ ]` / `[x]` markers
   - For each unchecked subtask: files to change, implementation
     note, and the pinned `test_cases` block.
5. Mark the feature as "in-progress" in the backlog and commit.
6. Assess the feature's overall complexity and include it as
   `complexity` in your response — one of `trivial` (a one-line
   change), `standard` (a normal feature — the default), `complex`
   (multi-file or multi-subtask), or `open_ended` (large/unbounded;
   expect deep iteration). This scales the coder/tester/reviewer
   tool-call budgets for every subtask of this feature. Do NOT
   inflate it to "be safe" — over-provisioning is capped and
   audited, and autonomy tasks are held to a tighter ceiling. Use
   `standard` when unsure.

If `CURRENT_TASK.md` exists with unchecked subtasks AND each unchecked
subtask already has a `test_cases` block, do NOT create a new feature —
just respond that work is ready to continue. If unchecked subtasks
LACK a `test_cases` block (older pre-TDD `CURRENT_TASK.md`), add the
missing blocks before continuing.

Respond with:
`{"analysis":{"feature":"<name>","subtask":"<next unchecked>","test_cases_pinned":N,"ready":true,"complexity":"standard"}}`

### implement

Read `project/.autonomy/CURRENT_TASK.md` for the feature spec and
subtask list. If `project/.autonomy/PROJECT_CONTEXT.md` exists, read
it for coding conventions.

Implement ONLY the next unchecked subtask — not the entire feature.
Look for the first `[ ]` item in the subtask checklist. Read the
pinned `test_cases` block for that subtask — these are the contract
you must satisfy.

TDD order (read the coder role's `systemPrompt` for full rules):

1. Write/extend tests so every pinned case (kind: unit | integration)
   is exercised by a real test that maps to its `id`. Document
   `kind: manual` cases in code/docs as the case prescribes.
2. Run the tests once and confirm they FAIL — proves the test is
   exercising the missing behaviour. Note the failure in your output.
3. Implement production code so each pinned case passes.
4. Re-run tests; commit tests AND implementation TOGETHER with a
   message naming the subtask and the pinned case ids covered.

After implementing:

1. Mark that subtask as `[x]` in `CURRENT_TASK.md`
2. Commit all changes with a descriptive message (single commit
   covers both tests and implementation)
3. Keep your changes small and focused — one subtask only

If this is a rework iteration (previous step result contains
`testing.cases[]` with `failed` or `missing` entries, or reviewer
feedback), fix the specific issues described — start by addressing
the failed/missing pinned cases by id.

Respond with:
`{"implementation":{"subtask":"<what you did>","files_changed":N,"committed":true,"cases_covered":["case_1","case_2"],"unimplemented_cases":[]}}`

### test

Read `project/.autonomy/CURRENT_TASK.md` — focus on the most recently
completed subtask AND its pinned `test_cases` block. The pinned cases
are the contract; you validate every one of them by `id`.
If `project/.autonomy/PROJECT_CONTEXT.md` exists, read it for test
framework details.

Run a focused verification:

1. Run the project's existing test suite (or the focused command the
   project conventions prescribe).
2. For EACH pinned case (every entry in the subtask's `test_cases`),
   locate the test that exercises it (by id reference, test name, or
   assertion content) and record its outcome under `testing.cases[]`
   with `status: passed | failed | missing | manual`. See the tester
   role's `systemPrompt` for the schema.
3. Set `testing.pinned_cases_validated=true` ONLY when every pinned
   case is `passed` or `manual`. Any `failed` or `missing` ⇒
   `testing.passed=false` (and `pinned_cases_validated=false`).
4. Check for obvious regressions in unrelated tests.

Do NOT invent your own substitute cases — if the pinned spec is wrong,
say so under `testing.summary` and fail the step.

Respond with a JSON object matching the tester schema:

```json
{"testing":{"passed":true,"pinned_cases_validated":true,
            "cases":[{"id":"case_1","status":"passed","evidence":"<test name or file:line>"}]}}
```

```json
{"testing":{"passed":false,"pinned_cases_validated":false,
            "failures":"<what failed>",
            "cases":[{"id":"case_2","status":"failed","evidence":"<excerpt>"}]}}
```

### review

Read `project/.autonomy/CURRENT_TASK.md` for the feature spec, the
pinned `test_cases` for the just-completed subtask, and subtask
progress. Check the latest git commits: run
`cd project && git log --oneline -5` and `cd project && git diff HEAD~1`
to see what changed.

Review ONLY the latest subtask implementation (not the entire feature).
Check: correctness, code quality, adherence to the subtask spec.

TDD enforcement (read the reviewer role's `systemPrompt` for full
rules):

- Cross-check that every pinned `test_case` for the subtask has an
  entry in the previous step's `testing.cases[]` with status `passed`
  or `manual`. Any `missing` or `failed` ⇒ reject.
- Verify the commit contains BOTH the test code AND the
  implementation — TDD requires they land together.
- If the pinned spec is wrong (case unimplementable, duplicate,
  etc.), reject with that diagnosis so the analyst can fix
  `CURRENT_TASK.md` on the retry.

If all subtasks in `CURRENT_TASK.md` are marked `[x]`, also mark the
feature as "done" in the backlog and commit.

CRITICAL: Respond with a JSON object.

- If approved AND all subtasks done:
  `{"review":{"approved":true,"all_done":true}}`
- If approved but more subtasks remain:
  `{"review":{"approved":true,"all_done":false}}`
- If changes needed:
  `{"review":{"approved":false,"all_done":false,"feedback":"<specific changes>"}}`

Note on `all_done:false`: this terminates the current task
cleanly (status COMPLETED, partial changelog written) and
records the unchecked subtasks in `CURRENT_TASK.md` so the
next autonomy tick or operator picks them up as a follow-up
task. This is the right shape for large features that span
multiple sessions — DO NOT loop within the current task
hoping to finish everything in one execution.

### report

Generate a release summary and patch set for the feature that was
just completed.

1. Read `project/.autonomy/CURRENT_TASK.md` for the feature name and
   subtask list.
2. Run: `cd project && git log --oneline --no-merges -20`
   Identify all commits that belong to this feature (since the last
   feature was completed).
3. Write a human-readable changelog to `artifacts/out/CHANGELOG.md`:
   - Feature name and one-line summary
   - What was implemented (bullet points per subtask)
   - Files changed (summary, not full list)
   - Any known limitations or follow-up work
4. Generate mailbox patches:
   Run: `cd project && git format-patch --output-directory /app/workspace/artifacts/out/ HEAD~N`
   where N is the number of commits for this feature.
   If unsure of the range, use the commits since the last tag or
   merge.
5. Clean up: delete `project/.autonomy/CURRENT_TASK.md` since the
   feature is done, and commit.

Respond with:
`{"analysis":{"changelog":"artifacts/out/CHANGELOG.md","patches":N,"feature_done":true}}`

The role here is `analyst`, whose `result.json` contract requires
a top-level `analysis` key — keep the field named `analysis`
even though this step's job is reporting. The fields inside
describe the report.

### checkpoint-report

Generate a checkpoint summary for the subtask that just landed.
The feature is NOT done — `CURRENT_TASK.md` still has unchecked
subtasks that a follow-up task will pick up.

1. Read `project/.autonomy/CURRENT_TASK.md` to identify the subtask
   just completed (the most recently flipped `[x]`) and the subtasks
   that remain (`[ ]` entries).
2. Run: `cd project && git log --oneline --no-merges -10`
   Identify the commits made during this session.
3. Write a partial-progress note to
   `artifacts/out/CHANGELOG-partial.md` with:
   - Feature name
   - Subtask just completed (what landed this session)
   - Commits in this session (hashes + messages)
   - Subtasks remaining (the `[ ]` entries from `CURRENT_TASK.md`)
   - Any blockers or context the next session should know
4. DO NOT delete `CURRENT_TASK.md`. DO NOT mark the feature
   "done" in the backlog. The unchecked subtasks remain in
   `CURRENT_TASK.md` so the next session resumes from the right
   spot. Commit only the partial changelog.

Respond with:
`{"analysis":{"changelog":"artifacts/out/CHANGELOG-partial.md","partial":true,"subtasks_remaining":N}}`

The role here is `analyst`, whose `result.json` contract
requires a top-level `analysis` key — keep the field named
`analysis` even though this step's job is a checkpoint
report. The fields inside describe the checkpoint.

### recover-checkpoint

A prior step (`implement`, `test`, or `review`) hit a HARD error and
the executor routed here to preserve progress instead of failing the
whole feature. Your input contains `context.recovery`:

```
context.recovery:
  failed_step:    "implement" | "test" | "review"
  failure_class:  "agent_error" | "tool_error" | ...
  failure_reason: "<error text, up to 1500 chars>"
```

Your ONE job is to park the stuck subtask cleanly and checkpoint — do
NOT retry the failed work, do NOT pick a new feature.

1. Read `project/.autonomy/CURRENT_TASK.md`. Identify the in-flight
   subtask from `context.recovery.failed_step`:
   - `failed_step == "implement"` → the FIRST `[ ]` unchecked subtask
     (implement only flips `[x]` on success, so the failure left it
     `[ ]`).
   - `failed_step == "test"` or `"review"` → the most-recently flipped
     `[x]` subtask. The implementation committed but verification
     errored, so it is NOT verified — re-flag it.
2. Mark that subtask `[!]` blocked in `CURRENT_TASK.md` with a one-line
   reason distilled from `failure_reason`, e.g.
   `[!] Subtask 3: add retry guard — blocked: tool_error: podman exec failed (...)`.
   Use `[!]` (not `[ ]` or `[x]`): it means "skip on the next tick; do
   not retry automatically".
3. Write a partial-progress note to `artifacts/out/CHANGELOG-partial.md`
   (same shape as the checkpoint-report step): feature name, subtasks
   completed this session, the blocked subtask + why, and the remaining
   `[ ]` subtasks.
4. DO NOT delete `CURRENT_TASK.md`. DO NOT mark the feature "done" in
   the backlog. Commit only `CURRENT_TASK.md` + the partial changelog.

Respond with:
`{"analysis":{"changelog":"artifacts/out/CHANGELOG-partial.md","blocked_subtask":"<name>","blocked_reason":"<class>","subtasks_remaining":N}}`

The role here is `analyst`, whose `result.json` contract requires a
top-level `analysis` key — keep the field named `analysis`.

## Error handling

Two distinct failure paths, deliberately kept separate:

- **Expected rework** (the common case) is gate-driven, NOT `on_fail`.
  `testing.passed == false` routes back to `implement`; reviewer
  rejections (`review.approved == false`) route back to `implement`.
  These fire on a *successful* step emission whose result says "not
  yet". `maxStepVisits=12` bounds the rework loop.

- **Hard errors** (timeout, broken build env, merge conflict, schema
  shape that survived the executor's shape-retry and modelFallback
  layers) hit `on_fail`. For the per-subtask loop (`implement`, `test`,
  `review`) `on_fail` routes to `recover-checkpoint`, which parks the
  stuck subtask (`[!]` blocked) and exits via the `checkpoint` terminal
  so the next autonomy tick resumes from the next subtask. `analyze`,
  `report`, and `checkpoint-report` keep `on_fail: "failed"` by
  design — a planning failure has no subtask to park, and a reporting
  failure must surface loudly because the work already landed.

Projects/workflows/tasks with `pedantic: true` skip recovery routing in
the executor: `on_fail` drops straight through to the terminal failure
target with no checkpoint, restoring the legacy hard-fail behaviour.
