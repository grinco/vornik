# Control plane

!!! note "Community Edition · Phase 1 (early)"

    The control-plane **proposal ledger**, the `vornikctl control-plane`
    surface, and `vornikctl doctor --offline` are in the free Community
    Edition. This is an early slice — it proposes changes for you to review;
    it never applies them itself.

The control plane helps you **operate** vornik — troubleshoot, tune, and
change configuration — instead of hand-editing YAML and reading raw logs. Its
Phase-1 core is a **human-gated proposal ledger**: a suggested change (a config
diff, a model swap) is recorded as a **proposal** that you review and approve
or reject. Nothing is ever applied automatically — proposing is the only action
the control plane takes, so it is safe by construction.

## The proposal ledger

A proposal has a **kind** (`config` / `model` / `scaffold`), a **blast radius**
(`model` ⊂ `project` ⊂ `swarm` ⊂ `daemon`), a diff, a rationale, and a status:

```
DRAFT ──approve──▶ APPROVED        (you then action it by hand — Phase 1)
      └─reject───▶ REJECTED        (kept for audit; supersede with a new one)
```

Proposals come from you (via the CLI) or from a **Tune detector** — a
leader-gated watcher that raises a DRAFT when a project's health signal
degrades, so a regression surfaces as a reviewable suggestion rather than
something you have to notice yourself. It watches two signals over a rolling
window and only fires after a **sustained** breach (three consecutive scans):

- **failed-task rate** — too many executions failing;
- **p95 latency** — executions taking too long.

So a project that starts failing or slowing down produces a DRAFT proposal
("investigate the failing step / consider a faster model or a timeout change")
you can review with `vornikctl cp proposals`.

### `vornikctl control-plane` (alias `cp`)

```bash
# See what's waiting for you
vornikctl cp proposals                 # add --status DRAFT / --project janka

# Inspect one (full diff + rationale)
vornikctl cp show <proposal-id>

# Decide
vornikctl cp approve <proposal-id>     # you must NOT be the proposer
vornikctl cp reject  <proposal-id>

# Raise one yourself
vornikctl cp propose --title "bump scraper timeout" \
    --project janka --kind config --blast-radius project \
    --diff '-timeout_seconds: 30
+timeout_seconds: 90' --rationale "web_fetch timing out"

# Apply an APPROVED, applyable proposal (hot-reload) — and roll it back
vornikctl cp apply <proposal-id>          # --ack-daemon for daemon-scope
vornikctl cp rollback <proposal-id>
```

## Applying a change (gated, reversible)

An APPROVED proposal that carries a concrete file rewrite can be **applied by
the daemon** via config hot-reload — no restart:

- **Whole-file replace only** — the proposal carries the full new file
  contents (no fragile diff-patching); the human-readable diff is for review.
- **Fail-closed preconditions**: only APPROVED applies (exactly once); a
  daemon-scope change needs an explicit `--ack-daemon`; apply is **refused
  while any task in scope is running** (retry in an idle window); the new
  content is parsed/validated **before** anything is written.
- **Atomic + snapshotted**: the current file is snapshotted, the new file is
  written atomically, then reloaded. If the reload **rejects** the config (or
  times out), the daemon **auto-rolls-back** to the snapshot — it never runs a
  bad config.
- **Rollback**: `cp rollback` restores the pre-apply snapshot and reloads.

Proposals from the Tune detector are **review-only** (they describe a problem,
not a specific rewrite) — you approve them and make the change yourself, or
attach a concrete rewrite. Auto-apply does not exist: a human always runs
`cp apply`.

The surface is **operator-scoped** (it works in Community and is never behind
the Enterprise admin gate). Two safety rules are enforced daemon-side: a
proposal can only be decided while it is `DRAFT` (a decided proposal is
terminal — supersede it with a new one), and **the proposer can never approve
their own proposal** — approval is always a separate human decision. So when
the Tune detector proposes (as `tune-detector`), a human approval always
clears the gate.

## Web console (Enterprise)

The **Admin → Control plane** page (`/ui/admin/control-plane`) is a browser
surface over the same ledger: browse proposals by status, read a proposal's
rationale + diff, and **approve / reject / apply / rollback** with a click. It
delegates to the same engine as the CLI, so every gate holds — a daemon-scope
apply needs the "affects all projects" checkbox, and the proposer can't approve
their own proposal. (The CLI above is the Community path; the console is an
Enterprise convenience.)

## `vornikctl doctor --offline`

The escape hatch for when the daemon **won't start**. It runs static checks
without any daemon RPC:

```bash
vornikctl doctor --offline
```

- **config** — the config file is present and parses;
- **database** — reachable (a direct connection, not the daemon's pool);
- **migrations** — the applied schema head vs the binary's expected head
  (so "applied 116 < head 117" tells you a migration is pending);
- **journal** — recent `fatal` / `panic` / `error` lines from the daemon's
  own log, so "why won't it boot" is answered in one command.

It never changes anything and exits non-zero if any check fails.

## What's not here yet

Config + model applies land now (Phase 2). Still ahead: scaffold-apply for new
projects/workflows, the richer diagnostic assembler over logs/metrics/traces,
config-as-conversation scaffolding, self-healing incident detection, and an
operator agent you can just talk to. See the design notes in the repo for the
full arc.
