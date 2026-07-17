---
sources:
    - path: internal/featuredoctor/feature_composer.go
      sha256: fab8e4c41115ab4a2edfa76013e0834c925255db3ab2b66f97eb6b551849d14e
---
# NL Automation Composer

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.


The conversational project wizard normally anchors a new project on a
vetted gallery template, filling in the pieces of a project a template
doesn't cover with small, typed customizations (a schedule, an MCP
server, an extra tool). The composer extends that same conversation
with a third path for requests that genuinely don't fit any template:
describe the automation you want in plain language, and the wizard can
synthesize a complete project — swarm, workflow, and plan — from a
curated library of role archetypes.

## What makes it safe

A synthesized automation is never taken on faith. Before it can ever be
shown as ready to commit, the server:

- **Re-validates the whole bundle** against the same rules a
  hand-authored project goes through — every reference resolves, every
  workflow step is reachable, nothing is left dangling.
- **Enforces deterministic guardrails** that are stricter than what an
  operator could hand-write: a role's tools never exceed its archetype's
  declared allowlist, autonomous scheduling is off unless you asked for
  a schedule, spend caps are always present, and any outward-facing
  step (sending an email, publishing, opening a request) gets an
  approval step in front of it by default.
- **Never silently rewrites what you approved.** A generated build that
  would violate one of those guardrails is never quietly corrected —
  the wizard either fixes an obviously-safe omission (like a missing
  spend cap) and tells you so, or it stops and asks you about anything
  that would change what the automation actually does.
- **Requires an explicit schedule confirmation** before any
  automation with a recurring schedule can go live — a cadence you
  never confirmed can never be committed.

The composer is off by default until it completes its rollout soak. An
operator enables it explicitly once the prerequisites — a configured
chat provider and at least one validated role archetype — are met.

See [the automation composer guide](../guides/automation-composer.md)
for the operator-facing walkthrough: the tier model, what gets
created, the schedule-confirmation gate, the cost estimate, and the
enable command.
