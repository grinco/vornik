---
description: Semantic-search this project's RAG memory via vornik
allowed-tools: mcp__vornik__recall
argument-hint: <query>
---

# Recall from vornik RAG memory

The user wants to know what vornik already knows about: **$ARGUMENTS**

Call `mcp__vornik__recall` with `query=$ARGUMENTS`. Optional args you
can include when the user's intent is clear:

- `limit` (default 10, max 50) — how many hits to consider.
- `min_score` — drop weak hits. 0.5 is a reasonable cutoff for
  "probably relevant"; tighten to 0.7 if the user wants high
  confidence only.
- `from_date` / `to_date` (RFC3339) — clip to a recency window when
  the user references "recent" or a specific timeframe.

Render the result inline:

- If hits come back, show the top 3 as a markdown table with columns
  `score | source | snippet` (truncate snippet to ~200 chars). Below
  the table, summarise what the hits collectively say in 2-3
  sentences — that's the answer to the user's actual question.
- If no hits come back (`returned: 0`), say so plainly and offer
  to `delegate` a research workflow (`companion-research-gather`)
  if the topic merits one.

**Cost.** Recall is cheap (~$0.0001 per call on the configured
embedder). Use it freely; in particular, **prefer `recall` before
`delegate`** — if vornik already answered the question last week,
re-running compute is wasteful. The skill `delegate` codifies
this rule.

If the response says `this key lacks memory_read`, surface the message
verbatim and tell the user the operator-side fix is
`vornikctl companion grant --memory-read` (or `--memory-all`).
