# Replay fixture: `snake-plan-20260905`

The first real recording consumed by `test/agent/replay_test.go`
(llm-exchange record/replay design §5.4). Captured 2026-09-05 from this
deployment, not authored by hand.

| | |
|---|---|
| project | `snake` (opted in with `recording: {llm_exchanges: true}`) |
| task | `task_20260905111933_35f7a7d6b736abf8` |
| execution | `exec_20260905111933_7f3ca59855fd92da` |
| workflow / step / role | `simple-workflow` / `plan` / `lead` |
| model | `glm-5.2` |
| exchanges | 2 (iterations 1 and 2, both `finish_reason: stop`, no tool calls) |
| daemon / image | `2026.9.1-106-g844308ad` + the WORKSPACE-export image (270a8b78) |

The task prompt asked for a planning-only answer and forbade tool use, so the
step is two model calls and nothing else. The workspace is therefore empty
(`workspace/.gitkeep`): no tool read it in the recorded run, and the replay
proves the loop rebuilds the same two requests from `task.json` alone.

## Files

- `recording.jsonl` — `vornikctl execution exchanges <exec> plan --export`,
  unmodified. `redactions` is 0 on both lines; the export was grepped for
  key/token/password patterns before it was checked in.
- `task.json` — the executor's input for the step, copied out of
  `/tmp/vornik-exec-*/input/` while the container ran (the executor deletes
  that root when the step ends; nothing persists it).
- `mcp_catalog.json` — `podman exec <container> mcp-bridge discover`, run
  against the live lead container. Five `mcp__*` tools; the harness serves
  them through a stub `mcp-bridge` so the advertised tools array matches.
- `env.json` — the container's `VORNIK_*` variables minus secrets, URLs, ids
  and the per-run budget/cost values. `VORNIK_MAX_TOOL_ITERATIONS=20` is the
  one that matters: the lead role sets it, and the tool-budget sentence in the
  system prompt carries the number.
- `expected_result.json` — written by the **first replay** of this recording
  (`VORNIK_REPLAY_RECORD=1`), because the production run's `result.json` is
  not persisted. Cross-checked by hand: the production step completed, and
  the replay's usage totals equal the recording's usage — what the production
  model consumed (prompt 8674, completion 898, 2 iterations).

## How it was captured

This fixture was captured before the daemon persisted a step's boundary
files, so `task.json` was copied out of the executor's temp root by a poller
and `expected_result.json` was pinned by the first replay. That provenance
stays true of this fixture and is **not** re-captured.

1. Deploy a daemon and agent image that carry the recorder (migration 177).
2. Add `recording: {llm_exchanges: true}` to the project's deployed YAML.
3. Start a watcher that copies `/tmp/vornik-exec-*/input/task.json` as it
   appears and runs `podman exec <container> mcp-bridge discover` and
   `podman inspect --format '{{json .Config.Env}}'` once the container is up.
4. Submit the task; wait for it to complete.
5. `vornikctl execution exchanges <exec> <step> --export recording.jsonl`.
6. Assemble this directory, then
   `VORNIK_REPLAY_RECORD=1 go test ./test/agent/ -run TestEntrypointReplay`
   once to pin `expected_result.json`, and once more without the variable.

## How the next fixture is captured

Since the step-I/O persistence design (2026-09-05, migration 178) the two
boundary files are exports, not copies, and `expected_result.json` is the
production run's file:

1. Opt the project in and submit the task as above.
2. `vornikctl execution exchanges <exec> <step> --export recording.jsonl`
3. `vornikctl execution input <exec> <step> --export task.json`
4. `vornikctl execution result <exec> <step> --export expected_result.json`
5. `mcp_catalog.json` and `env.json` are still taken from the running
   container (`podman exec … mcp-bridge discover`, `podman inspect`) — neither
   is a file at the container boundary, so neither is persisted; a step whose
   role sees no MCP tools needs no catalog.
6. `go test ./test/agent/ -run TestEntrypointReplay` — no record pass: the
   expected result is already production's.


## Tool registry snapshot (2026-09-05 backlog fix)

The positive grep `head_limit` constraint deliberately changes the current schema.
`tool_registry.generated.sh` preserves the recorded schema environment, using the
entrypoint's existing `VORNIK_TOOL_REGISTRY` override. It is reconstructed
byte-for-byte from `41a15a8a:images/vornik-agent/tool_registry.generated.sh`, not
exported from the original container. The original request hashes and unchanged
expected result validate that this source snapshot reproduces the captured tools.
The harness still runs the current entrypoint/helper. Current tool declarations
are tested separately by the tool-definition golden matrix; this fixture is a
historical environment, not a claim about the latest schemas. No recording or
expected-result bytes were changed to accommodate the schema update.
