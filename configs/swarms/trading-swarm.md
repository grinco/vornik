---
swarmId: trading-swarm
displayName: 'Trading: research → risk → execute'
roles:
    # Strategist reads account state + market data + indicators
    # and emits a structured proposal of candidate trades. Does
    # NOT place orders. Output schema: {proposals: [...]}. Each
    # proposal: {symbol, intent: open|close, action: BUY|SELL,
    # qty, conviction, order_type, limit_price, stop_loss_price,
    # rationale}. OPTIONAL carry-through fields (dark by default —
    # omit unless the project has opted into the scorecard/regime
    # floor; exact names/casing required when present):
    # holding_state: held|flat, region: us|eu|apac,
    # scorecard: {total, trend, momentum, macro},
    # regime: {score, label: RISK_ON|NEUTRAL|RISK_OFF, stale,
    # component_count}. Not part of requiredOutputKeys —
    # optional even on trading projects.
    - name: "strategist"
      model: "zai.glm-5"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
      permissions:
        allowedTools:
            - "current_time"
            - "file_read"
            - "mcp__broker__get_account_summary"
            - "mcp__broker__get_positions"
            - "mcp__broker__get_quote"
            - "mcp__broker__get_historical_bars"
            - "mcp__ta__sma"
            - "mcp__ta__ema"
            - "mcp__ta__rsi"
            - "mcp__ta__macd"
            - "mcp__ta__bbands"
            - "mcp__ta__trix"
            - "mcp__ta__regime"
            - "mcp__ta__scorecard"
            - "memory_search"
      requiredOutputKeys: ["proposals"]
      plausibilityRules:
        # If the strategist proposes anything, every proposal needs
        # an explicit rationale tied to the indicator values.
        # Without this, the agent's reasoning trail is invisible
        # and the Phase 3 judge has nothing to ground against.
        - name: proposals_have_rationale
          when: {has_proposals: true}
          require: ["proposals"]
    # Risk officer reviews the strategist's proposals against
    # operator-set caps, current open positions, drawdown state.
    # Approves a subset with explicit sizing; rejects the rest
    # with reasons. Does NOT place orders.
    - name: "risk-officer"
      model: "zai.glm-5"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
      permissions:
        allowedTools:
            - "current_time"
            - "file_read"
            - "mcp__broker__get_account_summary"
            - "mcp__broker__get_positions"
            - "mcp__broker__get_orders"
            - "memory_search"
      requiredOutputKeys: ["approved", "rejected"]
      plausibilityRules:
        # Risk decisions must explain themselves — both approvals
        # (with sizing logic) and rejections (with the rule that
        # tripped). Otherwise the audit trail is "the LLM said no".
        - name: rejections_explained
          when: {has_rejections: true}
          require: ["rejected"]
    # Executor places the approved orders via the broker MCP.
    # One order per LLM iteration (no batched submits) so each
    # placement gets its own audit row + hallucination signals +
    # judge verdict. Cheaper model — the work is mechanical.
    - name: "executor"
      model: "minimax.minimax-m2.5"
      runtime:
        image: "ghcr.io/grinco/vornik-agent:latest"
      permissions:
        allowedTools:
            - "current_time"
            - "mcp__broker__get_positions"
            - "mcp__broker__place_order"
            - "mcp__broker__cancel_order"
            - "mcp__broker__get_orders"
            - "memory_search"
      requiredOutputKeys: ["placed", "fills_observed"]
---

# Trading: research → risk → execute

## Role prompts

### strategist

SCORECARD + REGIME CARRY-THROUGH (dark by default — only
matters once the project opts into `trading.scorecard.enabled`
and `trading.regime.enabled`; harmless to follow even when
those flags are off). For each candidate symbol call
mcp__ta__scorecard(bars, region) (region ∈ us|eu|apac by the
symbol's listing) and mcp__ta__regime(region) once per region;
carry the returned values VERBATIM into that proposal's
`scorecard` and `regime` objects, and set `holding_state`
(held if you hold it per get_positions, else flat) and
`region`. A deterministic code floor will REJECT opens that
are below the entry threshold, long into a RISK_OFF regime, on
stale data, or on an incomplete regime panel (when enabled) —
so don't propose them. The floor also refuses to close
(intent=close) a protected symbol.

### risk-officer

Review each proposal's carried scorecard/regime; you MAY
annotate or reject; NEVER approve an intent=close against a
project `protected_symbols` entry. The deterministic code
floor is authoritative; your review is advisory on top.
