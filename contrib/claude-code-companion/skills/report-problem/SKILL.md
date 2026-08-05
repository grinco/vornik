---
name: report-problem
description: |
  Teaches Claude how to help a user report a Vornik problem — a bug, a crash,
  a misbehaving swarm, or an INSTALL failure — as an anonymized GitHub issue on
  the public grinco/vornik repo. Use this skill whenever the user says Vornik
  is broken / erroring / not installing / behaving wrong and wants to file it
  upstream. The heavy lifting is a deterministic CLI (`vornikctl report`); this
  skill is the guardrail around it (privacy, review-before-submit, install-time).
---

# Reporting a Vornik problem

Vornik ships a first-class, zero-auth reporting path so a user can file a
useful, **anonymized** bug report at any point in the product lifecycle —
from a failed install to a runtime crash — without hand-assembling
diagnostics and without leaking secrets, hostnames, or file paths into a
PUBLIC issue.

Your job is NOT to collect diagnostics yourself, paste raw logs, or write the
issue body from scratch. The daemon and the installer already produce a
scrubbed body + a prefilled `github.com/grinco/vornik` issue URL. Your job is
to run the right entry point, **show the user the anonymized body**, confirm it
carries no residual identifiers, and help them open + submit it **with their
own GitHub account**. Nothing is ever posted automatically.

## Pick the entry point by lifecycle stage

**1. Install / quickstart failure (no daemon, maybe no `vornikctl` yet).**
The `quickstart.sh` installer installs its own EXIT trap: on any non-zero
exit it prints a prefilled `grinco/vornik` issue URL (labels `bug,install`)
with scrubbed structured context — version, platform, exit code, and the
scrubbed failing command. If the user pastes that URL or the installer's
failure block, you don't need to run anything: help them review it and fill in
the `<Add what you were doing…>` placeholder. If they still have a shell but the
install half-completed, `vornikctl report --offline` (below) gives a richer
report from static checks.

**2. Daemon won't start / is down.** Run:

```
vornikctl report --offline --summary "one line: what's wrong"
```

`--offline` skips the daemon and runs the static doctor checks (config parses,
DB reachable, migration state, recent journal errors) — the same checks that
diagnose why the daemon won't boot.

**3. Daemon up, but something misbehaves** (a swarm loops, a task fails, a
wrong result). Run:

```
vornikctl report --summary "one line: what's wrong"
```

This pulls live diagnostics from `/api/v1/doctor` and folds them into the
anonymized body. If the user can point at a specific failed task or window,
add `--task <id>` or `--since <window>` — that appends a note offering a
detailed **attachable** support bundle (see the attachment caveat below).

## When the daemon is on ANOTHER host

Everything above assumes `vornikctl` can reach the daemon from the machine you
are on. If the daemon runs elsewhere — a server, a container host, someone
else's laptop — the CLI is not available to you, and until 2026-08-05 there was
no way to file a report through the companion at all.

Use the MCP verb instead:

```
mcp__vornik__report_problem  { "symptom": "<the user's own words>" }
```

It returns `review_url` and the anonymized `body`. Two properties matter, and
both are deliberate:

- **It collects NOTHING from the daemon host.** The report carries the build
  identity and the reporter's words, full stop — no doctor findings, no journal
  tail. No remotely-reachable path in Vornik may execute a program, and both of
  those collectors would (the doctor sweep invokes `podman`, the tail invokes
  `journalctl`). So this report is thinner than the CLI's by design, not by
  omission.
- **Nothing is submitted.** Show the body to the user, let them read it, and let
  THEM open the URL — it lands under their own GitHub identity.

If the report needs real diagnostics, that still comes from an operator running
`vornikctl report` on the daemon host themselves and attaching the result. Say
so plainly rather than implying the thin report is equivalent.

## Flags worth knowing

- `--summary "<text>"` — the user's one-line description. It is anonymized like
  everything else, so it is safe to pass free text; it never lands in the issue
  **title** (the title is built from controlled check names only).
- `--offline` — force the static, daemon-down checks.
- `--dry-run` — print ONLY the anonymized body, submit/emit nothing. Use this
  first if the user is nervous about privacy — it lets them read exactly what
  would be posted before any URL is produced.
- `--url-only` — print just the prefilled URL (for scripting / a terse user).

## Privacy — the part you must not skip

The issue body is PUBLIC. Vornik anonymizes it in two tiers — secret redaction
(tokens, keys, JWTs, PEM blocks) **plus** a public scrubber that strips emails,
home/`~` paths (the whole path, so project names in the tail don't leak), all
IPv4 addresses, and this machine's hostname. If anonymization ever fails, the
command fails closed with a static message and emits **no** body.

Even so, treat every report as **review-before-submit**:

1. Run the command (or `--dry-run` first).
2. **Read the anonymized body aloud to the user** — or at minimum confirm you
   see `<path>`, `<host>`, `<ip>`, `<email>`, `<redacted*>` where identifiers
   used to be, and no stray project name, username, or secret survived.
3. Only then hand them the prefilled URL and tell them plainly: *"This opens a
   PUBLIC issue on github.com/grinco/vornik. Review it once more in the browser,
   add any detail, then submit with your own account."*

## The attachment caveat

`vornikctl report` produces a small, tightly-anonymized body — enough to
triage. When the maintainers need more, the report points the user at
`vornikctl support-report --task <id>` (or `--since`), which builds a fuller
bundle. That bundle is redacted for **secrets** but MAY still carry project
names and other context. So if the user attaches it, tell them to **open and
inspect it first** (e.g. `grep MANIFEST.json` for their project name) before
uploading it to the public issue. The bundle is an opt-in attachment, never
posted automatically.

## Anti-patterns

- **Don't** paste raw logs, `journalctl` output, config files, or a stack
  trace directly into the issue body yourself — that bypasses the scrubber.
  Route everything through `vornikctl report` (or the installer's URL).
- **Don't** submit on the user's behalf or with a shared token — the design is
  deliberately zero-auth so the user files it under their own GitHub identity.
- **Don't** promise the report is 100% anonymous. Say it is anonymized and
  ask them to review — the scrubbers are strong but the final check is human.
