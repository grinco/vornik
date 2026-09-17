---
swarmId: trading-swarm
displayName: 'Trading: research → risk → execute'
# STATUS (2026-09-17): NO project binds this swarm. It is the generic
# reference template that ibkr-trader-swarm was forked from, and it is loaded
# into the registry (it appears in `vornikctl swarm list`) purely because it
# sits in configs/swarms/. Editing it does NOT change live paper trading —
# `vornikctl project show ibkr-trader` resolves swarmId ibkr-trader-swarm, and
# workflows bind roles by NAME only (`role: strategist`), never by swarm.
#
# It is kept current anyway, because a swarm that is one `swarmId:` line away
# from serving real orders is not allowed to carry model bindings nobody has
# checked. The repo copy had drifted furthest — it still held the pre-2026-07-15
# primaries (zai.glm-5, minimax.minimax-m2.5) with NO fallbacks at all, so a
# fresh host would have deployed two-month-old routing.
#
# If you bind a project to this swarm, read ibkr-trader-swarm.md first: it
# carries the incident history (degenerate loops, the scorecard_floor failures,
# the GLM XML-wrapper quirk) that these role choices are derived from.
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
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 primary
      # (subscription capacity); Bedrock zai.glm-5 (proven prior
      # primary, pay-per-token) as fallback (previously no fallback).
      # 2026-09-17: GLM REMOVED FROM THIS ROLE ENTIRELY — primary
      # glm-5.2 → kimi-k2.6 (Ollama Cloud), fallback zai.glm-5 →
      # moonshotai.kimi-k2.5 (Bedrock). Not a currency refresh: the
      # role prompt below describes the deterministic scorecard/regime
      # floor, and ibkr-trader-swarm.md records (2026-07-25) that
      # glm-5.2 on this exact step deterministically proposed sub-floor
      # and unscored long opens, which the floor hard-rejected — turning
      # what should be a NO_ACTION tick into a FAILED one. The fallback
      # moved for the same reason: degrading into the one model known to
      # break this role is worse than having no fallback. Moonshot on
      # both legs keeps cross-PROVIDER resilience (Ollama → Bedrock) and
      # stays cross-vendor against a GLM risk-officer.
      model: "kimi-k2.6"
      modelFallback: "moonshotai.kimi-k2.5"
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
      # 2026-07-15: Bedrock→Ollama Cloud cutover — glm-5.2 primary
      # (subscription capacity); Bedrock zai.glm-5 (proven prior
      # primary, pay-per-token) as fallback (previously no fallback).
      # 2026-09-17: glm-5.2 → glm-5.3. Same published rate on
      # ollama.com (1.40/4.40 per 1M), newer generation. GLM is correct
      # HERE — the 07-25 finding is specific to the strategize step —
      # and keeps a cross-vendor second opinion against the Moonshot
      # strategist above.
      model: "glm-5.3"
      modelFallback: "zai.glm-5"
      # 2026-09-17: JSON mode + shape-retry hint ported from
      # ibkr-trader-swarm.md. This role was carrying a GLM binding with
      # NO structural guard, which is the trap this file existed to be:
      # ibkr's copy documents two GLM failure modes on exactly this role
      # — a collapse to empty `approved` arrays under shape-retry
      # (task_20260504204356), and GLM-5 appending XML wrapper tokens
      # (`</arg_value><arg_key>…`) after an otherwise clean approval.
      # Inheriting the model without the guard inherits the bugs.
      responseFormat: "json_object"
      shapeRetryHint: "Two rules. (1) Preserve approvals from your prior reasoning unless a cap gate fired. An empty `approved` array on retry is ONLY correct when proposals were already empty OR every proposal genuinely failed a cap. Do NOT abandon valid approvals as a safe default — that wastes the strategist's signal. (2) Each tool call AND your final response MUST be a single JSON envelope. Do NOT embed `<tool_call>`, `<arg_value>`, `<arg_key>`, `<parameter`, `<invoke`, or any other XML-wrapper tokens INSIDE the JSON. GLM-5 has been observed appending `</arg_value><arg_key>has_rejections</arg_key>...` after a clean approval — that pollutes the output and forces this retry. Emit the structured JSON cleanly with no XML markup before, after, or inside it."
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
      # 2026-07-15: Bedrock→Ollama Cloud cutover — minimax-m2.7 (newer
      # point release) primary; Bedrock minimax.minimax-m2.5 (proven
      # prior primary) as fallback (previously no fallback).
      # 2026-09-17: re-checked, deliberately UNCHANGED. minimax-m2.7 is
      # still current on Ollama Cloud and still the right tier for a
      # mechanical placement loop; minimax-m3 exists but costs 2x
      # (0.60/2.40 against 0.30/1.20 per ollama.com) for work that is
      # one order per iteration. Repriced in configs/pricing.yaml the
      # same day — the rate moved, the model did not.
      model: "minimax-m2.7"
      modelFallback: "minimax.minimax-m2.5"
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
