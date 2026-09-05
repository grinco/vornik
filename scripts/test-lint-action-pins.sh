#!/usr/bin/env bash
#
# test-lint-action-pins.sh — tests for lint-action-pins.sh (LLD
# 2026-09-03-action-pin-gate-design §3). Drives the real script against fixture
# .github trees via VORNIK_GITHUB_DIR. No bats.
#
# Usage: scripts/test-lint-action-pins.sh
# Exit:  0 = all pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
LINT="$HERE/lint-action-pins.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/actionpins-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

PIN="actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1"

fresh() { GH="$TMP/gh$RANDOM"; mkdir -p "$GH/workflows" "$GH/actions/setup-go"; }
run_lint() { VORNIK_GITHUB_DIR="$GH" "$LINT" 2>&1; }

# --- Case A: a fully pinned tree passes, including a pinned composite action. ---
fresh
printf 'jobs:\n  b:\n    steps:\n      - uses: %s\n      - uses: ./.github/actions/setup-go\n      - uses: docker://alpine@sha256:abc\n' "$PIN" > "$GH/workflows/ci.yaml"
printf 'runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0\n' > "$GH/actions/setup-go/action.yaml"
out="$(run_lint)"; rc=$?
if [ "$rc" -eq 0 ]; then pass "pinned tree passes (local + docker refs exempt)"; else fail "pinned tree should pass (rc=$rc): $out"; fi

# --- Case B: an unpinned workflow action fails and is named. ---
fresh
printf 'jobs:\n  b:\n    steps:\n      - uses: actions/checkout@v5\n' > "$GH/workflows/ci.yaml"
out="$(run_lint)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'actions/checkout@v5'; then pass "unpinned workflow action fails"; else fail "unpinned workflow action should fail naming it (rc=$rc): $out"; fi

# --- Case C (the 2026-09-03 regression): an unpinned uses: INSIDE a composite
# action fails. The first version scanned .github/workflows only, so
# actions/setup-go@v6 — invoked from every CI job via ./.github/actions/setup-go
# — passed a gate that printed "every third-party action is pinned". ---
fresh
printf 'jobs:\n  b:\n    steps:\n      - uses: ./.github/actions/setup-go\n' > "$GH/workflows/ci.yaml"
printf 'runs:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n' > "$GH/actions/setup-go/action.yaml"
out="$(run_lint)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'actions/setup-go@v6'; then pass "unpinned action inside a composite action fails"; else fail "composite action's own uses: must be checked (rc=$rc): $out"; fi

# --- Case D: a commented-out uses: is ignored; .yml is scanned like .yaml. ---
fresh
printf 'jobs:\n  b:\n    steps:\n      # - uses: actions/checkout@v5\n      - uses: %s\n' "$PIN" > "$GH/workflows/ci.yml"
out="$(run_lint)"; rc=$?
if [ "$rc" -eq 0 ]; then pass "commented uses: ignored, .yml scanned"; else fail "commented uses: should be ignored (rc=$rc): $out"; fi

# --- Case E: a quoted, unpinned reference in a .yml composite action fails. ---
fresh
printf 'runs:\n  using: composite\n  steps:\n    - uses: "actions/cache@v4"\n' > "$GH/actions/setup-go/action.yml"
out="$(run_lint)"; rc=$?
if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -q 'actions/cache@v4'; then pass "quoted unpinned ref in action.yml fails"; else fail "quoted unpinned ref in action.yml should fail (rc=$rc): $out"; fi

if [ "$fails" -ne 0 ]; then echo "test-lint-action-pins: $fails FAILED"; exit 1; fi
echo "test-lint-action-pins: all cases passed"
