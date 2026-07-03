---
workflowId: trading
displayName: 'Trading: strategy → risk → execute'
description: "Three-step trading workflow: strategist generates a candidate trade plan, risk-officer reviews it, executor places the order if approvals exist."
version: "1.0"
entrypoint: strategize
maxStepVisits: 1
maxIterations: 15
maxWallClock: 25m
steps:
    execute:
        type: agent
        role: executor
        on_success: done
        timeout: 4m
    maybe_execute:
        type: gate
        on_success: done
        gates:
            - condition: has_approvals == true
              target: execute
    review_risk:
        type: agent
        role: risk-officer
        on_success: maybe_execute
        timeout: 4m
    strategize:
        type: agent
        role: strategist
        on_success: review_risk
        timeout: 12m
terminals:
    done:
        status: COMPLETED
---

# Trading: strategy → risk → execute

An example three-step trading workflow. The **strategist** proposes at most one
candidate trade, the **risk-officer** reviews it against the account's limits,
a **gate** advances only when an approval exists, and the **executor** places
the approved order. It is a template — adapt the prompts, roles, and limits to
your own broker, instruments, and risk policy before relying on it.

## Prompts

### strategize

You are the **strategist**. Using the market-data and account tools available
to you, produce **at most one** candidate trade proposal for the configured
universe — or explicitly propose *no trade* when nothing meets your criteria.

- Base the proposal on current data; do not act on stale quotes.
- Emit a single structured proposal: instrument, side, quantity, entry style,
  a protective stop, and a short rationale. Keep it small enough to review.
- Do **not** place orders here — that is the executor's job after approval.

### review_risk

You are the **risk-officer**. Review the strategist's proposal against the
account's risk policy before it can reach the executor.

- Check position sizing, per-instrument and total exposure caps, available
  buying power, and correlation with existing positions.
- Confirm a protective stop is present and sane for the proposed side.
- Record an explicit **approve / reject / revise** decision. Only an approval
  lets the downstream gate advance to execution.

### execute

You are the **executor**. You run only when the gate confirms an approval
exists, so never place an order that was not approved.

- Place exactly the approved order (approved quantity and protective stop)
  using the broker tools.
- Confirm the resulting fill or working order and report the outcome.
- If the market data or account state needed to place the order safely is
  unavailable, abort and report rather than guess.
