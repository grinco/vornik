#!/usr/bin/env bash
#
# config-drift-check-all.sh — run the config drift check against EVERY deployed
# tree on this host, not just the default one.
#
# WHY THIS EXISTS. config-drift-check.sh takes ONE tree ($VORNIK_CONFIGS_DIR,
# default ~/.config/vornik/configs). That is correct for a host running one
# daemon and blind on a host running several. Measured 2026-08-18: this box runs
# a production daemon AND a benchmark daemon with its own tree at
# ~/.config/vornik-bench/configs, and that tree held 12 of the 26 shipped
# workflows. `make config-diff` reported clean throughout, because prod had all
# 26 and the bench tree was never examined — so the benchmark could not exercise
# half the surface it exists to measure, and nothing said so.
#
# Same root cause as the two incidents in the drift LLD: a set that must be kept
# in step is enumerated in one place and consulted in another. Here the missing
# enumeration is "which trees exist", and the answer is not a constant — it is
# whichever trees the daemons on this host actually read.
#
# DISCOVERY. Trees come from the systemd user units that run the daemons, which
# is the same discipline as verifying a deploy against the RUNNING process
# rather than against a file. A unit that sets VORNIK_CONFIGS_DIR names its own
# tree; one that does not uses the default. Hosts with no systemd (CI,
# containers) fall back to the single default tree, so this stays runnable
# everywhere.
#
# Read-only, like the checker it wraps: it never edits either tree.
#
# Usage:  scripts/config-drift-check-all.sh [tree ...]
# Exit:   0 = every tree in sync, 1 = drift in at least one, 2 = a tree missing.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CHECK="$SCRIPT_DIR/config-drift-check.sh"
DEFAULT_TREE="${VORNIK_CONFIGS_DIR:-$HOME/.config/vornik/configs}"

discover_trees() {
	# Explicit arguments win — tests and operators pin what they mean.
	if [ "$#" -gt 0 ]; then
		printf '%s\n' "$@"
		return
	fi
	if ! command -v systemctl >/dev/null 2>&1; then
		printf '%s\n' "$DEFAULT_TREE"
		return
	fi
	local units unit env_line tree found=0
	units="$(systemctl --user list-units --type=service --all --no-legend 'vornik*.service' 2>/dev/null | awk '{print $1}')"
	for unit in $units; do
		# A unit template or a stray socket has no Environment; skip quietly.
		env_line="$(systemctl --user show -p Environment "$unit" 2>/dev/null || true)"
		tree="$(printf '%s' "$env_line" | tr ' ' '\n' | sed -n 's/^VORNIK_CONFIGS_DIR=//p' | head -1)"
		[ -n "$tree" ] || tree="$DEFAULT_TREE"
		printf '%s\n' "$tree"
		found=1
	done
	# No vornik units on this host: check the default tree so the command still
	# means something rather than silently passing on an empty set.
	[ "$found" -eq 1 ] || printf '%s\n' "$DEFAULT_TREE"
}

worst=0
checked=0
while IFS= read -r tree; do
	[ -n "$tree" ] || continue
	checked=$((checked + 1))
	echo "=== config drift: $tree ==="
	"$CHECK" "$tree"
	rc=$?
	# 2 (missing tree) outranks 1 (drift): a tree the daemon reads but which is
	# absent is a worse fact than one that merely differs.
	if [ "$rc" -gt "$worst" ]; then worst=$rc; fi
	echo
done < <(discover_trees "$@" | awk '!seen[$0]++')

if [ "$checked" -eq 0 ]; then
	echo "config-drift-check-all: no deployed tree to check" >&2
	exit 2
fi
echo "config-drift-check-all: checked $checked tree(s), worst status $worst"
exit "$worst"
