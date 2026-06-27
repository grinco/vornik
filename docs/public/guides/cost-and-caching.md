---
sources:
    - path: docs/operator/cost-and-caching.md
      sha256: e51da1ccf7fbac2199080b55800fa982f5b9fd0a2adbe2765d7eb6418690a1d8
---
# Cost and Caching

vornik routes a lot of traffic to language-model providers, and that traffic
is the single biggest line item in most deployments. This guide covers the
controls that keep that cost predictable: hard per-project spend caps, dynamic
per-role tool budgets, an effective-cost-per-success signal, prompt caching, an
embedding cache, memory consolidation tiers, per-project API keys, and rate
limits.

Most of the settings here live in your daemon config
(`~/.config/vornik/vornik.yaml`); spend caps are set per project. See
[Configuration reference](../reference/configuration.md) for the full list of
keys and defaults.

---

## Prompt caching

Most agent turns re-send the same system prompt and tool descriptions on every
request. Provider-native prompt caching lets the model reuse that unchanged
prefix instead of re-processing it, which can cut LLM cost substantially on
busy projects.

Caching is supported on Anthropic and AWS Bedrock model backends. Other
backends (Google Vertex, OpenAI-compatible HTTP endpoints, and local Ollama)
simply ignore the setting — there is no cost or behavior change if you enable
it while using them.

```yaml
chat:
  # Off by default. Recommended value: "auto".
  #   "auto"   - cache the system prompt on every request automatically.
  #   "prefix" - only cache requests the caller explicitly marks.
  prompt_cache_mode: "auto"
```

Because the setting is a no-op on backends that don't support caching, it is
safe to turn on globally and let only the projects that benefit pick it up.

!!! note "Provider caches have a short lifetime"
    Cached prefixes are held by the provider for a few minutes between hits. A
    project that runs only occasionally (for example, an autonomy loop that
    ticks every half hour) may let the cache expire between turns and see
    little or no saving. Busy, frequently-running projects benefit the most.

### Seeing the effect

The spend dashboard at `/ui/spend` adds two columns once caching is on: a
cache hit ratio and an estimated dollar saving versus the uncached rate. If a
project's hit ratio stays at zero, caching isn't helping there — usually
because the backend doesn't support it, the project runs too infrequently, or
your provider plan doesn't include cache capacity for large prompts.

---

## Embedding cache

When [memory and RAG](../features/memory-rag.md) ingests content, it asks an
embedding model to turn text into vectors. Identical text produces identical
vectors, so the embedding cache stores them and serves repeats locally instead
of paying for the same embedding twice.

```yaml
memory:
  embedding_cache_enabled: true   # default: false
```

Things to know:

- The cache is keyed on both the text and the embedding model. If you change
  `memory.embedding_model`, the previously cached vectors no longer match and
  the next ingest pass re-embeds under the new model.
- The cache stores 1024-dimension vectors. If your embedding model produces a
  different dimension, leave the cache off and pick a 1024-dimension model, or
  skip caching.

---

## Memory consolidation tiers

vornik periodically summarizes each project's memory into a short "gist" so
agents can orient quickly without re-reading everything. There are two tiers,
and they have very different cost profiles.

- **Tier 1 — keyword gist (LLM-free).** A fast term-frequency pass over each
  project's memory. It runs by default, costs no LLM tokens, and is cheap
  enough to leave alone.
- **Tier 2 — narrative gist (LLM-driven).** A short natural-language summary
  written on top of the keyword gist. This one calls a model, so it is
  off by default and opt-in.

```yaml
memory:
  # Tier 1 (keyword) - on by default.
  consolidate_interval_seconds: 600

  # Tier 2 (narrative) - opt in once Tier 1 has run for a few days.
  llm_consolidate_enabled: true
  llm_consolidate_interval_seconds: 3600
  llm_consolidate_model: "openai.gpt-oss-20b-1:0"
```

