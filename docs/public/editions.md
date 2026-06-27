# Editions: Community vs Enterprise

Vornik comes in two editions built from the same core:

- **Community Edition (CE)** — this repository, **AGPL-3.0**. The complete
  orchestration core; fully usable for personal and small-team work.
- **Enterprise Edition (EE)** — a proprietary overlay that adds advanced
  capabilities, offered with commercial support and a hosted SaaS by EaseIT.

## Feature matrix

> The table below lists which capabilities are included in each edition. A
> Community build includes only the rows marked **Community**.

<!-- BEGIN GENERATED editions-matrix -->

| Capability | Community | Enterprise |
|---|:---:|:---:|
| Task orchestration (tasks, leases, durable execution) | ✅ | ✅ |
| Workflows | ✅ | ✅ |
| Tool access over MCP | ✅ | ✅ |
| Control CLI (`vornikctl`) + HTTP API | ✅ | ✅ |
| Counterfactual replay / “Black Box” | — | ✅ |
| Learning / “Instinct” layer (learned budgets) | — | ✅ |
| Clustering / horizontal scale | — | ✅ |
| Admin suite | — | ✅ |
| Memory firewall | ✅ | ✅ |
| OIDC / SSO | — | ✅ |
| Log shipping (Logship) | — | ✅ |

<!-- END GENERATED editions-matrix -->

A Community build **registers and wires none** of the Enterprise capabilities —
they are not merely hidden, they are not present in the binary.

## Which edition do I need?

- **Community** is the right choice for personal use, evaluation, and small
  deployments — it is the full orchestration engine, not a crippled demo.
- **Enterprise** is for teams that need the advanced capabilities above, SSO and
  governance, horizontal scale, or commercial support / the hosted SaaS.

See [Support](support.md) for commercial options.
