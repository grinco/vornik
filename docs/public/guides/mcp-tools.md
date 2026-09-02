---
sources:
    - path: internal/registry/project.go
      sha256: 33ecc909ea6e423e344b89ece4a6723cc2d0be687913af4c7a30b9c3546da77b
    - path: internal/mcp/client.go
      sha256: f73d74f91615b04461455c8b986189c9f6d7fe6f09139ce122ca826b25f1a3bc
    - path: internal/mcp/ratelimit.go
      sha256: 19ad0c64e2abd9d25e95971e1ba5e3cfe91f34852edeca6e4f9c70825b2e901d
    - path: internal/cli/mcp.go
      sha256: de369186aea47ddb291a199cb73fb1e34b7b0e0823ccfcc859feb49568f4e28e
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

  If a stdio server exits or closes stdout on its own, the daemon **re-launches
  it automatically** on the next tool call and retries that call once. You do
  not need to restart the daemon to recover a crashed stdio server. Concurrent
  calls against a server that has just died share one relaunch rather than
  starting a subprocess each. If the relaunch itself fails — the command is
  gone, or it exits immediately — the tool call returns an error saying the
  server is not running, and the daemon log carries the launch failure.
- **`sse`** — the daemon connects to a long-running server at `url` over HTTP.
  Use this for tools that run as their own service.

`command`/`args`/`env` apply only to `stdio`; `url` applies only to `sse`.

### `project_id` is supplied by the daemon

If a server declares a `project_id` argument, the daemon fills it in on every
call from the calling project and **removes it from the schema the model sees**.
Servers use it for tenant isolation, quota accounting and per-project state, so
it is the daemon's to state rather than the model's to claim — a model naming
another project would otherwise be billed to it and could reach its state. It is
also simply unknowable to a chat model, and leaving it as a `required` field
made such tools fail validation outright.

Nothing is required of a server author beyond declaring the argument as usual;
if you call a tool by hand through `vornikctl mcp call -p <project>`, omit it.
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

## Authenticating to a server

Most hosted MCP servers require credentials. Add an `auth:` block to the server
entry. Omitting it (the default) means "no authentication", which is how every
server behaved before this block existed.

Credentials are **never written in the config**. Every credential field takes a
`secret://<name>` reference; the value lives in the secret store, and the name
must appear in the project's `permissions.secrets`. That is what lets a server
definition travel through the control plane as a reviewable diff without
leaking anything.

### `mode: static` — a bearer token or API key

For a server that authenticates with a fixed header (n8n's MCP Server Trigger,
and most servers that predate OAuth support):

```yaml
permissions:
  secrets: ["N8N_MCP_TOKEN"]        # required — the reference is checked against this
mcp:
  servers:
    - name: "n8n"
      transport: "streamable-http"
      url: "https://n8n.example.com/mcp/abc"
      auth:
        mode: static
        header: "Authorization"     # optional; Authorization is the default
        value_from: "secret://N8N_MCP_TOKEN"
        value_prefix: "Bearer "     # optional; note the trailing space
```

The resolved header is attached to every request for that server, and it wins
over a header of the same name set anywhere else.

### `mode: env` — credentials for a stdio server's own upstream

A stdio MCP server often holds its *own* account with a third party (a YouTube,
Reddit or Instagram wrapper holding a Google or Reddit app). There is no
handshake for vornik to run — the job is to hand the subprocess its secrets:

```yaml
permissions:
  secrets: ["REDDIT_CLIENT_ID", "REDDIT_CLIENT_SECRET"]
mcp:
  servers:
    - name: "reddit"
      transport: "stdio"
      command: "reddit-mcp"
      auth:
        mode: env
        env_from:
          REDDIT_CLIENT_ID: "secret://REDDIT_CLIENT_ID"
          REDDIT_CLIENT_SECRET: "secret://REDDIT_CLIENT_SECRET"
```

Resolved values are passed to the subprocess verbatim — unlike the legacy `env`
map, they are **not** `${VAR}`-expanded, so a credential containing `$` arrives
intact. Prefer `env_from` over `env` for anything secret: `env` resolves from
the daemon's own environment, so two projects on one daemon necessarily share
the value, and it bypasses the `permissions.secrets` check.

### What is validated, and when

Mistakes fail at config load rather than at the first tool call:

- `mode` outside `none | oauth | static | env`.
- `mode: env` on a non-stdio server, or `static`/`oauth` on a stdio one.
- A credential field holding a literal or a `${VAR}` placeholder instead of a
  `secret://` reference.
- A `secret://` name that is not in the project's `permissions.secrets`.
- A `header` the MCP protocol owns (`Content-Type`, `Accept`,
  `MCP-Protocol-Version`, `Mcp-Session-Id`) — it would be dropped before the
  request was sent.
- A field belonging to a different mode, which would otherwise be ignored
  silently.

A secret that is *missing at runtime* is reported separately: the server is
withheld rather than connected without its credential, and the daemon log names
the secret. A server that 401s on every call would look like a permissions
problem at the vendor; this points at the actual cause.

### `mode: oauth` — the consent flow

Most hosted MCP servers (Atlassian, Linear, Notion, Slack, GitHub, …) are OAuth
2.1 resource servers. Declare the block, then connect once per project:

```yaml
mcp:
  servers:
    - name: "atlassian"
      transport: "streamable-http"
      url: "https://mcp.atlassian.com/v1/mcp/authv2"
      auth:
        mode: oauth
        scopes: ["read:jira-work", "offline_access"]
```

Vornik discovers the authorization server, registers itself as an OAuth client
where the vendor supports it, and stores the resulting token per
`(project, server)`. That last part matters: one operator consents once, and
every task in the project uses that grant — including autonomous and cron runs,
which have no user to borrow a token from.

