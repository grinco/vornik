---
sources:
    - path: internal/featuredoctor/feature_cluster.go
      sha256: 5cbe5d2050087f754e6ea7cf1a3f4722d1e1dcba478b3fb12e3cbf6fee82a860
---
# Cluster topology and node roles

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.


vornik can run as a single all-in-one daemon (the default) or as a
multi-node cluster where each node takes a specialised **role**. Roles
let you scale the worker tier independently of the UI/API tier and
isolate public webhook ingress in a DMZ.

## Node profiles

A node's role is set with the `node.profile` config key. Each profile
is a preset over two capabilities — whether the node serves public
webhook ingress, and whether it runs the scheduler/executor and
leader-elected background workers:

| `node.profile` | Serves UI/API | Serves webhooks | Runs workers | Typical use |
|----------------|---------------|-----------------|--------------|-------------|
| `all` (default) | yes | yes | yes | single-node deployment |
| `ui` | yes | no | no | front-end / API tier |
| `worker` | no | no | yes | job tier (scheduler + executor) |
| `webhook` | no | yes | no | DMZ-isolated webhook relay |

Leave `node.profile` unset (or `all`) for a single-node install — that
is exactly today's behaviour. You can override an individual capability
on top of a profile with `node.serve_webhooks` and `node.run_workers`
when you need a combination the presets don't cover.

## DMZ webhook relay

A `webhook` node is meant to live in an isolated DMZ network. It
verifies the provider signature on each inbound webhook and then
forwards the verified event over mutual-TLS to the job tier — it never
touches Postgres or any LLM/broker credentials, so the DMZ blast radius
stays minimal.

- On the **webhook** node, set `node.relay.upstream` to the job tier's
  mTLS relay address.
- On the **worker** (job tier) node, set `node.relay_ingress.addr` to
  the address its mTLS relay listener should bind.

Relay keys are only coherent with the matching role: `node.relay.*`
requires a webhook node (`serve_webhooks` on, `run_workers` off), and
`node.relay_ingress.*` requires a node that runs workers. The feature
doctor flags a mismatch rather than starting in an inconsistent state.

## Applying changes

Node role and relay settings are **restart-gated** — changing them
takes effect on the next daemon start. Use the feature doctor to
validate a node's configuration before (re)starting it:

```bash
vornikctl doctor feature cluster
```

It confirms `node.profile` is a recognised preset and that any relay
configuration is coherent with the resolved role, with remediation
hints for anything that doesn't line up.

## Setting it up

For an end-to-end walkthrough — generating the mutual-TLS material,
configuring both sides of the relay, running the webhook node, and
validating the fleet — see the
[Clustering and the DMZ Webhook Relay](../guides/clustering.md) guide.