Keep the narrative model small. The narratives are short summaries, not deep
analysis, so a compact open-weight model produces results indistinguishable
from a large one at a fraction of the cost. Picking a large model here is a
common way to run up an unexpected bill.

You can read the latest gist for a project from the CLI:

```bash
vornikctl memory gist <project-id>
```

---

## Recall reranking (quality vs. latency)

By default, memory recall ranks results with a fast vector + keyword blend (no
model call). You can optionally add an **LLM reranker** that re-scores the top
candidates by relevance — it produces sharper ordering, and it is what makes
**scored-sufficiency** retrieval (widen-and-retry until enough genuinely
relevant hits) meaningful.

```yaml
memory:
  reranker:
    enabled: true                    # default false
    model: "openai.gpt-oss-20b-1:0"  # keep it small and fast
    max_candidates: 20               # how many hits the model scores
    timeout_seconds: 8               # degrade to the fast ranking past this
  sufficiency:
    enabled: true                    # widen-and-retry; only active with the reranker
    min_high_rel: 3
    score_floor: 0.6
    max_rounds: 2
```

!!! warning "Reranking adds seconds — it is scoped on purpose"
    The reranker is **one extra LLM call per recall**, and on a small model
    that is still a few seconds. To keep interactive use snappy, reranking is
    applied **only to the non-interactive context-assembly path** (the
    pre-delegation recall hint). The interactive surfaces — the agent's
    `memory_search` tool and the `recall` command — deliberately stay on the
    fast ranking and never pay this cost. If a rerank call exceeds
    `timeout_seconds` it falls back to the fast ranking; results are never
    dropped. Keep `model` small and watch recall latency; lower
    `max_candidates` or set `enabled: false` if it bites.

---

## Per-project API keys

API keys let you attribute traffic — and therefore cost — to the right
project, and contain a key that leaks or misbehaves without disrupting
everything else.

Each key is minted for a specific project. The bound project is the
authoritative target for cost accounting, so spend always lands on the right
ledger regardless of how the caller is configured.

```bash
# Mint a key (the secret is shown once - capture it immediately)
vornikctl key create --project personal-assistant --name "ha-loop" --expires 90d

# Mint a key with no expiry
vornikctl key create --project personal-assistant --name "scripts"

# List active keys (secrets are never shown again)
vornikctl key list --project personal-assistant

# Rotate (issue a new secret, revoke the old key)
vornikctl key rotate <key-id> --project personal-assistant

# Revoke
vornikctl key revoke <key-id> --project personal-assistant
```

`--expires` accepts a timestamp or a duration such as `30d`, `6m`, or `1y`.

!!! warning "The secret is shown only once"
    `vornikctl key create` and `vornikctl key rotate` print the raw secret a
    single time. vornik stores only a hash, so it cannot show it again. Save
    it to your secret store right away.

Rotation issues a fresh secret and revokes the old one immediately. Any client
still using the old key starts getting `401 Unauthorized` within seconds, so
coordinate rotation with whoever consumes the key.

For more on enabling and operating authentication, see
[Authentication](../features/auth.md).

---

## Rate limits

Rate limits protect both your budget and your upstream provider quotas. They
operate at several independent layers, so you can put a ceiling exactly where
you need one.

- **Per API key.** A token-bucket limit attached to a single key. Useful for
  containing a runaway integration without revoking its key. Off unless you
  set it.
- **Per IP.** A backstop that sits in front of authentication so an
  unauthenticated flood never reaches the rest of the system.
- **Per LLM route.** An in-process queue for each model backend that smooths
  autonomy bursts instead of forwarding them upstream as provider errors.
- **Per project.** A cap on how many tasks a project can create per minute and
  per hour, shared across all the ways tasks get created.

The per-IP backstop is configured on the daemon:

```yaml
api:
  rate_limit:
    per_ip:
      rps: 20
      burst: 40
      trusted_proxies:
        - "10.0.0.0/8"
        - "127.0.0.1/32"
```