**Precondition.** `server.public_base_url` must be set to an `https://` origin
(loopback also works for local testing). OAuth 2.1 requires a redirect URI the
vendor can reach, and Vornik's callback is
`<public_base_url>/auth/mcp/callback`. Connect is refused up front when it is
unset — better than failing after you have already consented at the vendor.

Connect from the CLI:

```
$ vornikctl mcp connect atlassian -p my-project
Connecting MCP server "atlassian" for project my-project

  Resource:     https://mcp.atlassian.com/v1/mcp/authv2
  Scopes:       read:jira-work offline_access
  Redirect URI: https://vornik.example.com/auth/mcp/callback

Open this URL to consent:

  https://auth.atlassian.com/authorize?...

Waiting for the callback (up to 5m0s)…
```

Open the URL, consent, and the daemon's own callback completes the exchange. The
command then **verifies** the recorded grant against what it displayed and exits
non-zero if the resource or scopes differ — it never sees the token, so a
matching record is the only evidence it can offer you.

The control-plane MCP tab (`/ui/admin/control-plane?section=mcp`) has the same
Connect/Disconnect buttons plus an authorized-vs-reachable status per row. Note
that "reachable" is auth-blind: a server that 401s every call is still
reachable, which is why the row shows the grant separately.

Other commands:

```
vornikctl mcp oauth-status atlassian -p my-project   # resource, scopes, expiry
vornikctl mcp disconnect atlassian -p my-project     # deletes the stored grant
```

**Servers with no dynamic registration** (Slack, GitHub, Box) need a
pre-registered client — set `client_id`, and `client_secret_from` when the
vendor issued a secret. **Servers with no discovery at all** (Intercom) need
`authorization_endpoint` and `token_endpoint` set explicitly. Vornik tells you
which case you are in rather than failing generically.

**Refresh and reconnect.** The credential is resolved **per request**, so an
access token refreshes shortly before it expires no matter how long the daemon
has been running, and rotated refresh tokens are handled. If the vendor rejects
a token Vornik still believes in — a grant revoked in their console, say — the
call is retried **once** after a forced refresh, and no more than once. If the
vendor revokes the grant outright, the server is flagged `needs_reconnect`: its
calls fail rather than retrying forever, `mcp oauth-status` says so, and
`mcp connect` fixes it. Disconnecting in Vornik does **not** revoke the
authorization at the vendor — do that in their console too if you want it
withdrawn there.

> **Changed in 2026.8.10.** Before this, the credential was resolved when the
> MCP client was *wired* — at boot, on config reload, and on consent — and
> frozen into that client's headers. A daemon that ran longer than the vendor's
> access-token lifetime therefore presented a dead bearer until something
> happened to trigger a reload, and every status surface reported the grant
> healthy throughout. If you are on an older build, reload config on a schedule
> or upgrade.

**When a connector loses auth, you are told.** A rejected credential is no
longer something you have to go looking for:

- The step that hit it **fails**, naming the connector and the
  `vornikctl mcp connect` line that fixes it — rather than the agent narrating
  a 401 into its output while the task reports success. A step that
  deliberately degrades across connectors can opt out of the failure with
  `auth_failure_mode: continue`; it stays visible in the two surfaces below
  either way.
- `vornikctl doctor` gains a **`connector_auth`** check. It reads the stored
  grant *and* recent authentication failures, so a connector that is failing
  right now is reported as an error even when its stored grant looks perfect.
- The operator alert channel gets a **push** the first time a connector starts
  failing, rate-limited to one message an hour per connector.

**If you change a server's `url`, reconnect it.** Vornik refuses to present a
grant whose origin no longer matches the configured URL, so moving a server to a
different host fails loudly. A change to the PATH only (`/v1` → `/v2` on the same
host) is not detected — if that vendor scopes access per path, run
`vornikctl mcp connect` again after the edit.

**A note on the grant record.** Every Connect and Disconnect is recorded in the
admin audit trail with who consented, to which resource, and with which scopes —
never the token. That is what makes a grant auditable after the fact.

### Daemon-level servers

The same `auth:` block works in `config.yaml`'s `mcp.servers`. Note the
difference in blast radius: a daemon-level server is reachable from **every**
project, so a credential attached there shares one account with all of them.
Prefer a project-scoped server for anything account-bearing.

The control-plane MCP tab can edit `auth:` blocks directly. The project-config
form has no auth fields, but it preserves a block you hand-wrote rather than
dropping it on an unrelated save.

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
transport, its status, how many tools it advertises, and its endpoint. Remember
that a server appearing here is **not** automatically available to a project —
grant access by adding it to that project's `mcp.servers`.

The status column has three values, and the third is easy to mistake for a
fault:

| Status | Meaning |
| --- | --- |
| `reachable` | The last health probe connected and listed tools. |
| `unreachable: <error>` | The last probe ran and failed, for the reason shown. |
| `checking` | **No probe has run yet.** The first probe after a daemon start or a config reload is asynchronous, so every server reads `checking` for a moment. Re-run the command rather than troubleshooting the server. |

The same three states appear on the Control Plane → MCP tab, where `checking`
renders as a neutral "checking…" badge. In `--json` output, a server that has
never been probed simply has no `last_checked_at` field.

`checking` covers the *first* probe only. Once a server has a verdict, a
re-probe does not change what you see: the row keeps showing the previous result
and the time it was taken, until the new probe lands and replaces it. So a
failing server reads `unreachable` continuously rather than flickering — the
`last checked` timestamp is what tells you how old that verdict is.

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
