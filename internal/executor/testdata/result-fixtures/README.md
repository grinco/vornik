# result-fixtures: replay corpus for output-schema validation

This directory holds representative `result.json` payloads from
agent runs. `result_replay_test.go` walks every fixture, looks up
the named role's `outputSchema` in the live `configs/swarms/`
tree, and runs the full validation chain
(`validateRequiredOutputKeys` → `EvaluatePlausibility`) against
the payload — asserting the fixture's declared outcome.

Item 12 of `https://docs.vornik.io`.

## What this catches

Two regression classes:

1. **Schema evolved; previously-valid payloads now fail.** The
   model has been emitting a particular shape for months, you
   tighten the schema to add a new required field, the test
   suite passes (no role uses that field yet), and production
   silently breaks at the next agent run. The corpus catches
   this at PR review.

2. **Schema regressed; previously-invalid payloads now pass.**
   A refactor of the validator weakens an assertion; the only
   tests are unit tests against synthetic fixtures the validator
   itself produces. Production payloads with the shape the
   validator USED to reject now slip through. The corpus is the
   independent witness.

## Directory layout

```
internal/executor/testdata/result-fixtures/
  <swarm-id>/
    <role>__<scenario>.json
```

Naming the file by `<role>__<scenario>` makes test output
readable: `assistant-swarm/writer__success_with_summary.json`
points to its purpose immediately. Multiple scenarios per role
are encouraged.

## Fixture schema

Each `.json` file contains:

```json
{
  "swarm":   "<swarm id from configs/swarms/*.yaml>",
  "role":    "<role name within that swarm>",
  "payload": { /* the result.json the agent produced */ },
  "expect":  "pass" | "fail",
  "reason":  "<one-line human description>"
}
```

The `reason` field is purely documentation — it appears in test
failure output to remind the reviewer why the fixture exists.

## Adding fixtures

Start with one passing + one failing case per role. Grow the
corpus organically: every time a real production failure
surfaces a new shape worth pinning, capture it here.

A failure case should specify `expect: "fail"`; the test
asserts the validation chain produces at least one
missing-key OR plausibility-violation. The exact violation is
NOT pinned, so a future refactor that re-categorises the same
failure (e.g. "missing key" → "plausibility violation under
when=...") doesn't churn the corpus.

To add a fixture from a real production run, copy the
result.json blob from `executions.result` (or the agent
container's output dir) into a new file under the right swarm
+ role. Run `go test ./internal/executor/ -run
TestResultReplay -v` to confirm the harness recognises it.
