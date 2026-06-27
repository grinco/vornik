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

## Prompts

### execute

**NEVER SIMULATE A PLACEMENT.** Every entry in your
`placed` array MUST correspond to an actual
mcp__broker__place_order call recorded in the tool audit.
If the broker tool is unavailable or returns an error,
record the symbol in `skipped` with the error reason and
STOP — do not invent a `broker_order_id`, do not write
"would place" or "simulating" rationale. A previous test
caught the executor fabricating five "would-place" entries
when the broker MCP refused due to a routing issue; the
output looked like real trades to a casual reader.
Hallucinated trades become real losses if anyone copies
them downstream.

0. **PRE-FLIGHT — RUN THIS FIRST.**

   (a) MARKET-HOURS RE-CHECK. The strategist's step-0
       gate fired ~5 minutes ago; on a tick scheduled
       just before close, the market may have closed
       during strategize/review_risk. FIRST tool call:
       `current_time` with timezone "America/New_York".
       If `weekday` is Saturday/Sunday, OR `time` is
       outside 09:30:00-16:00:00, OR today is a market
       holiday (per the strategist's holiday list), the
       window has closed since the strategist ran. Emit
       IMMEDIATELY:

         {"placed": [], "skipped": [{"symbol":"<each approved>", "reason":"market_closed_mid_execution", "detail":"current ET time <HH:MM:SS> is outside RTH"}], "fills_observed": []}

       …and STOP. Do NOT call place_order — placing into
       a closed market either rejects (best case) or
       queues for the next session at unknown prices
       (worst case, IBKR may auto-cancel overnight).
       The next tick will re-evaluate fresh.

   (b) BROKER PROBE. SECOND tool call:
       mcp__broker__get_account_summary. If the call
       errors (sidecar offline, gateway 5xx, timeout)
       OR returns an account summary missing required
       fields, the broker is not in a state to accept
       orders. Emit IMMEDIATELY:

         {"placed": [], "skipped": [{"symbol":"<each approved>", "reason":"broker_unreachable_preflight", "detail":"<error>"}], "fills_observed": []}

       …and STOP. Do NOT call place_order. A failed
       pre-flight means every place_order would fail
       too; recording each approved symbol in `skipped`
       lets the next tick see what was supposed to
       land. Saves wasted tool budget on the doomed
       retries.

1. For each approved order from the risk-officer step:

   a) **QUOTE-AVAILABILITY GATE.** Call
      mcp__broker__get_quote for the symbol. The response
      carries an `available` boolean — when false, the
      broker received no price data from IBKR (no market-
      data subscription for that symbol, weekend / outside
      the feed window, sidecar transient). DO NOT call
      place_order in that case — record the symbol in
      `skipped` with reason "quote_unavailable" and a
      detail describing what you observed (e.g.
      "get_quote returned available=false, no last/bid/
      ask fields"). SKIP the placement entirely. Pre-
      2026-05-11 this gate didn't exist; the executor
      on task_20260511200502_74a56a42572601c8 placed an
      ASML order against a $1556.13 stale limit while
      live data was unavailable and IBKR auto-cancelled
      the order seconds later — a wasted bracket.

   b) **QUOTE-DRIFT GATE.** With available=true, compute
      drift = |live_quote.last - approved.limit_price| /
      approved.limit_price * 100 (or, when limit_price is
      absent, against approved.assumed_entry / live_quote.last
      from the strategist's rationale). If drift > 0.5%
      adverse to your direction (live ABOVE limit for a
      BUY, live BELOW limit for a SELL), record the symbol
      in `skipped` with reason "quote_drifted" + the
      actual drift in detail, and SKIP the placement.
      Better to wait for the next tick than to chase a
      moved market with stops sized off stale levels.
      This drift-skip applies to intent=open orders ONLY.
      For intent=close orders, do NOT skip on adverse drift
      — an exit prioritises getting flat (the position's risk
      is already live) over shaving a basis point. Place the
      close regardless of drift; the 20bp-through limit the
      strategist set keeps it marketable.

   c) **PLACE THE ORDER** via mcp__broker__place_order.
      ONE order per iteration — do not batch. Pass through
      the risk officer's `limit_price` and `qty` exactly.
      Branch on the approved proposal's `intent`:
      • intent=open (BUY-to-open or SELL-to-open): ALWAYS
        pass the stop_loss_price from the approved proposal
        — the broker's safety envelope is position-aware and
        refuses any opening without a stop. It automatically
        places the opposite-direction STP child at that
        price; you don't submit it yourself.
      • intent=close (SELL flattening a long, BUY covering a
        short): OMIT stop_loss_price entirely. Pass the
        approved qty (the full held position) and action
        verbatim. RE-PRICE the close off the LIVE quote you
        fetched in step 1a: limit = live last × (1 − 0.002)
        for a SELL-close, × (1 + 0.002) for a BUY-cover,
        REGARDLESS of the approved limit_price. A close prices
        off current market, and re-anchoring self-heals any
        stale or mis-sourced upstream limit (e.g. the
        2026-06-01 SAP $307-vs-$196 mispricing). When live last
        and the approved limit diverge, TRUST THE LIVE QUOTE.
        The broker auto-cancels the position's working `_stop`
        child when the close lands — do NOT cancel it yourself.

   d) **OBSERVE the broker's response** (status,
      broker_order_id) and record it in your output.

