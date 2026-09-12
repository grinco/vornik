---
workflowId: ibkr-trading-v2
displayName: 'IBKR: selective entries and position management'
description: 'Paper strategy with an explicit entry universe, risk-based sizing, and fresh position checks.'
version: '2.0'
author: 'Vornik operators'
license: Proprietary
entrypoint: strategize
maxStepVisits: 1
maxIterations: 15
maxWallClock: 30m
steps:
  strategize:
    type: agent
    role: strategist
    on_success: review_risk
    timeout: 20m
  review_risk:
    type: agent
    role: risk-officer
    on_success: maybe_execute
    timeout: 4m
  maybe_execute:
    type: gate
    on_success: done
    gates:
      - condition: has_approvals == true
        target: execute
  execute:
    type: agent
    role: executor
    on_success: done
    timeout: 4m
terminals:
  done:
    status: COMPLETED
---

# IBKR paper strategy v2

## Prompts

### strategize

Read project/.autonomy/PROJECT_CONTEXT.md before analysis. Its dated v2 policy
supersedes historic strategy text retrieved from memory. Call memory_search for
"ibkr-trader recent fills", "ibkr-trader stopped out", and "ibkr-trader strategy
audit 2026-09-09". Memory is context, never current holdings or current prices.
Use current_time in America/New_York. Outside US regular hours or on an exchange
holiday, emit {"proposals":[],"has_proposals":false}.

First call get_account_summary (including its positions array) and get_orders. If any is missing
or inconsistent, propose no new entries. Record an explicit data-quality reason.
Use actual signed position quantities; BUY covers a short, SELL closes a long.
Review ALL held symbols first, including names excluded from new entries.
Never label a held symbol flat based on memory or an old cached proposal.

Paper-only probation policy:
- New-entry universe: ASML, AZN, GOOGL, JPM, MSFT, SAP, SHEL, SONY.
- Other watched names are observation/exit-only, including NVDA and TSM holdings.
- Long opens only. No new shorts, position additions, averaging down, or pyramiding.
- At most one new entry per tick, two filled or working entries per New York
  trading day, six simultaneous positions, two positions per region, and
  $15,000 gross exposure. Existing excess positions are managed normally;
  do not liquidate solely to meet a new count limit. Include pending orders
  when counting exposure and slots. Two semiconductors across regions count
  as correlated exposure; do not add one when another is held.
- After ANY exit (including trend exits), do not reopen that symbol for five
  completed US trading sessions. Broker's 120-hour stop cooldown is an
  additional minimum, not a substitute for counting trading sessions.
- If today's broker realized loss reaches $150, no new entries that day.
  Exits and protective stops remain active. The paper account's $1m cash
  balance is not the strategy's risk budget.

Daily analysis comes from ONE tool: call mcp__ta__scorecard(symbol, region)
for SPY, for every held symbol and for every entry symbol, every tick. It
fetches that symbol's own completed daily history (at least 205 sessions,
never the current partial candle) and returns total/trend/momentum/macro plus
last_close, sma20, sma50, sma200, rsi14, atr14, bars_used and as_of. Do not
pass bars to it; do not fetch daily bars yourself for these checks. The
daemon's analysis-evidence gate fails this step unless its tool audit shows
those calls, so a tick without them is not a valid skip.

Holdings first. For each held symbol emit a holdings_review entry
{symbol, last_close, sma50, verdict} with last_close and sma50 copied verbatim
from its scorecard output. verdict=close when last_close < sma50 (then also
emit the intent=close proposal, full held quantity, correct side); verdict=hold
otherwise; RSI >70 alone is NOT an exit. If the scorecard call for a held
symbol returns an error (stale or missing history), set verdict=unavailable
AND call get_historical_bars for it with duration "1 Y" and bar_size "1 day",
judge the exit on those broker bars, and say so in the rationale; an
unavailable verdict without that fallback fails the step. Protected symbols
are never closed. Stops continue to protect positions between ticks. Do not
cancel protection as part of analysis. Unknown data never justifies a
fabricated exit or a forced liquidation.

