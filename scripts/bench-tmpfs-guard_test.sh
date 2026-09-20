#!/usr/bin/env bash
# Unit tests for scripts/bench-tmpfs-guard.sh.
#
# The guard's failure mode is asymmetric, and both halves are tested here:
#   - too lax  → a six-hour pass is killed by the OOM killer partway through
#                (2026-09-17, four gold batches lost);
#   - too keen → a pass that would have been fine refuses to start, on a box
#                where the scratch is ordinary disk and 90% full is normal.
#
# Run: bash scripts/bench-tmpfs-guard_test.sh
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/bench-tmpfs-guard.sh
. "$HERE/bench-tmpfs-guard.sh"

pass=0; fail=0
ok()  { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1" >&2; }

want_refused() {
    _what="$1"; shift
    if "$@" >/dev/null 2>&1; then bad "$_what (allowed, expected refusal)"; else ok "$_what"; fi
}
want_allowed() {
    _what="$1"; shift
    if "$@" >/dev/null 2>&1; then ok "$_what"; else bad "$_what (refused, expected allowed)"; fi
}

# --- the incident itself: 66% of a 16 GB tmpfs ---
want_refused "a RAM-backed scratch at the 2026-09-17 incident's 66% refuses" \
    bench_assert_scratch_headroom 66 yes 50 /tmp

# --- the boundary, stated to the point rather than implied ---
want_refused "at the threshold exactly (50%) refuses"  bench_assert_scratch_headroom 50 yes 50 /tmp
want_allowed "one below the threshold (49%) proceeds"  bench_assert_scratch_headroom 49 yes 50 /tmp

# --- the same fullness on disk is not this guard's problem ---
want_allowed "a DISK-backed scratch at 66% proceeds"   bench_assert_scratch_headroom 66 "" 50 /var/tmp
want_allowed "a DISK-backed scratch at 99% proceeds"   bench_assert_scratch_headroom 99 "" 50 /var/tmp

# --- cannot-measure is not refuse. A guard that blocks when blind is a guard
#     an operator disables, and then it protects nothing at all. ---
want_allowed "an empty reading proceeds"               bench_assert_scratch_headroom "" yes 50 /tmp
want_allowed "a non-numeric reading proceeds"          bench_assert_scratch_headroom "N/A" yes 50 /tmp

# --- the refusal must SAY the numbers: an operator who is told only "too full"
#     cannot tell a 51% refusal from a 99% one, and the remedy differs. ---
msg="$(bench_assert_scratch_headroom 66 yes 50 /tmp 2>&1 >/dev/null || true)"
case "$msg" in
    *66%*50%*) ok "the refusal names both the reading and the ceiling" ;;
    *)         bad "the refusal does not name both numbers: $msg" ;;
esac
case "$msg" in
    *TMPDIR*)  ok "the refusal names a remedy the operator can act on" ;;
    *)         bad "the refusal offers no remedy" ;;
esac

# --- the readers, against this host's real filesystems ---
used_root="$(bench_fs_used_pct /)"
case "$used_root" in
    ''|*[!0-9]*) bad "bench_fs_used_pct / returned a non-number: '$used_root'" ;;
    *)           ok "bench_fs_used_pct reads an integer percent for /" ;;
esac

if [ -z "$(bench_fs_used_pct /no/such/path/at/all)" ]; then
    ok "bench_fs_used_pct is empty for a path that does not exist"
else
    bad "bench_fs_used_pct invented a reading for a missing path"
fi

# /dev/shm is tmpfs on every Linux this runs on; / is not. Both directions
# matter — a type check that answers "yes" everywhere would refuse every run.
if [ -d /dev/shm ]; then
    if [ "$(bench_fs_is_ram_backed /dev/shm)" = "yes" ]; then
        ok "bench_fs_is_ram_backed recognises /dev/shm as RAM-backed"
    else
        bad "bench_fs_is_ram_backed did not recognise /dev/shm as RAM-backed"
    fi
fi
if [ -z "$(bench_fs_is_ram_backed /)" ]; then
    ok "bench_fs_is_ram_backed does not claim / is RAM-backed"
else
    bad "bench_fs_is_ram_backed claimed / is RAM-backed"
fi

echo
echo "bench-tmpfs-guard: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