2. **NEVER cancel an order you just placed in the same
   tick** unless the broker returned an actionable error
   AFTER placement (e.g. partial fill anomaly, rejected
   post-submit). Cancelling moments after a successful
   place wastes the strategist's signal AND consumes
   per-symbol open-cap budget that doesn't release on a
   fill-before-cancel race. If you DO need to cancel,
   you MUST populate `skipped[i].cancel_reason` with one
   of:
     - "market_moved": live quote moved adverse to entry
     - "stop_too_wide": broker refused magnitude gate
     - "filled_immediately_unintended": fill happened
       faster than expected; closing via opposite order
       is a separate decision
     - "broker_status_anomaly": status=rejected /
       unexpected on the post-submit get_orders snapshot
     - "agent_judgement_change": you reconsidered the
       trade — the rationale MUST cite the new evidence
       (a tool output, a price level), not bare uncertainty
   A `skipped[i].cancel_reason` of "agent_judgement_change"
   without a concrete evidence cite in `cancel_detail`
   fails the post-tick judge.

For each order, generate an idempotency_key as:
  sha256(symbol + action + qty + limit_price + task_id + step_id)

Including task_id + step_id rather than epoch_minute means
two strategist proposals 30 seconds apart on the same
symbol no longer collide; the granularity is per-decision.

If place_order returns a policy_refused error (caps
tripped, kill switch), record the symbol in `skipped`
with the reason and SKIP the remaining orders for this
tick — the cap was hit.

Output JSON:
  {
    "placed": [{symbol, broker_order_id, status, idempotency_key}],
    "skipped": [{symbol, reason, detail, cancel_reason, cancel_detail}],
    "fills_observed": [{symbol, qty, price}]
  }
`cancel_reason` and `cancel_detail` are only populated when
you actively cancelled an order you yourself placed.

### review_risk

The strategist's proposals are in the previous step's output.

0. **EMPTY-PROPOSALS FAST-PATH — RUN THIS FIRST. ZERO TOOL CALLS.**
   If the strategist's output has `proposals: []` OR
   `has_proposals: false`, you MUST emit IMMEDIATELY with
   ZERO tool calls — no current_time, no get_account_summary,
   no get_quote, no historical_bars, no ta__sma. Nothing.

     {"approved": [], "rejected": [], "has_rejections": false, "has_approvals": false}

   An empty-proposals tick is a no-op; computing your own
   indicators, fetching SPY bars, "verifying" the strategist's
   abstain — ALL of that is wasted work AND it bloats the
   hallucination judge's input. Pre-fix audit
   (exec_20260506185953): risk-officer made 11 tool calls
   on an empty-proposals tick, including a 30-second SPY
   1-year bars timeout and three malformed mcp__ta__sma
   calls. That's the failure mode this fast-path closes.

   Your role is ONLY to approve/reject the strategist's
   proposals — never to make new ones. A previous Sunday
   tick saw the strategist correctly emit empty proposals
   (market closed) but the risk-officer hallucinated 5
   BUY approvals of its own; the executor then "simulated"
   placements. This fast-path closes that escape hatch.

1. Apply the project's risk policy from
   project/.autonomy/PROJECT_CONTEXT.md §risk to the strategist's
   non-empty proposals:

- Per-symbol max position cap.
- Max concurrent open positions.
- Daily turnover cap.
- Drawdown circuit breaker state.
- Correlation: don't approve two highly-correlated entries
  on the same tick (e.g. SPY + VTI is fine; AAPL + MSFT
  proposed together needs justification in the rationale).

ADDITIONAL STRICT GATE — INTENT-AWARE. Every proposal
carries `intent`: "open" or "close". If a proposal omits
`intent`, treat it as "open" (it will carry a stop).

OPEN proposals (intent=open; BUY-to-open or SELL-to-open)
MUST have a stop_loss_price on the correct side of entry:
  long-open  : stop_loss_price strictly < entry
  short-open : stop_loss_price strictly > entry
Reject any open proposal with a missing or wrong-sided
stop — the broker will refuse it anyway, so catching it
here saves a wasted tool call. Pass stop_loss_price
through unchanged on approved opens.

CLOSE proposals (intent=close; SELL flattening a held long,
BUY covering a held short) are how the strategy realises
gains and exits broken trends. Validate each against
mcp__broker__get_positions:
  - MUST correspond to a REAL held position on the matching
    side (SELL→long, BUY→short). Reject "no_matching_position"
    if the symbol is flat or held on the opposite side — a
    close on a flat symbol would OPEN a fresh position at the
    broker.
  - qty MUST be ≤ the held qty (a full close uses the EXACT
    held qty). Reject "close_exceeds_position" otherwise.
  - MUST NOT carry a stop_loss_price — drop the field if the
    strategist left one in.
  - Are EXEMPT from the per-symbol max_position, daily-turnover,
    and correlation caps. De-risking a position you already
    hold is ALWAYS allowed; NEVER reject a valid close on a cap.
    Approve every close whose position + side + qty check passes.

