---
sources:
    - path: internal/runtime/container.go
      sha256: 9ea37c3b5fab4c0d529f71a9e0c4f7c2812e12b0b22886dac5207e32ccef37b4
    - path: internal/runtime/manager.go
      sha256: b3e511138514c5e0ab45cd71647f89e1fd63d61b3b23ee070426bcd52578be58
---
# Zero-egress / local-first execution

!!! note "Community Edition"

    Included in the free, open-source **Community Edition**. See [Editions](../editions.md).


vornik is built to run agents that have **no direct path to the internet**. Each
agent runs in its own rootless Podman container; in the zero-egress posture that
container has no network device at all. Anything that genuinely needs to leave
the host — an LLM completion, an MCP tool call, a broker request — is made by the
**daemon**, not the agent. The agent asks the daemon over a local socket; the
daemon is the only process with outbound access.

This makes vornik a natural fit for air-gapped and data-sensitive environments,
especially when paired with self-hosted, open-weight models.

## Container hardening

Every agent container is launched with the same baseline regardless of network
mode:

- `--cap-drop ALL` — no Linux capabilities,
- `--security-opt no-new-privileges` — no privilege escalation,
- a user-namespace mapping, and an optional non-root `--user`,
- a small set of scoped bind mounts (the step's input, output, and workspace);
  the project's `.git` is mounted read-only.

## Network modes

A role's network access is one of five modes:

| Mode | What the container gets |
|------|-------------------------|
| `none` | no network device — fully isolated |
| `daemon-only` | no network device, **plus** a bind-mounted socket to the local daemon |
| `egress` | its own network namespace with outbound internet |
| `host` | shares the host network (denied unless explicitly allowed) |
| *(unset)* | inherits the daemon-wide default |

**`daemon-only` is the zero-egress workhorse.** On rootless Podman there is no
network setting that reaches the host daemon while blocking the internet, so
vornik removes the network device entirely and gives the agent a Unix socket to
the daemon instead. The daemon rewrites the agent's API, LLM, and memory
endpoints to that socket, then performs the real outbound calls itself. If no
daemon socket is configured the mount is simply omitted and the container stays
fully isolated — it **fails closed**, never open.

### Choosing modes per role

Set the mode on a swarm role, and the daemon-wide default for everything that
doesn't override it:

```yaml
# daemon config
runtime:
  default_network: daemon-only      # zero-egress by default
```

```yaml
# a swarm role that genuinely needs to fetch packages
roles:
  - name: builder
    runtime:
      network: egress
```

A typical setup runs reasoning/review roles on `daemon-only` and grants `egress`
only to the few roles that must install dependencies. An invalid
`default_network` value **fails closed to `daemon-only`**, so a typo can't
silently restore egress.

Two guardrails to know:

- **`daemon-only` requires `server.unix_socket`** to be set (that's the socket
  the container connects to). A role whose LLM endpoint is a direct external URL
  must use `egress`, not `daemon-only`.
- **`host` is denied by default.** Because host networking defeats the sandbox,
  it only works when the operator sets `VORNIK_ALLOW_NETWORK_HOST=1` on the
  daemon — a deliberate host-level trust decision.

> The daemon does not force zero-egress on you: with `runtime.default_network`
> unset, containers fall back to the historical rootless-egress behaviour. The
> zero-egress posture is enabled by setting `runtime.default_network:
> daemon-only` (which the shipped configuration template does).

## Local-first / open-weight models

Because the agent's LLM endpoint is plain OpenAI-compatible HTTP, pointing vornik
at a self-hosted model — Ollama on a LAN host, vLLM, or any compatible endpoint —
is configuration only:

```yaml
runtime:
  agent_llm:
    endpoint: "http://192.168.10.20:11434/v1"   # e.g. Ollama on the LAN
    model: "qwen3.6:35b"
    context_size: 262144
```

Any field left empty under `runtime.agent_llm` falls back to the daemon's
`chat.*` settings, so a single `chat:` section is enough when agents share the
daemon's model. Individual roles can override the model with a `model:` key. In a
`daemon-only` deployment these calls are made by the daemon, so the model
endpoint only has to be reachable from the daemon host — never from the agent
containers.

## What crosses which boundary

In the zero-egress posture:

- **Out of the container:** nothing over the network (there is no network
  device). The only channel is the local socket to the daemon, and filesystem
  access is limited to the step's scoped mounts.
- **Out of the daemon:** all real egress — LLM completions, MCP tool calls,
  broker/external API requests — performed by the daemon on the host.

The orchestrator holds the credentials and the network reach; the agent runtime
does not.
