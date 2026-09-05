---
name: troubleshoot-vornik
description: |
  Teaches Claude how to diagnose a Vornik deployment that is down, degraded,
  or failing tasks. Use this skill whenever the user says Vornik is broken,
  erroring, stuck, hanging, won't start, a task failed, a feature is dark, or
  a change didn't take effect. Vornik ships a rich diagnostic surface — a
  multi-check doctor, a failure-class playbook corpus, task post-mortems — and
  the discipline is to route by symptom to the right one, read it before
  acting, and hand off to the report-problem skill when the ladder bottoms
  out. Do not improvise a fix from first principles; the answer is usually
  already named.
---

# Troubleshooting Vornik

Vornik diagnoses itself better than you can guess. Almost every failure has a
named check, a named error class, or a written remediation already. Your job
is to run the right diagnostic, **read it out to the user**, and only then
act — not to open with a fix.

Work the ladder in order. Each rung is cheap and read-only.

## Route by symptom

### A. Daemon down, won't start, unreachable

The daemon can't answer, so anything that needs its API is useless. Use the
static escape hatch:

```
vornikctl doctor --offline
```

It runs config parse, database reachability, migration state, and recent
journal errors without the daemon. Then read the unit's own logs for the
actual startup failure:

```
journalctl --user -u vornik -n 100 --no-pager
```

Drop `--user` for a packaged system-unit install.

> **`--offline` sees your shell, not the dead daemon.** With no daemon to
> ask, it resolves config and registry paths from the *invoking* environment.
> On a non-default install it will check a different tree than the daemon was
> started with and cheerfully report it healthy. There is no `--config` or
> `--configs-dir` flag to pin this. Read the unit's real environment first —
> `systemctl --user show vornik.service --property=Environment
> --property=EnvironmentFile` — export those `VORNIK_*` values, and only then
> trust an `--offline` result.

### B. Daemon up, something degraded

```
vornikctl doctor            # full check set
vornikctl doctor --json     # when you need to parse it
```

The checks span operational state, schema and storage, runtime, security
posture, models, and cost/budget. When one specific capability is dark rather
than the whole daemon:

```
vornikctl doctor feature <id>
```

### C. A specific task failed

```
vornikctl task get <id>          # status and last error class
vornikctl task explain <id>      # post-mortem for a terminal task
vornikctl task tail <id>         # logs (bare `vornikctl tail` is an alias)
vornikctl playbook show <CLASS>  # remediation for that error class
vornikctl execution prompt <executionId> <stepId>   # what the step's model was TOLD
```

**Read the prompt before blaming the tool.** Since 2026-09-04 the daemon keeps
each step's first model request — system prompt, user content, tools array —
redacted, content-addressed, keyed from the step's outcome row. A step that
failed in prompt assembly (a guidance block that did not land, a skill injected
that should not have been, a budget that truncated the wrong thing) shows it
there and nowhere else. `--part system|user|tools` prints one part bare. A 404
`PROMPT_NOT_RECORDED` means the agent image predates the contract or the step
never reached its first request — check image freshness. Execution ids come
from `vornikctl execution list`; step ids from `vornikctl task explain`.

**The playbook is the highest-value and least-known path in the product.**
The executor stamps a failure class on failed tasks, and the playbook corpus
carries a written, rule-based remediation for each one. When the class is
unknown or unfamiliar, `vornikctl playbook list` prints the whole corpus.
Check it before reasoning from scratch — someone already wrote the answer.

### D. A config change didn't take effect

```
vornikctl config reload-status
```

Then, in order: the env-file trap (systemd reads `Environment=` and
`EnvironmentFile=` only at start, so unit changes need a **restart**, not a
reload — and `Environment=` edits have no doctor check at all), and then the
wrong-tree question — was the edited file in the tree the daemon actually
resolved? Both are covered in the **configure-vornik** skill.

### E. A project, swarm, or workflow isn't there at all

Usually not a per-object bug. The whole registry tree failed to resolve, so
*nothing* loaded:

```
vornikctl project list
vornikctl swarm list
```

An empty or short result points straight at the resolution chain in
**configure-vornik** step 1 — most often a `VORNIK_CONFIGS_DIR` silently
skipped because the directory was missing one of `projects/`, `swarms/`, or
`workflows/`. The adjacent case, where files load but fail validation, shows
up as `config_validation` in `vornikctl doctor`.

## Hard rules

**Diagnose read-only before mutating.** Run the diagnostic, read it, tell the
user what it found. A fix proposed before a diagnostic is a guess.

**`--fix` is narrow.** `vornikctl doctor --fix` repairs only the operational
findings: stale leases, orphaned watchers, stuck executions, task state
audit, orphan FK rows, orphan worktrees, secrets permissions, and dispatcher
role. The schema, runtime, security-posture, pricing, and budget checks are
**diagnostic only** — they need an operator config change or an external
runtime action. Never imply `--fix` will clear one of those; it sends the
operator in a circle.

**No restart with live tasks.** Check `vornikctl task list` for `RUNNING` or
`LEASED` before suggesting a daemon restart, and wait for an idle window.

**Never edit the database directly** to clear a stuck task or execution.
That is exactly what the doctor's repairable checks are for.

**Never paste raw logs, journal output, or config anywhere public.** They
carry hostnames, paths, and secrets. Routing that to a public issue is the
report-problem skill's job, and it scrubs.

## Anti-patterns

- **Don't** open with a fix. Open with a diagnostic.
- **Don't** run `--fix` hoping it clears a finding it doesn't own.
- **Don't** reason a failure class out from first principles before checking
  `vornikctl playbook show`.
- **Don't** restart the daemon as a first move. It destroys the evidence and
  may kill live work.
- **Don't** trust an `--offline` result without matching the daemon's
  environment first.
- **Don't** report "it's fixed" without re-running the diagnostic that
  originally failed.

## When the ladder bottoms out

Doctor is clean, the playbook has no entry, the logs show something
unexplained — stop. Don't invent a theory.

Hand off to the **report-problem** skill. The diagnostic output is already in
hand, and `vornikctl report` will collect and anonymize it into a prefilled
issue on `github.com/grinco/vornik` that the user reviews and submits under
their own GitHub identity. Nothing is ever posted automatically.

---

*Commands in this skill were verified against `vornikctl` 2026.7.4. If one is
rejected as unknown, your daemon is likely older or newer than this skill —
check `vornikctl <command> --help` and trust the CLI over this document.*