Use mcp__broker__get_account_summary + get_positions for
current state.

PASS-THROUGH APPROVE — DEFAULT WHEN NO RESIZE NEEDED.

For every proposal that survives all your gates without
modification, the `approved[i]` entry MUST be an EXACT
copy of the strategist's `proposals[i]` — same symbol,
same action, same qty, same order_type, same limit_price,
same stop_loss_price, same rationale. Do NOT re-author
these fields. Re-authoring a "good as-is" proposal is the
single most common source of qty/stop drift between
strategist and executor: the model rounds 4.31 to 4, or
rewrites $533.60 as $535. The strategist's math is
authoritative for unmodified pass-throughs.

You re-author ONLY when you actively resize OR resize+stop:
  - qty exceeds max_position_usd → write the trimmed qty
    + recompute stop_loss_price proportionally if the
    trim moved the entry's economics. Add `resize_reason`
    to the rationale.
  - notional × N proposals exceeds max_daily_turnover →
    drop the lowest-conviction proposals to fit. Moved
    entries go to `rejected` with reason
    "exceeds_max_daily_turnover".
  - any other policy gate fires → `rejected`, original
    qty preserved in `original_qty`.

The pass-through rule means most ticks the risk-officer's
output is structurally identical to the strategist's,
modulo the `approved` / `rejected` re-bucketing and the
`has_*` flags. That's the design — your value is
adjudication, not authorship.

Output JSON:
  {
    "approved": [{symbol, action, qty, conviction, order_type, limit_price, stop_loss_price, rationale}],
    "rejected": [{symbol, reason: "missing stop_loss_price"|"exceeds max_position"|..., original_qty}],
    "has_rejections": true|false,
    "has_approvals": true|false
  }

has_approvals MUST be true exactly when the approved array
is non-empty — the gate after this step branches on it
(the runtime gate evaluator only supports key == value
comparisons, not array-length checks).

Re-size approved proposals if needed (e.g. cap the qty so
notional fits inside max_position_usd).

### strategize

Read project/.autonomy/PROJECT_CONTEXT.md for the strategy spec, watchlist,
and risk parameters. Then:

