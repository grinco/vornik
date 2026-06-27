# Configuration

How to configure a Vornik Community daemon. This page covers *where* config lives
and the *shape* of it; the full, generated key reference is in
[Configuration reference](reference/configuration.md) (emitted from the config
schema, so it never drifts).

## Where configuration lives

Vornik searches for its config file in this order (first match wins):

1. the path in the `$VORNIK_CONFIG` environment variable,
2. `./config.yaml` (or `./vornik.yaml`) in the working directory,
3. `/etc/vornik/config.yaml`,
4. an XDG/home location (`$XDG_CONFIG_HOME/vornik/` or `~/.config/vornik/`).

Selected settings can be overridden by environment variables (for example the
server address and database connection), which take precedence over the file —
useful for container and systemd deployments. The exact override variables are
listed in the [Configuration reference](reference/configuration.md).

Config is **hot-reloadable**: send `SIGHUP` (or `vornikctl config reload`) and the
daemon re-reads the file and re-applies what can change without a restart. Use
`vornikctl config show` to see the effective configuration.

## What you configure

- **`api`** — bind address / ports for the HTTP API, and whether an API key is
  required (`api.auth_enabled` — turn it on for any network-reachable deployment).
- **`runtime.agent_llm`** — the OpenAI-compatible endpoint, model, and key the
  agents call. `chat` configures the conversational/router provider.
- **`storage` / database** — where durable task state is kept.
- **`secrets`** — secret-detection on conversation channels (detect / redact /
  block).
- **MCP / tools** — the tool endpoints the daemon brokers for agents.

See the [Configuration reference](reference/configuration.md) for every key, its
type, and its default.

## Secrets

Never commit secrets. Sensitive values have **environment-variable overrides** —
set those at runtime instead of inlining the value in a committed config file.
The [reference](reference/configuration.md) flags each one (for example the
database password and object-store credentials say "prefer the … environment
variable").

```yaml
# config.yaml — point at your model host; keep the key out of the file
runtime:
  agent_llm:
    endpoint: http://localhost:11434/v1
    model: <your-model-id>
    # api_key: set via its environment-variable override, not inline
```

```sh
# inject the secret at runtime (never committed); then start the daemon
export VORNIK_DATABASE_PASSWORD=...
vornik
```
