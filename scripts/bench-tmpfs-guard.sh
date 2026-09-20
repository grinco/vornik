#!/usr/bin/env bash
# bench-tmpfs-guard.sh — refuse to start a long benchmark pass when its scratch
# filesystem is RAM-backed and already full enough to get the pass killed.
#
# Sourced by scripts/agentbench-reproduce.sh; exercised directly by
# scripts/bench-tmpfs-guard_test.sh.
#
# WHY THIS EXISTS.
#
# 2026-09-17: three background jobs — an agent-bench gold pass, its chained
# successor, and a daemon start — were killed with "the system is running low on
# memory" six hours into the pass. Nothing had leaked. Total RSS across all 617
# processes on the box was 5.5 GB; /tmp is a 16 GB tmpfs and held 11 GB of
# files, and tmpfs pages ARE resident memory. A stale 6.2 GB Go build cache
# nobody owned was most of it. Deleting it took /tmp from 66% to 26% and gave
# back 2 GB of available memory with nothing restarted.
#
# The loss was four gold batches, ~30 minutes, and it was recoverable only
# because batching had already shipped. The same pressure on an unbatched run
# costs the whole pass.
#
# THE CHECK IS ONE df, AND IT ONLY REFUSES WHEN THE SCRATCH IS RAM.
#
# A disk-backed scratch filling up is a different and much louder failure: the
# write fails, the step fails, and the operator sees ENOSPC. A RAM-backed one
# kills a neighbouring process instead, and the traceback lands on whatever the
# OOM killer picked — which is why the 2026-09-17 incident read as a daemon leak
# for the first hour. So the refusal is scoped to the case where the reading
# means something, and on a disk-backed scratch the guard reports and proceeds.
#
# The threshold is not tuned to the incident, it is BELOW it. The box died at
# 66% of a 16 GB tmpfs; refusing at 50% leaves a pass the room to grow into,
# because the pass itself is one of the things doing the growing (the same
# session's builds and bench artifacts are what pushed the cache over).

# bench_fs_used_pct PATH — print the integer percent-used of the filesystem
# holding PATH. Prints nothing when it cannot be read, and the caller treats
# "cannot read" as "do not refuse": a guard that cannot measure must not become
# a guard that always blocks.
#
# df -P is the POSIX output format — one line per filesystem, fixed column
# order — so the capacity column is $5 and not wherever a locale or a long
# device name put it.
bench_fs_used_pct() {
    [ -n "${1:-}" ] || return 0
    df -P "$1" 2>/dev/null | awk 'NR==2 { sub(/%$/, "", $5); if ($5 ~ /^[0-9]+$/) print $5 }'
}

# bench_fs_is_ram_backed PATH — print "yes" when PATH sits on a filesystem whose
# pages are resident memory, nothing otherwise.
#
# stat -f reports the filesystem TYPE, which is the property that matters, and
# is what distinguishes the two cases the guard treats differently. Matching on
# the mount point (/tmp) instead would be wrong in both directions: /tmp is disk
# on a default Debian install, and a deployment is free to point TMPDIR at a
# tmpfs that is not called /tmp — which is exactly what an operator does after
# being bitten once.
bench_fs_is_ram_backed() {
    [ -n "${1:-}" ] || return 0
    case "$(stat -f -c %T "$1" 2>/dev/null)" in
        tmpfs|ramfs) echo "yes" ;;
        *)           ;;
    esac
}

# bench_assert_scratch_headroom USED_PCT IS_RAM_BACKED THRESHOLD PATH
#
# The decision, split from the two readings above so it can be tested without a
# filesystem of the right type to hand. Returns 1 and explains on stderr when
# the scratch is RAM-backed and at or above THRESHOLD; returns 0 otherwise.
#
# An empty or non-numeric USED_PCT returns 0. That is deliberate and it is the
# same rule as the reader: this guard exists to stop a known-bad start, not to
# stop every start it cannot vouch for.
bench_assert_scratch_headroom() {
    _used="${1:-}"
    _ram="${2:-}"
    _threshold="${3:-50}"
    _path="${4:-the scratch filesystem}"

    case "$_used" in
        ''|*[!0-9]*) return 0 ;;
    esac
    [ "$_ram" = "yes" ] || return 0
    [ "$_used" -ge "$_threshold" ] || return 0

    echo "refusing: ${_path} is RAM-backed (tmpfs) and ${_used}% full, at or above the" >&2
    echo "  ${_threshold}% ceiling. Every byte in it is resident memory, so a long pass" >&2
    echo "  does not fail with ENOSPC — the kernel kills a process, and the one it picks" >&2
    echo "  is not necessarily this run (2026-09-17: a gold pass, its successor and a" >&2
    echo "  daemon start were all killed while total process RSS was 5.5 GB and /tmp held" >&2
    echo "  11 GB of files)." >&2
    echo "  Free it, or point TMPDIR at a disk-backed directory, then re-run:" >&2
    echo "    du -xh --max-depth=1 ${_path} | sort -h | tail" >&2
    echo "    export TMPDIR=/var/tmp" >&2
    return 1
}