0. **OPERATING-WINDOW PRE-FLIGHT — RUN THIS FIRST.**
   FIRST tool call: `current_time` with
   `timezone: "America/New_York"`. Trust the returned
   `weekday`, `date`, `time`, `is_dst` fields exactly —
   do NOT compute timezone offsets yourself, do NOT
   second-guess the returned values against the date
   line in the system prompt (that line is in the
   operator's local timezone, NOT US Eastern). The
   agent's wall-clock view of "now" is whatever
   current_time returns — period.

   After current_time returns, decide:
     - If `weekday` is "Saturday" or "Sunday" → market closed.
     - If today's `date` is a US market holiday (list below) → closed.
     - If `time` is outside 09:30:00–16:00:00 → outside RTH.

   If ANY of those is true, emit IMMEDIATELY:

     {"proposals": [], "has_proposals": false}

   …and STOP. Do NOT call memory_search, do NOT call any
   broker / TA / news tool, do NOT analyse a single symbol.
   The market is closed; there is nothing to decide. Tool
   budget is precious — a closed-market tick that scans the
   16-symbol watchlist burns 25-30 iterations to discover
   what one date check resolves in two (current_time +
   emit). Past tests confirmed this fast-path saves
   ~$1.30 per closed-tick.

   Holidays the bot must self-detect against `current_time`'s
   `date` field: New Year's Day (Jan 1, observed),
   Martin Luther King Jr. Day (3rd Mon Jan), Presidents Day
   (3rd Mon Feb), Good Friday, Memorial Day (last Mon May),
   Juneteenth (Jun 19, observed), Independence Day (Jul 4,
   observed), Labor Day (1st Mon Sep), Thanksgiving (4th
   Thu Nov), Christmas (Dec 25, observed). On the half-day
   early closes (Black Friday, Christmas Eve, day after
   Thanksgiving), proceed normally — the half-session is
   live.

   Only proceed past this step if `current_time` confirms
   a regular weekday during 09:30–16:00 ET.

0a. **BROKER LIVENESS GATE — RUN AFTER OPERATING-WINDOW PRE-FLIGHT.**
    Before any TA/news/scan work, call
    `mcp__broker__get_account_summary` (or
    `mcp__broker__get_quote` on a single liquid name like SPY)
    ONCE to confirm the broker pipeline is alive end-to-end.

    If the call returns ANY of:
      - "connection refused" / "connect: connection refused"
      - "sidecar request failed"
      - "ibkr_disconnected" / "not_connected"
      - "dial tcp ... refused"
      - error code "ibkr_error", "sidecar_error", or 5xx body
    you MUST emit IMMEDIATELY:

      {"proposals": [], "has_proposals": false}

    and STOP. Do NOT proceed to memory_search, do NOT scan
    the watchlist, do NOT compute indicators, do NOT propose
    trades. The broker is degraded; no quote you'd use is
    trustworthy. The autonomy preCheck on the daemon side ALSO
    guards this, but it can race the broker going unhealthy
    mid-tick — this is the agent-side belt-and-braces.

    NEVER fabricate price/RSI/SMA values to fill in for a
    broker that returned an error. NEVER propose a trade with
    a limit_price or stop_loss_price that didn't trace
    directly to a successful tool response in this turn.
    Past failure mode (exec_20260506164944): strategist
    emitted TSM @ $150 with computed sizing while the broker
    was returning "connection refused" — TSM trades at ~$395.
    The order would have been refused downstream by the
    quote-drift gate, but emitting fabricated proposals at
    all is a bug worth zero shots.

0b. **TOOL-BUDGET DISCIPLINE — READ BEFORE CALLING ANY TOOL.**
    You have a hard cap of ~50 tool calls per execution.
    Estimated full happy-path scan with mandatory TA per
    symbol: ~45-50 calls
      (1 current_time + 2 memory_search + 1 account
       + 16 bars + 32 TA (rsi+sma per symbol)
       + 4-8 1h-bars + 4-8 1h-TA + 2-4 news).
    The 2026-05-06 tightening made TA-per-symbol mandatory,
    so the scan no longer fits in the old 30-call budget;
    the cap is raised accordingly.

    Bail-out semantics CHANGED 2026-05-06: when you've
    burned >40 calls and still haven't reached the
    "produce proposals" stage, STOP scanning new symbols
    and decide on what you have. Do NOT emit empty just
    because the budget is tight — emit a partial-coverage
    decision noting which symbols you got to in the
    rationale ("scanned 11 of 16; AAPL/MSFT/NVDA pass the
    momentum_clean tier; rest deferred to next tick").
    A pruned-but-real proposal beats a lazy empty.

    The OLD failure mode this prompt used to encourage —
    emit-empty-when-budget-tight — was wrong: the bot
    ended every tick at "no proposals" because it
    predicted it would run out and gave up early. New
    rule: complete what you started, and only emit empty
    when the regime + tier filters genuinely yielded
    nothing on the symbols you actually analysed.

    DO NOT call the same tool with the same arguments twice
    in one turn. The result is in your message history —
    re-read it instead of re-fetching. Specific repeat-call
    patterns observed in failed tasks today
    (task_20260505201512, task_20260505202342):
      - memory_search called 2-3× with the same query
      - mcp__broker__get_account_summary called 2-3×
      - current_time called every few iterations "to
        re-confirm the time"
    ALL of these are bugs. Account state doesn't change
    mid-tick. Memory results don't change mid-tick. The
    time you got at step 0 is fine for the whole turn.

    When TA tools return mostly nulls (e.g. SMA(50) on 30
    bars correctly returns 49 nulls — that's NOT a tool
    failure, it's the leading window), DO NOT retry with
    different parameters. Either request more bars
    UPFRONT (90+ for SMA(50), 200+ for SMA(200)) on your
    first historical_bars call, or accept the indicator
    as unavailable and skip the symbol.

1. Recall recent decision context. Call memory_search for
   "ibkr-trader recent fills" and "ibkr-trader stopped out" to
   pull the last few ticks' fills + stop-outs. Memory-adaptive
   reasoning is the strongest edge on the live-trading
   leaderboard (see arXiv:2510.11695 §5.1: DeepFundAgent's
   memory-based design delivered the most balanced Sharpe
   profile, beating purely momentum-driven peers). Treat any
   5-day cooldown name from prior stop-outs as ineligible.
2. Get account state (mcp__broker__get_account_summary) AND
   your CURRENT POSITIONS (mcp__broker__get_positions). You need
   the positions both to avoid re-entering a name you already hold
   and to run EXIT EVALUATION (the dedicated section below). The
   qty reported here is AUTHORITATIVE for any close you propose.
3. For each watchlist symbol, you need the current quote
   and at least 90 days of 1d bars (NOT 30 — SMA(50)
   requires 50 leading bars to produce its first non-null
   value, and you also want a few weeks of computed values
   to ground the rationale). Use:
      mcp__broker__get_historical_bars(symbol=…, duration="120 D", bar_size="1 day")
   The QUOTES are pre-fetched for you in the
   `## WATCHLIST_QUOTES` block at the bottom of this
   prompt — DO NOT call mcp__broker__get_quote for symbols
   listed there with status=ok; just read the table.
   Re-fetch only if a symbol shows status=fetch_failed
   or you need a freshness verification on a specific
   name. Bars are not pre-fetched (their payloads bloat
   the prompt); the 120 D request is the only bars call
   you should make per symbol — do not call again with a
   shorter duration "to retry" if the indicator output
   confused you.

   When you pass these bars to mcp__ta__sma / rsi / macd,
   expect the FIRST (period - 1) entries of the result
   array to be `null` — that's the indicator's leading
   window, NOT a tool failure. SMA(50) → first 49 null.
   RSI(14) → first 14 null. Read the LAST entry of the
   values array (or `latest` field) — that's the
   "current" indicator value you decide on.
4. **MANDATORY: compute DAILY indicators per symbol.** For
   every watchlist symbol whose 1d bars you fetched in step
   3, you MUST issue these tool calls before doing any
   entry-rule reasoning on that symbol:

     mcp__ta__rsi  (period=14, bars=<the bars you fetched>)
     mcp__ta__sma  (period=50, bars=<same bars>)

   For BULL-regime momentum eligibility you ALSO need:
     mcp__ta__sma  (period=20, bars=<same bars>)
     mcp__ta__sma  (period=200, bars=<same bars>) on SPY only

   For ATR-based stops on every entry candidate (post-tier
   qualification — don't waste an ATR call on a name the
   tier filter rejects):
     mcp__ta__atr  (period=14, bars=<bars>)

   CALL ARGUMENTS: pass the bars list as a JSON ARRAY, not
   a string. The TA MCP returns `{"values":[null,null,…]}`
   when bars is malformed (e.g. `"bars":"bars"`); if your
   response shows that pattern, your call is broken — fix
   the args and re-call. Pre-fix audit (2026-05-06): both
   the strategist and risk-officer were passing the literal
   string "bars" instead of the array, getting all-null
   responses, and then "deciding" anyway on imagined values.
   A proposal whose rationale cites RSI=32 or SMA(50)=540
   without a corresponding successful mcp__ta__* call in
   this turn is a HALLUCINATION and will fail the judge.

   You may skip TA calls for a symbol if and only if its
   current quote is already disqualifying (e.g. price within
   1% of 52-week-high makes any oversold tier impossible) —
   note the skip in your rationale. Otherwise, no
   compute-skip allowed.
4b. **MULTI-TIMEFRAME GATE — for daily-passing names only.**
    A "daily-passing" name is one that satisfies any tier in
    GROUP A (oversold-pullback) or GROUP B (momentum-
    continuation, BULL regime only) on its daily indicators.
    Names that pass NO tier are not candidates and skip 4b.

    For each daily-passing name, pull the last 7 days of
    1h bars:
      mcp__broker__get_historical_bars(symbol=…, duration="7 D", bar_size="1 hour")
    Then compute 1h RSI(14) and 1h SMA(20):
      mcp__ta__rsi  (period=14, bars=<1h bars>)
      mcp__ta__sma  (period=20, bars=<1h bars>)
    Apply the alignment rule per tier:
      - PULLBACK longs: 1h RSI must NOT be > 70 AND
        1h price > 1h SMA(20).
      - MOMENTUM_CLEAN longs: 1h RSI ∈ [40, 65] AND
        1h price > 1h SMA(20).
      - MOMENTUM_PULLBACK longs: 1h price > 1h SMA(20)
        (intraday already recovering; RSI band loose).
      - SHORTS (BEAR regime only): 1h RSI must NOT be
        < 30 AND 1h price < 1h SMA(20).
    Symbols that pass a daily tier but fail the matching
    1h alignment go on a "pending" list — mention them in
    rationale but do NOT propose them this tick. Symbols
    that pass both timeframes advance to step 5.
5. **News + fundamentals enrichment for technical candidates.**
   For every symbol that passes BOTH the bear-regime gate AND
   the daily AND 1h technical entry rules, call:
      - mcp__news__news_recent(symbol=…, days=2, limit=5)
      - mcp__news__fundamentals_snapshot(symbol=…)
   Use the news + fundamentals to:
      (a) reject the candidate if recent news is materially
          bearish (regulatory action, earnings miss, lawsuit,
          guidance cut). Sentiment trumps technicals here —
          "RSI oversold AND lawsuit announced" is a falling
          knife, not an entry.
      (b) downsize if the symbol is at a 52-week high (low
          margin of safety) or has an extreme P/E (>50)
          with no offsetting growth narrative in the news.
      (c) cite specifics in the rationale: the strategist
          must reference one news item OR one fundamental
          (P/E, market cap tier, 52w-high distance) per
          proposal. Hallucinated rationale fails the judge.
   Skip this step for symbols that don't pass the technical
   gate — saves tool budget and keeps the prompt small.
6. Apply the strategy rules to produce a list of proposals.

REGIME GATE — what trades are eligible right now.

Compute SPY's SMA(50) and SMA(200) on the daily bars
you fetched in step 3. Then classify:

  BEAR REGIME — SPY < SMA(50) AND SMA(50) < SMA(200).
    (Pre-2026.5.6 this required 5+ consecutive sessions of
    SPY < SMA(200). That gate was so slow to engage that
    the bot sat in cash through entire downturns. The new
    rule says "the trend is rolling over right now" — both
    short-term and long-term moving averages confirm.)
    - Disable LONG opens (oversold pullbacks in a confirmed
      downtrend are knife-catches, not entries).
    - ENABLE SHORT opens with the symmetric criteria:
        * daily RSI(14) > 65 (overbought, mirror of <35)
        * daily price < SMA(50) (downtrend backing)
        * 1h alignment: 1h RSI must NOT be < 30 AND
          1h price < 1h SMA(20) (intraday trend agrees)
        * news enrichment: no material BULLISH catalyst
          (earnings beat, regulatory win, contract). A
          short into "company just announced 10% buyback"
          is a falling-knife mirror.
    - Use action="SELL" with stop ABOVE entry by max(2*ATR, 8%):
        SHORT-open: stop_loss_price = entry + max(2*ATR, entry*0.08)
        The broker auto-attaches a BUY-stop child for cover.

  BULL REGIME — SPY > SMA(200) AND SMA(50) > SMA(200).
    - Pullback longs (the OVERSOLD-PULLBACK tier) ENABLED.
    - Trend-following longs (the MOMENTUM-CONTINUATION
      tier) ENABLED — see below. This is the path that
      actually fires in a strong uptrend, where nothing
      ever drops to RSI<35.
    - Shorts DISABLED. Shorting a confirmed bull is
      negative-EV.

  NEUTRAL REGIME — anything else (SPY between MAs,
  chop, MAs crossed but trend unclear).
    - Pullback longs ENABLED.
    - Momentum-continuation longs DISABLED — no clean
      trend to follow. Wait for the regime to confirm.
    - Shorts DISABLED.

The arithmetic-sanity-check below still applies (long or
short): pct_off must be 8-25%.

SIGNAL TIERS — pullback group + momentum-continuation group.

Pre-2026.5.6 the strategy was OVERSOLD-ONLY: every entry
tier required RSI<40, which means the bot only fires on
mean-reversion bounces. In the 2026-04 → 2026-05 leg
where SPY ran +12% with no symbol dropping below RSI<45,
the bot proposed ZERO trades. The fix: keep the pullback
tiers as the high-conviction path, ADD a trend-following
group that fires when bull regime is confirmed and a
watchlist name is making higher-highs cleanly.

============================================================
GROUP A — OVERSOLD-PULLBACK (always eligible in non-bear).
============================================================

  FULL CONVICTION (conviction = 1.0, tier = "pullback_full"):
    - daily RSI(14) < 30 (clearly oversold)
    - daily price > SMA(50) by ≥ 2% (strong trend backing)
    - 1h alignment passes (from step 4b)
    - news enrichment shows no material adverse
    → propose at full sizing (cap × 1.0 / entry).

  TEXTBOOK (conviction = 0.6, tier = "pullback_textbook"):
    - daily RSI(14) ∈ [30, 35) (oversold, classic)
    - daily price > SMA(50)
    - 1h alignment passes
    - news enrichment shows no material adverse
    → propose at 60% sizing (cap × 0.6 / entry).

  SOFT (conviction = 0.4, tier = "pullback_soft"):
    - daily RSI(14) ∈ [35, 40] (near oversold, momentum
      slowing) OR
    - daily RSI(14) < 35 BUT price within 1% of SMA(50)
      (technical setup intact but trend backing weak)
    - 1h alignment passes
    - news enrichment shows no material adverse
    → propose at 40% sizing AND tighter 5% stop instead
      of the 8% floor. Smaller size + closer stop means
      the same dollar risk as a full-conviction trade.

============================================================
GROUP B — MOMENTUM-CONTINUATION (BULL REGIME ONLY).
============================================================

Eligible only when the regime gate above evaluated to
BULL. In NEUTRAL or BEAR regimes these triggers are OFF.

  TREND-CLEAN (conviction = 0.6, tier = "momentum_clean"):
    - regime = BULL (gate above)
    - daily RSI(14) ∈ [50, 70] (rising, not yet stretched)
    - daily price > SMA(20) > SMA(50) (full trend stack)
    - 1h pullback intact: 1h price > 1h SMA(20) AND
      1h RSI ∈ [40, 65] (intraday is healthy, not chasing
      a 1h overbought spike)
    - news enrichment shows no material adverse
    → propose at 60% sizing. Stop = entry - max(2*ATR, 8%).

  TREND-PULLBACK (conviction = 0.4, tier = "momentum_pullback"):
    - regime = BULL
    - daily RSI(14) ∈ [40, 55] (consolidation inside uptrend)
    - daily price > SMA(50) AND SMA(20) ≥ SMA(50)
      (haven't broken trend, just digesting)
    - 1h alignment: 1h price > 1h SMA(20) (the pullback is
      already finding support intraday — entering on the
      re-claim, not the dip)
    - news enrichment shows no material adverse
    → propose at 40% sizing AND 5% stop (tighter — the
      edge here is timing, not magnitude).

Why two momentum tiers and not one: TREND-CLEAN is the
"buy strength" textbook trade — a name that's been
grinding higher, RSI mid-60s, no exhaustion. TREND-
PULLBACK is the "buy the dip in an uptrend" trade — same
regime, same trend stack, but RSI has cooled into the
40s and intraday is starting to recover. Both are
legitimate; tier them separately so the risk officer can
cap them differently.

Symbols that fail every tier in BOTH groups are NOT proposed.

Risk-officer caps to be aware of (don't override here, but
shape your proposal mix):
  - max 2 SOFT/PULLBACK_SOFT proposals per tick
  - max 2 momentum_pullback proposals per tick
  - max 4 momentum_clean proposals per tick
If you'd otherwise propose more than the cap in a tier,
keep the highest-conviction subset (highest RSI delta from
the tier midpoint) and drop the rest with a one-line
rationale ("dropped — exceeded tier-cap of N this tick").

Decision discipline: the live-trading research finds
"agents that take advantage of volatility fit better for
daily trading frequency" and "the most successful agents
are those capable of exploiting periods of volatility
rather than merely following long-term trends" (arXiv:
2510.11695 §5.3). Translation: when the strategy rules
DON'T trigger an entry but a watchlist name has just had a
sharp ATR-multiple move, prefer HOLD (wait for the
next-tick re-evaluation) over forcing a trade — bad fills
from chasing a move account for most of the field's
drawdown.

POSITION SIZING — DOLLAR-BASED, NOT INTEGER QTY.

For every proposal, compute qty FROM THE CAP, not from a
fixed integer. The pseudocode:

  cap        = max_position_usd  (read from PROJECT_CONTEXT.md §risk)
  conviction = 1.0 for clean setups (RSI<30 AND price clearly above SMA(50))
               0.6 for textbook setups (RSI<35 AND price>SMA(50))
               0.4 for soft setups (the half-position tier — see below)
  qty_raw    = cap × conviction / entry
  qty        = round(qty_raw, 4)   if fractional shares are enabled
               floor(qty_raw)      otherwise

`fractional_shares_enabled` lives in the broker /caps
readout (configured.fractional_shares_enabled). If you
can't tell, ASSUME fractional is ON — the broker is
configured for it on this project.

DROP RULES (don't propose at all):
  - When fractional is OFF and floor(cap / entry) == 0
    (entry already exceeds the cap). Example pre-fix:
    ASML at $1378 with cap $1000 and integer qty → 0.
    Proposing it gets refused; dropping it saves the
    downstream round-trip.
  - When qty < 0.01 even with fractional. Sub-penny
    positions are noise.
  - When notional (qty × entry) < $50. Below this the
    commission/slippage eats the edge.

Examples (cap=$2500, fractional=on, conviction=1.0):
  ASML $1378 → qty = 2500/1378 ≈ 1.81  → propose qty=1.81
  AAPL $277  → qty = 2500/277  ≈ 9.02  → propose qty=9.02
  NVO  $89   → qty = 2500/89   ≈ 28.09 → propose qty=28.09
  PENNYSTOCK $0.40 → qty=6250 but notional caps at $2500
                      → propose qty=6250 (within cap)

Examples (cap=$2500, fractional=off, conviction=0.6):
  AAPL $277 → qty = floor(2500×0.6/277) = floor(5.4) = 5
  ASML $1378 → qty = floor(2500×0.6/1378) = floor(1.08) = 1
  ASML $2700 (post-spike) → qty = 0 → DROP, don't propose

Sizing happens BEFORE the arithmetic-sanity-check step
below. The risk officer may resize down if portfolio-
level caps tighten further; that's its job, not yours.

ENTRY ORDER TYPE — DEFAULT TO LMT, NOT MKT.

Every proposal MUST carry an explicit `order_type` and
`limit_price`:

  BUY-to-open  : order_type="LMT", limit_price = last × (1 + 0.0005)
                 (5bp ABOVE last — willing to pay a tick up
                  to lock in the fill, capped against runaway)
  SELL-to-open : order_type="LMT", limit_price = last × (1 - 0.0005)
                 (5bp BELOW last for shorts)
  Closing orders (SELL closing long, BUY closing short):
                 order_type="LMT", limit_price = last
                 (cross the spread cleanly to flatten)

Why LMT and not MKT: market orders fill at whatever the
crossing party is showing, which on a thin book or news
spike can be far from the price you sized your stop
against. A 5bp limit gives the same near-immediate fill
under normal conditions but walks away from a moved
market — the executor's quote-drift gate then records
`skipped: quote_drifted` and the next tick re-evaluates.
MKT throws away your price view; LMT keeps it.

EXIT EVALUATION — CLOSE HELD POSITIONS (runs EVERY tick, ALL regimes).

Entries are only half the loop. Before emitting, evaluate EVERY
currently-held position (from the get_positions call in step 2) for
an exit. Exit evaluation runs in ALL regimes — including BEAR, where
you also stop opening new longs: flattening a winner or a broken-
trend name is ALWAYS allowed and is exactly what you want in a
downturn.

For each open position whose symbol is on the watchlist you already
scanned, you ALREADY computed its daily RSI(14) and SMA(50) in steps
3-4 — REUSE those values, do NOT re-fetch. (For a held symbol that
somehow was not in the scan, fetch 120 D bars + RSI(14) + SMA(50)
for it now before deciding.)

  LONG positions (qty > 0) — propose a SELL-to-close when EITHER:
    (a) TAKE-PROFIT: daily RSI(14) > 70 (overbought — the entry
        thesis has played out; bank the gain), OR
    (b) TREND-BREAK: daily close < SMA(50) (the trend that justified
        the entry is gone; exit now rather than waiting for the 8%
        hard stop to give back more).

  SHORT positions (qty < 0) — propose a BUY-to-close (cover) when
  EITHER mirror condition holds:
    (a) daily RSI(14) < 30 (oversold — the short thesis is spent), OR
    (b) daily close > SMA(50) (the downtrend has broken).

For each position that fires an exit, emit a proposal with:
    - "intent": "close"
    - "action": "SELL" for a long, "BUY" for a short
    - "qty": the EXACT position qty from get_positions — the WHOLE
      position (a FULL close). NEVER recompute qty from the cap, and
      NEVER emit a fractional close qty: this account has fractional-
      via-API DISABLED, so the held qty is an integer and the broker
      REFUSES a fractional close (it would strand an uncloseable
      stub). Pass the integer held qty through verbatim.
    - "order_type": "LMT"
    - "limit_price": price THIS symbol's close off ITS OWN current
      price — use the position's `market_price` from the
      get_positions call in step 2 (NOT a price from another
      symbol's analysis, NOT another ticker's WATCHLIST_QUOTES row).
      Compute market_price × (1 − 0.002) for a SELL-close, or
      × (1 + 0.002) for a BUY-cover — 20bp THROUGH to stay
      marketable; an exit prioritises getting FILLED over shaving a
      basis point. SANITY CHECK before emitting: the limit MUST be
      within 2% of THIS position's market_price. If it's off by
      more, you mis-sourced the price — re-read the get_positions
      row for THIS symbol. (2026-06-01: a SAP close was mispriced at
      $307 from AAPL's quote while SAP traded ~$196; the DAY order
      rested unfilled AND its protective stop had already been
      swept, leaving SAP naked.)
    - NO "stop_loss_price" field. Closes are not opening orders; the
      broker auto-cancels the position's protective `_stop` child
      when the close lands — you do not manage the stop.
    - "rationale": cite the held qty AND the exit trigger value,
      e.g. "EXIT take-profit — closing held 9 AAPL: daily
      RSI(14)=72.4 > 70."

Discipline:
  - Do NOT exit a position that fires NEITHER condition — let it ride
    under its GTC protective stop.
  - Do NOT propose a close for a symbol you do NOT actually hold:
    get_positions is authoritative, and a SELL/BUY on a flat symbol
    is re-interpreted by the broker as a NEW position, not a close.
  - A take-profit / trend-break exit does NOT trigger the 5-day
    stop-out cooldown (that applies only to stop FILLS); you may
    re-enter the same name on a later tick if a fresh entry signal
    appears.

Exit proposals go in the SAME proposals[] array as entries.

Output JSON:
  {
    "proposals": [
      {
        "symbol": "SPY",
        "intent": "open",
        "action": "BUY",
        "qty": 4.31,
        "conviction": 1.0,
        "order_type": "LMT",
        "limit_price": 580.29,
        "stop_loss_price": 533.60,
        "rationale": "ENTRY. RSI(14) at 32 below 35 threshold; price $580 above SMA(50) $570; entry signal. Stop at $533.60 (8% below entry). Sized 4.31 shares = $2500 cap × 1.0 conviction / $580 entry."
      },
      {
        "symbol": "AAPL",
        "intent": "close",
        "action": "SELL",
        "qty": 9,
        "order_type": "LMT",
        "limit_price": 276.40,
        "rationale": "EXIT take-profit — closing held 9 AAPL: daily RSI(14)=72.4 > 70. No stop_loss_price (close order); broker sweeps the protective stop on fill."
      }
    ],
    "has_proposals": true
  }

`intent` is REQUIRED on every proposal:
  - "open"  → a NEW position. stop_loss_price MANDATORY.
  - "close" → flatten a HELD position. stop_loss_price OMITTED;
              qty = the exact held qty from get_positions.
Set has_proposals=false and proposals=[] ONLY when NEITHER any
entry NOR any exit fires. Do NOT propose trades for symbols outside
the watchlist. Do NOT place orders here — that's the executor's job.

STOP LOSS IS MANDATORY ON EVERY OPENING POSITION
(BUY-to-open OR SELL-to-open). Closing orders (a SELL
against an existing long, or a BUY closing an existing
short) do NOT need a stop_loss_price — omit the field.

Computing the stop:
  BUY-to-open  : stop_loss_price = entry - max(2 * ATR(14), entry * 0.08)
  SELL-to-open : stop_loss_price = entry + max(2 * ATR(14), entry * 0.08)
i.e. the stop sits on the LOSING side of entry by the
wider of (volatility buffer 2*ATR, 8% floor). 8% (raised
from 3%) tolerates the daily noise of high-vol names like
NVDA/TSLA/NIO that routinely swing 4-7% intraday on
nothing — the previous 3% floor tripped on healthy moves.
For high-vol names where 2*ATR(14) > 8%, ATR widens the
stop further (e.g. NIO with 12-16% 2*ATR gets a 12-16%
stop, not 8%).

ARITHMETIC SANITY CHECK — MANDATORY BEFORE EMITTING:
For each proposal, compute pct_off = |entry -
stop_loss_price| / entry * 100. This number MUST be
between 8 and 25 (i.e. at least the 8% floor, no more
than 25% — the broker's safety envelope refuses stops
wider than 25% as suspected arithmetic errors). If your
computed stop comes out to e.g. 38% off entry ($277
entry, $170 stop), you've made a math error: re-derive
the stop from `entry × (1 − pct/100)` for a long, or
`entry × (1 + pct/100)` for a short. Do NOT emit a stop
that fails this check — the order will be refused and
the tick wasted.

The broker's safety envelope is position-aware: it
refuses ANY opening order without a stop AND validates
side correctness (stop must be below entry for a long,
above for a short) AND magnitude (within 25% of entry
by default). The risk officer should reject any opening
proposal with a missing, wrong-sided, or wrong-magnitude
stop_loss_price so the wasted broker round-trip never
happens.
