---
swarmId: trading-swarm
displayName: 'Trading: research → risk → execute'
roles:
    # Strategist reads account state + market data + indicators
    # and emits a structured proposal of candidate trades. Does
    # NOT place orders. Output schema: {proposals: [...]}.
    - name: "strategist"
      model: "zai.glm-5"
      runtime:
        image: "localhost/vornik-agent:latest"
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
        image: "localhost/vornik-agent:latest"
      permissions:
        allowedTools:
            - "current_time"
            - "file_read"
            - "mcp__broker__get_account_summary"
            - "mcp__broker__get_positions"
            - "mcp__broker__get_orders"
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
        image: "localhost/vornik-agent:latest"
      permissions:
        allowedTools:
            - "current_time"
            - "mcp__broker__get_positions"
            - "mcp__broker__place_order"
            - "mcp__broker__cancel_order"
            - "mcp__broker__get_orders"
      requiredOutputKeys: ["placed", "fills_observed"]
---

# Trading: research → risk → execute