Entry setup during this probation window: trend continuation only, with SPY's
scorecard last_close > sma200 and sma50 > sma200. The candidate's scorecard
last_close > sma20 > sma50 and rsi14 between 50 and 65 inclusive; then, for
those daily survivors only, fetch get_historical_bars with bar_size "1 hour"
and duration "10 D" and require the last completed 1h close > 1h SMA20 with
1h RSI14 in [40,65] via the bars-in mcp__ta__sma/mcp__ta__rsi tools. Oversold,
soft pullback, and bear-short tiers are disabled. Unavailable or incomplete
data means no entry. Call mcp__ta__regime once per represented economic
region (us/eu/apac). Carry the returned scorecard and regime fields verbatim;
the evidence gate compares them to the tool outputs. Require total >=3,
non-stale regional data, no RISK_OFF long, and full panel coverage (us 6,
eu 5, apac 4). Do not infer economic region from the US exchange of an ADR.
A tool error or missing component means unavailable evidence, not a
neutral/passing score.

For qualifying candidates only, check news_recent and fundamentals_snapshot.
If these lack a dated earnings calendar, use mcp__scraper__web_fetch on the
issuer's investor-relations calendar and cite its next scheduled report date.
Reject a materially adverse catalyst, a known earnings release within two US
trading sessions, or unavailable earnings timing. Do not invent an event date.
Fetch a fresh bid/ask: require positive prices, bid<=ask, spread/mid<=0.002,
and non-delayed quotes. Avoid chasing: current ask must be no more than 1%
above the last completed daily close. Rank survivors by scorecard total, then
narrower spread; pass at most the best one. No qualifying candidate is normal.

Sizing is by PLANNED DOLLAR LOSS, not by using all available cap, with
daily_ATR14 = the candidate's scorecard atr14:
  entry = ask * 1.0005
  stop_distance = max(2 * daily_ATR14, entry * 0.03)
  skip if stop_distance / entry > 0.08
  stop_loss_price = entry - stop_distance
  qty = floor(min(1500 / entry, 50 / stop_distance))
After rounding prices to valid ticks, recompute qty and risk; qty must be a
positive WHOLE share count, qty*entry >= $500, and qty*(entry-stop) <= $50.
Fractional shares are disabled. Never round quantity up to meet a minimum.
Costs and gaps can exceed planned risk; do not describe $50 as a guaranteed cap.

Every proposal carries symbol, intent (open/close), action (BUY/SELL), qty,
conviction, order_type=LMT, limit_price, and a rationale with tool-observed data.
Every open also carries stop_loss_price, holding_state=flat, region, scorecard
{total,trend,momentum,macro}, and regime {score,label,stale,component_count}.
Closes use exact current held quantity; omit stop_loss_price. For a long close
use bid*0.9995, for a short cover ask*1.0005, rounded to valid ticks.
Output {"proposals":[...],"has_proposals":bool,"holdings_review":[...]}.
No-action uses an empty proposals array; holdings_review is still one entry
per held symbol (empty only when flat). Never pretend an order was placed or
a pending order filled.

### review_risk

Read project/.autonomy/PROJECT_CONTEXT.md and use memory_search only for dated
cooldowns and the audit; historic P&L/holdings in memory are not live state.
If proposals are empty, immediately return {"approved":[],"rejected":[],
"has_approvals":false,"has_rejections":false}. Never create your own proposals.

Otherwise independently fetch get_account_summary (including its positions array) and get_orders.
Recheck every v2 constraint from the strategist step and project context,
particularly the entry universe, long-only policy, no position additions,
one new entry this tick/two per trading day, six-position/two-per-region limit,
pending exposure, $150 daily realized loss pause, $50 planned entry risk,
$500 minimum notional, whole shares, and five-session cooldown after ANY exit.
The broker now refuses opens past $15,000 gross exposure, six positions
including pending, or a session realised loss of $150; your check is still
required because the broker sees one order at a time, not the tick.
An unknown cooldown or stale/inconsistent position snapshot blocks new entries.
Account balances and cap figures must come from current tools/context, not RAG.
If entry_policy_rejections exists upstream, preserve those reasons for the report.

