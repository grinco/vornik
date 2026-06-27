#!/usr/bin/env bash
#
# config-drift-check.sh — report divergence between the repo's tracked
# config source (configs/{swarms,workflows}) and the deployed tree the
# daemon actually reads ($VORNIK_CONFIGS_DIR, default
# ~/.config/vornik/configs).
#
# vornik reads ONLY the deployed copy (configs live in two trees). A
# deploy that ships a new binary but forgets to sync configs leaves the
# daemon running stale swarm/workflow definitions — exactly how the
# `network: daemon-only` lines ended up in HEAD but not in the deployed
# trading-swarm.md. This is a read-only DIAGNOSTIC: it never edits
# either tree. Some drift may be intentional host-specific tuning — the
# tool surfaces it; the operator decides.
#
# Usage: scripts/config-drift-check.sh [deployed-configs-dir]
# Exit: 0 = in sync, 1 = drift found, 2 = deployed tree missing.
set -euo pipefail

REPO_CONFIGS="$(cd "$(dirname "$0")/.." && pwd)/configs"
DEPLOYED="${1:-${VORNIK_CONFIGS_DIR:-$HOME/.config/vornik/configs}}"

if [ ! -d "$DEPLOYED" ]; then
	echo "config-drift-check: deployed configs dir not found: $DEPLOYED" >&2
	exit 2
fi

drift=0
for sub in swarms workflows; do
	src="$REPO_CONFIGS/$sub"
	[ -d "$src" ] || continue
	for f in "$src"/*.md; do
		[ -e "$f" ] || continue
		base="$(basename "$f")"
		dep="$DEPLOYED/$sub/$base"
		if [ ! -f "$dep" ]; then
			echo "MISSING (in repo, not deployed): $sub/$base"
			drift=1
		elif ! diff -q "$f" "$dep" >/dev/null 2>&1; then
			echo "DRIFT: $sub/$base"
			drift=1
		fi
	done
	# Deployed files with no repo counterpart (host-only or orphaned).
	for dep in "$DEPLOYED/$sub"/*.md; do
		[ -e "$dep" ] || continue
		base="$(basename "$dep")"
		[ -f "$src/$base" ] || echo "DEPLOYED-ONLY (not in repo): $sub/$base"
	done
done

if [ "$drift" -eq 0 ]; then
	echo "config-drift-check: repo configs/{swarms,workflows} in sync with $DEPLOYED"
else
	echo ""
	echo "config-drift-check: drift found. Review with:  diff configs/<sub>/<file> $DEPLOYED/<sub>/<file>"
	echo "If the repo copy is canonical, sync it into the deployed tree and reload; if the"
	echo "deployed copy is intentional host tuning, fold the change back into the repo."
fi
exit "$drift"
