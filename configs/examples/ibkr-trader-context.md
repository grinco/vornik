# IBKR Trader — policy v2, effective 2026-09-09 (amended 2026-09-10)

This is the operational strategy for workflow `ibkr-trading-v2`, in PAPER mode.
It supersedes earlier v1/v1.1 context and historical RAG trading instructions.
RAG records prior observations; current broker holdings, orders and prices are
authoritative. Never infer performance from the uncorrected pre-July fill ledger.

## Risk

Strategy capital ceiling: $15,000 gross holdings plus pending entry exposure.
The paper account's approximately $1m idle cash is not available strategy risk.
Per-symbol broker cap remains $2,500; new entries are capped at $1,500 notional
and $50 planned loss at the approved stop. Gaps, slippage and fees may exceed it.
Whole shares only; minimum entry notional $500. Never round quantity upward.
Maximum six positions including pending entries, two per economic region,
one new entry per tick and two filled/working entries per New York trading day.
Existing excess holdings remain managed: no forced exit solely for count/risk
budget changes, and no new positions while the book is over a limit.

No additions to existing or pending positions, averaging down, or new shorts.
Do not add correlated semiconductor exposure while a semiconductor is held.
After ANY exit, wait five completed US trading sessions before re-entry; also
obey the broker's 120-hour stop-out cooldown. Use dated order/fill evidence.
If today's broker realized P&L <= -$150, pause new entries for that trading day.
Missing live state or uncertain cooldown means no new entries.
Valid risk-reducing exits remain allowed regardless of entry restrictions.
Broker daily-turnover/order-rate/stop/kill-switch gates are additional controls.

## Watched and eligible symbols

Keep all 17 symbols observable so positions and benchmarks do not disappear:
AAPL, MSFT, GOOGL, NVDA, TSLA, JPM, ASML, SAP, NVO, SHEL, AZN, TSM, BABA,
SONY, INFY, HDB, SPY.

New entries during probation: **ASML, AZN, GOOGL, JPM, MSFT, SAP, SHEL, SONY**.
Every other watched name is observation/exit-only. In particular NVDA and TSM
holdings remain managed; quarantine never means suppressing an exit.
SPY is a regime benchmark, not an entry candidate. Geographic quotas are caps,
not a requirement to buy a weak region. Use economic region for ADRs.

## Signals and sizing

Evaluate hourly during US regular trading hours, with current exchange holiday
checks. Review every holding first. Use completed daily and hourly candles;
never let the current partial candle count as a completed close.

Daily evidence is mcp__ta__scorecard(symbol, region), called every tick for
SPY, every held symbol and every entry symbol. It fetches the symbol's own
completed daily history (205+ sessions) and returns the score plus last_close,
sma20/50/200, rsi14 and atr14; it takes no bars. Long entries require SPY
last_close > sma200 and sma50 > sma200; candidate last_close > sma20 > sma50,
rsi14 in [50,65], and (hourly broker bars, daily survivors only) completed 1h
close > 1h SMA20 with 1h RSI14 in [40,65]. Candidate scorecard total >=3;
fresh regional regime not RISK_OFF, with complete components (us>=6, eu>=5,
apac>=4). Preserve tool results verbatim: the daemon's analysis-evidence gate
fails a strategist step whose tool audit lacks those calls, whose
holdings_review values differ from the scorecard outputs or break the SMA50
exit rule, or whose open proposals carry numbers the tools did not return.
Tool errors are missing evidence, not passing scores. Oversold/soft pullback
and bear-short tiers are disabled during probation.

For otherwise qualifying entries, require no material adverse news and no
earnings release within two US trading sessions. Unknown earnings timing blocks
an entry. Require a valid non-delayed bid/ask, spread/mid <=0.2%, and current ask
no more than 1% above the last completed daily close. These filters are candidate
rules for a paper experiment, not established evidence of future profitability.

Use BUY/LMT at ask*1.0005. Stop distance=max(2*daily_ATR14, entry*0.03); if above
8% of entry, skip. Stop=entry-distance. Round prices to valid ticks and then
qty=floor(min(1500/entry, 50/(entry-stop))). Recheck $500 minimum and $50 risk
after rounding. The original stop never widens to fit an entry. At execution,
adverse quote drift >0.5% means skip; otherwise reduce qty as needed to preserve
risk at the repriced limit. Do not increase approved qty.

Long exit: last completed daily close < SMA50. Legacy short exit: completed
daily close > SMA50. RSI >70 alone is NOT a take-profit signal. Let intact
trends run with their native protective stops active between ticks. Close the
actual held quantity with the correct side; omit stop_loss_price. Use fresh
marketable limits. Never cancel protection just because another entry needs a
slot. Confirm fills and cancellations; uncertain responses are not executions.

## Role responsibilities and evidence

Strategist: memory_search for dated audit/recent fills/cooldowns, then fresh
account/positions/orders. Review held names first; rank eligible entries by
scorecard, then spread. Return proposals/has_proposals, including no-action.

Risk officer: independent live position and pending-order checks; enforce all
portfolio/session constraints and recompute risk. No fabricated proposals or
automatic approval of a stale holding_state. Preserve approved fields verbatim.

Executor: fresh state before EACH order, close-first sequencing, same stable
idempotency key on retries, no duplicate/replacement entry while uncertain.
Observe actual broker order IDs/fills; report skipped reasons honestly.

Code filters both proposals and approvals for entry universe, BUY-only, LMT,
positive size/correct-sided stop, $50 planned risk, $500 minimum notional,
duplicate symbols and one entry per tick. Since 2026-09-10 the broker also
refuses any open past $15,000 gross exposure (held value + pending parents +
the order), six positions including pending, or a session realised loss of
$150, and the analysis-evidence gate fails an unexamined strategist step.
Per-region quotas, per-session entry counts, cooldowns and earnings checks
remain role checks. Direct manual broker calls are outside the workflow filter
but inside the broker gates.

## Evaluation and rotation

Funnel checkpoint on 2026-10-08 (the twentieth NYSE session after 2026-09-10)
regardless of trade count, reading the evidence-gate, floor, entry-policy and
broker refusal counters; the position-count review (20 independent completed
positions) is a separate later gate. Do not count FIFO lot fragments as independent
trades. Compare net realized plus unrealized changes and commissions, separately
subtract recorded LLM costs, and compare against SPY over the same dates and
the same strategy capital. Do not use the full paper-account equity growth as
strategy return: idle-cash accrual and other cash flows contaminate it.

For each symbol and entry tier collect entry/exit timestamps, live position
before/after, scorecard/regime, completed bar timestamps, spread, ATR, initial
planned risk, realized result, fees, hold time, and rejection reason. Do not
increase risk or remove a quarantine on a handful of winning trades. Require a
reconciled ledger and a separate forward paper window before any live promotion.

## Audit provenance

The 2026-09-09 audit read project RAG before changes and found nine unclosed
legacy ledger lots. A 2026-07-22 00:03:45 UTC broker snapshot (JPM 7, ASML 1)
plus subsequent execution fills reconstructs every current held quantity.
For Aug 10 16:00 UTC through Sep 9, matched lots lost about $700.61 after known
commissions; recorded model costs added about $105.43. All entry/exit fees in
that reconstruction are present. INFY/BABA/TSM/NVDA contributed about $671 of
net losses. These are retrospective diagnostics, not an out-of-sample backtest.
September INFY/AAPL exits reconcile to the broker's approximately -$174.61.
Keep raw audit exports local; never fabricate historical fills to repair P&L.
