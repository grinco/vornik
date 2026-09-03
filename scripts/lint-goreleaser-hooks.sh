#!/usr/bin/env bash
# lint-goreleaser-hooks.sh — refuse a goreleaser before-hook that writes into dist/.
#
# WHY THIS EXISTS. goreleaser runs `before.hooks` BEFORE it ensures the
# distribution directory, and its emptiness check does not care that the hook is
# the thing that filled it. So a hook writing dist/<anything> aborts EVERY run
# with
#
#   ⨯ release failed after 0s
#     error=dist is not empty, remove it before running goreleaser or use the
#     --clean flag
#
# which names --clean as the remedy while --clean is already set, and reads as a
# stale-directory problem rather than a config one.
#
# This has now shipped TWICE on the same nfpm entry. b4381b44 removed
# dist/images.json because no package had ever contained it; 3fe3cc3f restored it
# via a before-hook whose comment reasons "dist/ is wiped at the start of a run,
# so a record written by an earlier step would be deleted before nfpm read it" --
# correct about the wipe, wrong about the ordering. Neither instance was caught,
# because ci.yaml's packaging gate runs the identical command and Actions are
# DISABLED on this fork, so the gate that would have failed never ran. Found
# 2026-09-03 by building packages by hand for the first time since.
#
# A hook may still PRODUCE a file nfpm ships -- it just has to write it somewhere
# goreleaser does not police, and have `contents.src` point there.
set -euo pipefail

cd "$(dirname "$0")/.."

status=0

for cfg in .goreleaser*.yaml; do
    [ -e "$cfg" ] || continue
    # The before-hooks block only: from `before:` to the next top-level key.
    # Comment lines are stripped so the explanatory prose above a hook -- which
    # legitimately mentions dist/ -- cannot trip the check.
    hooks="$(sed -n '/^before:/,/^[a-z]/p' "$cfg" | sed 's/#.*//')"
    offending="$(printf '%s\n' "$hooks" | grep -nE '(^|[^A-Za-z0-9_/.-])dist/' || true)"
    if [ -n "$offending" ]; then
        echo "lint-goreleaser-hooks: $cfg — a before-hook writes into dist/:" >&2
        printf '%s\n' "$offending" | sed 's/^/    /' >&2
        status=1
    fi
done

if [ "$status" -ne 0 ]; then
    cat >&2 <<'MSG'

goreleaser runs before-hooks BEFORE it ensures dist/, and then refuses to start
because dist/ is not empty -- so this breaks every packaging run, including
`make package-enterprise` and ci.yaml's packaging gate.

Fix: write the artifact somewhere else (build/ is gitignored) and point the
nfpm `contents.src` at that path instead.
MSG
    exit 1
fi

echo "lint-goreleaser-hooks: OK — no before-hook writes into dist/"
