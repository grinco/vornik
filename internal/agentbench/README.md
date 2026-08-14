# agentbench — operator runbook

Scores the decisions Vornik's control logic makes: what the lead granted, whether
roles followed their output schemas, and whether agents called tools correctly.

The design and the full incident record are in
[https://docs.vornik.io](../../https://docs.vornik.io).
This file is the short operational path.

## Before the first run

A benchmark run submits **real tasks** to a **real daemon**. It therefore needs a
daemon of its own — not a project on the production one.

That is not a stylistic preference. The harness asks the daemon which database it
writes (companion `whoami`) and refuses on mismatch, so pointing production at a
differently-named project is refused, correctly. The check exists because on
2026-08-12 a run naming a throwaway database wrote twelve documents into a
production corpus and left the named one empty.

The bench instance needs to differ from production on **every** axis that could
carry state between them:

| Axis | Why |
|---|---|
| port, metrics port, unix socket | otherwise it will not start |
| database | the write-target check enforces it |
| data dir, workspaces, artifacts | shared state contaminates both |
| **`runtime.agent_llm.endpoint`** | pointed at production's port, every benchmark LLM call is recorded in **production's** ledger |
| **`mcp.servers`** | the inherited catalogue handed benchmark agents a **broker** and the operator's **home automation** |
| **`chat.router.model_fallbacks`** and `bedrock.enabled` | a fallback crosses providers silently: unbudgeted spend, and the arm's model set changes mid-run |
| **`configs/pricing.yaml`** | absent, every `cost_usd` records `0.0000` and real spend is reported as free |

The last four are the ones a copied config gets wrong. `scripts/agentbench-reproduce.sh`
refuses on each.

## Running

```sh
export VORNIK_URL=http://127.0.0.1:8090          # the BENCH daemon
export VORNIK_COMPANION_TOKEN=$(vornikctl companion grant -p agentbench --client claude-code --json | jq -r .secret)
export VORNIK_BENCH_DSN="host=localhost dbname=<bench-db> user=... password=... sslmode=disable"

# 1. Record ground truth (unrestricted ceiling, 3 runs per task).
scripts/agentbench-reproduce.sh --gold --runs 3

# 2. Review the gold set. It defines what "correct" means; the harness cannot
#    certify its own ground truth. Look for tasks excluded as "not measured".

# 3. Score an arm against it.
scripts/agentbench-reproduce.sh --arm baseline \
    --preregistration prereg.json \
    --context-policy "suppression=none;advert=gated" \
    --gold-manifest agentbench-runs/gold.json
```

`bench agent run` also works directly; the script exists to enforce the
preflight above, and every one of its refusals maps to a run that produced a
wrong number.

## Comparing releases

`bench agent compare` **refuses** to diff two runs whose arms disagree, naming
every differing axis. The key covers:

harness version · binary sha · config sha · context policy · task-set sha ·
gold sha · probe set · **observed models**

Models are read from the ledger after the run, not from config, because a router
fallback served `zai.glm-5` on Bedrock in place of `glm-5.2` on Ollama Cloud for
473k tokens with nothing declaring the arm had changed.

Pass `--daemon-binary` and `--daemon-config` or the key goes **PARTIAL** and says
so. A partial key means comparability is *unverified*, which is not the same as
verified-identical.

`HarnessVersion` (`arm.go`) must be bumped whenever a probe's **definition**
changes — not its implementation. It sat at `1` through four scoring changes in a
single day, each of which invalidated every prior figure.

## Reading the output

Three probes, reported as vectors and never blended:

| Probe | Needs gold? | Measures |
|---|---|---|
| `schema-following` | no | did each role produce output matching its schema, and at what retry cost |
| `tool-use` | no | real tool names, valid arguments, budget spent not exhausted |
| `tool-grant` | **yes** | did the lead grant what the task demonstrably needed |

The first two ground themselves in *configuration* (a declared output schema, a
tool's parameter schema), so they gate from day one without any gold pass.

Rules the output enforces rather than assumes:

- **$/successful task** carries failed-run spend in the numerator. Total spend
  over successes — not the mean cost of the runs that worked.
- **Success rate is broken out by class.** A blended rate absorbs a provider
  outage, a context overflow and an agent that could not do the work.
- **`context_overflow` is its own class**, not infra: it is the context policy
  under test failing at its job.
- **Harness failures leave the denominator.** A benchmark bug is not the system
  under test failing.
- **Request precision is a diagnostic, never a target.** It improves when the
  lead asks for less, so it is read only against escalation and stall rate.

## Known limits

- The gold set is **operator-reviewed**, and until it is, path coverage measures
  against unratified ground truth.
- σ was measured on a three-task subset; it must be re-measured on the full set
  before it gates anything.
- `cost_usd` has never been exercised against a non-zero value on this
  deployment — every model is Ollama Cloud or free-tier.
