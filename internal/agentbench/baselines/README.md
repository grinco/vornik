# Scored-arm baselines

A scored arm is only worth the hours it costs if a LATER release can be
measured against it. That needs two things this directory provides: the
artifacts have to still exist, and the axes a future arm must match have to be
written down rather than reconstructed.

## Why the artifacts live here and not in `agentbench-runs/`

`agentbench-runs/` is in `.gitignore` and nothing under it has ever been
tracked. Every gold pass this host has produced — 2026-08-21, 2026-08-23,
2026-08-28 — exists only as untracked local files, which is part of why the
2026.9.0 arm left nothing behind to compare against. A baseline that a `rm -rf`
or a rebuilt machine destroys is not a baseline.

A gold manifest is ~25 KB. The cost of keeping one is nil; the cost of losing
one is the pass that produced it.

## What must MATCH for a future arm to be comparable

The arm key hashes these. A future arm that differs on any of them is a
different measurement, and `bench agent rollup` will refuse to merge it —
correctly.

| axis | value for the 2026.9.4 baseline |
|---|---|
| task set | `dev-swarm-tasks-v1.json` @ `9b6fffe10fe0` (30 tasks) |
| gold manifest | `2026.9.4-rc/gold.json` in this directory |
| metric | `pinned_case_validation_score` |
| context policy | `tool_budget=enabled(0.25/0.5/1.0/2.0,max2.0);guidance=failure_playbooks+architect_priors+memory_hygiene+tool_budget+application_feedback+lift_eval` |
| observed model | `vllm/qwen3.8:27b` |
| swarm | `dev-swarm` |

## What may DIFFER

Declared in the pre-registration as `independentAxes`, because they are expected
to change between releases and changing them is the point of the comparison:

- `binary_sha256` — the daemon under test
- `agent_images` — the agent image, which is rebuilt per pass

## The trap that would silently break this

**The observed model string is recorded from the LEDGER, not from config.** It
is `vllm/qwen3.8:27b` because the arm reaches the model through LiteLLM. Going
back to the direct vLLM endpoint would record `Qwen/Qwen3.8-27B-FP8` for the
same physical model on the same hardware, and the two arms would not compare —
not because anything regressed, but because the transport changed the name.

If the transport is ever reverted, either keep the LiteLLM name or re-baseline
deliberately. Do not discover this at rollup.

The same applies to the reranker and to any role model: `stampObservedModels`
records what actually served, precisely so a router fallback cannot make two
different systems key alike.

## What the 2026.9.4 journals are, and what they are NOT

`2026.9.4-rc/journals/` holds the ten batch journals of the 2026-09-17 scored
arm, **re-scored under harness 6** (`bench agent rescore --tasks`). The
pre-rescore copies are deliberately not kept: they carry harness-5 task scores
and are incomparable with everything that follows.

**This is a harness shakedown, not a quality baseline, and the numbers must not
be quoted as one.** Across all 20 scored tasks the case evidence contains

    passed: 213 (credited)   manual: 12 (credited)   absent: 51   failed: 0

— not one case the agent's own verifier reported as FAILED. Every point of the
0.8414 mean that is missing from 1.000 was lost to case-id bookkeeping: the
verifier omitting ids it was given, or renumbering them. `dp-09-atomic-write`
is the extreme: 6 reported ids, zero of them matching any of the 15 pinned
ids, so it scores 0.000 with `diagnostic: unknown_case_id` while its 15 pinned
cases were never evaluated at all.

So the mean measures **whether the verifier reports the contract it was
handed**, and says nothing yet about code quality. A later arm that moves this
number has most likely changed §12.11.7's contract legibility, not the model.
Read `extraCaseCount` and the `absent` count before reading the score.

## Adding the next one

1. Run the arm with its own pre-registration naming both arms.
2. Copy `gold.json` and the arm journals into `baselines/<release>/` — the
   journals RE-SCORED under the current harness, never a mix of versions.
3. Add a row to `docs/public/benchmarks/results.md` stating what it may and may
   not be compared with.
