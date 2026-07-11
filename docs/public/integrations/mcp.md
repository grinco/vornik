# MCP tool servers

!!! note "Not managed in the Integrations Hub"

    MCP tool servers are **not** one of the Integrations Hub's guided
    integrations. The daemon's MCP catalog has its own management surface —
    read on for where, per edition.

vornik reaches external tools over MCP (Model Context Protocol) servers.
Two settings are involved, and they live in different places:

1. **The daemon-level catalog** (`mcp.servers` in `config.yaml`) — the
   shared inventory of servers your installation knows about. Adding a
   server here is a **discovery step, not an access grant**: it makes the
   server visible so projects can reuse it, but connects nothing by itself.
2. **A project's subscription** (`mcp.servers` in the project's YAML) — a
   project only sees the tools of a server it explicitly lists by name.
   See [Connect Your Tools (MCP)](../guides/mcp-tools.md) for how a
   project declares and restricts the servers it actually uses.

## Managing the daemon catalog

- **Enterprise Edition** — use the control-plane hub's **MCP servers**
  tab (`/ui/admin/control-plane`, admin-only). It lists the catalog with
  live reachability badges and tool counts, lets you **test** a candidate
  endpoint before committing (the daemon connects and enumerates the
  server's tools), and files every add/remove as a
  [control-plane proposal](../features/control-plane.md) you review as a
  diff and apply — never a silent config write.
- **Community Edition** — edit `mcp.servers` in `config.yaml` directly:

  ```yaml
  mcp:
    servers:
      - name: home-assistant
        transport: streamable-http   # or "sse", or "stdio" with command:
        url: https://mcp.example.com/mcp
  ```

  See the [configuration reference](../reference/configuration.md) for
  the full field list.

## Good to know

- **Daemon-catalog changes are picked up on daemon restart**, not by the
  hot config reload. A project's own `mcp.servers` subscription list, by
  contrast, does apply on reload. Either way a running task's tool set is
  fixed when its container starts — changes only affect new runs.
- The daemon catalog is admin territory in every edition: it is shared,
  installation-wide state, which is why it never appears in the
  Integrations Hub next to per-project channels like email or Slack.
