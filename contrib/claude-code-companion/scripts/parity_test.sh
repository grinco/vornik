#!/usr/bin/env bash
# The deposit script ships in BOTH companion distributions, because a plugin is
# installed as a self-contained directory and cannot reach across to a sibling.
# Duplication is therefore the distribution mechanism — and duplication drifts.
#
# The two copies must stay byte-identical. Their SKILL/command wrappers are
# tailored per client (Claude has slash commands, Codex does not); the LOGIC is
# not, and a divergence here would mean one client silently deduping or
# secret-scanning differently from the other.
#
# Run: bash contrib/claude-code-companion/scripts/parity_test.sh
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
A="$ROOT/contrib/claude-code-companion/scripts"
B="$ROOT/contrib/codex-companion/scripts"
fail=0
for f in vornik-backlog-deposit.sh vornik-backlog-deposit_test.sh; do
  if [ ! -f "$B/$f" ]; then
    echo "  FAIL: $f missing from the codex distribution"; fail=1; continue
  fi
  if diff -q "$A/$f" "$B/$f" >/dev/null; then
    echo "  ok: $f identical across both distributions"
  else
    echo "  FAIL: $f has DRIFTED between the two distributions:"
    diff -u "$A/$f" "$B/$f" | head -20
    fail=1
  fi
done
[ "$fail" -eq 0 ] && echo "PASS: companion script parity" || exit 1
