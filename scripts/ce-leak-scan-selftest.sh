#!/usr/bin/env bash
# Self-test for ce-leak-scan.sh — proves the gate actually catches leaks, so a
# future regression that neuters the scanner (always-pass) is caught. Regression
# guard for the 2026-06-27 CE-export IP leak. Uses a throwaway denylist so it
# depends on no real operator token. Run by CI before the real export verify.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCAN="$HERE/ce-leak-scan.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

deny="$TMP/deny.txt"
printf '%s\n' '# throwaway denylist' 'secret-token-xyz' >"$deny"
export CE_DENYLIST="$deny"

pass=0
fail=0
expect() { # <desc> <want-rc> <got-rc>
  if [ "$2" -eq "$3" ]; then echo "  ok   $1"; pass=$((pass + 1)); else
    echo "  FAIL $1 (want rc=$2 got rc=$3)"; fail=$((fail + 1)); fi
}

# 1. Clean tree => pass.
clean="$TMP/clean"; mkdir -p "$clean/sub"; echo "nothing to see" >"$clean/sub/a.txt"
set +e; "$SCAN" "$clean" >/dev/null 2>&1; expect "clean tree passes" 0 $?; set -e

# 2. Planted token anywhere => fail.
dirty="$TMP/dirty"; mkdir -p "$dirty/sub"; echo "oops secret-token-xyz here" >"$dirty/sub/b.txt"
set +e; "$SCAN" "$dirty" >/dev/null 2>&1; expect "planted token fails" 1 $?; set -e

# 3. Shipped denylist file (even token-free content) => fail (it must be pruned).
shipped="$TMP/shipped"; mkdir -p "$shipped/scripts"; echo "x" >"$shipped/scripts/docs-ip-denylist.txt"
set +e; "$SCAN" "$shipped" >/dev/null 2>&1; expect "shipped denylist file fails" 1 $?; set -e

echo "ce-leak-scan self-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
