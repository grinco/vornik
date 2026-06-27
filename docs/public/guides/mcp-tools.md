---
sources:
    - path: internal/registry/project.go
      sha256: a6b13685371ebdecfddfa9870a8ae7fc37653f83374519c7d9cb9dc6ec0aeb82
    - path: internal/mcp/client.go
      sha256: 58b7947e57225689fc091810856c14ef3e7e3f3c0c8ca1eb35d5391d214fde93
    - path: internal/mcp/ratelimit.go
      sha256: 19ad0c64e2abd9d25e95971e1ba5e3cfe91f34852edeca6e4f9c70825b2e901d
    - path: internal/cli/mcp.go
      sha256: 7b14785dee6f77dc7c252e92c0c0cf1b270b230216440a9d6b8c262b284baed0
---
# Connect Your Tools (MCP)

vornik gives a project its capabilities through **MCP servers** — external
processes (or HTTP endpoints) that expose **tools** a model can call: fetch a
web page, place a broker order, query a database, and so on. This guide covers
the three things you do to wire tools into a project safely:

1. **Declare** the MCP servers a project may use.
2. **Restrict** each server to an explicit set of tools.
3. **Throttle** individual tools so a runaway loop can't hammer them.

It finishes with the `vornikctl mcp` commands for inspecting what a project can
actually see and calling a tool by hand while debugging.

Tools are scoped **per project**: a server you declare in one project is not
visible to another. There is also a daemon-level server inventory (see
[Inspecting from the CLI](#inspecting-from-the-cli)), but listing a server
there does **not** grant any project access to it — access always comes from
the project's own `mcp.servers` list.

## Declaring MCP servers

Add an `mcp:` block to your project file. Each entry under `servers` is one
MCP server:

```yaml
mcp:
  servers:
    - name: "scraper"           # unique within the project
      transport: "stdio"        # subprocess launched by the daemon
      command: "/usr/local/bin/acme-scraper"
      args: ["--feeds", "/etc/acme/news-feeds.yaml"]
      env:
        SCRAPER_TOKEN: "${ACME_SCRAPER_TOKEN}"
    - name: "broker"            # an HTTP/SSE server already running
      transport: "sse"
      url: "http://127.0.0.1:7081/sse"
```

There are two transports:

- **`stdio`** — the daemon launches `command` (with `args`) as a child process
  and speaks MCP over its stdin/stdout. Use this for tools you ship alongside
  vornik. `env` values support `${VAR}` expansion from the daemon's own
  environment, so secrets stay out of the project file.
- **`sse`** — the daemon connects to a long-running server at `url` over HTTP.
  Use this for tools that run as their own service.

`command`/`args`/`env` apply only to `stdio`; `url` applies only to `sse`.
`name` must be unique within the project — it's how tools are namespaced and
how you target a server from the CLI.

## Restricting tools with `allowed_tools`

By default a server exposes **every** tool it advertises. In almost all cases
you want to narrow that to the tools the project actually needs. Add
`allowed_tools` to a server:

```yaml
mcp:
  servers:
    - name: "broker"
      transport: "sse"
      url: "http://127.0.0.1:7081/sse"
      allowed_tools:
        - "get_quote"
        - "get_position"
        - "place_order"
        - "cancel_order"
```

An empty or omitted `allowed_tools` means "expose all tools." When it's set,
vornik enforces it in two places:

- **The catalog the model sees is filtered.** Only the allow-listed tools are
  advertised to the model, so it never learns the others exist. A smaller, more
  focused tool list also makes the model more reliable.
- **Calls are checked again at invocation time.** Even if a model hallucinates
  a tool name that isn't in the catalog, vornik rejects the call *before* it
  reaches the server — so a tool the server would happily run under a broad
  credential can never be reached just because the model guessed its name.

Keeping `allowed_tools` tight is the single most effective control here: it is
both a safety boundary and a reliability win.

## Throttling tools with `toolRateLimits`

Some tools are expensive or sensitive — placing orders, scraping the web. Give
them an in-daemon ceiling so vornik degrades gracefully instead of leaning on
the upstream server to push back. Add a `toolRateLimits` map to the `mcp`
block:

```yaml
mcp:
  servers:
    - name: "broker"
      transport: "sse"
      url: "http://127.0.0.1:7081/sse"
      allowed_tools: ["get_quote", "place_order"]
    - name: "scraper"
      transport: "stdio"
      command: "/usr/local/bin/acme-scraper"
      allowed_tools: ["web_fetch"]
  toolRateLimits:
    broker.place_order:        # most specific: this server's tool
      rps: 2
      burst: 5
    broker.get_quote:
      rps: 20
      burst: 40
    web_fetch:                 # server-agnostic: any tool named web_fetch
      rps: 2
      burst: 5
```

Each entry is a token bucket with two integer fields, **`rps`** (steady
sustained rate) and **`burst`** (the bucket size — how many calls can fire
back-to-back before throttling kicks in). Both must be greater than zero for an
entry to take effect; a zero or negative value disables it.

Keys are matched most-specific first:

1. **`server.tool`** (e.g. `broker.place_order`) — applies to that one tool on
   that one server, with its own isolated bucket.
2. **`tool`** (e.g. `web_fetch`) — a server-agnostic ceiling for any tool of
   that name; entries that match this way share one bucket.
3. **No entry** — the tool is unlimited.

When a bucket is drained, vornik refuses the call locally (it never reaches the
server) and returns a rate-limit error carrying a `Retry-After` hint, rounded
up to whole seconds. The agent recognizes that error and backs off rather than
amplifying the burst.

## Inspecting from the CLI

`vornikctl mcp` lets you see exactly what a project can reach and call a tool by
hand — invaluable when a tool isn't behaving as expected.

**List the tools a project's servers advertise** (after `allowed_tools`
filtering — this is what the model actually sees):

```
vornikctl mcp tools -p acme
```

`-p/--project` is required. Add `--json` for machine-readable output. The table
shows each tool and its description.

**List the daemon-level server inventory**, with reachability and tool counts:

```
vornikctl mcp servers
```

This is daemon-scoped (there is no `--project` flag); it reports each server's
transport, whether it's `reachable`, how many tools it advertises, and its
endpoint. Remember that a server appearing here is **not** automatically
available to a project — grant access by adding it to that project's
`mcp.servers`.

**Call a tool directly**, skipping the model — the fastest way to confirm a
tool works and check its arguments:

```
vornikctl mcp call -p acme --tool mcp__scraper__web_fetch \
    --args '{"url":"https://example.com","text_only":true,"max_bytes":2000}'
```

`--project` and `--tool` are required. The tool name is the fully qualified
`mcp__<server>__<tool>` form. `--args` takes a JSON object (default `{}`). Add
`--json` for raw output.

## Notes and gotchas

- **YAML key casing is mixed.** The per-server allow-list is `allowed_tools`
  (snake_case); the rate-limit map is `toolRateLimits` (camelCase). It's easy
  to get one wrong — the project will fail to load with a clear error if you
  do.
- **`transport` is exactly `stdio` or `sse`.** Any other value is rejected.
- **Rate-limit fields are `rps` and `burst`, both integers > 0.** A negative
  value is rejected at load time.
- **Changes take effect on config reload** — you don't need to restart the
  daemon to add a server, tighten `allowed_tools`, or adjust a limit.