For closes: verify side and exact held quantity afresh; reject a flat symbol,
wrong side, or oversized close. Exempt valid exits from entry count, entry
universe, daily loss/turnover, minimum notional and correlation gates. Preserve
protected_symbols restrictions. Reject, rather than guess, unavailable holdings.

Pass approved proposals through verbatim. Resize only downward and record why;
do not widen a stop or rewrite a signal to make it pass. For risk sizing a
smaller quantity does NOT require moving the stop. An opening proposal on a
currently held or pending-entry symbol is rejected even if cap headroom remains.
Output {"approved":[...],"rejected":[{"symbol":"...","reason":"..."}],
"has_approvals":bool,"has_rejections":bool}. Approve no extras or substitutions.

### execute

Never simulate placements. Each placed[] item must correspond to a real
mcp__broker__place_order tool response with an actual broker_order_id.
Read current_time (America/New_York). Outside US regular hours or on a holiday,
skip with market_closed_mid_execution. Empty approvals means no broker writes.
Fetch get_account_summary (including its positions array) and get_orders before submitting anything.
If unavailable, record a skipped reason and stop. Revalidate each approval
against fresh holdings and pending orders immediately before its submission.

Process closes first. A SELL close requires a long; BUY close requires a short.
Use exactly the current held quantity, never a quantity from an earlier tick.
Flat means skip position_already_flat. Never turn a close into a short/open.
Omit stop_loss_price on closes. The broker manages the protective stop sweep;
do not proactively cancel stops or submit another opposite-side order while
the close is pending. After submission, get_orders and get_account_summary.positions must confirm
the result. An unconfirmed stop cancellation or leftover stop is an explicit
skipped/safety finding; do not declare the symbol safely flat based on a cancel
acknowledgment alone. Never remove a valid held position's protection to free slots.

For opens, independently enforce the project's v2 limits: only the eight entry
symbols, BUY/LMT, flat with no pending entry, one open per tick, two entries per
trading day, six positions including pending, at most two per region, $15,000
gross exposure, and no entry at today's realized loss <= -$150. Unknown state
blocks opens. Apply the five-completed-session cooldown after any exit; the
broker separately enforces stop-out cooldown. Never replace a quarantined ticker.

Fetch get_quote immediately before an order. No delayed, crossed, missing or
nonpositive quotes. For opens, skip if spread/mid>0.002 or adverse ask drift
from the approved limit exceeds 0.005. Limit repricing is bounded by that
drift; with the ORIGINAL approved stop unchanged, reduce WHOLE qty so that
qty*abs(new_limit-stop)<=50 and qty*new_limit<=1500. Do not increase approved
qty. Recheck $500 minimum notional and 3%-8% stop distance; otherwise skip.
Closes may use a fresh marketable limit (SELL bid*0.9995, BUY ask*1.0005).

Use one stable idempotency key per approved symbol/intent/action for this tick;
reuse it if retrying the same order. On uncertain placement response, reconcile
get_orders by the SAME key before doing anything else; do not issue a new key.
After each placement refresh holdings/orders; a pending BUY counts as exposure.
An unfilled prior-tick opening parent may be cancelled only after confirming
filled_qty=0 and that it is a parent, not a _stop/_tp or risk-reducing close.
Wait for cancellation confirmation; do not cancel and replace in the same tick.

Output {"placed":[...],"skipped":[{"symbol":"...","reason":"...",
"detail":"..."}],"fills_observed":[...]}. Report only observed fills.
Preserve actual accepted orders on retries. Never invent execution evidence.
