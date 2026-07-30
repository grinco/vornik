---
sources:
    - path: internal/registry/project.go
      sha256: 3fda8198ac3b13b11f7b85239d2dd7718c7c7662bde56295873112decae05bc3
    - path: internal/mcp/client.go
      sha256: 5d5e53e20bf973d0babfe3951651d7e5897c8c0a3520626272eb1dbbe882700c
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
  environment, so secrets stay out of the project file. On disconnect the
  daemon terminates the server's whole process group, so launchers that fork
  a worker process (`npx`, `tsx`, shell wrappers) are cleaned up with it —
  don't rely on an MCP subprocess outliving its connection.
- **`sse`** — the daemon connects to a long-running server at `url` over HTTP.
  Use this for tools that run as their own service.

`command`/`args`/`env` apply only to `stdio`; `url` applies only to `sse`.
`name` must be unique within the project — it's how tools are namespaced and
how you target a server from the CLI.

### Per-server request timeout

`sse`/`streamable-http` servers use a **30-second** per-request timeout by
default. If a server has tools that legitimately run longer — e.g. a scraper
whose `web_fetch` hits slow or anti-bot sites — raise it with
`timeout_seconds` so a slow call completes instead of failing with
`context deadline exceeded`:

```yaml
mcp:
  servers:
    - name: "scraper"
      transport: "sse"
      url: "http://127.0.0.1:8787"
      timeout_seconds: 90      # 0 or omitted = 30s default
```

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

### Noticing when a server's tool set changes under you

`allowed_tools` is a list *you* maintain against a catalogue the *server* owns. A
third-party server can add a tool in any release — including a destructive one —
and your allow-list will quietly keep filtering it out. That is the correct
behaviour, but you probably want to know it happened, because the credential that
server uses may still permit whatever the new tool does.

Vornik classifies every advertised tool as read-only or mutating at connect time,
using the server's own `readOnlyHint` / `destructiveHint` annotations where it
supplies them and the tool name otherwise. Anything it cannot recognise as a read
is treated as mutating — the safe direction, since the tool nobody anticipated is
exactly the one a denylist would miss.

Names are split on separators *and* on camelCase, so `drive_search`,
`calendar_listEvents` and `calendar.listEvents` are all read correctly. Servers
mix these conventions freely and you do not get to choose which one yours uses.

When a server advertises a mutating tool your `allowed_tools` does not name, the
daemon logs a warning listing them. Nothing is exposed to the model, but the
change is visible.

If a server's annotations **disagree** with what the tool name suggests — declaring
a `delete`-looking tool read-only, or a `get`-looking one destructive — the daemon
logs that too, at INFO. The server is believed either way, because it knows its own
semantics; the log exists so a third party cannot quietly reclassify a destructive
tool as safe without leaving a trace.

For a server whose tool set you do not control and want to be strict about, add:

```yaml
      require_declared_tools: true
```

With that set, the daemon **refuses to register the server** if it advertises a
mutating tool while `allowed_tools` is empty — that combination means "expose
everything", and everything now includes something destructive. If
`allowed_tools` *is* set, registration proceeds and you get the warning, because
the allow-list is already keeping the tool away from the model and taking a whole
integration offline because an upstream release added a tool would be worse than
the problem.

It is **off by default**, deliberately: plenty of servers legitimately expose
mutating tools with no allow-list — a page publisher, a home-automation bridge —
and those should keep working untouched.

### Tools that reach your filesystem

Some MCP tools take a path on **your host** rather than a remote identifier —
`localPath`, `filePath`, `outputPath` and similar. A stdio server runs as a child
of the daemon, with the daemon's OS user, so such a tool is file access with the
daemon's permissions: it can read whatever that user can read (including the
daemon's own config and secrets) and overwrite whatever that user can write.

This is easy to miss, because a tool's name usually gives no hint. Google's
Workspace MCP server, for example, advertises four tools that write a
caller-chosen absolute path — and all four begin with `download` or `get`, which
read as harmless fetches. None of them annotates itself.

Vornik therefore treats **any tool taking a host filesystem parameter as
mutating**, whatever its name suggests, and this cannot be overridden by the
server's `readOnlyHint`. That hint is a claim about the *remote* service; a third
party has no standing to declare a write to your disk safe. At connect time the
daemon logs these tools at WARN, naming the parameter, and separately flags any
that your `allowed_tools` actually exposes.

The practical rule: **keep host-path tools out of `allowed_tools` unless you
specifically want a model writing files as your daemon user.** If you do want one,
declare it explicitly, so the choice is recorded rather than inherited from an
expose-everything default.

A generic parameter name such as `path` or `dir` is treated the same way. Where
that parameter is really a *remote* path, the cost is one line in `allowed_tools`;
the opposite mistake costs arbitrary file access.

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
