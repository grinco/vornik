---
sources:
    - path: internal/cli/eval.go
      sha256: e5c29284e2533d34894099a89b5e96b18e9f82721e470732a9ec9fb64318894a
    - path: internal/cli/eval_predicate.go
      sha256: b340f9f2a2127d975816fe86f781107c72f0610ce52b39c37e7b2b6784a1be08
---
# Evals: repeatable swarm tests

An **eval suite** is a set of tasks with pass/fail checks that you run on
demand — a regression test for a swarm's behaviour. You author the suite once,
then `vornikctl eval run` submits every case, waits for it to finish, checks
each result against your predicates, and prints a scoreboard. It exits non-zero
if any case fails, so you can wire it into CI.

## Running a suite

```bash
vornikctl eval run <swarm> --project <project>
```

The positional argument is the swarm id; it also names the default suite file
(`configs/evals/<swarm>.json`) and seeds a stable per-case idempotency key, so
re-running a suite re-submits the same cases rather than piling up duplicates.

| Flag | Default | Purpose |
|---|---|---|
| `-p, --project` | — | Project to submit the eval tasks to. Required unless the suite file sets `project_id`. |
| `--file` | `configs/evals/<swarm>.json` | Path to the suite file. |
| `--wait` | `true` | Poll each task until it reaches a terminal status. |
| `--timeout` | `30m` | Maximum time to wait for all cases. |
| `--json` | `false` | Emit the scoreboard as JSON instead of the text table. |

The text run exits non-zero when any case fails its predicate — that's the hook
for CI. The `--json` form always returns success and puts the outcome in its
payload, for tooling that parses results itself.

## Authoring a suite

A suite is a JSON file: a little shared context plus a list of cases.

```json
{
  "project_id": "my-project",
  "workflow_id": "adaptive",
  "cases": [
    {
      "name": "summarizes-context",
      "prompt": "scout: read the project context and summarize the current state.",
      "expect": { "outcome": "COMPLETED" }
    }
  ]
}
```

Suite fields:

- **`project_id`** — default project for every case (a `--project` flag
  overrides it).
- **`workflow_id`** — default workflow for every case (optional; a case can
  override it).
- **`cases`** — the list of evals (at least one required).

Each case:

- **`name`** — a label, shown on the scoreboard and used in the idempotency key.
- **`prompt`** — the task input.
- **`workflow_id`** — optional per-case override of the suite default.
- **`expect`** — the predicate (below). Omit it for a smoke test: an empty
  `expect` passes as long as the task reaches `COMPLETED`.

## Predicates

`expect` supports four checks. When you set more than one they are **AND-ed** —
every one must pass:

- **`outcome`** — the task's terminal status: `COMPLETED`, `FAILED`, or
  `CANCELLED` (case-insensitive). Great for negative tests ("this *should*
  fail"). On its own it's cheap — no result body is fetched.
- **`equals`** — the task's structured result must deep-equal this JSON value.
  Both sides are parsed before comparing, so key order and whitespace don't
  matter.
- **`contains`** — a top-level subset match: every key you list must be present
  in the result with a matching value. Use it to assert one field
  (`{"approved": false}`) without pinning the whole object.
- **`regex`** — a Go regular expression matched against the result body. The
  lowest-fidelity check — handy when a value varies (an id, a timestamp) but a
  substring should always appear.

## The scoreboard and regression diff

A run prints how many cases passed and a line per case:

```
Eval my-project on project my-project: 2/3 passed
  ✓ summarizes-context [COMPLETED] (task_…) — ok
  ✗ rejects-empty-diff [COMPLETED] (task_…) — expected approved=false
  ✓ small-edit [COMPLETED] (task_…) — ok
```

vornik remembers each run's per-case verdicts (under your local state
directory, e.g. `$HOME/.local/state/vornik/evals/<swarm>.json`) and diffs the
next run against it. The scoreboard then calls out:

- **Regressions** — cases that passed last time and fail now.
- **Recovered** — cases that failed last time and pass now.

Cases added or removed between runs aren't counted as either — only quality
changes on cases present both times. The first run on a fresh machine simply
has nothing to compare against.

## A worked example

This suite exercises all four predicate styles across three roles:

```json
{
  "project_id": "my-project",
  "workflow_id": "adaptive",
  "cases": [
    {
      "name": "read-context",
      "prompt": "scout: read the project context if present and summarize the current state. If it is missing, write a short missing-context report instead.",
      "expect": { "outcome": "COMPLETED" }
    },
    {
      "name": "small-edit",
      "prompt": "editor: make a tiny documentation-only update describing this eval run, then commit it.",
      "expect": { "outcome": "COMPLETED", "regex": "modified_files" }
    },
    {
      "name": "review-rejects-empty-diff",
      "prompt": "reviewer: review a change described as 'I made no changes'. Reject when there is no actual diff. Return {approved: false, feedback: ...}.",
      "expect": { "outcome": "COMPLETED", "contains": { "approved": false } }
    }
  ]
}
```

Save it as `configs/evals/my-swarm.json` and run `vornikctl eval run my-swarm
-p my-project`. Add `--json` and check the exit code (or the `regressed` array)
from CI to gate a deploy on the swarm still behaving.
