# The automation composer ("Describe it")

!!! abstract "Enterprise Edition"

    This capability is part of the **Enterprise Edition** — a proprietary overlay on the open-source core. See [Editions](../editions.md) for the full Community vs Enterprise matrix.

The conversational project wizard normally starts from a vetted gallery
template and fills in the pieces a template doesn't cover with small,
typed customizations — a schedule, an extra tool, a parameter. The
**composer** extends that same "Describe it" conversation with a third
path for requests that don't fit any template: describe the automation
you want in plain language, and the wizard synthesizes a complete,
validated project — a swarm of roles and one or more workflows — from
a curated library of role archetypes.

You never choose the path yourself. The wizard escalates invisibly,
turn by turn, as your description sharpens:

1. **Template + parameters** — your request matches a gallery template
   closely; the wizard fills in the blanks.
2. **Template + customizations** — a template is a good base, but you
   asked for something extra (a schedule, a tool, an approval step)
   that a small addition covers.
3. **From-scratch composition** — nothing in the gallery fits, so the
   wizard assembles a new automation from role archetypes instead.

If your description closely matches an existing template, the wizard
prefers the cheaper, better-tested template path even when a
from-scratch build was technically possible — composing less is more
reliable. If you genuinely want a from-scratch build despite a
close-matching template, just say so when asked and the wizard will
proceed.

## What gets created

A composed automation is the same shape as a hand-built one: a
project, a swarm of one or more roles (each drawn from a curated role
archetype — researcher, writer, reviewer, coder, tester, analyst,
publisher, and similar), and one or more workflows wiring those roles
together into runnable steps. Nothing about how it runs, is monitored,
or is billed differs from a project you'd build by hand or from the
template gallery.

## What makes it safe

A synthesized automation is never taken on faith. Before it can ever be
shown to you as ready to commit, the server:

- **Re-validates the whole bundle** against the same rules a
  hand-authored project goes through — every role reference resolves,
  every workflow step is reachable, nothing is left dangling, and every
  automation ships with at least one workflow (a swarm with no workflow
  is never produced).
- **Never exceeds a role's declared tool allowlist.** Each role
  archetype in the library declares the maximum set of tools it may
  ever use; the composer can select a subset, never add to it — no
  description can talk it into granting a role a tool its archetype
  doesn't already allow.
- **Never omits a spend budget.** Every composed project carries a
  budget with conservative default caps if you don't set your own; the
  plan preview always states the caps.
- **Never auto-sends without an approval step, unless you explicitly
  ask for it and it's shown to you.** Any step with an outward
  side-effect — sending an email, publishing, opening a request —
  gets an approval step in front of it by default. If you ask the
  wizard to skip that approval, the plan preview calls it out
  prominently, and the request is kept in the conversation record as
  the audit trail for that choice.
- **Never silently rewrites what you approved.** A generated build
  that would violate one of the guarantees above is never quietly
  corrected: the wizard either fixes an obviously-safe omission (like
  filling in a missing budget) and tells you so, or it stops and asks
  you about anything that would change what the automation actually
  does.
- **Falls back to a template rather than guessing forever.** If the
  wizard has trouble producing a valid build after a few attempts in a
  row, it stops retrying from scratch and offers the closest matching
  gallery template instead, so you're never stuck watching it fail
  silently on repeat.

## The schedule-confirmation gate

If your automation should run on a recurring schedule, the plan
preview shows the exact cadence as a dedicated confirmation chip (for
example, "Runs every Monday 07:00 — confirm"). The automation cannot
be committed until you've confirmed that specific cadence — being
otherwise "ready to commit" is not enough on its own. If a later
message in the conversation changes the schedule, you're asked to
reconfirm the new one before it can go live. This means a cadence you
never actually saw and approved can never start running.

## Cost estimate

The plan preview shows a cost estimate for the automation — a rough
per-run figure, and a per-month figure if it runs on a schedule. This
number is computed by the server from the roles the automation
actually uses, not taken from the conversation itself, and it is
always labelled as an estimate rather than a bill.

## Enabling it

The composer is off by default until it completes its rollout
verification period. An operator turns it on explicitly once its
prerequisites are met — a configured chat provider and at least one
validated role archetype in the library:

```bash
vornikctl doctor feature enable composer
```

See [the feature doctor guide](feature-doctor.md) for how enabling and
prerequisite checks work in general, and
[the automation composer feature page](../features/composer.md) for the
full prerequisite and safety-guarantee list.
