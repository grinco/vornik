#!/usr/bin/env bash
# Regression guard + PARITY guard: resolve_path must recover an absolute
# container path whose leading slash the model dropped, and must agree with
# its Go port while doing it.
#
# 2026-09-16. exec_20260916164618_4c53e9deafd4e8e6 died in 8s on a file that
# was present: the model called file_read with
# "app/workspace/artifacts/in/<base>" — the canonical path minus its leading
# slash — and the resolver joined it onto the workspace, producing
# /app/workspace/app/workspace/artifacts/in/<base>. Two misses on that path
# tripped the repeat-miss guard, which reported a missing upstream artifact.
# The audit ledger for that one execution has the model spelling the same file
# three ways; only this one missed.
#
# Two resolvers are live and must not diverge: this bash one keys the
# file_read cache and the repeat-miss guard, while internal/agentloop's Go
# port performs the read. A divergence means the guard tracks a path the
# reader never opened. The table below is the same one
# TestResolvePath_RelativePathRestatingWorkspaceRoot pins in Go.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ep="$here/entrypoint.sh"
tmp="$(mktemp -d)"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT

export WORKSPACE="$tmp/app/workspace"
mkdir -p "$WORKSPACE/artifacts/in"
printf 'the design body' > "$WORKSPACE/artifacts/in/design.md"
ws_real="$(cd "$WORKSPACE" && pwd -P)"
deslashed="${ws_real#/}"

fails=0
pass() { echo "ok   - $1"; }
fail() { echo "FAIL - $1" >&2; fails=$((fails+1)); }

# shellcheck disable=SC1090
eval "$(sed -n '/^resolve_path()/,/^}/p' "$ep")"

check() { # check <raw> <expected>
  local got
  if ! got="$(resolve_path "$1" 2>&1)"; then
    fail "resolve_path refused '$1': $got"
    return
  fi
  if [ "$got" = "$2" ]; then
    pass "'$1' -> '$2'"
  else
    fail "'$1' -> '$got', want '$2'"
  fi
}

# The incident shape: the canonical path minus its leading slash.
check "$deslashed/artifacts/in/design.md" "$ws_real/artifacts/in/design.md"
# The workspace root itself, de-slashed.
check "$deslashed" "$ws_real"
# An ordinary relative path is untouched.
check "artifacts/in/design.md" "$ws_real/artifacts/in/design.md"
# The mirror-image tolerance that was already here: an absolute path outside
# the workspace is re-rooted under it, not refused.
check "/etc/passwd" "$ws_real/etc/passwd"
# String prefix WITHOUT a component boundary stays an ordinary relative path.
check "${deslashed}-other/x" "$ws_real/${deslashed}-other/x"

# A NON-CANONICAL workspace must resolve identically. The two implementations
# computed the de-slashed root differently (Go: filepath.Clean then strip one
# separator; bash: lstrip every leading separator, no clean), so they agreed
# only while WORKSPACE happened to be canonical — true of the container image
# and of nothing else (review-20260916-2c6c, finding 1). Pinned rather than
# relied on.
for odd in "$WORKSPACE/" "$WORKSPACE//" "$WORKSPACE/."; do
  (
    export WORKSPACE="$odd"
    got="$(resolve_path "$deslashed/artifacts/in/design.md" 2>&1)" || {
      echo "FAIL - non-canonical WORKSPACE '$odd' refused: $got" >&2; exit 1; }
    [ "$got" = "$ws_real/artifacts/in/design.md" ] || {
      echo "FAIL - non-canonical WORKSPACE '$odd' -> '$got'" >&2; exit 1; }
    echo "ok   - non-canonical WORKSPACE '$odd' resolves identically"
  ) || fails=$((fails+1))
done

# The confinement refusal is unchanged.
if resolve_path "../../../etc/passwd" >/dev/null 2>&1; then
  fail "dot-dot escape must still refuse"
else
  pass "dot-dot escape still refuses"
fi

echo
[ "$fails" -eq 0 ] || { echo "$fails case(s) failed" >&2; exit 1; }
echo "all cases passed"
