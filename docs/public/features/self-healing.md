---
sources:
    - path: internal/enterprise/blackbox/engine/detector.go
      sha256: 05d47800b4dc20c956b4c324e1829ee066a977692036fdace2741eddfb928e45
    - path: internal/workflowhealing/promoter.go
      sha256: d5051c869ee61b27d015e9d3111ffdf98b93e3383fbe309a0498eb6d0eea9c2e
---
# Self-healing workflow genome

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.


Workflows drift. A model changes, an upstream API gets slower, a prompt that
used to work starts failing — and the failure rate or cost of a workflow creeps
up. The self-healing genome is the loop that **notices the regression, proposes
a repair, proves the repair against the runs that actually failed, and asks you
to promote it.** Nothing it produces reaches production without an operator
clicking *promote*.

It is built on the [Autonomy Black Box](blackbox.md): the detector reads the
same telemetry, and the "prove it" step is a counterfactual replay over recorded
evidence. Everything here is admin-gated.

## The loop

```text
detect → generate candidate → trial (replay vs recorded failures) → scorecard → operator promotes
  ▲ automatic              ▲ operator-initiated, everything downstream is too
```

1. **Detect** — a background sweeper compares each workflow's recent behaviour
   to its own baseline and opens a *trigger* when it regresses.
2. **Generate a candidate** — *you* click "generate candidate" on a trigger.
   vornik tries deterministic repair recipes first, then falls back to the
   workflow architect.
3. **Trial** — run the candidate against the recorded failures, with all
   real-world side effects blocked.
4. **Scorecard** — a pure comparison gate scores the candidate against the
   baseline and marks it passed or failed.
5. **Promote** — *you* promote a passing candidate; only then is the workflow
   change written, validated, committed, and hot-reloaded.

The detector is the **only** automatic component, and it only writes triggers —
it never generates or promotes anything.

## Detection

The sweeper runs hourly. For every workflow with at least 10 runs in both
windows, it compares the **last 24 hours** against a **7-day baseline** and opens
a trigger when a metric regresses past its threshold:

| Trigger class | Fires when | Default threshold |
|---------------|-----------|-------------------|
| `failure_rate_spike` | failure rate rises | +25% relative |
| `cost_regression` | average cost per run rises | +40% relative |

Each trigger captures up to five **evidence executions** (failed first, then
most expensive) — these are the runs a repair has to prove itself against. Only
one trigger stays open per (project, workflow, class) at a time, so a persistent
regression doesn't spam the list.

Browse and triage triggers:

```bash
vornikctl blackbox trigger list --status open
vornikctl blackbox trigger dismiss <trigger_id>          # accept it / not actionable
vornikctl blackbox trigger generate-candidate <trigger_id>
```

You can tune thresholds or temporarily mute a noisy workflow per tuple:

```bash
vornikctl blackbox override set --project P --workflow W --class cost_regression \
    --threshold-pct 60 --mute-hours 48
```

## Candidate generation

When you ask a trigger to generate a candidate, vornik first tries **deterministic
recipes** — currently a retry-budget adjustment and a verifier-insertion step —
because a known-good structural fix is cheaper and more predictable than asking a
model. If no recipe applies, it falls back to the workflow architect, which
proposes a structural change from the trigger's evidence. If the architect
judges no change is warranted, that's reported plainly, not as an error.

Candidate generation is **always operator-initiated**. The detector never
proposes.

## Proving a repair: trials and the scorecard

A candidate is a proposed new version of the workflow. Before it can be promoted
it has to be *tried* — and there are two trial modes:

- **Static** — checks the candidate's shape and policy only. Useful as a quick
  sanity pass, but **not sufficient for promotion**.
- **Replay** — re-runs the trigger's recorded evidence executions through the
  candidate workflow, using the Black Box replay engine so every side-effecting
  tool is stubbed. This is the real test: does the candidate actually do better
  on the runs that failed?

The **scorecard** then compares the candidate's trial against the baseline. The
default gate requires the candidate to not regress on any axis:

- success rate must not drop,
- cost must stay within +10%,
- latency must stay within +10%,
- hallucination and verifier-failure counts must not increase.

The scorecard **only scores** — it never promotes.

## Promotion is always yours

You promote a passing candidate from the **Candidates** tab at
`/ui/admin/blackbox/candidates`, which shows the diff, the motivation, the
evidence links, and the scorecard. Promotion:

- refuses any candidate that hasn't reached a passing trial;
- **requires a replay-gated pass** — a static-only pass is deliberately not
  promotable;
- applies the change through the normal workflow path (write the workflow
  definition, validate it, commit it, hot-reload it), stamping who promoted it
  and when.

There is no background promotion loop anywhere in the system. If you never click
promote, nothing changes.

## Kill switches

| Control | Effect |
|---------|--------|
| `VORNIK_ARCHITECT_PAUSED` | pauses architect-based candidate generation (the detector keeps writing triggers; deterministic recipes still apply) |
| `VORNIK_ARCHITECT_DISABLED_KINDS` | refuses specific proposal kinds |
| `vornikctl blackbox override set … --mute-hours` | silences detection for one (project, workflow, class) tuple |

## Where to find it

| Surface | Location |
|---------|----------|
| CLI | `vornikctl blackbox trigger {list, dismiss, bulk-dismiss, generate-candidate}` and `vornikctl blackbox override {list, set, delete}` |
| Web UI | `/ui/admin/blackbox` — **Triggers**, **Candidates**, **Overrides** tabs |
| API | `/api/v1/admin/workflow-healing/*` (admin-scoped key required) |

Detection currently covers the failure-rate and cost regression classes; the
trace and replay foundation it builds on is described in the
[Autonomy Black Box](blackbox.md) page.
