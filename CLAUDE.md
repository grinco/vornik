# Working on Vornik

Operative rules for anyone — human or agent — changing this codebase. They are
stated here, in full, rather than linked, because every one of them has been
carried as a pointer that was re-injected into context, read, and not followed.
A rule behind a link is a rule the reader chooses; that choice is what this file
removes.

Not a runbook for USING Vornik — that is `AGENTS.md`, which covers install,
connecting a model, and first tasks. This file is about changing the code.

## 1. Design before code

**Recall the subsystem's design before reading or editing its code, and again
when the subsystem changes.** The designs in `https://docs.vornik.io` are the
authoritative record; code confirms what a named file, flag or function still
is, because a design freezes facts at write time.

Then one of three things is true, and each has an obligation:

| the design | you |
|---|---|
| is silent on what you are about to do | add it |
| contradicts what shipped | correct it, dated, and mark the stale sections inline |
| does not exist for this surface | write one |

**Touch a surface a design covers → amend that design in the same change.**
Never a second document about one surface: retrieval serves whichever chunk
ranks highest, and neither document says it is the incomplete one.

The failure this prevents is not hypothetical and not rare. It is skipped most
often on changes that look contained — "one line", "obviously right" — and those
are the ones where the design turns out to disagree.

*(In the public Community repository `https://docs.vornik.io` is not present;
the rule still holds for whatever design record that fork keeps.)*

## 2. Verify at CI scope, not at "it compiles"

Before claiming anything works:

```
go test ./...                 # the sqlite lane
make lint                     # golangci-lint + the LLD contract checks + shellcheck
make test                     # unit + agent shell + script tests
```

and, **for any change under `internal/persistence` or that touches SQL**, the
Postgres lane as well:

```
make test-integration         # -tags=integration; the pgvector lane
```

`go test ./...` is SQLite. A repository can diverge between the two drivers and
pass — that is how a JSONB byte-exactness difference reached CI, and how a
`LIKE` pattern silently matched nothing on one driver for weeks.

**Read the exit code directly.** Never through a pipe: `go test ./... | tail`
reports the exit status of `tail`.

## 3. A bug fix without a failing test is a guess

Write the test that fails before the fix and passes after it, and name the
incident in a comment. A fix whose test cannot distinguish the broken code from
the fixed code has not been demonstrated.

The same applies to a rare bug: make the interleaving deterministic (a seam, a
hook) rather than relying on a 1-in-600 stochastic reproduction as the gate.

## 4. Say what is true

- A number with no scope reads as a guarantee — publish what it does NOT cover.
- A control that cannot distinguish "examined and clean" from "never examined"
  reports the first and means the second.
- A documented behaviour nothing implements is worse than an absent one: it
  stops the next person looking.
- Report failures with their output. "Tests pass" after a partial run is a
  claim about a run that did not happen.

## 5. Before improvising, check what exists

This codebase is large and most surfaces have been solved once. Search for the
prior art before building the second one — a safety check with two
implementations has one that is wrong, and the wrong one is usually the newer.

The same holds for the operator-facing tooling: prefer the shipped skills and
`vornikctl` verbs over hand-rolled commands.

## 6. Commit discipline

- `make lint` before every commit.
- Stage the exact paths you changed. Never `git add -A` or `git add .` — other
  work happens in this tree concurrently.
- Commit locally; do not push unless asked.
- The commit message explains WHY, and states what was verified.
