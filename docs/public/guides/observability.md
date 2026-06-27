---
sources:
    - path: internal/observability/metrics.go
      sha256: 71ab1bc0f72aea69510677c929b401a7ab7030c1371905197f6504c5a71c120b
    - path: internal/ui/spend.go
      sha256: 94800af6c00c8266261e8a4f7d16bb35d80b91e7001203788fd71dc578a5ec67
---
# Observability

vornik is built to run unattended, so it gives you several ways to see what it's
doing: a set of operator dashboards in the Web UI, and a Prometheus metrics
endpoint for your own monitoring stack.

## The dashboards

### Home

The Web UI landing page (`/ui/`) is an operator front page of live tiles:
active tasks (queued, leased, running), active chats, a countdown to the next
[autonomy](autonomy.md) evaluation per project, lifetime cache savings, a memory
pipeline snapshot, and an **Inbox** tile for anything that needs you.

### Insight

The **Insight** area collects the analytical views:

- **Spend** (`/ui/spend`) — the cost deep-dive (below).
- **Trends** (`/ui/insights/trends`) — daily throughput and success-rate over a
  trailing window, plus a recovery-events series when recovery tracking is
  wired.
- **Tool budget** (`/ui/insights/tool-budget`) — a histogram of how many tool
  calls executions actually use, plotted against the
  [complexity-tier budget](cost-and-caching.md#dynamic-per-role-tool-budgets),
  with mean / P50 / P95. Use it to tune budgets to reality.

### Health

Admin-gated health tabs under `/ui/admin/health/` cover the daemon's operational
state: **leases** (lease status and recent lease transitions — the surface for
diagnosing stuck or lost work), the stuck-execution **watchdog**, **MCP** server
reachability, **runtime** probes, and **cluster** leader/heartbeat status.

## Spend and cost-efficiency

The spend dashboard (`/ui/spend`) slices cost by window (24h / 7d / 30d),
project, source, task, and role+model. Alongside total cost and token counts it
surfaces a few efficiency signals:

- **Input ratio** — prompt tokens as a share of total; a persistently high ratio
  flags context bloat.
- **Cache hit ratio** and **dollars saved** — how much
  [prompt/embedding caching](cost-and-caching.md) is actually saving.
- **Effective cost per success** and its **drift ratio** — cost per *useful*
  result (spend divided by successful outcomes), and how the recent window
  compares to a baseline. A cheap model that fails often can cost more per
  success than an expensive one that doesn't; the drift badge (green/amber/red)
  flags when that's getting worse. See
  [Cost and caching](cost-and-caching.md#effective-cost-per-success).

From the CLI, `vornikctl cache-stats` reports cache effectiveness. (Note:
`vornikctl cpc` is the cross-project-call ledger, not a cost command — there is no
`vornikctl spend`.)

## Prometheus metrics

vornik exposes Prometheus metrics at **`/metrics`**. Enable the dedicated metrics
server and point your scraper at it:

```yaml
metrics:
  enabled: true
  addr: ":9090"            # dedicated metrics listener; scrape http://<host>:9090/metrics
  require_admin: false     # when true (with auth on), the main-port /metrics needs a token
  # scrape_token: "…"      # a read-only credential accepted alongside the admin key
```

`/metrics` is also mounted on the main API port. When you run with
[authentication](../features/auth.md) on and set `require_admin: true`, the
main-port endpoint requires the admin key or a dedicated scrape token; the
separate `metrics.addr` listener is the intended scrape target — bind it to a
trusted interface.

The exported metrics cover, in plain terms:

- **Task and execution lifecycle** — active, started, completed, failed,
  cancelled, retried counts, and durations.
- **Queue, scheduling, and leases** — queue depth by status, how long tasks wait
  before running, leases acquired and expired, and recoveries of expired leases.
- **LLM cost and tokens** — dollar spend per project and per model, prompt and
  completion tokens, tool-calling iterations, and cache savings.
- **Model quality** — per role and model: success rate, effective cost per
  success, and rates of parse failures, schema violations, refusals, and
  degenerate loops.
- **Tool calls, autonomy, and cross-project orchestration** — tool invocation
  counts, autonomy approval resolutions and configured caps, and cross-project
  call and spawn counts.
- **HTTP and process** — request counts, durations, in-flight requests, plus
  standard Go/process collectors.

OpenTelemetry tracing can be enabled alongside metrics with `tracing.enabled` and
`tracing.endpoint`.

See the [Configuration reference](../reference/configuration.md) for every
`metrics.*` key and default.
