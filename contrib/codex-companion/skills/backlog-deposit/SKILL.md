---
name: backlog-deposit
description: |
  Teaches Codex to capture an off-scope finding into the repository's own
  backlog without derailing the task in hand. Use when you notice a defect,
  inefficiency, risk or missing capability that is NOT what you were asked to
  work on. Files it with dedup and a secret scan via a shipped script; does not
  touch the vornik daemon.
---

# Depositing an off-scope finding

While working a task you will find problems that are not that task: a bug in a
neighbouring file, a control that reports success over a surface it cannot see,
a missing capability. The rule is **file it and keep going** — do not fix it,
do not widen your diff, do not silently drop it.

## When to use this

**Do** deposit when you find, outside your current scope:
- a defect, with evidence (`file.go:120`, a task id, a measurement);
- an inefficiency or risk worth someone's attention;
- a missing capability the codebase clearly wants.

**Do not** deposit:
- the thing you were actually asked to do — that is the task, not a finding;
- linter-level nitpicks;
- something you already deposited (the script will refuse it anyway);
- anything you can fix correctly inside your current scope in one line.

Deposit ONE item, say one sentence about it in your output, and return to the
task immediately.

## How

The logic ships with this plugin:

```
printf '%s\n' "<body>" | \
  ./scripts/vornik-backlog-deposit.sh \
    --title "<one-line title>" \
    --kind bug|feature|optimisation|inefficiency|refactor \
    --priority P0|P1|P2|P3 \
    [--evidence "internal/api/foo.go:120"] \
    [--dry-run]
```

It finds the backlog by walking up from the cwd (`https://docs.vornik.io`, then
`BACKLOG.md`), or takes `--file <path>`.

Write the body as you would write the item — paragraphs, tables, code
references. Structure is preserved, so include the evidence that makes the
finding actionable rather than compressing it to a sentence.

## What it refuses, and what to do about it

| exit | meaning | what to do |
|---|---|---|
| 3 | a similar item already exists (it prints which, and its state) | Read it. If yours is genuinely distinct, re-run with `--allow-duplicate`. If it is the same finding, say so in your output and move on. |
| 4 | the content contains something shaped like a credential | Redact it and retry. The backlog is committed and pushed. |
| 5 | no backlog file found | Pass `--file`, or say in your output that the repo has no backlog. |
| 2 | bad arguments | Fix them. |

Dedup matches both grammars the file uses — `## [ ] P2 — Title (date)`
headings and `- [ ] **Bold lead.**` bullets — ignoring priority tags and
trailing dates, and it matches `[x]` done items too, so completed work is not
re-filed.

## Why this is client-side, not a vornik tool

The vornik daemon deliberately cannot write your checkout; that containment
boundary is load-bearing. The daemon-side `backlog_deposit` tool is correct for
its own caller (an agent inside a vornik task, on a project whose autonomy loop
consumes the backlog), but it renders one whitespace-flattened line capped at
600 characters — and this repository's median backlog item is ~1,800 characters
with tables and code references. You own the checkout; writing it here is not a
boundary question, and nothing is truncated.

See `https://docs.vornik.io`.