Notes:

- Setting either `rps` or `burst` to `0` disables the per-IP layer entirely —
  both must be non-zero for it to enforce anything.
- If vornik sits behind a reverse proxy or load balancer, list the proxy's
  address ranges in `trusted_proxies`. Otherwise every request appears to come
  from the proxy and the per-IP limiter cannot tell clients apart.

The per-project task-creation limiter warns as you approach the cap (logged
and surfaced in the UI) and only blocks once you exceed it, so you get a heads
up before requests start failing.

---

## Hard spend caps

The strongest cost control is a per-project USD cap. Caps are set in the
**project** configuration (not the daemon config) under a `budget` block, with
separate soft and hard limits on daily and monthly spend:

```yaml
budget:
  daily_soft_usd: 15.0
  daily_hard_usd: 25.0
  monthly_hard_usd: 400.0
  timezone: "Europe/Prague"     # when the daily/monthly windows reset
```

- A **soft** cap is advisory: tasks keep running, but vornik logs the breach and
  raises an operator alert.
- A **hard** cap is enforced: once the project's committed plus in-flight spend
  would exceed it, new tasks are **refused with `429 BUDGET_EXCEEDED`**. The cap
  is enforced identically across every way a task can be created — the API, the
  dispatcher, and autonomy — so a project can't slip past it through a side door.

A zero on any dimension means "no cap" for that dimension. Set the `timezone` to
your billing locale so daily and monthly windows reset when you expect. Caps are
editable from the project's config page in the UI.

---

## Dynamic per-role tool budgets

Each role runs with a ceiling on how many tool-calling iterations a task may use.
Dynamic tool budgets scale that ceiling by the **complexity** of the task, so a
trivial task doesn't get the same loop budget as an open-ended one:

```yaml
tool_budget:
  enabled: true              # default false → roles use their static budget unchanged
  max_factor: 2.0            # hard ceiling on any scale factor
  autonomy_max_factor: 1.5   # tighter ceiling for unattended tasks
  min_step_timeout_seconds: 300
```

When enabled, the role's configured budget is treated as the **`complex` (1.0×)**
anchor, and the planner's complexity verdict scales it:

| Tier | Factor |
|------|--------|
| `trivial` | 0.25× |
| `standard` | 0.5× |
| `complex` | 1.0× |
| `open_ended` | 2.0× |

Two safety properties worth knowing: an absent or unrecognised verdict resolves
to **1.0×** (never a silent downscale — only an explicit `trivial`/`standard`
verdict reduces the budget), and the same factor scales the coupled step timeout.
The optional learned-budget signal from the [instinct layer](../features/instinct.md)
is **advisory and off by default** — it never changes a budget on its own.

!!! note "Recalibration when you first enable it"
    `standard` was previously the 1.0× reference; it is now 0.5×. Turning the
    feature on therefore *halves* the budget for tasks the planner rates
    `standard`. That's intended (most tasks are small), but expect the change in
    behaviour the first time you enable it.

---

## Effective cost per success

Raw spend doesn't tell you whether money is being *wasted* — a workflow that
fails half its runs costs far more per useful result than its sticker price.
vornik tracks an **effective cost per success** (cost divided by successful
outcomes) per role and model, and surfaces a drift ratio that compares the last
24 hours to a 7-day baseline.

The spend dashboard at `/ui/spend` shows the drift ratio as a coloured badge per
role/model (green below 1, amber 1–2, red above 2), and the project detail page
carries a 24-hour cost-per-success summary. A drift monitor can also alert when
the ratio crosses a threshold — see the `effective_cost.*` keys in the
[Configuration reference](../reference/configuration.md).

---

## Where to go next

- [Storage and retention](storage-and-retention.md) — keep historical cost and
  task data from growing without bound.
- [Workflows and LLM controls](workflows-and-llm-controls.md) — choose which
  models run where.
- [Configuration reference](../reference/configuration.md) — every key and its
  default.
