#!/usr/bin/env bash
#
# test-config-drift-check-all.sh — tests for the MULTI-TREE drift wrapper.
#
# Regression of record, measured 2026-08-18: config-drift-check.sh takes one
# tree, and this host runs two daemons. The benchmark daemon's tree held 12 of
# the 26 shipped workflows while `make config-diff` reported clean, because it
# only ever looked at production. A benchmark that cannot see half the surface
# it exists to measure, with nothing saying so.
#
# Usage: scripts/test-config-drift-check-all.sh
# Exit:  0 = all cases pass, 1 = a case failed
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
ALL="$HERE/config-drift-check-all.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/cfgdriftall-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1"; fails=$((fails+1)); }

# One repo fixture; two deployed trees built from it.
REPO="$TMP/repo/configs"
mkdir -p "$REPO/swarms" "$REPO/workflows"
echo "dev swarm" > "$REPO/swarms/dev-swarm.md"
echo "adaptive"  > "$REPO/workflows/adaptive.md"
echo "research"  > "$REPO/workflows/research.md"

new_tree() {
	local dir="$1"
	mkdir -p "$dir/swarms" "$dir/workflows"
	cp "$REPO/swarms/dev-swarm.md" "$dir/swarms/"
	cp "$REPO/workflows/adaptive.md" "$dir/workflows/"
	cp "$REPO/workflows/research.md" "$dir/workflows/"
}

run_all() { VORNIK_REPO_CONFIGS_DIR="$REPO" "$ALL" "$@" 2>&1; }

# --- Case 1: two in-sync trees pass, and BOTH are reported. ---
new_tree "$TMP/prod"; new_tree "$TMP/bench"
out="$(run_all "$TMP/prod" "$TMP/bench")"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "checked 2 tree"; then
	pass "two in-sync trees pass and both are checked"
else
	fail "two in-sync trees: rc=$rc out=$out"
fi

# --- Case 2 (THE regression): the FIRST tree is clean, the SECOND is missing a
# workflow. A single-tree check of the first would report clean — which is
# exactly how the bench tree sat at 12 of 26 unnoticed. ---
rm -f "$TMP/bench/workflows/research.md"
out="$(run_all "$TMP/prod" "$TMP/bench")"; rc=$?
if [ "$rc" -eq 1 ] && printf '%s' "$out" | grep -q "workflows/research.md"; then
	pass "drift in the SECOND tree is found (regression: bench tree at 12/26)"
else
	fail "second-tree drift: rc=$rc out=$out"
fi

# --- Case 3: a tree the daemon reads but which does not exist outranks drift.
# Missing is a worse fact than different, so the exit status must say so. ---
out="$(run_all "$TMP/prod" "$TMP/bench" "$TMP/nonexistent")"; rc=$?
if [ "$rc" -eq 2 ]; then
	pass "a missing tree (2) outranks drift (1) in the aggregate status"
else
	fail "missing tree should exit 2, got rc=$rc"
fi

# --- Case 4: the same tree named twice is checked once. Discovery can yield
# duplicates (several units sharing the default tree) and a doubled report
# would misstate how much of the host was actually examined. ---
new_tree "$TMP/bench"
out="$(run_all "$TMP/prod" "$TMP/prod")"; rc=$?
if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q "checked 1 tree"; then
	pass "duplicate trees are de-duplicated"
else
	fail "dedupe: rc=$rc out=$out"
fi

echo
if [ "$fails" -gt 0 ]; then
	echo "$fails case(s) failed"
	exit 1
fi
echo "all cases passed"
