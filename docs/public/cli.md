# CLI reference

Vornik ships two binaries:

- **`vornik`** — the orchestration daemon (Community Edition main).
- **`vornikctl`** — the control CLI: submit tasks, inspect state, manage the daemon.

## `vornik` (daemon)

The daemon takes no required flags. It finds its config file via the search order
in [Getting started](getting-started.md#first-run) (`$VORNIK_CONFIG`, then
`./config.yaml`, then `/etc/vornik/config.yaml`, then an XDG/home location).

| Invocation | Effect |
|---|---|
| `vornik` | Start the daemon (loads config, runs the orchestration loop and API). |
| `vornik --version` | Print the build version and edition (Community), then exit. |
| `kill -HUP <pid>` | Reload config in place (also `systemctl --user reload vornik`). |

## `vornikctl` (control)

`vornikctl` is the operator CLI. The common groups:

| Group | Purpose |
|---|---|
| `init` | Scaffold a project or swarm config. |
| `task` | Submit, list, inspect, tail, cancel, and retry tasks. |
| `project` / `swarm` / `workflow` | Manage projects, swarms, and workflows. |
| `config` | Show config and trigger a hot reload. |
| `doctor` | Diagnose daemon health and per-feature readiness. |
| `memory` | Inspect and manage project RAG memory. |
| `key` | Create, list, rotate, and revoke API keys. |
| `mcp` | List MCP servers/tools and call a tool. |
| `version` | Print the CLI version and edition. |

The **complete, generated** command and flag reference — every subcommand, every
flag, every default — is in **[`vornikctl` reference](reference/vornikctl.md)**.
That page is generated from the actual command definitions, so it
never drifts from the binary; this page is just the orientation. Enterprise-only
subcommands are not present in the Community build and do not appear there.

## See also

- [Getting started](getting-started.md)
- [Configuration](configuration.md)
